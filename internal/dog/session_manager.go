// Package dog provides dog session management for Deacon's helper workers.
package dog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Session errors
var (
	ErrSessionRunning                = errors.New("session already running")
	ErrSessionNotFound               = errors.New("session not found")
	ErrSessionStartCleanupIncomplete = errors.New("session start cleanup incomplete")
	ErrSessionGenerationUnavailable  = errors.New("dog session generation is unavailable; preserve lifecycle custody for recovery")
)

const dogSessionTeardownTimeout = 15 * time.Second

type sessionGenerationController interface {
	CaptureSessionGeneration(string) (tmux.SessionGeneration, error)
	GetPaneID(string) (string, error)
	SendKeysRawGeneration(tmux.SessionGeneration, string) error
	KillSessionGenerationWithProcessesPortableContext(context.Context, tmux.SessionGeneration) error
}

// SessionManager handles dog session lifecycle.
type SessionManager struct {
	tmux                     *tmux.Tmux
	mgr                      *Manager
	townRoot                 string
	startSession             func(*tmux.Tmux, session.SessionConfig) (*session.StartResult, error)
	captureSessionGeneration func(string) (tmux.SessionGeneration, error)
	killSessionGeneration    func(context.Context, tmux.SessionGeneration) error
	controllerForGeneration  func(tmux.SessionGeneration) (sessionGenerationController, error)
	tmuxForGeneration        func(tmux.SessionGeneration) (*tmux.Tmux, error)
	persistSessionGeneration func(string, string, time.Time, *tmux.SessionGeneration, tmux.SessionGeneration) (bool, error)
}

// NewSessionManager creates a new dog session manager.
// The Manager parameter is used to sync persistent dog state (idle/working)
// when sessions start and stop.
func NewSessionManager(t *tmux.Tmux, townRoot string, mgr *Manager) *SessionManager {
	m := &SessionManager{
		tmux:                     t,
		mgr:                      mgr,
		townRoot:                 townRoot,
		startSession:             session.StartSession,
		captureSessionGeneration: t.CaptureSessionGeneration,
		killSessionGeneration:    t.KillSessionGenerationWithProcessesPortableContext,
		controllerForGeneration: func(generation tmux.SessionGeneration) (sessionGenerationController, error) {
			return tmux.NewTmuxForSessionGeneration(generation)
		},
		tmuxForGeneration: tmux.NewTmuxForSessionGeneration,
	}
	m.persistSessionGeneration = func(
		name string,
		expectedWork string,
		expectedStartedAt time.Time,
		expectedPrior *tmux.SessionGeneration,
		generation tmux.SessionGeneration,
	) (bool, error) {
		if m.mgr == nil {
			return false, errors.New("dog session generation store is unavailable")
		}
		return m.mgr.setSessionGenerationIfAssignmentMatchesLocked(
			name,
			expectedWork,
			expectedStartedAt,
			expectedPrior,
			generation,
		)
	}
	return m
}

// SessionStartOptions configures dog session startup.
type SessionStartOptions struct {
	// WorkDesc is the work description (formula or bead ID) for the startup prompt.
	WorkDesc string

	// AssignmentReceipt is the in-memory capability returned by the exact fresh
	// assignment transaction. It is unavailable from legacy durable state.
	AssignmentReceipt AssignmentStartReceipt

	// AgentOverride specifies an alternate agent (e.g., "gemini", "claude-haiku").
	AgentOverride string
}

// SessionInfo contains information about a running dog session.
type SessionInfo struct {
	// DogName is the dog name.
	DogName string `json:"dog_name"`

	// SessionID is the tmux session identifier.
	SessionID string `json:"session_id"`

	// Running indicates if the session is currently active.
	Running bool `json:"running"`

	// Attached indicates if someone is attached to the session.
	Attached bool `json:"attached,omitempty"`

	// Created is when the session was created.
	Created time.Time `json:"created,omitempty"`
}

// SessionName generates the tmux session name for a dog.
// Pattern: hq-dog-{name}
// Dogs are town-level (managed by deacon), so they use the hq- prefix.
// We use "hq-dog-" instead of "hq-deacon-" to avoid tmux prefix-matching
// collisions with the "hq-deacon" session.
func (m *SessionManager) SessionName(dogName string) string {
	return fmt.Sprintf("hq-dog-%s", dogName)
}

// kennelPath returns the path to the dog's kennel directory.
func (m *SessionManager) kennelPath(dogName string) string {
	return filepath.Join(m.townRoot, "deacon", "dogs", dogName)
}

