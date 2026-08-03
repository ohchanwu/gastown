package nudge

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

func testPollerIdentity(session string) pollerIdentity {
	return pollerIdentity{StartTime: "test-start", Command: "gt nudge-poller " + session}
}

func TestStartPollerSerializesConcurrentLaunches(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	release := make(chan struct{})
	var launches atomic.Int32

	launcher := func(string, string, []string) (pollerLaunch, error) {
		switch launches.Add(1) {
		case 1:
			close(firstStarted)
		case 2:
			close(secondStarted)
		}
		<-release
		return pollerLaunch{pid: os.Getpid(), identity: testPollerIdentity(session)}, nil
	}

	results := make(chan int, 2)
	errs := make(chan error, 2)
	start := func() {
		pid, err := startPollerWithLauncherStatus(townRoot, session, nil, launcher, os.WriteFile, func(root, name string) (int, bool, error) {
			data, err := os.ReadFile(pollerPidFile(root, name))
			if os.IsNotExist(err) {
				return 0, false, nil
			}
			record, parseErr := parsePollerRecord(string(data))
			return record.PID, parseErr == nil, parseErr
		})
		results <- pid
		errs <- err
	}
	go start()
	<-firstStarted
	secondCalling := make(chan struct{})
	go func() {
		close(secondCalling)
		start()
	}()
	<-secondCalling

	duplicate := false
	select {
	case <-secondStarted:
		duplicate = true
	case <-time.After(3 * time.Second):
	}
	close(release)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if pid := <-results; pid != os.Getpid() {
			t.Fatalf("pid = %d, want %d", pid, os.Getpid())
		}
	}
	if duplicate || launches.Load() != 1 {
		t.Fatalf("launcher called %d times, want 1 serialized start", launches.Load())
	}
}

func TestStartPollerPIDWriteFailureTerminatesLaunchedProcess(t *testing.T) {
	townRoot := t.TempDir()
	writeErr := errors.New("injected PID write failure")
	var terminated, released atomic.Int32

	launcher := func(string, string, []string) (pollerLaunch, error) {
		return pollerLaunch{
			pid:       123,
			identity:  testPollerIdentity("gt-gastown-polecat-test"),
			release:   func() error { released.Add(1); return nil },
			terminate: func() error { terminated.Add(1); return nil },
		}, nil
	}
	writer := func(string, []byte, os.FileMode) error { return writeErr }

	pid, err := startPollerWithLauncher(townRoot, "gt-gastown-polecat-test", nil, launcher, writer)
	if !errors.Is(err, writeErr) {
		t.Fatalf("error = %v, want injected PID write failure", err)
	}
	if pid != 0 {
		t.Fatalf("pid = %d, want 0 on failed persistence", pid)
	}
	if got := terminated.Load(); got != 1 {
		t.Fatalf("terminate calls = %d, want 1", got)
	}
	if got := released.Load(); got != 0 {
		t.Fatalf("release calls = %d, want 0", got)
	}
}

func TestStopPollerSignalFailurePreservesPIDCustody(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte("123"), 0644); err != nil {
		t.Fatal(err)
	}
	signalErr := errors.New("injected SIGTERM failure")
	removed := false

	err := stopPollerWithOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) error { return signalErr },
		func(string) error { removed = true; return nil },
	)
	if !errors.Is(err, signalErr) {
		t.Fatalf("error = %v, want SIGTERM failure", err)
	}
	if removed {
		t.Fatal("StopPoller removed PID custody after SIGTERM failure")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("PID file stat = %v, want custody preserved", err)
	}
}

func TestStopPollerSerializesStartCustodyTransition(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	signalStarted := make(chan struct{})
	releaseSignal := make(chan struct{})
	launchStarted := make(chan struct{})
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- stopPollerWithOps(townRoot, session,
			func(string) ([]byte, error) { return []byte("123"), nil },
			func(int) bool { return true },
			func(int) error {
				close(signalStarted)
				<-releaseSignal
				return nil
			},
			os.Remove,
		)
	}()
	<-signalStarted

	launcher := func(string, string, []string) (pollerLaunch, error) {
		close(launchStarted)
		return pollerLaunch{pid: os.Getpid(), identity: testPollerIdentity(session)}, nil
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := startPollerWithLauncherStatus(townRoot, session, nil, launcher, os.WriteFile, func(root, name string) (int, bool, error) {
			data, err := os.ReadFile(pollerPidFile(root, name))
			if os.IsNotExist(err) {
				return 0, false, nil
			}
			record, parseErr := parsePollerRecord(string(data))
			return record.PID, parseErr == nil, parseErr
		})
		startDone <- err
	}()

	select {
	case <-launchStarted:
		t.Fatal("StartPoller entered launch while StopPoller held custody lock")
	case <-time.After(3 * time.Second):
	}
	close(releaseSignal)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pollerPidFile(townRoot, session)); err != nil {
		t.Fatalf("final PID custody missing after serialized transition: %v", err)
	}
}

