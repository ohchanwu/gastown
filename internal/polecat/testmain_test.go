package polecat

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/util"
)

func TestMain(m *testing.M) {
	socketDir, err := os.MkdirTemp("/tmp", "gt-polecat-tmux-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create tmux test socket directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(socketDir)
	oldSocketDir, hadSocketDir := os.LookupEnv("TMUX_TMPDIR")
	_ = os.Setenv("TMUX_TMPDIR", socketDir)
	defer func() {
		if hadSocketDir {
			_ = os.Setenv("TMUX_TMPDIR", oldSocketDir)
		} else {
			_ = os.Unsetenv("TMUX_TMPDIR")
		}
	}()
	tmux.SetDefaultSocket(fmt.Sprintf("gt-polecat-test-%d", os.Getpid()))
	defer tmux.SetDefaultSocket("")

	// Unit tests create disposable worktrees and must not depend on host disk
	// pressure. Disk policy itself is covered by internal/util tests.
	checkDiskSpace = func(string) (util.DiskSpaceLevel, string, error) {
		return util.DiskSpaceOK, "", nil
	}
	code := m.Run()
	testutil.TerminateDoltContainer()
	os.Exit(code)
}