// Start creates and starts a new session for a dog.
// Dogs run agent sessions that check mail for work and execute formulas.
func (m *SessionManager) Start(dogName string, opts SessionStartOptions) error {
	if err := validateDogName(dogName); err != nil {
		return err
	}
	if m.mgr == nil {
		return errors.New("dog session generation store is unavailable")
	}
	fl, err := m.mgr.lockDog(dogName)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	kennelDir := m.kennelPath(dogName)
	if _, err := os.Stat(kennelDir); os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrDogNotFound, dogName)
	}

	sessionID := m.SessionName(dogName)

	state, err := m.mgr.loadState(dogName)
	if err != nil {
		return fmt.Errorf("reading dog assignment before session start: %w", err)
	}
	if state.State != StateWorking || state.WorkStartedAt.IsZero() {
		return fmt.Errorf("dog %s has no active assignment for session start", dogName)
	}
	startTmux := m.tmux
	freshAssignment := state.SessionGeneration == nil
	if freshAssignment {
		if !state.SessionAbsenceProven {
			return ErrSessionGenerationUnavailable
		}
		if opts.WorkDesc != state.Work ||
			!opts.AssignmentReceipt.matches(dogName, state.Work, state.WorkStartedAt) {
			return errors.New("dog assignment changed or fresh start receipt is unavailable")
		}
	}
	var priorGeneration *tmux.SessionGeneration
	if state.SessionGeneration != nil {
		prior := state.SessionGeneration.Tmux()
		priorGeneration = &prior
		_, _, running, err := m.persistedSessionStatus(state.SessionGeneration)
		if err != nil {
			return fmt.Errorf("checking persisted dog session before start: %w", err)
		}
		if running {
			return fmt.Errorf("%w: %s", ErrSessionRunning, sessionID)
		}
		startTmux, err = m.tmuxForGeneration(prior)
		if err != nil {
			return fmt.Errorf("restoring persisted dog session transport: %w", err)
		}
	}

	// A same-name session is not attempt-owned. Preserve it for exact health or
	// recovery handling instead of killing it by reusable name during startup.
	running, err := startTmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking existing dog session: %w", err)
	}
	if running {
		return fmt.Errorf("%w: %s", ErrSessionRunning, sessionID)
	}

	restoreFreshAbsence := func() error {
		if !freshAssignment {
			return nil
		}
		state.SessionAbsenceProven = true
		state.UpdatedAt = time.Now()
		return m.mgr.saveState(dogName, state)
	}
	if freshAssignment {
		// Consume the durable no-session proof before creation. Any uncertain
		// startup outcome therefore leaves the dog recovery-blocked.
		state.SessionAbsenceProven = false
		state.UpdatedAt = time.Now()
		if err := m.mgr.saveState(dogName, state); err != nil {
			return fmt.Errorf("consuming dog session absence proof: %w", err)
		}
	}

	// Build instructions for the dog.
	// For plugin work, explicitly direct the dog to read mail for the full
	// plugin instructions rather than trying to locate the plugin locally.
	// This prevents dogs from scanning their worktree's plugins/ directory
	// and escalating "plugin not found" when the plugin is town-level.
	workInfo := ""
	if opts.WorkDesc != "" {
		if strings.HasPrefix(opts.WorkDesc, "plugin:") {
			pluginName := strings.TrimPrefix(opts.WorkDesc, "plugin:")
			workInfo = fmt.Sprintf(" Plugin %s dispatched — full instructions are in your mail. Do NOT look for the plugin locally; read mail instead.", pluginName)
		} else {
			workInfo = fmt.Sprintf(" Work assigned: %s.", opts.WorkDesc)
		}
	}
	instructions := fmt.Sprintf("I am Dog %s.%s IMPORTANT: If your hook is empty and you have no mail, WAIT — the dispatcher is still setting up your assignment. Do NOT search for work, scan directories, or take autonomous action. Check hook (`"+cli.Name()+" hook`) and mail (`"+cli.Name()+" mail inbox`). If neither has work, wait 10 seconds and re-check. Execute only assigned work. When done, run `"+cli.Name()+" dog done` — this clears your work and auto-terminates the session.", dogName, workInfo)

	// Use unified session lifecycle.
	theme := tmux.DogTheme()
	startResult, err := m.startSession(startTmux, session.SessionConfig{
		SessionID: sessionID,
		WorkDir:   kennelDir,
		Role:      "dog",
		TownRoot:  m.townRoot,
		AgentName: dogName,
		Beacon: session.BeaconConfig{
			Recipient: session.BeaconRecipient("dog", dogName, ""),
			Sender:    "deacon",
			Topic:     "assigned",
		},
		Instructions:   instructions,
		AgentOverride:  opts.AgentOverride,
		Theme:          &theme,
		WaitForAgent:   true,
		WaitFatal:      true,
		AcceptBypass:   true,
		ReadyDelay:     true,
		VerifySurvived: true,
		TrackPID:       true,
	})
	if err != nil {
		if errors.Is(err, tmux.ErrSessionCleanupUnreconciled) {
			return errors.Join(ErrSessionStartCleanupIncomplete, err)
		}
		if restoreErr := restoreFreshAbsence(); restoreErr != nil {
			return errors.Join(
				ErrSessionStartCleanupIncomplete,
				err,
				fmt.Errorf("restoring dog session absence proof: %w", restoreErr),
			)
		}
		return err
	}
	if startResult == nil {
		return fmt.Errorf("%w: session startup returned no generation receipt", ErrSessionStartCleanupIncomplete)
	}
	generation := startResult.SessionGeneration
	if err := validateSessionGenerationForDog(dogName, generation); err != nil {
		return fmt.Errorf("%w: invalid session creation receipt: %v", ErrSessionStartCleanupIncomplete, err)
	}
	persisted, err := m.persistSessionGeneration(
		dogName,
		state.Work,
		state.WorkStartedAt,
		priorGeneration,
		generation,
	)
	if err != nil || !persisted {
		if err == nil {
			err = errors.New("dog assignment or prior session generation changed during startup")
		}
		persistErr := fmt.Errorf("persisting dog session generation: %w", err)
		if killErr := m.teardownSessionGeneration(generation); killErr != nil {
			return errors.Join(
				ErrSessionStartCleanupIncomplete,
				persistErr,
				fmt.Errorf("rolling back created dog session generation: %w", killErr),
			)
		}
		if restoreErr := restoreFreshAbsence(); restoreErr != nil {
			return errors.Join(
				ErrSessionStartCleanupIncomplete,
				persistErr,
				fmt.Errorf("restoring dog session absence proof: %w", restoreErr),
			)
		}
		return persistErr
	}

	return nil
}

