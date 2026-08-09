package witness

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Common errors
var (
	ErrNotRunning     = errors.New("witness not running")
	ErrAlreadyRunning = errors.New("witness already running")
)

const witnessCleanupTimeout = 10 * time.Second

type teardownBudgets struct {
	Init       time.Duration
	Broker     time.Duration
	Supervisor time.Duration
	Tmux       time.Duration
	Poller     time.Duration
}

func (budgets teardownBudgets) commitTimeout() time.Duration {
	return budgets.Init + budgets.Broker + budgets.Supervisor + budgets.Tmux
}

var witnessTeardownBudgets = teardownBudgets{
	Init:       3 * time.Second,
	Broker:     2 * time.Second,
	Supervisor: 3 * time.Second,
	Tmux:       2 * time.Second,
	Poller:     3 * time.Second,
}

// Manager handles witness lifecycle and monitoring operations.
// ZFC-compliant: tmux session is the source of truth for running state.
type Manager struct {
	rig                    *rig.Rig
	tmux                   *tmux.Tmux
	capturePaneGeneration  func(tmux.SessionGeneration) (tmux.PaneProcessGeneration, error)
	prepareSessionCleanup  func(tmux.SessionGeneration, tmux.PaneProcessGeneration) (sessionGenerationCleanup, error)
	prepareExplicitCleanup func(tmux.SessionGeneration, tmux.PaneProcessGeneration) (sessionGenerationCleanup, error)
	wrapSessionCommand     func(string) (string, string, error)
	startPoller            func(string, string) (int, error)
	cleanupTimeout         time.Duration
	teardownBudgets        teardownBudgets
	zombieKillGrace        time.Duration
	pendingCleanupMu       sync.Mutex
	pendingCleanups        []sessionGenerationCleanup
}

type sessionGenerationCleanup interface {
	PrepareCommit(context.Context) (func(context.Context) (bool, error), error)
	Close() error
}

func (m *Manager) closeOrRetainCleanup(cleanup sessionGenerationCleanup) error {
	if cleanup == nil {
		return nil
	}
	if err := cleanup.Close(); err != nil {
		m.pendingCleanupMu.Lock()
		m.pendingCleanups = append(m.pendingCleanups, cleanup)
		m.pendingCleanupMu.Unlock()
		return err
	}
	return nil
}

func (m *Manager) retryPendingCleanups() error {
	m.pendingCleanupMu.Lock()
	pending := m.pendingCleanups
	m.pendingCleanups = nil
	m.pendingCleanupMu.Unlock()
	if len(pending) == 0 {
		return nil
	}

	var errs []error
	var failed []sessionGenerationCleanup
	for _, cleanup := range pending {
		if err := cleanup.Close(); err != nil {
			errs = append(errs, err)
			failed = append(failed, cleanup)
		}
	}
	if len(failed) != 0 {
		m.pendingCleanupMu.Lock()
		m.pendingCleanups = append(failed, m.pendingCleanups...)
		m.pendingCleanupMu.Unlock()
	}
	return errors.Join(errs...)
}

// NewManager creates a new witness manager for a rig.
func NewManager(r *rig.Rig) *Manager {
	return NewManagerWithTmux(r, tmux.NewTmux())
}

// NewManagerWithTmux creates a witness manager on the caller's tmux transport.
func NewManagerWithTmux(r *rig.Rig, t *tmux.Tmux) *Manager {
	return &Manager{
		rig:                   r,
		tmux:                  t,
		capturePaneGeneration: t.CapturePaneProcessGeneration,
		startPoller:           nudge.StartPoller,
		cleanupTimeout:        witnessCleanupTimeout,
		teardownBudgets:       witnessTeardownBudgets,
		zombieKillGrace:       constants.ZombieKillGracePeriod,
		prepareSessionCleanup: func(
			generation tmux.SessionGeneration,
			paneGeneration tmux.PaneProcessGeneration,
		) (sessionGenerationCleanup, error) {
			return t.PrepareSessionGenerationProcessCleanup(generation, paneGeneration)
		},
		prepareExplicitCleanup: func(
			generation tmux.SessionGeneration,
			paneGeneration tmux.PaneProcessGeneration,
		) (sessionGenerationCleanup, error) {
			return t.PrepareSessionGenerationExplicitCleanup(generation, paneGeneration)
		},
		wrapSessionCommand: func(command string) (string, string, error) {
			executable, err := os.Executable()
			if err != nil || !tmux.CanWrapCurrentExecutable(executable) {
				return command, "", nil
			}
			return tmux.WrapSessionCommandWithCustody(executable, command)
		},
	}
}

