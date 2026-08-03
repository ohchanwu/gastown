// poller.go provides a background nudge-queue poller for agents that lack
// turn-boundary drain hooks (e.g., Gemini, Codex). Claude Code drains its
// queue via the UserPromptSubmit hook on every turn. Other runtimes have no
// equivalent hook, so queued nudges would sit undelivered forever.
//
// The poller runs as a background goroutine launched by crew/manager.Start().
// It polls the queue every PollInterval, waits for the agent to be idle, then
// drains and injects the formatted nudges via tmux NudgeSession.
//
// Lifecycle: StartPoller() → background loop → StopPoller() (or session death).
// A PID file at <townRoot>/.runtime/nudge_poller/<session>.pid allows Stop()
// to clean up even if the original manager has been replaced.
package nudge

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/util"
)

// Poller tuning defaults (overridable via flags or tests).
var (
	// DefaultPollInterval is how often the poller checks the queue.
	DefaultPollInterval = "10s"
	// DefaultIdleTimeout is how long to wait for the agent to become idle
	// before skipping this poll cycle and trying again next interval.
	DefaultIdleTimeout = "3s"
)

// pollerPidDir returns the directory for poller PID files.
func pollerPidDir(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_poller")
}

// pollerPidFile returns the PID file path for a session's poller.
func pollerPidFile(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	return filepath.Join(pollerPidDir(townRoot), safe+".pid")
}

func pollerStopFile(townRoot, session string) string {
	return pollerPidFile(townRoot, session) + ".stop"
}

// StartPoller launches a background `gt nudge-poller <session>` process.
// The process is detached (Setpgid) so it survives the caller's exit.
// Returns the PID of the launched process, or an error.
func StartPoller(townRoot, session string) (int, error) {
	return startPoller(townRoot, session, nil)
}

// StartPollerWithEnv launches a poller with an explicit child environment.
// Isolated callers use this to preserve their private tmux/config boundary.
func StartPollerWithEnv(townRoot, session string, env []string) (int, error) {
	return startPoller(townRoot, session, env)
}

// StopRequested reports whether the session's cooperative stop generation is set.
func StopRequested(townRoot, session string) bool {
	data, err := os.ReadFile(pollerStopFile(townRoot, session))
	if err != nil {
		return false
	}
	current, err := os.ReadFile(pollerPidFile(townRoot, session))
	return err == nil && bytes.Equal(data, current)
}

func startPoller(townRoot, session string, env []string) (int, error) {
	return startPollerWithLauncher(townRoot, session, env, launchPoller, os.WriteFile)
}

type pollerLaunch struct {
	pid       int
	identity  pollerIdentity
	release   func() error
	terminate func() error
}

type pollerIdentity struct {
	StartTime  string
	Command    string
	Generation string
}

type pollerRecord struct {
	PID      int
	Identity pollerIdentity
	Session  string
	Legacy   bool
}

func startPollerWithLauncher(
	townRoot, session string,
	env []string,
	launcher func(string, string, []string) (pollerLaunch, error),
	writePID func(string, []byte, os.FileMode) error,
) (int, error) {
	return startPollerWithLauncherStatus(townRoot, session, env, launcher, writePID, pollerStatus)
}