// Stop terminates a dog session.
func (m *SessionManager) Stop(dogName string, force bool) error {
	if m.mgr == nil {
		return errors.New("dog session generation store is unavailable")
	}
	snapshot, err := m.mgr.Get(dogName)
	if err != nil {
		return fmt.Errorf("reading dog state before stop: %w", err)
	}
	return m.StopIfMatches(snapshot, force)
}

// StopIfMatches stops only the exact durable dog snapshot supplied by the
// caller. Daemon patrols must use this form because a reusable dog name may be
// reassigned between their List snapshot and cleanup attempt.
func (m *SessionManager) StopIfMatches(snapshot *Dog, force bool) error {
	if m.mgr == nil {
		return errors.New("dog session generation store is unavailable")
	}
	if snapshot == nil || snapshot.Name == "" {
		return errors.New("dog stop snapshot is unavailable")
	}
	if snapshot.SessionGeneration == nil {
		return ErrSessionGenerationUnavailable
	}
	sessionID := m.SessionName(snapshot.Name)

	expected := snapshot.SessionGeneration.Tmux()
	controller, controllerErr := m.controllerForGeneration(expected)
	if controllerErr != nil {
		return fmt.Errorf("checking exact dog session transport: %w", controllerErr)
	}
	current, captureErr := controller.CaptureSessionGeneration(sessionID)
	teardown := func(generation tmux.SessionGeneration) error {
		ctx, cancel := context.WithTimeout(context.Background(), dogSessionTeardownTimeout)
		defer cancel()
		return controller.KillSessionGenerationWithProcessesPortableContext(ctx, generation)
	}
	if captureErr == nil {
		if !current.Equal(expected) {
			return tmux.ErrSessionGenerationChanged
		}
		teardown = func(generation tmux.SessionGeneration) error {
			if !force {
				_ = controller.SendKeysRawGeneration(generation, "C-c")
			}
			ctx, cancel := context.WithTimeout(context.Background(), dogSessionTeardownTimeout)
			defer cancel()
			return controller.KillSessionGenerationWithProcessesPortableContext(ctx, generation)
		}
	} else if !errors.Is(captureErr, tmux.ErrSessionNotFound) && !errors.Is(captureErr, tmux.ErrNoServer) {
		return fmt.Errorf("checking exact dog session: %w", captureErr)
	}

	var (
		stopped bool
		err     error
	)
	if snapshot.State == StateIdle && snapshot.Work == "" {
		stopped, err = m.mgr.RetireSessionWithTeardownIfMatches(snapshot.Name, expected, teardown)
	} else {
		stopped, err = m.mgr.CompleteWorkWithTeardownIfMatches(
			snapshot.Name,
			snapshot.Work,
			snapshot.WorkStartedAt,
			expected,
			teardown,
		)
	}
	if err != nil {
		return fmt.Errorf("stopping exact dog session: %w", err)
	}
	if !stopped {
		return errors.New("dog assignment or session generation changed during stop")
	}
	return nil
}