func appendWitnessCustodyExecutable(paths []string, command, pathEnv string) []string {
	command = strings.TrimSpace(command)
	if command == "" {
		return paths
	}
	resolved := command
	if !filepath.IsAbs(resolved) {
		for _, dir := range filepath.SplitList(pathEnv) {
			candidate := filepath.Join(dir, command)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				resolved = candidate
				break
			}
		}
		if !filepath.IsAbs(resolved) {
			if candidate, err := exec.LookPath(command); err == nil {
				resolved = candidate
			}
		}
	}
	if !filepath.IsAbs(resolved) {
		return paths
	}
	resolved, _ = filepath.Abs(resolved)
	if canonical, err := filepath.EvalSymlinks(resolved); err == nil {
		resolved = canonical
	}
	for _, systemRoot := range []string{"/bin", "/sbin", "/usr", "/opt"} {
		if resolved == systemRoot || strings.HasPrefix(resolved, systemRoot+string(filepath.Separator)) {
			return paths
		}
	}
	return append(paths, filepath.Dir(resolved))
}

func witnessSessionCustodyPaths(witnessDir, runtimeConfigDir, witnessSettingsDir string, runtimeConfig *config.RuntimeConfig, envVars map[string]string) (string, error) {
	paths := []string{witnessDir, witnessSettingsDir}
	for _, path := range []string{runtimeConfigDir, envVars["CLAUDE_CONFIG_DIR"], envVars["CODEX_HOME"], envVars["GT_CONTEXT_FILE"]} {
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	pathEnv := envVars["PATH"]
	if pathEnv == "" {
		pathEnv = os.Getenv("PATH")
	}
	if runtimeConfig == nil {
		return "", errors.New("resolved Witness runtime configuration is unavailable")
	}
	paths = appendWitnessCustodyExecutable(paths, runtimeConfig.Command, pathEnv)
	if len(runtimeConfig.ExecWrapper) != 0 {
		paths = appendWitnessCustodyExecutable(paths, runtimeConfig.ExecWrapper[0], pathEnv)
	}
	if executable, err := os.Executable(); err == nil {
		paths = appendWitnessCustodyExecutable(paths, executable, pathEnv)
	}

	// Privileged contained-flow tests use disposable executables and evidence
	// paths outside witnessDir. Admit only their absolute test-scoped values;
	// production runtime overrides cannot expand this allowlist.
	if strings.HasSuffix(os.Args[0], ".test") {
		for key, value := range envVars {
			if (strings.HasPrefix(key, "GT_TEST_FLOW_") || key == "BD_PATH") && key != "GT_TEST_FLOW_OUTER_PATH" && filepath.IsAbs(value) {
				info, err := os.Stat(value)
				if err == nil && !info.IsDir() {
					value = filepath.Dir(value)
				}
				paths = append(paths, value)
			}
		}
	}

	return tmux.EncodeSessionCustodyPaths(paths)
}

func (m *Manager) revalidateSessionGenerationUnderLease(
	generation tmux.SessionGeneration,
	paneGeneration tmux.PaneProcessGeneration,
	requireDead bool,
) error {
	alive, err := m.tmux.IsAgentAliveChecked(generation.SessionID)
	if err != nil {
		return fmt.Errorf("rechecking witness liveness under delivery lease: %w", err)
	}
	if requireDead && alive {
		return ErrAlreadyRunning
	}
	currentGeneration, err := m.tmux.CaptureSessionGeneration(generation.Name)
	if err != nil {
		return fmt.Errorf("rechecking witness session identity under delivery lease: %w", err)
	}
	if !currentGeneration.Equal(generation) {
		return tmux.ErrSessionGenerationChanged
	}
	currentPaneGeneration, err := m.capturePaneGeneration(currentGeneration)
	if err != nil {
		return fmt.Errorf("rechecking witness pane identity under delivery lease: %w", err)
	}
	if !currentPaneGeneration.Equal(paneGeneration) {
		return tmux.ErrSessionGenerationChanged
	}
	return nil
}

func (m *Manager) teardownSessionGeneration(
	townRoot, sessionID string,
	generation tmux.SessionGeneration,
	paneGeneration tmux.PaneProcessGeneration,
	pollerGeneration nudge.PollerGeneration,
	requireDead bool,
) error {
	if err := m.retryPendingCleanups(); err != nil {
		return fmt.Errorf("reconciling retained session cleanup ownership: %w", err)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), m.cleanupTimeout)
	defer cancelCleanup()
	releaseDeliveryLease, err := m.tmux.AcquireNudgeLeaseContext(cleanupCtx, townRoot, sessionID)
	if err != nil {
		return fmt.Errorf("acquiring session teardown delivery lease: %w", err)
	}
	defer releaseDeliveryLease()

	var cleanup sessionGenerationCleanup
	closeCleanup := func() error {
		if cleanup == nil {
			return nil
		}
		err := m.closeOrRetainCleanup(cleanup)
		cleanup = nil
		return err
	}
	replaceErr := nudge.ReplaceBeforeStoppingPollerGenerationOutcomeContext(
		cleanupCtx,
		townRoot,
		sessionID,
		pollerGeneration,
		m.teardownBudgets.commitTimeout(),
		m.teardownBudgets.Poller,
		func(ctx context.Context) (nudge.PollerReplacementOutcomeCommit, error) {
			if err := m.revalidateSessionGenerationUnderLease(generation, paneGeneration, requireDead); err != nil {
				return nil, err
			}
			var err error
			cleanup, err = m.prepareSessionCleanup(generation, paneGeneration)
			if err != nil && !requireDead &&
				(errors.Is(err, tmux.ErrSessionCustodyUnsupported) || errors.Is(err, tmux.ErrProcessReferenceUnsupported)) {
				cleanup, err = m.prepareExplicitCleanup(generation, paneGeneration)
			}
			if err != nil {
				return nil, errors.Join(err, closeCleanup())
			}
			commit, err := cleanup.PrepareCommit(ctx)
			if err != nil {
				return nil, errors.Join(err, closeCleanup())
			}
			if commit == nil {
				return nil, errors.Join(errors.New("session cleanup preparation returned no commit operation"), closeCleanup())
			}
			return func(commitCtx context.Context) (nudge.PollerReplacementOutcome, error) {
				committed, commitErr := commit(commitCtx)
				return nudge.PollerReplacementOutcome{
					Committed: committed,
					Terminal:  committed && !errors.Is(commitErr, tmux.ErrSessionCleanupUnreconciled),
				}, errors.Join(commitErr, closeCleanup())
			}, nil
		},
	)
	// The normal commit path closes retained custody while both the delivery
	// lease and poller lifecycle lock are held. This final close is a defensive
	// pre-commit/error-path release while the delivery lease is still held.
	return errors.Join(replaceErr, closeCleanup())
}