func startPollerWithLauncherStatus(
	townRoot, session string,
	env []string,
	launcher func(string, string, []string) (pollerLaunch, error),
	writePID func(string, []byte, os.FileMode) error,
	status func(string, string) (int, bool, error),
) (int, error) {
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		return 0, fmt.Errorf("creating poller pid dir: %w", err)
	}

	startLock, err := lockPoller(townRoot, session)
	if err != nil {
		return 0, err
	}
	defer func() { _ = startLock.Unlock() }()

	// Check if a poller is already running for this session.
	if pid, alive, statusErr := status(townRoot, session); statusErr != nil {
		return 0, statusErr
	} else if alive {
		return pid, nil // already running
	}
	_ = os.Remove(pollerStopFile(townRoot, session))

	launched, err := launcher(townRoot, session, env)
	if err != nil {
		return 0, err
	}

	// Write PID file for later cleanup.
	pidPath := pollerPidFile(townRoot, session)
	if launched.identity.StartTime == "" || launched.identity.Command == "" || launched.identity.Generation == "" || session == "" {
		return 0, cleanupLaunchedPoller(launched, errors.New("nudge-poller identity unavailable"))
	}
	record := formatPollerRecord(launched.pid, launched.identity, session)
	if err := writePID(pidPath, []byte(record), 0644); err != nil {
		persistErr := fmt.Errorf("persisting nudge-poller PID: %w", err)
		if launched.terminate == nil {
			return 0, persistErr
		}
		if terminateErr := launched.terminate(); terminateErr != nil {
			return 0, errors.Join(persistErr, fmt.Errorf("terminating untracked nudge-poller: %w", terminateErr))
		}
		return 0, persistErr
	}
	if launched.release != nil {
		_ = launched.release()
	}

	return launched.pid, nil
}

func lockPoller(townRoot, session string) (*flock.Flock, error) {
	lockPath := pollerPidFile(townRoot, session) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("creating nudge-poller lock dir: %w", err)
	}
	startLock := flock.New(lockPath)
	if err := startLock.Lock(); err != nil {
		return nil, fmt.Errorf("acquiring nudge-poller start lock: %w", err)
	}
	return startLock, nil
}

func launchPoller(townRoot, session string, env []string) (pollerLaunch, error) {
	// Find the gt binary.
	gtBin, err := os.Executable()
	if err != nil {
		return pollerLaunch{}, fmt.Errorf("finding gt binary: %w", err)
	}

	cmd := buildPollerCommand(gtBin, townRoot, session, env)
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return pollerLaunch{}, fmt.Errorf("creating poller generation: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return pollerLaunch{}, fmt.Errorf("starting nudge-poller: %w", err)
	}
	identity := pollerIdentityForProcess(cmd.Process.Pid)
	identity.Generation = fmt.Sprintf("%x", generation[:])

	return pollerLaunch{
		pid:      cmd.Process.Pid,
		identity: identity,
		release:  cmd.Process.Release,
		terminate: func() error {
			killErr := cmd.Process.Kill()
			waitErr := cmd.Wait()
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				waitErr = nil
			}
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
			return errors.Join(killErr, waitErr)
		},
	}, nil
}

func formatPollerRecord(pid int, identity pollerIdentity, sessionName string) string {
	if identity.Generation == "" {
		return ""
	}
	b, _ := json.Marshal(pollerRecord{PID: pid, Identity: identity, Session: sessionName})
	return string(b) + "\n"
}

func formatPollerRecordValue(record pollerRecord) string {
	return formatPollerRecord(record.PID, record.Identity, record.Session)
}

func parsePollerRecord(value string) (pollerRecord, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, "{") {
		pid, err := strconv.Atoi(trimmed)
		if err != nil || pid <= 0 {
			return pollerRecord{}, errors.New("invalid nudge-poller ownership record")
		}
		return pollerRecord{PID: pid, Legacy: true}, nil
	}
	var record pollerRecord
	if err := json.Unmarshal([]byte(trimmed), &record); err != nil || record.PID <= 0 || record.Identity.StartTime == "" || record.Identity.Command == "" || record.Identity.Generation == "" || record.Session == "" {
		return pollerRecord{}, errors.New("invalid nudge-poller ownership record")
	}
	return record, nil
}

func pollerIdentityForProcess(pid int) pollerIdentity {
	start, err := session.ProcessStartTime(pid)
	if err != nil {
		return pollerIdentity{}
	}
	cmd := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid))
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return pollerIdentity{}
	}
	return pollerIdentity{StartTime: strings.TrimSpace(start), Command: strings.TrimSpace(string(out))}
}

