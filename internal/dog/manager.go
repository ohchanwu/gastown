package dog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Common errors
var (
	ErrDogExists   = errors.New("dog already exists")
	ErrDogNotFound = errors.New("dog not found")
	ErrDogWorking  = errors.New("dog is currently working")
	ErrNoRigs      = errors.New("no rigs configured")
	ErrInvalidName = errors.New("invalid dog name")
)

// Manager handles dog lifecycle in the kennel.
type Manager struct {
	townRoot            string
	kennelPath          string // ~/gt/deacon/dogs/
	rigsConfig          *config.RigsConfig
	assignmentFinalizer func(name, work string, startedAt time.Time) error
}

// NewManager creates a new dog manager.
func NewManager(townRoot string, rigsConfig *config.RigsConfig) *Manager {
	m := &Manager{
		townRoot:   townRoot,
		kennelPath: filepath.Join(townRoot, "deacon", "dogs"),
		rigsConfig: rigsConfig,
	}
	m.assignmentFinalizer = m.archivePluginAssignmentMails
	return m
}

// lockDog acquires an exclusive file lock for a specific dog's full lifecycle.
// Locks live outside the removable dog directory so removal cannot unlink the
// held inode and let a same-name replacement acquire a different lock.
// Caller must defer fl.Unlock().
func (m *Manager) lockDog(name string) (*flock.Flock, error) {
	lockDir := filepath.Join(m.kennelPath, ".locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating dog lock dir: %w", err)
	}
	lockPath := filepath.Join(lockDir, fmt.Sprintf("dog-%s.lock", name))
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("acquiring dog lock for %s: %w", name, err)
	}
	return fl, nil
}

// validateDogName checks that a dog name is safe for use as a directory name.
func validateDogName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name cannot be empty", ErrInvalidName)
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("%w: name cannot contain path separators", ErrInvalidName)
	}
	if name == "." || name == ".." || strings.Contains(name, "..") {
		return fmt.Errorf("%w: name cannot contain path traversal", ErrInvalidName)
	}
	return nil
}

// dogDir returns the directory for a dog.
func (m *Manager) dogDir(name string) string {
	return filepath.Join(m.kennelPath, name)
}

// exists checks if a dog exists.
func (m *Manager) exists(name string) bool {
	_, err := os.Stat(m.dogDir(name))
	return err == nil
}

// stateFilePath returns the path to a dog's state file.
func (m *Manager) stateFilePath(name string) string {
	return filepath.Join(m.dogDir(name), ".dog.json")
}