func (m *Manager) stopPollerForAbsentSession(
	townRoot, sessionID string,
	pollerGeneration nudge.PollerGeneration,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.teardownBudgets.Poller)
	defer cancel()
	releaseDeliveryLease, err := m.tmux.AcquireNudgeLeaseContext(ctx, townRoot, sessionID)
	if err != nil {
		return err
	}
	defer releaseDeliveryLease()
	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("rechecking absent witness session under delivery lease: %w", err)
	}
	if running {
		return tmux.ErrSessionGenerationChanged
	}
	return nudge.StopPollerGeneration(townRoot, sessionID, pollerGeneration)
}

func (m *Manager) rollbackFailedStart(
	townRoot, sessionID string,
	generation tmux.SessionGeneration,
	paneGeneration tmux.PaneProcessGeneration,
	pollerGeneration nudge.PollerGeneration,
	startupErr, captureErr error,
) error {
	if captureErr != nil {
		return fmt.Errorf("waiting for witness to start; preserving unprovable session: %w", errors.Join(startupErr, captureErr))
	}
	rollbackErr := m.teardownSessionGeneration(
		townRoot,
		sessionID,
		generation,
		paneGeneration,
		pollerGeneration,
		false,
	)
	return fmt.Errorf("waiting for witness to start: %w", errors.Join(startupErr, rollbackErr))
}

