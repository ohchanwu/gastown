package polecat

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	testMainCleanupChildEnv    = "GT_POLECAT_TESTMAIN_CLEANUP_CHILD"
	testMainCleanupEvidenceEnv = "GT_POLECAT_TESTMAIN_CLEANUP_EVIDENCE"
	testMainKillErrorChildEnv  = "GT_POLECAT_TESTMAIN_KILL_ERROR_CHILD"
)

func envWithPath(path string) []string {
	env := []string{"PATH=" + path}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PATH=") {
			env = append(env, value)
		}
	}
	return env
}

func TestTestMainWithoutTmuxDoesNotFail(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$", "-test.count=1")
	cmd.Env = envWithPath(t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("filtered child without tmux: %v\n%s", err, out)
	}
}

func TestTestMainPreservesSocketRootOnKillError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux not supported on Windows")
	}

	evidence := filepath.Join(t.TempDir(), "kill-error-root.txt")
	if os.Getenv(testMainKillErrorChildEnv) == "1" {
		evidence = os.Getenv(testMainCleanupEvidenceEnv)
		socketRoot := tmux.SocketDir()
		if err := os.MkdirAll(socketRoot, 0o700); err != nil {
			t.Fatalf("create child socket root: %v", err)
		}
		socketPath := filepath.Join(socketRoot, tmux.GetDefaultSocket())
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
		if err != nil {
			t.Fatalf("create child socket: %v", err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatalf("close child socket: %v", err)
		}
		if err := os.WriteFile(evidence, []byte(os.Getenv("TMUX_TMPDIR")), 0o600); err != nil {
			t.Fatalf("write kill-error evidence: %v", err)
		}
		if err := os.Setenv("PATH", t.TempDir()); err != nil {
			t.Fatalf("remove tmux from child PATH: %v", err)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTestMainPreservesSocketRootOnKillError$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		testMainKillErrorChildEnv+"=1",
		testMainCleanupEvidenceEnv+"="+evidence,
	)
	if err := cmd.Run(); err == nil {
		t.Fatal("child with tmux kill error exited 0, want failure")
	}
	rootData, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatalf("read kill-error evidence: %v", err)
	}
	root := string(rootData)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("preserved tmux recovery root: %v", err)
	}
}

func TestTestMainCleansTmuxResources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tmux not supported on Windows")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	if os.Getenv(testMainCleanupChildEnv) == "1" {
		tm := tmux.NewTmux()
		if err := tm.NewSessionWithCommand("gt-testmain-cleanup-child", t.TempDir(), "sleep 300"); err != nil {
			t.Fatalf("create child tmux session: %v", err)
		}
		evidence := os.Getenv(testMainCleanupEvidenceEnv)
		data := os.Getenv("TMUX_TMPDIR") + "\n" + tmux.GetDefaultSocket() + "\n"
		if err := os.WriteFile(evidence, []byte(data), 0o600); err != nil {
			t.Fatalf("write child cleanup evidence: %v", err)
		}
		return
	}

	evidence := filepath.Join(t.TempDir(), "child-tmux.txt")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTestMainCleansTmuxResources$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		testMainCleanupChildEnv+"=1",
		testMainCleanupEvidenceEnv+"="+evidence,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child test process: %v\n%s", err, out)
	}

	data, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatalf("read child cleanup evidence: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		t.Fatalf("child cleanup evidence fields = %d, want 2", len(fields))
	}
	childSocketDir, childSocket := fields[0], fields[1]

	oldSocketDir, hadSocketDir := os.LookupEnv("TMUX_TMPDIR")
	if err := os.Setenv("TMUX_TMPDIR", childSocketDir); err != nil {
		t.Fatalf("set child TMUX_TMPDIR: %v", err)
	}
	childTmux := tmux.NewTmuxWithSocket(childSocket)
	serverLeaked := childTmux.ServerPID() != 0
	if serverLeaked {
		_ = childTmux.KillServer()
	}
	if hadSocketDir {
		_ = os.Setenv("TMUX_TMPDIR", oldSocketDir)
	} else {
		_ = os.Unsetenv("TMUX_TMPDIR")
	}

	_, statErr := os.Stat(childSocketDir)
	rootLeaked := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		t.Fatalf("stat child tmux root: %v", statErr)
	}
	_ = os.RemoveAll(childSocketDir)

	if serverLeaked || rootLeaked {
		t.Fatalf("child TestMain leaked tmux resources: server=%t socket_root=%t", serverLeaked, rootLeaked)
	}
}

func stopTestTmuxServers() error {
	entries, err := os.ReadDir(tmux.SocketDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var firstErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := tmux.NewTmuxWithSocket(entry.Name()).KillServer(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("kill %s: %w", entry.Name(), err)
		}
	}
	return firstErr
}

func TestMain(m *testing.M) {
	socketDir, err := os.MkdirTemp("/tmp", "gt-polecat-tmux-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create tmux test socket directory: %v\n", err)
		os.Exit(1)
	}
	oldSocketDir, hadSocketDir := os.LookupEnv("TMUX_TMPDIR")
	_ = os.Setenv("TMUX_TMPDIR", socketDir)
	socket := fmt.Sprintf("gt-polecat-test-%d", os.Getpid())
	tmux.SetDefaultSocket(socket)

	// Unit tests create disposable worktrees and must not depend on host disk
	// pressure. Disk policy itself is covered by internal/util tests.
	checkDiskSpace = func(string) (util.DiskSpaceLevel, string, error) {
		return util.DiskSpaceOK, "", nil
	}
	code := m.Run()
	stopErr := stopTestTmuxServers()
	tmux.SetDefaultSocket("")
	if hadSocketDir {
		err = os.Setenv("TMUX_TMPDIR", oldSocketDir)
	} else {
		err = os.Unsetenv("TMUX_TMPDIR")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore TMUX_TMPDIR: %v\n", err)
		code = 1
	}
	if stopErr != nil {
		fmt.Fprintf(os.Stderr, "stop tmux test servers: %v; recovery root preserved at %s\n", stopErr, socketDir)
		code = 1
	} else if err := os.RemoveAll(socketDir); err != nil {
		fmt.Fprintf(os.Stderr, "remove tmux test socket directory: %v\n", err)
		code = 1
	}
	testutil.TerminateDoltContainer()
	os.Exit(code)
}
