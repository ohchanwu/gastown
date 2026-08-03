package nudge

import (
	"bytes"
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
	return pollerIdentity{StartTime: "test-start", Command: "gt nudge-poller " + session, Generation: "test-generation", Transport: "fixture\x00fixture"}
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

func TestNormalizePollerTransportAliases(t *testing.T) {
	if normalizePollerTransport([]string{"GT_TOWN_SOCKET=a", "GT_TMUX_SOCKET=a"}) != "a\x00a" {
		t.Fatal("alias normalization mismatch")
	}
	if normalizePollerTransport([]string{"GT_TOWN_SOCKET=a"}) != "a\x00a" {
		t.Fatal("single town alias mismatch")
	}
	if normalizePollerTransport([]string{"GT_TMUX_SOCKET=a"}) != "a\x00a" {
		t.Fatal("single tmux alias mismatch")
	}
}

func TestStartPollerDifferentTransportFailsClosedWithoutLaunch(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = "old\x00old"
	_ = os.MkdirAll(pollerPidDir(root), 0755)
	_ = os.WriteFile(pollerPidFile(root, session), []byte(formatPollerRecord(1, identity, session)), 0644)
	launched := false
	_, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=new"}, func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil }, os.WriteFile, func(string, string) (int, bool, error) { return 1, true, nil })
	if err == nil || launched {
		t.Fatalf("transport mismatch err=%v launched=%v", err, launched)
	}
}

func TestStartPollerSameTransportReusesLivePID(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = "a\x00a"
	_ = os.MkdirAll(pollerPidDir(root), 0755)
	_ = os.WriteFile(pollerPidFile(root, session), []byte(formatPollerRecord(7, identity, session)), 0644)
	launched := false
	pid, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=a"}, func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil }, os.WriteFile, func(string, string) (int, bool, error) { return 7, true, nil })
	if err != nil || pid != 7 || launched {
		t.Fatalf("reuse err=%v pid=%d launched=%v", err, pid, launched)
	}
}

func TestStartPollerMissingTransportFailsClosed(t *testing.T) {
	root, session := t.TempDir(), "s"
	identity := testPollerIdentity(session)
	identity.Transport = ""
	_ = os.MkdirAll(pollerPidDir(root), 0755)
	_ = os.WriteFile(pollerPidFile(root, session), []byte(`{"PID":7,"Identity":{"StartTime":"test-start","Command":"gt nudge-poller s","Generation":"g"},"Session":"s"}`), 0644)
	launched := false
	_, err := startPollerWithLauncherStatus(root, session, []string{"GT_TOWN_SOCKET=a"}, func(string, string, []string) (pollerLaunch, error) { launched = true; return pollerLaunch{}, nil }, os.WriteFile, func(string, string) (int, bool, error) { return 7, true, nil })
	if err == nil || launched {
		t.Fatalf("missing transport err=%v launched=%v", err, launched)
	}
}

func TestStartPollerNilEnvUsesInheritedTransport(t *testing.T) {
	if normalizePollerTransport(effectivePollerEnv(nil)) != normalizePollerTransport(os.Environ()) {
		t.Fatal("nil env did not inherit")
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

func TestStartPollerEmptyGenerationTerminatesWithoutWriting(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	var terminated, wrote atomic.Bool

	pid, err := startPollerWithLauncher(townRoot, session, nil,
		func(string, string, []string) (pollerLaunch, error) {
			return pollerLaunch{
				pid: 123,
				identity: pollerIdentity{
					StartTime: "test-start",
					Command:   "gt nudge-poller " + session,
				},
				terminate: func() error { terminated.Store(true); return nil },
			}, nil
		},
		func(string, []byte, os.FileMode) error { wrote.Store(true); return nil },
	)
	if err == nil || pid != 0 || !terminated.Load() || wrote.Load() {
		t.Fatalf("empty generation start = pid %d err %v terminated %v wrote %v", pid, err, terminated.Load(), wrote.Load())
	}
}

func TestStopPollerRequestFailurePreservesCustody(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	pidPath := pollerPidFile(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pidPath, record, 0644); err != nil {
		t.Fatal(err)
	}
	requestErr := errors.New("injected stop request failure")
	removed := false

	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
		func([]byte) error { return requestErr },
		func(int, pollerRecord) error { t.Fatal("failed stop request waited for exit"); return nil },
		func(string, []byte) error { t.Fatal("matching ownership quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if !errors.Is(err, requestErr) {
		t.Fatalf("error = %v, want stop request failure", err)
	}
	if removed {
		t.Fatal("StopPoller removed custody after stop request failure")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("ownership stat = %v, want custody preserved", err)
	}
}

func TestStopPollerSerializesStartCustodyTransition(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	launchStarted := make(chan struct{})
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	record := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pollerPidFile(townRoot, session), record, 0644); err != nil {
		t.Fatal(err)
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- stopPollerWithGenerationOps(townRoot, session,
			os.ReadFile,
			func(int) bool { return true },
			func(int) (pollerIdentity, error) { return testPollerIdentity(session), nil },
			func(data []byte) error {
				close(stopStarted)
				<-releaseStop
				return os.WriteFile(pollerStopFile(townRoot, session), data, 0600)
			},
			func(int, pollerRecord) error { return nil },
			func(string, []byte) error { return nil },
			os.Remove,
		)
	}()
	<-stopStarted

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
	case <-time.After(500 * time.Millisecond):
	}
	close(releaseStop)
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
	record := formatPollerRecord(123, pollerIdentity{StartTime: "old", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, session)
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), []byte(record), 0644); err != nil {
		t.Fatal(err)
	}
	stopRequested := false
	quarantined := ""
	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "new", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, nil
		},
		func([]byte) error { stopRequested = true; return nil },
		func(int, pollerRecord) error { return nil },
		func(path string, _ []byte) error { quarantined = path + ".stale-test"; return nil },
		os.Remove,
	)
	if err == nil {
		t.Fatal("identity mismatch returned nil error")
	}
	if stopRequested {
		t.Fatal("identity mismatch published a cooperative stop request")
	}
	if quarantined == pollerPidFile(townRoot, session) || quarantined == "" {
		t.Fatalf("quarantine path = %q, want deterministic non-colliding stale path", quarantined)
	}
}