func TestStopPollerIdentityMismatchQuarantinesWithoutSignal(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := formatPollerRecord(123, pollerIdentity{StartTime: "old", Command: "gt nudge-poller " + session}, session)
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), []byte(record), 0644); err != nil {
		t.Fatal(err)
	}
	signaled := false
	quarantined := ""
	err := stopPollerWithOwnershipOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "new", Command: "gt nudge-poller " + session}, nil
		},
		func(int) error { signaled = true; return nil },
		func(int, pollerRecord) error { return nil },
		func(path string, _ []byte) error { quarantined = path + ".stale-test"; return nil },
		os.Remove,
	)
	if err == nil {
		t.Fatal("identity mismatch returned nil error")
	}
	if signaled {
		t.Fatal("identity mismatch signaled unrelated process")
	}
	if quarantined == pollerPidFile(townRoot, session) || quarantined == "" {
		t.Fatalf("quarantine path = %q, want deterministic non-colliding stale path", quarantined)
	}
}

func TestStopPollerRemovesRecordOnlyAfterConfirmedExit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session}, Session: session}
	removed := ""
	waited := false
	err := stopPollerWithOwnershipOps(townRoot, session,
		func(string) ([]byte, error) { return []byte(formatPollerRecordValue(record)), nil },
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return record.Identity, nil },
		func(int) error { return nil },
		func(int, pollerRecord) error { waited = true; return nil },
		func(string, []byte) error { return nil },
		func(path string) error { removed = path; return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !waited || removed != pollerPidFile(townRoot, session) {
		t.Fatalf("confirmed exit custody = waited %v, removed %q", waited, removed)
	}
}

func TestStopPollerTimeoutPreservesRecord(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session}, Session: session}
	removed := false
	err := stopPollerWithOwnershipOps(townRoot, session,
		func(string) ([]byte, error) { return []byte(formatPollerRecordValue(record)), nil },
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return record.Identity, nil },
		func(int) error { return nil },
		func(int, pollerRecord) error { return errors.New("exit confirmation timeout") },
		func(string, []byte) error { removed = true; return nil },
		os.Remove,
	)
	if err == nil {
		t.Fatal("timeout returned nil error")
	}
	if removed {
		t.Fatal("timeout removed live process ownership record")
	}
}

func TestStopPollerReplacementRecordPreserved(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	oldRecord := []byte(formatPollerRecord(123, pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session}, session))
	newRecord := []byte(formatPollerRecord(456, pollerIdentity{StartTime: "new", Command: "gt nudge-poller " + session}, session))
	reads := 0
	removed := false
	err := stopPollerWithOwnershipOps(townRoot, session,
		func(string) ([]byte, error) {
			reads++
			if reads == 1 {
				return oldRecord, nil
			}
			return newRecord, nil
		},
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session}, nil
		},
		func(int) error { return nil },
		func(int, pollerRecord) error { return nil },
		func(string, []byte) error { return nil },
		func(string) error { removed = true; return nil },
	)
	if err == nil || removed || reads != 2 {
		t.Fatalf("replacement transition = err %v removed %v reads %d", err, removed, reads)
	}
}

func TestStopPollerRejectsSameStartDifferentCommand(t *testing.T) {
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session}, Session: session}
	signaled := false
	err := stopPollerWithOwnershipOps(t.TempDir(), session,
		func(string) ([]byte, error) { return []byte(formatPollerRecordValue(record)), nil },
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "same", Command: "gt other-command " + session}, nil
		},
		func(int) error { signaled = true; return nil },
		func(int, pollerRecord) error { return nil },
		func(string, []byte) error { return nil },
		func(string) error { return nil },
	)
	if err == nil || signaled {
		t.Fatalf("same-start command mismatch = err %v signaled %v", err, signaled)
	}
}

func TestStartPollerReadErrorBlocksLaunch(t *testing.T) {
	launched := false
	readErr := errors.New("permission denied")
	_, err := startPollerWithLauncherStatus(t.TempDir(), "gt-gastown-polecat-test", nil,
		func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil },
		os.WriteFile,
		func(string, string) (int, bool, error) { return 0, false, readErr },
	)
	if !errors.Is(err, readErr) || launched {
		t.Fatalf("read error start = err %v launched %v", err, launched)
	}
}