// IsRunning checks if the witness session is active and healthy.
// Checks both tmux session existence AND agent process liveness to avoid
// reporting zombie sessions (tmux alive but Claude dead) as "running".
// ZFC: tmux session existence is the source of truth for session state,
// but agent liveness determines if the session is actually functional.
func (m *Manager) IsRunning() (bool, error) {
	t := tmux.NewTmux()
	status := t.CheckSessionHealth(m.SessionName(), 0)
	return status == tmux.SessionHealthy, nil
}

// IsHealthy checks if the witness is running and has been active recently.
// Unlike IsRunning which only checks process liveness, this also detects hung
// sessions where Claude is alive but hasn't produced output in maxInactivity.
// Returns the detailed ZombieStatus for callers that need to distinguish
// between different failure modes.
func (m *Manager) IsHealthy(maxInactivity time.Duration) tmux.ZombieStatus {
	t := tmux.NewTmux()
	return t.CheckSessionHealth(m.SessionName(), maxInactivity)
}

// SessionName returns the tmux session name for this witness.
func (m *Manager) SessionName() string {
	return session.WitnessSessionName(session.PrefixFor(m.rig.Name))
}

// Status returns information about the witness session.
// ZFC-compliant: tmux session is the source of truth.
func (m *Manager) Status() (*tmux.SessionInfo, error) {
	t := tmux.NewTmux()
	sessionID := m.SessionName()

	running, err := t.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return nil, ErrNotRunning
	}

	return t.GetSessionInfo(sessionID)
}

// witnessDir returns the working directory for the witness.
// Prefers witness/rig/ for existing legacy clones, otherwise uses witness/.
func (m *Manager) witnessDir() string {
	witnessRigDir := filepath.Join(m.rig.Path, "witness", "rig")
	if _, err := os.Stat(witnessRigDir); err == nil {
		return witnessRigDir
	}

	return filepath.Join(m.rig.Path, "witness")
}

func (m *Manager) prepareWitnessDir(townRoot string) (string, error) {
	witnessDir := m.witnessDir()
	if err := os.MkdirAll(witnessDir, 0755); err != nil {
		return "", fmt.Errorf("creating witness dir: %w", err)
	}
	if err := beads.SetupRedirect(townRoot, witnessDir); err != nil {
		return "", fmt.Errorf("ensuring witness beads redirect: %w", err)
	}
	return witnessDir, nil
}