func TestStopPollerRemovesRecordOnlyAfterConfirmedExit(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, Session: session}
	data := []byte(formatPollerRecordValue(record))
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), data, 0644); err != nil {
		t.Fatal(err)
	}
	removed := make([]string, 0, 2)
	waited := false
	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return record.Identity, nil },
		func(stopData []byte) error { return os.WriteFile(pollerStopFile(townRoot, session), stopData, 0600) },
		func(int, pollerRecord) error { waited = true; return nil },
		func(string, []byte) error { return nil },
		func(path string) error { removed = append(removed, path); return os.Remove(path) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !waited || len(removed) != 2 || removed[0] != pollerStopFile(townRoot, session) || removed[1] != pollerPidFile(townRoot, session) {
		t.Fatalf("confirmed exit custody = waited %v, removed %q", waited, removed)
	}
}

func TestStopPollerPreservesMismatchedStopRequest(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	oldRecord := pollerRecord{PID: 123, Identity: testPollerIdentity(session), Session: session}
	oldData := []byte(formatPollerRecordValue(oldRecord))
	replacement := oldRecord
	replacement.Identity.Generation = "replacement-generation"
	replacementData := []byte(formatPollerRecordValue(replacement))
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), oldData, 0644); err != nil {
		t.Fatal(err)
	}

	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return oldRecord.Identity, nil },
		func([]byte) error { return os.WriteFile(pollerStopFile(townRoot, session), replacementData, 0600) },
		func(int, pollerRecord) error { return nil },
		func(string, []byte) error { t.Fatal("matching ownership quarantined"); return nil },
		os.Remove,
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(pollerStopFile(townRoot, session))
	if err != nil || !bytes.Equal(got, replacementData) {
		t.Fatalf("replacement stop request = %q, err %v", got, err)
	}
	if _, err := os.Stat(pollerPidFile(townRoot, session)); !os.IsNotExist(err) {
		t.Fatalf("old ownership still present after confirmed exit: %v", err)
	}
}

func TestStopPollerTimeoutPreservesRecord(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, Session: session}
	data := []byte(formatPollerRecordValue(record))
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), data, 0644); err != nil {
		t.Fatal(err)
	}
	removed := false
	err := stopPollerWithGenerationOps(townRoot, session,
		os.ReadFile,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) { return record.Identity, nil },
		func(stopData []byte) error { return os.WriteFile(pollerStopFile(townRoot, session), stopData, 0600) },
		func(int, pollerRecord) error { return errors.New("exit confirmation timeout") },
		func(string, []byte) error { t.Fatal("matching ownership quarantined"); return nil },
		func(string) error { removed = true; return nil },
	)
	if err == nil {
		t.Fatal("timeout returned nil error")
	}
	if removed {
		t.Fatal("timeout removed live process ownership record")
	}
	for _, path := range []string{pollerPidFile(townRoot, session), pollerStopFile(townRoot, session)} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("timeout custody %s: %v", path, statErr)
		}
	}
}

