//go:build !integration

package cmd

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestMain(m *testing.M) {
	baseline := doltserver.FindAllDoltListeners()
	code := m.Run()
	if leaked := newListenerPIDs(baseline, doltserver.FindAllDoltListeners()); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "cmd TestMain: new Dolt listener PIDs remained at package exit: %v\n", leaked)
		code = 1
	}
	os.Exit(code)
}