func pollerIdentityMatches(identity pollerIdentity, record pollerRecord, sessionName string) bool {
	if record.Session != sessionName || identity.StartTime != record.Identity.StartTime || strings.TrimSpace(identity.Command) != strings.TrimSpace(record.Identity.Command) {
		return false
	}
	return pollerCommandMatches(identity.Command, sessionName)
}

func pollerCommandMatches(command, sessionName string) bool {
	fields := strings.Fields(command)
	return len(fields) >= 3 && filepath.Base(fields[len(fields)-3]) == "gt" && fields[len(fields)-2] == "nudge-poller" && fields[len(fields)-1] == sessionName
}

func cleanupLaunchedPoller(launched pollerLaunch, cause error) error {
	if launched.terminate == nil {
		return cause
	}
	if err := launched.terminate(); err != nil {
		return errors.Join(cause, fmt.Errorf("terminating untracked nudge-poller: %w", err))
	}
	return cause
}

func buildPollerCommand(gtBin, townRoot, session string, env []string) *exec.Cmd {
	cmd := exec.Command(gtBin, "nudge-poller", session)
	cmd.Dir = townRoot
	if env != nil {
		cmd.Env = append([]string(nil), env...)
	}
	cmd.Stdout = nil // discard
	cmd.Stderr = nil // discard
	util.SetDetachedProcessGroup(cmd)
	return cmd
}

// StopPoller terminates the nudge-poller for a session, if running.
func StopPoller(townRoot, session string) error {
	return stopPollerWithGenerationOps(townRoot, session, os.ReadFile, pollerProcessAlive, lookupPollerIdentity,
		func(data []byte) error { return os.WriteFile(pollerStopFile(townRoot, session), data, 0600) }, waitPollerExit,
		func(path string, data []byte) error {
			return quarantinePollerRecord(path, data, func(destination string) error { return os.Rename(path, destination) })
		}, os.Remove)
}

const (
	pollerExitTimeout  = 2 * time.Second
	pollerExitInterval = 50 * time.Millisecond
)

func stopPollerWithOwnershipOps(
	townRoot, sessionName string,
	readPID func(string) ([]byte, error),
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
	signal func(int) error,
	waitExit func(int, pollerRecord) error,
	quarantine func(string, []byte) error,
	remove func(string) error,
) error {
	return stopPollerWithGenerationOps(townRoot, sessionName, readPID, alive, identity,
		func(_ []byte) error { return signal(0) }, waitExit, quarantine, remove)
}

func stopPollerWithGenerationOps(
	townRoot, sessionName string,
	readPID func(string) ([]byte, error),
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
	stop func([]byte) error,
	waitExit func(int, pollerRecord) error,
	quarantine func(string, []byte) error,
	remove func(string) error,
) error {
	lock, err := lockPoller(townRoot, sessionName)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()

	pidPath := pollerPidFile(townRoot, sessionName)
	data, err := readPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading poller ownership: %w", err)
	}
	record, err := parsePollerRecord(string(data))
	if err != nil {
		return fmt.Errorf("parsing poller ownership: %w", err)
	}
	if !alive(record.PID) {
		if err := remove(pidPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing dead poller ownership: %w", err)
		}
		return nil
	}
	current, err := identity(record.PID)
	if err != nil {
		if !alive(record.PID) {
			if removeErr := remove(pidPath); removeErr != nil && !os.IsNotExist(removeErr) {
				return fmt.Errorf("removing dead poller ownership: %w", removeErr)
			}
			return nil
		}
		return fmt.Errorf("validating poller identity: %w", err)
	}
	if record.Legacy {
		if !pollerCommandMatches(current.Command, sessionName) {
			return fmt.Errorf("legacy poller identity mismatch for session %s; preserving ownership", sessionName)
		}
		return fmt.Errorf("legacy poller ownership requires separately verified migration; preserving PID %d", record.PID)
	}
	if !pollerIdentityMatches(current, record, sessionName) {
		if quarantineErr := quarantine(pidPath, data); quarantineErr != nil {
			return errors.Join(errors.New("poller identity mismatch"), quarantineErr)
		}
		return errors.New("poller identity mismatch; ownership quarantined")
	}
	if err := stop(data); err != nil {
		return fmt.Errorf("sending SIGTERM to poller (pid %d): %w", record.PID, err)
	}
	if err := waitExit(record.PID, record); err != nil {
		return fmt.Errorf("waiting for poller (pid %d) to exit: %w", record.PID, err)
	}
	latest, err := readPID(pidPath)
	if err != nil {
		return fmt.Errorf("rereading poller ownership before removal: %w", err)
	}
	if !bytes.Equal(latest, data) {
		return errors.New("poller ownership changed before removal; preserving replacement record")
	}
	stopPath := pollerStopFile(townRoot, sessionName)
	if stopData, readErr := os.ReadFile(stopPath); readErr == nil && bytes.Equal(stopData, data) {
		if removeErr := remove(stopPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return fmt.Errorf("removing poller stop request: %w", removeErr)
		}
	}
	if err := remove(pidPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing poller ownership: %w", err)
	}
	return nil
}