func TestWaitPollerExitTreatsReusedPIDAsOriginalExit(t *testing.T) {
	session := "gt-gastown-polecat-test"
	record := pollerRecord{PID: 123, Identity: testPollerIdentity(session), Session: session}
	identityChecks := 0
	err := waitPollerExitWithOps(record.PID, record,
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			identityChecks++
			return pollerIdentity{
				StartTime:  "reused-start",
				Command:    "unrelated-process",
				Generation: "unrelated-generation",
			}, nil
		},
	)
	if err != nil || identityChecks != 1 {
		t.Fatalf("PID reuse exit = err %v identityChecks %d", err, identityChecks)
	}
}

func TestStopPollerReplacementRecordPreserved(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	oldRecord := []byte(formatPollerRecord(123, pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, session))
	newRecord := []byte(formatPollerRecord(456, pollerIdentity{StartTime: "new", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, session))
	reads := 0
	removed := false
	err := stopPollerWithGenerationOps(townRoot, session,
		func(string) ([]byte, error) {
			reads++
			if reads == 1 {
				return oldRecord, nil
			}
			return newRecord, nil
		},
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, nil
		},
		func(stopData []byte) error {
			if !bytes.Equal(stopData, oldRecord) {
				t.Fatalf("stop request = %q, want exact old ownership", stopData)
			}
			return nil
		},
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
	record := pollerRecord{PID: 123, Identity: pollerIdentity{StartTime: "same", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, Session: session}
	stopRequested := false
	err := stopPollerWithGenerationOps(t.TempDir(), session,
		func(string) ([]byte, error) { return []byte(formatPollerRecordValue(record)), nil },
		func(int) bool { return true },
		func(int) (pollerIdentity, error) {
			return pollerIdentity{StartTime: "same", Command: "gt other-command " + session, Generation: "fixture-generation"}, nil
		},
		func([]byte) error { stopRequested = true; return nil },
		func(int, pollerRecord) error { return nil },
		func(string, []byte) error { return nil },
		func(string) error { return nil },
	)
	if err == nil || stopRequested {
		t.Fatalf("same-start command mismatch = err %v stopRequested %v", err, stopRequested)
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
			return pollerIdentity{StartTime: "current", Command: "gt nudge-poller " + session, Generation: "fixture-generation"}, nil
		},
		func(string, []byte) error { t.Fatal("live legacy record quarantined"); return nil },
		func(string) error { t.Fatal("live legacy record removed"); return nil },
	)
	if err == nil || pid != 123 || !alive {
		t.Fatalf("live legacy status = pid %d alive %v err %v; want preserved migration error", pid, alive, err)
	}
}

func TestStopRequestedIsSessionBound(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	data := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	if err := os.WriteFile(pollerStopFile(townRoot, session), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), data, 0600); err != nil {
		t.Fatal(err)
	}
	if !StopRequested(townRoot, session) || StopRequested(townRoot, session+"-other") {
		t.Fatal("cooperative stop generation was not session-bound")
	}
}

func TestStopRequestedRejectsStaleGeneration(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-polecat-test"
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	old := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	current := []byte(formatPollerRecord(123, pollerIdentity{StartTime: "test-start", Command: "gt nudge-poller " + session, Generation: "new-generation"}, session))
	if err := os.WriteFile(pollerStopFile(townRoot, session), old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pollerPidFile(townRoot, session), current, 0600); err != nil {
		t.Fatal(err)
	}
	if StopRequested(townRoot, session) {
		t.Fatal("stale stop request matched replacement ownership")
	}
}

func TestPollerRecordRoundTripsDelimitersAndGeneration(t *testing.T) {
	session := "session|with|pipes"
	identity := pollerIdentity{StartTime: "start", Command: "gt|nudge-poller|" + session, Generation: "generation-b"}
	record, err := parsePollerRecord(formatPollerRecord(123, identity, session))
	if err != nil || record.Identity != identity || record.Session != session {
		t.Fatalf("record round trip = %#v, err %v", record, err)
	}
}

func TestStopPollerRemovesDeadStructuredRecord(t *testing.T) {
	removed := false
	session := "gt-gastown-polecat-test"
	data := []byte(formatPollerRecord(123, testPollerIdentity(session), session))
	err := stopPollerWithGenerationOps(t.TempDir(), session,
		func(string) ([]byte, error) { return data, nil },
		func(int) bool { return false },
		func(int) (pollerIdentity, error) {
			t.Fatal("dead structured identity was queried")
			return pollerIdentity{}, nil
		},
		func([]byte) error { t.Fatal("dead structured poller received a stop request"); return nil },
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
	err := stopPollerWithGenerationOps(t.TempDir(), session,
		func(string) ([]byte, error) { return data, nil },
		func(int) bool { livenessChecks++; return livenessChecks == 1 },
		func(int) (pollerIdentity, error) { return pollerIdentity{}, errors.New("identity unavailable") },
		func([]byte) error { t.Fatal("exited poller received a stop request"); return nil },
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