// Add creates a new dog in the kennel with worktrees into each rig.
// Each dog gets a worktree per rig (e.g., dogs/alpha/gastown/, dogs/alpha/beads/).
// Worktrees are created from each rig's bare repo (.repo.git) or mayor/rig.
func (m *Manager) Add(name string) (*Dog, error) {
	if err := validateDogName(name); err != nil {
		return nil, err
	}
	fl, err := m.lockDog(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fl.Unlock() }()

	if m.exists(name) {
		return nil, ErrDogExists
	}

	// Verify we have rigs to create worktrees into
	if len(m.rigsConfig.Rigs) == 0 {
		return nil, ErrNoRigs
	}

	dogPath := m.dogDir(name)

	// Create kennel dir if needed
	if err := os.MkdirAll(m.kennelPath, 0755); err != nil {
		return nil, fmt.Errorf("creating kennel dir: %w", err)
	}

	// Create dog directory
	if err := os.MkdirAll(dogPath, 0755); err != nil {
		return nil, fmt.Errorf("creating dog dir: %w", err)
	}

	// Track cleanup on failure
	cleanup := func() { _ = os.RemoveAll(dogPath) }
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()

	// Create worktrees into each rig
	worktrees := make(map[string]string)
	for rigName := range m.rigsConfig.Rigs {
		worktreePath, err := m.createRigWorktree(dogPath, name, rigName)
		if err != nil {
			return nil, fmt.Errorf("creating worktree for rig %s: %w", rigName, err)
		}
		worktrees[rigName] = worktreePath
	}

	// Create initial state file
	now := time.Now()
	state := &DogState{
		Name:       name,
		State:      StateIdle,
		LastActive: now,
		Worktrees:  worktrees,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := m.saveState(name, state); err != nil {
		return nil, fmt.Errorf("saving state: %w", err)
	}

	success = true
	return &Dog{
		Name:       name,
		State:      StateIdle,
		Path:       dogPath,
		Worktrees:  worktrees,
		LastActive: now,
		CreatedAt:  now,
	}, nil
}

// createRigWorktree creates a worktree for a dog into a specific rig.
// Uses the rig's bare repo (.repo.git) if available, otherwise mayor/rig.
// Branch naming: dog/<dog-name>-<rig>-<timestamp> for uniqueness.
func (m *Manager) createRigWorktree(dogPath, dogName, rigName string) (string, error) {
	rigPath := filepath.Join(m.townRoot, rigName)
	worktreePath := filepath.Join(dogPath, rigName)

	// Find the repo base (bare repo or mayor/rig)
	repoGit, err := m.findRepoBase(rigPath)
	if err != nil {
		return "", fmt.Errorf("finding repo base for %s: %w", rigName, err)
	}

	// Determine the start point for the new worktree
	// Use origin/<default-branch> to ensure we start from the rig's configured branch
	defaultBranch := "main"
	if rigCfg, err := rig.LoadRigConfig(rigPath); err == nil && rigCfg.DefaultBranch != "" {
		defaultBranch = rigCfg.DefaultBranch
	}
	startPoint := fmt.Sprintf("origin/%s", defaultBranch)

	// Unique branch per dog-rig combination
	branchName := fmt.Sprintf("dog/%s-%s-%d", dogName, rigName, time.Now().UnixMilli())

	// Create worktree with new branch from default branch
	if err := repoGit.WorktreeAddFromRef(worktreePath, branchName, startPoint); err != nil {
		return "", fmt.Errorf("creating worktree from %s: %w", startPoint, err)
	}

	return worktreePath, nil
}

// findRepoBase locates the git repo base for a rig.
// Prefers .repo.git (bare repo), falls back to mayor/rig.
func (m *Manager) findRepoBase(rigPath string) (*git.Git, error) {
	// Check for shared bare repo
	bareRepoPath := filepath.Join(rigPath, ".repo.git")
	if info, err := os.Stat(bareRepoPath); err == nil && info.IsDir() {
		return git.NewGitWithDir(bareRepoPath, ""), nil
	}

	// Fall back to mayor/rig
	mayorPath := filepath.Join(rigPath, "mayor", "rig")
	if _, err := os.Stat(mayorPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("no repo base found (neither .repo.git nor mayor/rig exists)")
	}
	return git.NewGit(mayorPath), nil
}

// Remove deletes a dog from the kennel.
// Removes all worktrees and the dog directory.
func (m *Manager) Remove(name string) error {
	if err := validateDogName(name); err != nil {
		return err
	}
	if !m.exists(name) {
		return ErrDogNotFound
	}
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()
	return m.removeLocked(name)
}

// RemoveIfMatches removes only the exact idle lifecycle snapshot supplied by
// the caller. State, assignment activity, and session generation are compared
// under the same per-dog lock that guards dispatch and closeout, so a stale
// reaper cannot delete a replacement assignment or runtime generation.
func (m *Manager) RemoveIfMatches(snapshot *Dog) (bool, error) {
	if snapshot == nil {
		return false, errors.New("dog removal snapshot is unavailable")
	}
	if err := validateDogName(snapshot.Name); err != nil {
		return false, err
	}
	if !m.exists(snapshot.Name) {
		return false, ErrDogNotFound
	}
	fl, err := m.lockDog(snapshot.Name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(snapshot.Name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if !dogStateMatchesRemovalSnapshot(state, snapshot) {
		return false, nil
	}
	if err := m.removeLocked(snapshot.Name); err != nil {
		return false, err
	}
	return true, nil
}

func dogStateMatchesRemovalSnapshot(state *DogState, snapshot *Dog) bool {
	if state == nil || snapshot == nil || snapshot.State != StateIdle || snapshot.Work != "" || snapshot.SessionGeneration != nil {
		return false
	}
	return state.Name == snapshot.Name &&
		state.State == snapshot.State &&
		state.Work == snapshot.Work &&
		state.WorkStartedAt.Equal(snapshot.WorkStartedAt) &&
		state.LastActive.Equal(snapshot.LastActive) &&
		state.SessionGeneration == nil
}

func (m *Manager) removeLocked(name string) error {

	dogPath := m.dogDir(name)

	// Load state to get worktree paths
	state, err := m.loadState(name)
	if err != nil {
		// State file may be missing, proceed with cleanup
		state = &DogState{Worktrees: make(map[string]string)}
	}

	// Remove worktrees from each rig
	for rigName, worktreePath := range state.Worktrees {
		rigPath := filepath.Join(m.townRoot, rigName)
		repoGit, err := m.findRepoBase(rigPath)
		if err != nil {
			// Log but continue with other rigs
			style.PrintWarning("could not find repo base for %s: %v", rigName, err)
			continue
		}

		// Try to remove worktree properly
		if err := repoGit.WorktreeRemove(worktreePath, true); err != nil {
			// Log but continue - will remove directory below
			style.PrintWarning("could not remove worktree %s: %v", worktreePath, err)
		}

		// Prune stale entries
		_ = repoGit.WorktreePrune()
	}

	// Remove dog directory
	if err := os.RemoveAll(dogPath); err != nil {
		return fmt.Errorf("removing dog dir: %w", err)
	}

	return nil
}

// List returns all dogs in the kennel.
func (m *Manager) List() ([]*Dog, error) {
	entries, err := os.ReadDir(m.kennelPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading kennel: %w", err)
	}

	var dogs []*Dog
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dog, err := m.Get(entry.Name())
		if err != nil {
			continue // Skip invalid dogs
		}
		dogs = append(dogs, dog)
	}

	return dogs, nil
}

// Get returns a specific dog by name.
// Returns ErrDogNotFound if the dog directory or .dog.json state file doesn't exist.
func (m *Manager) Get(name string) (*Dog, error) {
	if err := validateDogName(name); err != nil {
		return nil, err
	}
	if !m.exists(name) {
		return nil, ErrDogNotFound
	}

	state, err := m.loadState(name)
	if err != nil {
		// No .dog.json means this isn't a valid dog worker
		// (e.g., "boot" is the boot watchdog using .boot-status.json, not a dog)
		return nil, ErrDogNotFound
	}

	return &Dog{
		Name:              name,
		State:             state.State,
		Path:              m.dogDir(name),
		Worktrees:         state.Worktrees,
		LastActive:        state.LastActive,
		Work:              state.Work,
		WorkStartedAt:     state.WorkStartedAt,
		CreatedAt:         state.CreatedAt,
		SessionGeneration: state.SessionGeneration,
	}, nil
}

// SetState updates a dog's state and last-active timestamp.
func (m *Manager) SetState(name string, state State) error {
	if err := validateDogName(name); err != nil {
		return err
	}
	if !m.exists(name) {
		return ErrDogNotFound
	}

	// Acquire per-dog lock to prevent concurrent load-modify-save races
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	dogState, err := m.loadState(name)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	if state == StateIdle &&
		(dogState.State == StateWorking || dogState.Work != "" || dogState.SessionGeneration != nil) {
		return ErrDogWorking
	}

	dogState.State = state
	dogState.LastActive = time.Now()
	dogState.UpdatedAt = time.Now()

	return m.saveState(name, dogState)
}

// AssignWork assigns work to a dog and sets it to working state.
func (m *Manager) AssignWork(name, work string) error {
	_, err := m.AssignWorkIfIdle(name, work)
	return err
}

// AssignWorkIfIdle assigns work only if the dog is still idle, returning the
// saved state so callers can later perform exact compare-and-clear cleanup.
func (m *Manager) AssignWorkIfIdle(name, work string) (*DogState, error) {
	if err := validateDogName(name); err != nil {
		return nil, err
	}
	if !m.exists(name) {
		return nil, ErrDogNotFound
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return nil, fmt.Errorf("loading state: %w", err)
	}
	if state.State != StateIdle || state.SessionGeneration != nil {
		return nil, ErrDogWorking
	}

	state.State = StateWorking
	state.Work = work
	state.WorkStartedAt = time.Now()
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()

	if err := m.saveState(name, state); err != nil {
		return nil, err
	}
	return state, nil
}

// ClearWork clears a dog's work assignment and sets it to idle.
func (m *Manager) ClearWork(name string) error {
	if err := validateDogName(name); err != nil {
		return err
	}
	if !m.exists(name) {
		return ErrDogNotFound
	}

	// Acquire per-dog lock to prevent concurrent load-modify-save races
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}
	if state.SessionGeneration != nil {
		return errors.New("dog still owns an exact session generation")
	}
	if err := m.finalizeAssignment(state); err != nil {
		return err
	}

	state.State = StateIdle
	state.Work = ""
	state.WorkStartedAt = time.Time{}
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()

	return m.saveState(name, state)
}

// ClearWorkIfMatches clears a dog's work assignment only if it still matches
// the expected work and assignment timestamp. The compare-and-clear happens
// under the dog lock so failed dispatch cleanup cannot erase a newer assignment.
func (m *Manager) ClearWorkIfMatches(name, expectedWork string, expectedStartedAt time.Time) (bool, error) {
	return m.ClearWorkWithFinalizeIfMatches(name, expectedWork, expectedStartedAt, nil)
}

// ClearWorkWithFinalizeIfMatches clears an exact generation-free assignment
// only after finalize succeeds, while the per-dog lifecycle lock is held. A
// failed finalizer therefore leaves the assignment non-assignable and exactly
// retryable instead of publishing idle state with old instructions still open.
func (m *Manager) ClearWorkWithFinalizeIfMatches(
	name, expectedWork string,
	expectedStartedAt time.Time,
	finalize func() error,
) (bool, error) {
	if err := validateDogName(name); err != nil {
		return false, err
	}
	if !m.exists(name) {
		return false, ErrDogNotFound
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if state.State != StateWorking ||
		state.Work != expectedWork ||
		!state.WorkStartedAt.Equal(expectedStartedAt) ||
		state.SessionGeneration != nil {
		return false, nil
	}
	if err := m.finalizeAssignment(state); err != nil {
		return false, err
	}
	if finalize != nil {
		if err := finalize(); err != nil {
			return false, fmt.Errorf("finalizing dog assignment: %w", err)
		}
	}

	state.State = StateIdle
	state.Work = ""
	state.WorkStartedAt = time.Time{}
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()

	if err := m.saveState(name, state); err != nil {
		return false, err
	}
	return true, nil
}

// SetSessionGenerationIfAssignmentMatches records a newly created session only
// when the assignment and prior runtime generation are unchanged. A stale
// startup may therefore tear down only the generation it created; it cannot
// overwrite custody for a replacement assignment or session.
func (m *Manager) SetSessionGenerationIfAssignmentMatches(
	name string,
	expectedWork string,
	expectedStartedAt time.Time,
	expectedPrior *tmux.SessionGeneration,
	generation tmux.SessionGeneration,
) (bool, error) {
	if err := validateDogName(name); err != nil {
		return false, err
	}
	if !m.exists(name) {
		return false, ErrDogNotFound
	}
	if err := validateSessionGenerationForDog(name, generation); err != nil {
		return false, err
	}
	if expectedPrior != nil {
		if err := validateSessionGenerationForDog(name, *expectedPrior); err != nil {
			return false, err
		}
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()
	return m.setSessionGenerationIfAssignmentMatchesLocked(
		name,
		expectedWork,
		expectedStartedAt,
		expectedPrior,
		generation,
	)
}

func (m *Manager) setSessionGenerationIfAssignmentMatchesLocked(
	name string,
	expectedWork string,
	expectedStartedAt time.Time,
	expectedPrior *tmux.SessionGeneration,
	generation tmux.SessionGeneration,
) (bool, error) {
	state, err := m.loadState(name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if state.State != StateWorking ||
		state.Work != expectedWork ||
		!state.WorkStartedAt.Equal(expectedStartedAt) ||
		!sessionGenerationMatches(state.SessionGeneration, expectedPrior) {
		return false, nil
	}

	state.SessionGeneration = SessionGenerationFromTmux(generation)
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()
	return true, m.saveState(name, state)
}

func sessionGenerationMatches(stored *SessionGeneration, expected *tmux.SessionGeneration) bool {
	if expected == nil {
		return stored == nil
	}
	return stored != nil && stored.EqualTmux(*expected)
}

// CompleteWorkWithTeardownIfMatches keeps a dog non-assignable while it owns
// an exact runtime generation. The exact teardown runs under the same dog lock
// as the final state transition, so replacement assignment and session records
// cannot interleave between proof, teardown, and durable closeout.
func (m *Manager) CompleteWorkWithTeardownIfMatches(
	name string,
	expectedWork string,
	expectedStartedAt time.Time,
	expectedGeneration tmux.SessionGeneration,
	teardown func(tmux.SessionGeneration) error,
) (bool, error) {
	return m.CompleteWorkWithTeardownAndFinalizeIfMatches(
		name,
		expectedWork,
		expectedStartedAt,
		expectedGeneration,
		teardown,
		nil,
	)
}

// CompleteWorkWithTeardownAndFinalizeIfMatches tears down an exact session and
// runs the exact assignment finalizer before committing idle state. Teardown
// and finalization failures leave durable assignment custody intact so retry is
// idempotent and a replacement cannot start prematurely.
func (m *Manager) CompleteWorkWithTeardownAndFinalizeIfMatches(
	name string,
	expectedWork string,
	expectedStartedAt time.Time,
	expectedGeneration tmux.SessionGeneration,
	teardown func(tmux.SessionGeneration) error,
	finalize func() error,
) (bool, error) {
	if err := validateDogName(name); err != nil {
		return false, err
	}
	if !m.exists(name) {
		return false, ErrDogNotFound
	}
	if err := validateSessionGenerationForDog(name, expectedGeneration); err != nil {
		return false, err
	}
	if teardown == nil {
		return false, errors.New("dog session teardown is unavailable")
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if state.State != StateWorking ||
		state.Work != expectedWork ||
		!state.WorkStartedAt.Equal(expectedStartedAt) ||
		state.SessionGeneration == nil ||
		!state.SessionGeneration.EqualTmux(expectedGeneration) {
		return false, nil
	}
	if err := teardown(expectedGeneration); err != nil {
		return false, err
	}
	if err := m.finalizeAssignment(state); err != nil {
		return false, err
	}
	if finalize != nil {
		if err := finalize(); err != nil {
			return false, fmt.Errorf("finalizing dog assignment: %w", err)
		}
	}

	state.State = StateIdle
	state.Work = ""
	state.WorkStartedAt = time.Time{}
	state.SessionGeneration = nil
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()
	if err := m.saveState(name, state); err != nil {
		return false, err
	}
	return true, nil
}

// finalizeAssignment retires durable instructions owned by the exact current
// assignment before any path can publish the reusable idle state. Keeping this
// invariant in Manager prevents CLI, daemon, health, and rollback callers from
// accidentally bypassing assignment-bound cleanup.
func (m *Manager) finalizeAssignment(state *DogState) error {
	if state == nil || !strings.HasPrefix(state.Work, "plugin:") {
		return nil
	}
	if m.assignmentFinalizer == nil {
		return errors.New("dog assignment finalizer is unavailable")
	}
	if err := m.assignmentFinalizer(state.Name, state.Work, state.WorkStartedAt); err != nil {
		return fmt.Errorf("finalizing dog assignment: %w", err)
	}
	return nil
}

// RetireSessionWithTeardownIfMatches removes an exact runtime generation from
// an already-idle dog. It is the orphan-session counterpart to guarded work
// completion and keeps the generation non-assignable until teardown succeeds.
func (m *Manager) RetireSessionWithTeardownIfMatches(
	name string,
	expectedGeneration tmux.SessionGeneration,
	teardown func(tmux.SessionGeneration) error,
) (bool, error) {
	if err := validateDogName(name); err != nil {
		return false, err
	}
	if !m.exists(name) {
		return false, ErrDogNotFound
	}
	if err := validateSessionGenerationForDog(name, expectedGeneration); err != nil {
		return false, err
	}
	if teardown == nil {
		return false, errors.New("dog session teardown is unavailable")
	}

	fl, err := m.lockDog(name)
	if err != nil {
		return false, err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return false, fmt.Errorf("loading state: %w", err)
	}
	if state.State != StateIdle || state.Work != "" ||
		state.SessionGeneration == nil ||
		!state.SessionGeneration.EqualTmux(expectedGeneration) {
		return false, nil
	}
	if err := teardown(expectedGeneration); err != nil {
		return false, err
	}

	state.SessionGeneration = nil
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()
	return true, m.saveState(name, state)
}

func validateSessionGenerationForDog(name string, generation tmux.SessionGeneration) error {
	wantName := "hq-dog-" + name
	if generation.Name != wantName ||
		strings.TrimSpace(generation.SessionID) == "" ||
		strings.TrimSpace(generation.Nonce) == "" ||
		generation.ServerPID <= 0 ||
		strings.TrimSpace(generation.ServerIdentity) == "" {
		return fmt.Errorf("invalid session generation for dog %s", name)
	}
	return nil
}

// Refresh recreates all worktrees for a dog with fresh branches.
// This is useful when worktrees have drifted or become stale.
// Each rig is refreshed atomically with a state save, so a failure at rig N
// leaves rigs 1..N-1 correctly updated and rigs N+1..M untouched.
func (m *Manager) Refresh(name string) error {
	if err := validateDogName(name); err != nil {
		return err
	}
	if !m.exists(name) {
		return ErrDogNotFound
	}

	// Acquire per-dog lock to prevent concurrent load-modify-save races
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	// Refuse to refresh a working dog — its agent is using the worktrees.
	if state.State == StateWorking {
		return ErrDogWorking
	}

	dogPath := m.dogDir(name)

	// Refresh each rig atomically: remove old, create new, persist state.
	for rigName := range m.rigsConfig.Rigs {
		rigPath := filepath.Join(m.townRoot, rigName)
		oldWorktreePath := state.Worktrees[rigName]

		// Find repo base
		repoGit, err := m.findRepoBase(rigPath)
		if err != nil {
			// Save partial progress before returning
			state.LastActive = time.Now()
			state.UpdatedAt = time.Now()
			_ = m.saveState(name, state)
			return fmt.Errorf("finding repo base for %s: %w", rigName, err)
		}

		// Remove old worktree if it exists
		if oldWorktreePath != "" {
			_ = repoGit.WorktreeRemove(oldWorktreePath, true)
			_ = os.RemoveAll(oldWorktreePath)
			_ = repoGit.WorktreePrune()
		}

		// Fetch latest from origin
		_ = repoGit.Fetch("origin")

		// Create fresh worktree
		worktreePath, err := m.createRigWorktree(dogPath, name, rigName)
		if err != nil {
			// Old worktree is gone but new one failed. Remove stale path
			// from state so it doesn't reference a deleted directory.
			delete(state.Worktrees, rigName)
			state.UpdatedAt = time.Now()
			_ = m.saveState(name, state)
			return fmt.Errorf("creating worktree for %s: %w", rigName, err)
		}

		// Persist state after each rig so completed rigs aren't lost on
		// a later failure.
		state.Worktrees[rigName] = worktreePath
		state.LastActive = time.Now()
		state.UpdatedAt = time.Now()
		if err := m.saveState(name, state); err != nil {
			return fmt.Errorf("saving state after refreshing %s: %w", rigName, err)
		}
	}

	return nil
}

// RefreshRig recreates the worktree for a specific rig.
func (m *Manager) RefreshRig(name, rigName string) error {
	if err := validateDogName(name); err != nil {
		return err
	}
	if !m.exists(name) {
		return ErrDogNotFound
	}

	if _, ok := m.rigsConfig.Rigs[rigName]; !ok {
		return fmt.Errorf("rig %s not found in config", rigName)
	}

	// Acquire per-dog lock to prevent concurrent load-modify-save races
	fl, err := m.lockDog(name)
	if err != nil {
		return err
	}
	defer func() { _ = fl.Unlock() }()

	state, err := m.loadState(name)
	if err != nil {
		return fmt.Errorf("loading state: %w", err)
	}

	// Refuse to refresh a working dog — its agent is using the worktrees.
	if state.State == StateWorking {
		return ErrDogWorking
	}

	dogPath := m.dogDir(name)
	rigPath := filepath.Join(m.townRoot, rigName)
	oldWorktreePath := state.Worktrees[rigName]

	// Find repo base
	repoGit, err := m.findRepoBase(rigPath)
	if err != nil {
		return fmt.Errorf("finding repo base: %w", err)
	}

	// Remove old worktree if it exists
	if oldWorktreePath != "" {
		_ = repoGit.WorktreeRemove(oldWorktreePath, true)
		_ = os.RemoveAll(oldWorktreePath)
		_ = repoGit.WorktreePrune()
	}

	// Fetch latest
	_ = repoGit.Fetch("origin")

	// Create fresh worktree
	worktreePath, err := m.createRigWorktree(dogPath, name, rigName)
	if err != nil {
		// Old worktree is gone but new one failed. Remove stale path
		// from state so it doesn't reference a deleted directory.
		delete(state.Worktrees, rigName)
		state.UpdatedAt = time.Now()
		_ = m.saveState(name, state)
		return fmt.Errorf("creating worktree: %w", err)
	}

	// Update state
	state.Worktrees[rigName] = worktreePath
	state.LastActive = time.Now()
	state.UpdatedAt = time.Now()

	return m.saveState(name, state)
}

// CleanupStaleBranches removes orphaned dog branches from all rigs.
// Returns total branches deleted across all rigs.
func (m *Manager) CleanupStaleBranches() (int, error) {
	totalDeleted := 0

	for rigName := range m.rigsConfig.Rigs {
		rigPath := filepath.Join(m.townRoot, rigName)
		repoGit, err := m.findRepoBase(rigPath)
		if err != nil {
			continue
		}

		deleted, err := m.cleanupStaleBranchesForRig(repoGit, rigName)
		if err != nil {
			style.PrintWarning("cleanup failed for rig %s: %v", rigName, err)
			continue
		}
		totalDeleted += deleted
	}

	return totalDeleted, nil
}

// cleanupStaleBranchesForRig removes orphaned dog branches in a specific rig.
func (m *Manager) cleanupStaleBranchesForRig(repoGit *git.Git, rigName string) (int, error) {
	// List all dog branches
	branches, err := repoGit.ListBranches("dog/*")
	if err != nil {
		return 0, err
	}

	if len(branches) == 0 {
		return 0, nil
	}

	// Get list of current dogs
	dogs, err := m.List()
	if err != nil {
		return 0, err
	}

	// Build set of current dog branches for this rig
	currentBranches := make(map[string]bool)
	for _, dog := range dogs {
		if dog.Worktrees != nil {
			if worktreePath, ok := dog.Worktrees[rigName]; ok {
				// Get branch name for this worktree
				worktreeGit := git.NewGit(worktreePath)
				if branch, err := worktreeGit.CurrentBranch(); err == nil {
					currentBranches[branch] = true
				}
			}
		}
	}

	// Delete orphaned branches
	deleted := 0
	for _, branch := range branches {
		if currentBranches[branch] {
			continue
		}
		if err := repoGit.DeleteBranch(branch, true); err != nil {
			style.PrintWarning("could not delete branch %s: %v", branch, err)
			continue
		}
		deleted++
	}

	return deleted, nil
}

// loadState loads a dog's state from .dog.json.
func (m *Manager) loadState(name string) (*DogState, error) {
	data, err := os.ReadFile(m.stateFilePath(name))
	if err != nil {
		return nil, err
	}

	var state DogState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}

	return &state, nil
}

// saveState saves a dog's state to .dog.json using atomic write (write-to-temp + rename).
// This prevents concurrent loadState from seeing a truncated/empty file.
func (m *Manager) saveState(name string, state *DogState) error {
	return atomicfile.WriteJSON(m.stateFilePath(name), state)
}

// GetIdleDog returns an idle dog suitable for work assignment.
// Returns nil if no idle dogs are available.
func (m *Manager) GetIdleDog() (*Dog, error) {
	dogs, err := m.List()
	if err != nil {
		return nil, err
	}

	for _, dog := range dogs {
		if dog.State == StateIdle && dog.SessionGeneration == nil {
			return dog, nil
		}
	}

	return nil, nil // No idle dogs
}

// IdleCount returns the number of idle dogs.
func (m *Manager) IdleCount() (int, error) {
	dogs, err := m.List()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, dog := range dogs {
		if dog.State == StateIdle && dog.SessionGeneration == nil {
			count++
		}
	}
	return count, nil
}

// WorkingCount returns the number of working dogs.
func (m *Manager) WorkingCount() (int, error) {
	dogs, err := m.List()
	if err != nil {
		return 0, err
	}

	count := 0
	for _, dog := range dogs {
		if dog.State == StateWorking {
			count++
		}
	}
	return count, nil
}