func TestStartPollerIdentityCleanupFailureIsReturned(t *testing.T) {
	cleanupErr := errors.New("wait failed")
	_, err := startPollerWithLauncher(t.TempDir(), "gt-gastown-polecat-test", nil,
		func(string, string, []string) (pollerLaunch, error) {
			return pollerLaunch{pid: 123, terminate: func() error { return cleanupErr }}, nil
		}, os.WriteFile)
	if !errors.Is(err, cleanupErr) || err == nil {
		t.Fatalf("identity cleanup error = %v, want joined cleanup failure", err)
	}
}

func TestPollerStatusLiveLegacyRequiresVerifiedMigration(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pid, alive, err := pollerStatusWithOps(townRoot, session,
		func(string) ([]byte, error) { return []byte("123"), nil },
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "current", Command: "gt nudge-poller " + session}, nil
		},
		func(string, []byte) error { t.Fatal("live legacy record quarantined"); return nil },
		func(string) error { t.Fatal("live legacy record removed"); return nil },
	)
	if err == nil || pid != 123 || !alive {
		t.Fatalf("live legacy status = pid %d alive %v err %v; want preserved migration error", pid, alive, err)
	}
}

func TestStopPollerRemovesDeadStructuredRecord(t *testing.T) {
	removed := false
	session := "gt-gastown-polecat-test"
	data := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	err := stopPollerWithOwnershipOps(t.TempDir(), session,
		func(string) ([]byte, error) { return data, nil },
		func(int) bool { return false },
		func(int) (pollerIdentity, error) {
			t.Fatal("dead structured identity was queried")
			return pollerIdentity{}, nil
		},
		func(int) error { t.Fatal("dead structured poller was signaled"); return nil },
		func(int, pollerRecord) error { t.Fatal("dead structured poller was waited on"); return nil },
		func(string, []byte) error { t.Fatal("dead structured record quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if err != nil || !removed {
		t.Fatalf("dead structured stop = err %v removed %v; want removal", err, removed)
	}
}

func TestStopPollerRemovesRecordWhenIdentityLookupSeesExit(t *testing.T) {
	removed := false
	livenessChecks := 0
	session := "gt-gastown-polecat-test"
	data := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	err := stopPollerWithOwnershipOps(t.TempDir(), session,
		func(string) ([]byte, error) { return data, nil },
		func(int) bool { livenessChecks++; return livenessChecks == 1 },
		func(int) (pollerIdentity, error) { return pollerIdentity{}, errors.New("identity unavailable") },
		func(int) error { t.Fatal("exited poller was signaled"); return nil },
		func(int, pollerRecord) error { t.Fatal("exited poller was waited on"); return nil },
		func(string, []byte) error { t.Fatal("exited record quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if err != nil || !removed || livenessChecks != 2 {
		t.Fatalf("identity-race stop = err %v removed %v liveness checks %d; want removal after recheck", err, removed, livenessChecks)
	}
}

func TestStartPollerLiveLegacyDoesNotLaunchDuplicate(t *testing.T) {
	launched := false
	_, err := startPollerWithLauncherStatus(t.TempDir(), "gt-gastown-polecat-test", nil,
		func(string, string, []string) (pollerLaunch, error) {
			launched = true
			return pollerLaunch{}, nil
		},
		os.WriteFile,
		func(string, string) (int, bool, error) {
			return 123, true, errors.New("legacy poller ownership requires separately verified migration")
		},
	)
	if err == nil || launched {
		t.Fatalf("live legacy start = err %v launched %v; want migration error without duplicate", err, launched)
	}
}

func TestPollerStatusDeadLegacyRemovesRecord(t *testing.T) {
	removed := false
	_, alive, err := pollerStatusWithOps(t.TempDir(), "gt-gastown-polecat-test",
		func(string) ([]byte, error) { return []byte("123"), nil },
		func(int) bool { return false },
		func(int) (pollerIdentity, error) {
			t.Fatal("dead legacy identity was queried")
			return pollerIdentity{}, nil
		},
		func(string, []byte) error { return nil },
		func(string) error { removed = true; return nil },
	)
	if err != nil || alive || !removed {
		t.Fatalf("dead legacy status = alive %v err %v removed %v", alive, err, removed)
	}
}

func TestPollerStatusMalformedLegacyPreservesRecord(t *testing.T) {
	removed := false
	_, alive, err := pollerStatusWithOps(t.TempDir(), "gt-gastown-polecat-test",
		func(string) ([]byte, error) { return []byte("not-a-pid"), nil },
		func(int) bool { t.Fatal("malformed legacy liveness was queried"); return false },
		func(int) (pollerIdentity, error) { return pollerIdentity{}, nil },
		func(string, []byte) error { return nil },
		func(string) error { removed = true; return nil },
	)
	if err == nil || alive || removed {
		t.Fatalf("malformed legacy status = alive %v err %v removed %v; want visible migration error and custody", alive, err, removed)
	}
}

func TestPollerPidFile(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-bear"

	pidFile := pollerPidFile(townRoot, session)
	expected := filepath.Join(townRoot, ".runtime", "nudge_poller", session+".pid")
	if pidFile != expected {
		t.Errorf("pollerPidFile() = %q, want %q", pidFile, expected)
	}
}

func TestPollerPidFile_SlashSanitized(t *testing.T) {
	townRoot := t.TempDir()
	session := "some/session"

	pidFile := pollerPidFile(townRoot, session)
	// Slashes should be replaced with underscores
	expected := filepath.Join(townRoot, ".runtime", "nudge_poller", "some_session.pid")
	if pidFile != expected {
		t.Errorf("pollerPidFile() = %q, want %q", pidFile, expected)
	}
}

func TestPollerAlive_NoPidFile(t *testing.T) {
	townRoot := t.TempDir()
	_, alive := pollerAlive(townRoot, "nonexistent-session")
	if alive {
		t.Error("pollerAlive() returned true for nonexistent PID file")
	}
}

func TestPollerAlive_StalePid(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	// Write a PID file with an invalid PID (process doesn't exist).
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	// Use a very high PID that's almost certainly not running.
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	_, alive := pollerAlive(townRoot, session)
	if alive {
		t.Error("pollerAlive() returned true for dead PID")
	}

	// Stale PID file should be cleaned up.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stale PID file was not cleaned up")
	}
}

func TestPollerAlive_CorruptPidFile(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}

	_, alive := pollerAlive(townRoot, session)
	if alive {
		t.Error("pollerAlive() returned true for corrupt PID file")
	}
}

func TestStopPoller_NoPidFile(t *testing.T) {
	townRoot := t.TempDir()
	// Should be a no-op, no error.
	if err := StopPoller(townRoot, "nonexistent"); err != nil {
		t.Errorf("StopPoller() unexpected error: %v", err)
	}
}

func TestStopPoller_StalePid(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	// Write a stale PID file.
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	// Should succeed and clean up the stale PID file.
	if err := StopPoller(townRoot, session); err != nil {
		t.Errorf("StopPoller() unexpected error: %v", err)
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("StopPoller did not clean up stale PID file")
	}
}

func TestPollerAlive_LiveProcess(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"

	// Write our own PID — we're definitely alive.
	pidDir := pollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	myPid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(myPid)), 0644); err != nil {
		t.Fatal(err)
	}

	pid, alive := pollerAlive(townRoot, session)
	if !alive {
		t.Error("pollerAlive() returned false for live process")
	}
	if pid != myPid {
		t.Errorf("pollerAlive() pid = %d, want %d", pid, myPid)
	}
}