func (m *SessionManager) teardownSessionGeneration(generation tmux.SessionGeneration) error {
	controller, err := m.controllerForGeneration(generation)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dogSessionTeardownTimeout)
	defer cancel()
	return controller.KillSessionGenerationWithProcessesPortableContext(ctx, generation)
}

func (m *SessionManager) persistedSessionStatus(stored *SessionGeneration) (
	sessionGenerationController,
	tmux.SessionGeneration,
	bool,
	error,
) {
	if stored == nil {
		return nil, tmux.SessionGeneration{}, false, ErrSessionGenerationUnavailable
	}
	expected := stored.Tmux()
	controller, err := m.controllerForGeneration(expected)
	if err != nil {
		return nil, tmux.SessionGeneration{}, false, err
	}
	current, err := controller.CaptureSessionGeneration(expected.Name)
	if errors.Is(err, tmux.ErrSessionNotFound) || errors.Is(err, tmux.ErrNoServer) {
		return controller, expected, false, nil
	}
	if err != nil {
		return nil, tmux.SessionGeneration{}, false, err
	}
	if !current.Equal(expected) {
		return nil, tmux.SessionGeneration{}, false, tmux.ErrSessionGenerationChanged
	}
	return controller, expected, true, nil
}

func exactSessionPane(
	controller sessionGenerationController,
	expected tmux.SessionGeneration,
) (string, error) {
	pane, err := controller.GetPaneID(expected.Name)
	if err != nil {
		return "", err
	}
	current, err := controller.CaptureSessionGeneration(expected.Name)
	if err != nil {
		return "", err
	}
	if !current.Equal(expected) {
		return "", tmux.ErrSessionGenerationChanged
	}
	return pane, nil
}

// IsRunning checks if a dog session is active.
func (m *SessionManager) IsRunning(dogName string) (bool, error) {
	sessionID := m.SessionName(dogName)
	return m.tmux.HasSession(sessionID)
}

// Status returns detailed status for a dog session.
func (m *SessionManager) Status(dogName string) (*SessionInfo, error) {
	sessionID := m.SessionName(dogName)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}

	info := &SessionInfo{
		DogName:   dogName,
		SessionID: sessionID,
		Running:   running,
	}

	if !running {
		return info, nil
	}

	tmuxInfo, err := m.tmux.GetSessionInfo(sessionID)
	if err != nil {
		return info, nil
	}

	info.Attached = tmuxInfo.Attached

	return info, nil
}

// GetPane returns the pane ID for a dog session.
func (m *SessionManager) GetPane(dogName string) (string, error) {
	sessionID := m.SessionName(dogName)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return "", ErrSessionNotFound
	}

	// Get pane ID from session
	pane, err := m.tmux.GetPaneID(sessionID)
	if err != nil {
		return "", fmt.Errorf("getting pane: %w", err)
	}

	return pane, nil
}

// EnsureRunning ensures a dog session is running, starting it if needed.
// Returns the pane ID.
func (m *SessionManager) EnsureRunning(dogName string, opts SessionStartOptions) (string, error) {
	if m.mgr == nil {
		return "", errors.New("dog session generation store is unavailable")
	}
	state, err := m.mgr.Get(dogName)
	if err != nil {
		return "", fmt.Errorf("reading dog state before ensuring session: %w", err)
	}
	if state.SessionGeneration != nil {
		controller, expected, running, statusErr := m.persistedSessionStatus(state.SessionGeneration)
		if statusErr != nil {
			return "", fmt.Errorf("checking persisted dog session: %w", statusErr)
		}
		if running {
			pane, paneErr := exactSessionPane(controller, expected)
			if paneErr != nil {
				return "", fmt.Errorf("getting exact dog pane: %w", paneErr)
			}
			return pane, nil
		}
	}

	if err := m.Start(dogName, opts); err != nil {
		return "", err
	}
	state, err = m.mgr.Get(dogName)
	if err != nil {
		return "", fmt.Errorf("reading dog state after session start: %w", err)
	}
	controller, expected, running, err := m.persistedSessionStatus(state.SessionGeneration)
	if err != nil {
		return "", fmt.Errorf("checking started dog session: %w", err)
	}
	if !running {
		return "", ErrSessionNotFound
	}
	pane, err := exactSessionPane(controller, expected)
	if err != nil {
		return "", fmt.Errorf("getting started dog pane: %w", err)
	}
	return pane, nil
}
