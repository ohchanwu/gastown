//go:build !integration

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestMain(m *testing.M) {
	BuiltProperly = "1"
	baseline := doltserver.FindAllDoltListeners()
	code := m.Run()
	if leaked := newListenerPIDs(baseline, doltserver.FindAllDoltListeners()); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "cmd TestMain: new Dolt listener PIDs remained at package exit: %v\n", leaked)
		code = 1
	}
	os.Exit(code)
}

func canonicalTestTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return resolved
}