// Start starts the witness.
// If foreground is true, returns an error (foreground mode deprecated).
// Otherwise, spawns a Claude agent in a tmux session.
// agentOverride optionally specifies a different agent alias to use.
// envOverrides are KEY=VALUE pairs that override all other env var sources.
// ZFC-compliant: no state file, tmux session is source of truth.
func (m *Manager) Start(foreground bool, agentOverride string, envOverrides []string) error {
	t := m.tmux
	sessionID := m.SessionName()
	townRoot := m.townRoot()

	if foreground {
		// Foreground mode is deprecated - patrol logic moved to mol-witness-patrol
		return fmt.Errorf("foreground mode is deprecated; use background mode (remove --foreground flag)")
	}

	// Check if session already exists
	running, err := t.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking witness session: %w", err)
	}
	if running {
		generation, err := t.CaptureSessionGeneration(sessionID)
		if err != nil {
			return fmt.Errorf("reading witness session identity: %w", err)
		}
		paneGeneration, err := m.capturePaneGeneration(generation)
		if err != nil {
			return fmt.Errorf("reading witness pane process identity: %w", err)
		}
		pollerGeneration, err := nudge.CapturePollerGeneration(townRoot, sessionID)
		if err != nil {
			return fmt.Errorf("reading witness poller generation: %w", err)
		}

		// Session exists - check if Claude is actually running (healthy vs zombie)
		alive, err := t.IsAgentAliveChecked(generation.SessionID)
		if err != nil {
			return fmt.Errorf("checking witness liveness: %w", err)
		}
		if alive {
			// Healthy - Claude is running
			return ErrAlreadyRunning
		}
		// Zombie detected — tmux alive but agent dead. Wait briefly, then
		// re-verify the immutable tmux generation before destructive cleanup.
		time.Sleep(m.zombieKillGrace)

		if err := m.teardownSessionGeneration(townRoot, sessionID, generation, paneGeneration, pollerGeneration, true); err != nil {
			return fmt.Errorf("killing zombie session: %w", err)
		}
	}

	// Note: No PID check per ZFC - tmux session is the source of truth

	// Ensure runtime settings exist in the shared witness parent directory.
	// Settings are passed to Claude Code via --settings flag.
	// ResolveRoleAgentConfig is internally serialized (resolveConfigMu in
	// package config) to prevent concurrent rig starts from corrupting the
	// global agent registry.
	// Working directory
	witnessDir, err := m.prepareWitnessDir(townRoot)
	if err != nil {
		return err
	}

	// Resolve CLAUDE_CONFIG_DIR from accounts.json so witness sessions
	// use the correct account. Mirrors the daemon restart path (lifecycle.go).
	accountsPath := constants.MayorAccountsPath(townRoot)
	runtimeConfigDir, _, _ := config.ResolveAccountConfigDir(accountsPath, "")
	if runtimeConfigDir == "" {
		runtimeConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}

	runtimeConfig := config.ResolveRoleAgentConfig("witness", townRoot, m.rig.Path)
	witnessSettingsDir := config.RoleSettingsDir("witness", m.rig.Path)
	if err := runtime.EnsureSettingsForRole(witnessSettingsDir, witnessDir, "witness", runtimeConfig); err != nil {
		return fmt.Errorf("ensuring runtime settings: %w", err)
	}

	// Ensure .gitignore has required Gas Town patterns
	if err := rig.EnsureGitignorePatterns(witnessDir); err != nil {
		style.PrintWarning("could not update witness .gitignore: %v", err)
	}

	roleConfig, err := m.roleConfig()
	if err != nil {
		// Non-fatal: role config is optional. Log and continue with defaults.
		log.Printf("warning: could not load witness role config for %s: %v", m.rig.Name, err)
		roleConfig = nil
	}

	// Compute environment BEFORE creating the session so it can be passed to
	// tmux via -e flags. This ensures the initial shell — and any subprocesses
	// Claude spawns (notably bd) — inherit BEADS_DOLT_PORT and friends.
	// Setting env after session creation via SetEnvironment only affects newly
	// spawned panes, not the subprocess tree of the already-running pane (gt-neycp).
	envVars := config.AgentEnv(config.AgentEnvConfig{
		Role:             "witness",
		Rig:              m.rig.Name,
		TownRoot:         townRoot,
		RuntimeConfigDir: runtimeConfigDir,
		Agent:            agentOverride,
		SessionName:      sessionID,
	})
	envVars = session.MergeRuntimeLivenessEnv(envVars, runtimeConfig)

	// Generate the GASTA run ID for this witness session.
	runID := uuid.New().String()
	envVars["GT_RUN"] = runID

	// Apply role config env vars (non-fatal). Skip keys already set by AgentEnv
	// to prevent TOML env overriding the canonical qualified GT_ROLE.
	// See: https://github.com/steveyegge/gastown/issues/2492
	roleEnv := roleConfigEnvVars(roleConfig, townRoot, m.rig.Name)
	for key, value := range roleEnv {
		if _, alreadySet := envVars[key]; alreadySet {
			continue
		}
		envVars[key] = value
	}

	// Apply CLI env overrides last (highest priority).
	for _, override := range envOverrides {
		if key, value, ok := strings.Cut(override, "="); ok {
			envVars[key] = value
		}
	}

	// Build the runtime invocation without repeating env vars inline. The tmux
	// session environment is authoritative; containment may rewrite its network
	// endpoint values before the runtime starts.
	// NOTE: No gt prime injection needed - SessionStart hook handles it automatically.
	// Pass m.rig.Path so rig agent settings are honored (not town-level defaults)
	command, resolvedEnv, err := buildWitnessStartCommandWithEnv(m.rig.Path, m.rig.Name, townRoot, sessionID, agentOverride, roleConfig, runtimeConfigDir, envVars)
	if err != nil {
		return err
	}
	envVars = resolvedEnv
	command, custody, err := m.wrapSessionCommand(command)
	if err != nil {
		return fmt.Errorf("preparing witness session containment: %w", err)
	}
	if custody != "" {
		envVars[tmux.EnvSessionCustody] = custody
		allowedPaths, err := witnessSessionCustodyPaths(witnessDir, runtimeConfigDir, witnessSettingsDir, runtimeConfig, envVars)
		if err != nil {
			return fmt.Errorf("preparing witness filesystem containment: %w", err)
		}
		envVars[tmux.EnvSessionCustodyPaths] = allowedPaths
	}

	// Create session with command and env vars via -e flags so the initial
	// shell (and Claude's subprocesses) inherit them from the start.
	// See: https://github.com/anthropics/gastown/issues/280 (race condition fix)
	if err := t.NewSessionWithCommandAndEnv(sessionID, witnessDir, command, envVars); err != nil {
		return fmt.Errorf("creating tmux session: %w", err)
	}
	startupGeneration, generationErr := t.CaptureSessionGeneration(sessionID)
	var startupPaneGeneration tmux.PaneProcessGeneration
	var paneGenerationErr error
	if generationErr == nil {
		startupPaneGeneration, paneGenerationErr = m.capturePaneGeneration(startupGeneration)
	}
	startupPollerGeneration, pollerGenerationErr := nudge.CapturePollerGeneration(townRoot, sessionID)
	rollbackCaptureErr := errors.Join(generationErr, paneGenerationErr, pollerGenerationErr)

	// Apply Gas Town theming (non-fatal: theming failure doesn't affect operation)
	theme := tmux.ResolveSessionTheme(townRoot, m.rig.Name, "witness", "")
	_ = t.ConfigureGasTownSession(sessionID, theme, m.rig.Name, "witness", "witness")

	// Wait for Claude to start - fatal if Claude fails to launch
	if err := t.WaitForCommand(sessionID, constants.SupportedShells, constants.ClaudeStartTimeout); err != nil {
		return m.rollbackFailedStart(
			townRoot,
			sessionID,
			startupGeneration,
			startupPaneGeneration,
			startupPollerGeneration,
			err,
			rollbackCaptureErr,
		)
	}

	// Accept startup dialogs (workspace trust + bypass permissions) if they appear.
	if err := t.AcceptStartupDialogs(sessionID); err != nil {
		log.Printf("warning: accepting startup dialogs for %s: %v", sessionID, err)
	}

	// Track PID for defense-in-depth orphan cleanup (non-fatal)
	if err := session.TrackSessionPID(townRoot, sessionID, t); err != nil {
		log.Printf("warning: tracking session PID for %s: %v", sessionID, err)
	}

	// Start nudge-queue poller (gt-dgf). Claude's UserPromptSubmit hook only
	// drains when the agent submits a prompt. Idle agents never submit, so
	// queued nudges deadlock. The poller breaks the cycle by polling every 10s.
	startPoller := m.startPoller
	if startPoller == nil {
		startPoller = nudge.StartPoller
	}
	if _, pollerErr := startPoller(townRoot, sessionID); pollerErr != nil {
		log.Printf("warning: could not start nudge poller for %s: %v", sessionID, pollerErr)
	}

	_ = runtime.RunStartupFallback(t, sessionID, "witness", runtimeConfig)
	initialPrompt := session.BuildStartupPrompt(session.BeaconConfig{
		Recipient: session.BeaconRecipient("witness", "", m.rig.Name),
		Sender:    "deacon",
		Topic:     "patrol",
	}, "Run `gt prime --hook` and begin patrol.")
	_ = runtime.DeliverStartupPromptFallback(t, sessionID, initialPrompt, runtimeConfig, constants.ClaudeStartTimeout)

	// Stream witness's Claude Code JSONL conversation log to VictoriaLogs (opt-in).
	if os.Getenv("GT_LOG_AGENT_OUTPUT") == "true" && os.Getenv("GT_OTEL_LOGS_URL") != "" {
		if err := session.ActivateAgentLogging(sessionID, witnessDir, runID); err != nil {
			log.Printf("warning: agent log watcher setup failed for %s: %v", sessionID, err)
		}
	}

	// Record the agent instantiation event (GASTA root span).
	session.RecordAgentInstantiateFromDir(context.Background(), runID, runtimeConfig.ResolvedAgent,
		"witness", "witness", sessionID, m.rig.Name, townRoot, "", witnessDir)

	time.Sleep(constants.ShutdownNotifyDelay)

	return nil
}