func TestBuildPollerCommand_UsesDetachedProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group management is not supported on Windows")
	}
	townRoot := t.TempDir()
	env := []string{"PATH=/usr/bin", "GT_TOWN_SOCKET=private-town"}
	cmd := buildPollerCommand("/tmp/fake-gt", townRoot, "gt-gastown-crew-bear", env)

	if got, want := cmd.Dir, townRoot; got != want {
		t.Fatalf("cmd.Dir = %q, want %q", got, want)
	}
	if got, want := cmd.Path, "/tmp/fake-gt"; got != want {
		t.Fatalf("cmd.Path = %q, want %q", got, want)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "nudge-poller" || cmd.Args[2] != "gt-gastown-crew-bear" {
		t.Fatalf("cmd.Args = %#v, want poller invocation", cmd.Args)
	}
	if cmd.Cancel != nil {
		t.Fatal("buildPollerCommand() installed cmd.Cancel; detached pollers must leave it nil")
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("buildPollerCommand() should discard stdout/stderr")
	}
	if got := strings.Join(cmd.Env, "|"); got != "PATH=/usr/bin|GT_TOWN_SOCKET=private-town" {
		t.Fatalf("buildPollerCommand() env = %q, want isolated child environment", got)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("buildPollerCommand() did not configure SysProcAttr")
	}
}

func TestSetProcessGroup_InstallsCancelHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SetProcessGroup is a no-op on Windows")
	}
	cmd := exec.Command("true")
	util.SetProcessGroup(cmd)

	if cmd.Cancel == nil {
		t.Fatal("SetProcessGroup() should install a cancel hook")
	}
}
