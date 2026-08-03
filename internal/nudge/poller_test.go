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
		return pollerLaunch{pid: os.Getpid()}, nil
	}

	results := make(chan int, 2)
	errs := make(chan error, 2)
	start := func() {
		pid, err := startPollerWithLauncher(townRoot, session, nil, launcher, os.WriteFile)
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
		return pollerLaunch{pid: os.Getpid()}, nil
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := startPollerWithLauncher(townRoot, session, nil, launcher, os.WriteFile)
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