func (m *Manager) roleConfig() (*beads.RoleConfig, error) {
	townRoot := m.townRoot()
	roleDef, err := config.LoadRoleDefinition(townRoot, m.rig.Path, "witness")
	if err != nil {
		return nil, fmt.Errorf("loading witness role config: %w", err)
	}
	return &beads.RoleConfig{
		SessionPattern: roleDef.Session.Pattern,
		WorkDirPattern: roleDef.Session.WorkDir,
		NeedsPreSync:   roleDef.Session.NeedsPreSync,
		StartCommand:   roleDef.Session.StartCommand,
		EnvVars:        roleDef.Env,
	}, nil
}

func (m *Manager) townRoot() string {
	townRoot, err := workspace.Find(m.rig.Path)
	if err != nil || townRoot == "" {
		return m.rig.Path
	}
	return townRoot
}

func roleConfigEnvVars(roleConfig *beads.RoleConfig, townRoot, rigName string) map[string]string {
	if roleConfig == nil || len(roleConfig.EnvVars) == 0 {
		return nil
	}
	expanded := make(map[string]string, len(roleConfig.EnvVars))
	for key, value := range roleConfig.EnvVars {
		expanded[key] = beads.ExpandRolePattern(value, townRoot, rigName, "", "witness", session.PrefixFor(rigName))
	}
	return expanded
}