func quarantinePollerRecord(pidPath string, data []byte, rename func(string) error) error {
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	return rename(pidPath + ".stale-" + digest[:16])
}

func lookupPollerIdentity(pid int) (pollerIdentity, error) {
	identity := pollerIdentityForProcess(pid)
	if identity.StartTime == "" || identity.Command == "" {
		return pollerIdentity{}, errors.New("process identity unavailable")
	}
	return identity, nil
}

func waitPollerExit(pid int, record pollerRecord) error {
	deadline := time.Now().Add(pollerExitTimeout)
	for time.Now().Before(deadline) {
		if !pollerProcessAlive(pid) {
			return nil
		}
		if current, err := lookupPollerIdentity(pid); err == nil && !pollerIdentityMatches(current, record, record.Session) {
			return nil
		}
		time.Sleep(pollerExitInterval)
	}
	return errors.New("exit confirmation timeout")
}

func stopPollerWithOps(
	townRoot, session string,
	readPID func(string) ([]byte, error),
	processAlive func(int) bool,
	signal func(int) error,
	remove func(string) error,
) error {
	stopLock, err := lockPoller(townRoot, session)
	if err != nil {
		return err
	}
	defer func() { _ = stopLock.Unlock() }()

	pidPath := pollerPidFile(townRoot, session)

	data, err := readPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no poller to stop
		}
		return fmt.Errorf("reading poller PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		_ = os.Remove(pidPath)
		return nil // corrupt PID file, clean up
	}

	if !processAlive(pid) {
		// Process already dead.
		_ = remove(pidPath)
		return nil
	}

	// Send SIGTERM for graceful shutdown. Preserve custody if signaling fails.
	if err := signal(pid); err != nil {
		return fmt.Errorf("sending SIGTERM to poller (pid %d): %w", pid, err)
	}

	_ = remove(pidPath)
	return nil
}

// pollerAlive checks if a poller is running for the given session.
// Returns the PID and whether the process is alive.
func pollerStatus(townRoot, session string) (int, bool, error) {
	return pollerStatusWithOps(townRoot, session, os.ReadFile, pollerProcessAlive, lookupPollerIdentity,
		func(path string, data []byte) error {
			return quarantinePollerRecord(path, data, func(destination string) error { return os.Rename(path, destination) })
		}, os.Remove)
}

