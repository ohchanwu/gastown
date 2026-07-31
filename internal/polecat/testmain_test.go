package polecat

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/util"
)

func TestMain(m *testing.M) {
	// Unit tests create disposable worktrees and must not depend on host disk
	// pressure. Disk policy itself is covered by internal/util tests.
	checkDiskSpace = func(string) (util.DiskSpaceLevel, string, error) {
		return util.DiskSpaceOK, "", nil
	}
	code := m.Run()
	testutil.TerminateDoltContainer()
	os.Exit(code)
}