func buildWitnessStartCommand(rigPath, rigName, townRoot, sessionName, agentOverride string, roleConfig *beads.RoleConfig, runtimeConfigDir string) (string, error) {
	command, _, err := buildWitnessStartCommandWithEnv(rigPath, rigName, townRoot, sessionName, agentOverride, roleConfig, runtimeConfigDir, nil)
	return command, err
}

func buildWitnessStartCommandWithEnv(rigPath, rigName, townRoot, sessionName, agentOverride string, roleConfig *beads.RoleConfig, runtimeConfigDir string, envVars map[string]string) (string, map[string]string, error) {
	if agentOverride != "" {
		roleConfig = nil
	}
	if roleConfig != nil && roleConfig.StartCommand != "" {
		rc := config.ResolveRoleAgentConfig("witness", townRoot, rigPath)
		if !config.IsResolvedAgentClaude(rc) {
			// Non-Claude agent: skip TOML start_command entirely.
			// Built-in role TOMLs hardcode "exec claude ..." which is wrong
			// for non-Claude agents. Fall through to BuildStartupCommandFromConfig
			// which uses the resolved agent's command and args.
		} else if !isBuiltinClaudeStartCommand(roleConfig.StartCommand) && !config.HasExplicitRoleAgent("witness", townRoot, rigPath) {
			// Custom (non-builtin) start_command with Claude agent and no explicit
			// role_agents mapping: use TOML pattern with template expansion.
			cmd := beads.ExpandRolePattern(roleConfig.StartCommand, townRoot, rigName, "", "witness", session.PrefixFor(rigName))
			if strings.HasPrefix(cmd, "exec ") {
				cmd = "exec env -u CLAUDECODE NODE_OPTIONS='' " + strings.TrimPrefix(cmd, "exec ")
			} else {
				cmd = "env -u CLAUDECODE NODE_OPTIONS='' " + cmd
			}
			return cmd, envVars, nil
		}
		// Non-Claude agent OR Claude with built-in start_command: fall
		// through to BuildStartupCommandFromConfig for proper agent and
		// model flag resolution.
	}
	initialPrompt := session.BuildStartupPrompt(session.BeaconConfig{
		Recipient: session.BeaconRecipient("witness", "", rigName),
		Sender:    "deacon",
		Topic:     "patrol",
	}, "Run `gt prime --hook` and begin patrol.")
	if envVars == nil {
		envVars = config.AgentEnv(config.AgentEnvConfig{
			Role:             "witness",
			Rig:              rigName,
			TownRoot:         townRoot,
			RuntimeConfigDir: runtimeConfigDir,
			Prompt:           initialPrompt,
			Topic:            "patrol",
			SessionName:      sessionName,
		})
	}
	command, resolvedEnv, err := config.BuildStartupCommandWithAgentOverrideInheritedEnv(envVars, rigPath, initialPrompt, agentOverride)
	if err != nil {
		return "", nil, fmt.Errorf("building startup command: %w", err)
	}
	return command, resolvedEnv, nil
}