func pollerStatusWithOps(
	townRoot, session string,
	readPID func(string) ([]byte, error),
	alive func(int) bool,
	identity func(int) (pollerIdentity, error),
	quarantine func(string, []byte) error,
	remove func(string) error,
) (int, bool, error) {
	pidPath := pollerPidFile(townRoot, session)

	data, err := readPID(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("reading poller ownership: %w", err)
	}

	record, err := parsePollerRecord(string(data))
	if err != nil {
		return 0, false, fmt.Errorf("parsing poller ownership: %w", err)
	}

	if !alive(record.PID) {
		_ = remove(pidPath)
		return 0, false, nil
	}
	current, err := identity(record.PID)
	if err != nil {
		if !alive(record.PID) {
			_ = remove(pidPath)
			return 0, false, nil
		}
		return record.PID, true, fmt.Errorf("validating poller identity: %w", err)
	}
	if record.Legacy {
		if !pollerCommandMatches(current.Command, session) {
			return record.PID, true, fmt.Errorf("legacy poller identity mismatch for session %s; preserving ownership", session)
		}
		return record.PID, true, fmt.Errorf("legacy poller ownership requires separately verified migration; preserving PID %d", record.PID)
	} else if !pollerIdentityMatches(current, record, session) {
		if err := quarantine(pidPath, data); err != nil {
			return 0, false, fmt.Errorf("quarantining poller ownership: %w", err)
		}
		return 0, false, nil
	}
	return record.PID, true, nil
}

func pollerAlive(townRoot, session string) (int, bool) {
	pid, alive, _ := pollerStatus(townRoot, session)
	return pid, alive
}

// Watcher provides a filesystem-event-driven interface to the nudge queue.
// This is an ACP-safe alternative to polling and is preferred for long-running
// watchers like ACP Propeller.
type Watcher struct {
	townRoot string
	session  string
	dir      string
	closed   chan struct{}
	wg       sync.WaitGroup
	events   chan struct{}
}

// NewWatcher creates a new watcher for the given town root and session.
// The watcher observes nudge queue writes and signals via the Events() channel.
func NewWatcher(townRoot, session string) (*Watcher, error) {
	dir := queueDir(townRoot, session)
	// Ensure the directory exists so watch can start immediately.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating nudge queue dir: %w", err)
	}

	w := &Watcher{
		townRoot: townRoot,
		session:  session,
		dir:      dir,
		closed:   make(chan struct{}),
		events:   make(chan struct{}, 1), // buffer one signal for coalescing
	}

	w.wg.Add(1)
	go w.watch()
	return w, nil
}

// Events returns a channel that receives a struct{} when the queue may have
// changed. Multiple changes within a short window are coalesced.
func (w *Watcher) Events() <-chan struct{} {
	return w.events
}

// Close stops the watcher and releases resources.
func (w *Watcher) Close() error {
	select {
	case <-w.closed:
		return fmt.Errorf("watcher already closed")
	default:
	}
	close(w.closed)
	w.wg.Wait()
	return nil
}

func (w *Watcher) watch() {
	defer w.wg.Done()

	// Use fsnotify directly.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		// Log but don't block; fallback behavior is explicit in callers.
		fmt.Fprintf(os.Stderr, "nudge watcher init failed for %s: %v\n", w.dir, err)
		return
	}
	defer func() { _ = watcher.Close() }()

	// Watch the directory.
	if err := watcher.Add(w.dir); err != nil {
		fmt.Fprintf(os.Stderr, "nudge watcher failed to add dir %s: %v\n", w.dir, err)
		return
	}

	// Coalescing window.
	coalesceTimer := time.NewTicker(100 * time.Millisecond)
	defer coalesceTimer.Stop()

	pending := false
	for {
		select {
		case <-w.closed:
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Only care about file creation/modification in the queue dir
			if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				// Filter: only .json files in the queue directory
				if strings.HasSuffix(event.Name, ".json") && filepath.Dir(event.Name) == w.dir {
					pending = true
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "nudge watcher error: %v\n", err)
		case <-coalesceTimer.C:
			if pending {
				pending = false
				select {
				case w.events <- struct{}{}:
				default:
				}
			}
		}
	}
}

// WatcherForSession returns a Watcher for a specific session or an error if
// creation fails (e.g., filesystem issues). Callers should handle cleanup.
func WatcherForSession(townRoot, session string) (*Watcher, error) {
	return NewWatcher(townRoot, session)
}
