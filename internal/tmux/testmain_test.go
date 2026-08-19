package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

const storedPaneReceiverRunArg = "-test.run=^TestNudgeStoredPaneReceiverHelper$"

func isStoredPaneNudgeReceiverInvocation(mode, nonce string, args []string) bool {
	if mode != "1" || nonce == "" {
		return false
	}
	for _, arg := range args {
		if arg == storedPaneReceiverRunArg {
			return true
		}
	}
	return false
}

// TestMain sets up a dedicated tmux server for the package's integration tests.
// All tests that call newTestTmux() share this isolated server, which is torn
// down after all tests complete. This prevents test sessions from appearing on
// the user's interactive tmux and avoids socket conflicts with other packages.
func TestMain(m *testing.M) {
	if requested, err := runSessionCustodyInit(); requested {
		if err != nil {
			fmt.Fprintf(os.Stderr, "session custody init: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if isStoredPaneNudgeReceiverInvocation(
		os.Getenv(storedPaneReceiverEnv),
		os.Getenv(storedPaneNonceEnv),
		os.Args[1:],
	) ||
		os.Getenv("GT_TEST_SESSION_CUSTODY_HELPER") != "" ||
		os.Getenv("GT_TEST_SESSION_CUSTODY_WORKLOAD") != "" ||
		os.Getenv("GT_TEST_SESSION_CGROUP_PROVISION_HELPER") != "" {
		os.Exit(m.Run())
	}
	socket := fmt.Sprintf("gt-test-%d", os.Getpid())
	socketDir, err := os.MkdirTemp("/tmp", "gt-tmux-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create tmux test socket directory: %v\n", err)
		os.Exit(1)
	}
	oldSocketDir, hadSocketDir := os.LookupEnv("TMUX_TMPDIR")
	_ = os.Setenv("TMUX_TMPDIR", socketDir)
	// Every server created by this package must ignore personal tmux
	// configuration, not only the package sentinel. Secondary-server tests use
	// the production constructor, so give tmux an empty home/config root for the
	// entire package process.
	_ = os.Setenv("HOME", socketDir)
	_ = os.Setenv("XDG_CONFIG_HOME", socketDir)

	// Set defaultSocket so NewTmux() connects to the test server, not the
	// user's personal server or the sentinel that indicates "no town context".
	SetDefaultSocket(socket)

	// Start a sentinel session to keep the server alive for the entire test run.
	// Without this, tests that kill their last session inadvertently take down
	// the server, leaving a stale socket that prevents subsequent new-session
	// calls from restarting it (tmux sees the socket file but no listener).
	// The sentinel uses a name no individual test touches, so it outlives all
	// per-test sessions. Ignore the user's tmux configuration: synchronous
	// plugins can otherwise block before m.Run starts, outside Go's test timeout.
	// TestMain kills the whole server at the end.
	if _, err := exec.LookPath("tmux"); err == nil {
		_ = exec.Command("tmux", "-u", "-f", os.DevNull, "-L", socket, "new-session", "-d", "-s", "gt-test-sentinel").Run()
	}

	code := m.Run()

	// Kill the test tmux server and restore the original socket state.
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	SetDefaultSocket("")
	if hadSocketDir {
		_ = os.Setenv("TMUX_TMPDIR", oldSocketDir)
	} else {
		_ = os.Unsetenv("TMUX_TMPDIR")
	}
	_ = os.RemoveAll(socketDir)

	os.Exit(code)
}