// isBuiltinClaudeStartCommand returns true if the start_command is the
// built-in default from role TOMLs ("exec claude --dangerously-skip-permissions").
// Custom start_commands (e.g., "exec run --town {town}") return false.
func isBuiltinClaudeStartCommand(cmd string) bool {
	trimmed := strings.TrimPrefix(cmd, "exec ")
	return trimmed == "claude --dangerously-skip-permissions"
}

// Stop stops the witness.
// ZFC-compliant: tmux session is the source of truth.
func (m *Manager) Stop() error {
	t := m.tmux
	sessionID := m.SessionName()
	townRoot := m.townRoot()

	running, err := t.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking witness session: %w", err)
	}
	pollerGeneration, err := nudge.CapturePollerGeneration(townRoot, sessionID)
	if err != nil {
		return fmt.Errorf("reading witness poller generation: %w", err)
	}
	if !running {
		if err := m.stopPollerForAbsentSession(townRoot, sessionID, pollerGeneration); err != nil {
			return fmt.Errorf("stopping poller for absent witness session: %w", err)
		}
		return ErrNotRunning
	}
	generation, err := t.CaptureSessionGeneration(sessionID)
	if err != nil {
		return fmt.Errorf("reading witness session identity: %w", err)
	}
	paneGeneration, err := m.capturePaneGeneration(generation)
	if err != nil {
		return fmt.Errorf("reading witness pane process identity: %w", err)
	}
	if err := m.teardownSessionGeneration(townRoot, sessionID, generation, paneGeneration, pollerGeneration, false); err != nil {
		return fmt.Errorf("stopping witness session: %w", err)
	}
	return nil
}
