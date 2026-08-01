package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/health"
)

func TestRunHealthReturnsNonzeroForControlPlaneFailure(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)

	oldCollect, oldServer, oldProcess, oldJSON := collectControlPlaneHealth, checkServerHealthForCommand, checkProcessHealthForCommand, healthJSON
	checkServerHealthForCommand = func(string) *ServerHealth { return &ServerHealth{Running: false} }
	checkProcessHealthForCommand = func(string) *ProcessHealth {
		return &ProcessHealth{ActionableCount: 7}
	}
	var gotActionable int
	collectControlPlaneHealth = func(_ string, _ string, actionable int, reachable bool) (health.ControlPlaneVerdict, error) {
		gotActionable = actionable
		if reachable {
			t.Fatal("temporary town unexpectedly reported canonical Dolt reachable")
		}
		return health.ControlPlaneVerdict{Failures: []health.ControlPlaneFailure{{
			Subsystem: "wake-delivery", Diagnostic: "gt nudge --help",
		}}}, nil
	}
	healthJSON = true
	t.Cleanup(func() {
		collectControlPlaneHealth = oldCollect
		checkServerHealthForCommand = oldServer
		checkProcessHealthForCommand = oldProcess
		healthJSON = oldJSON
	})

	err := runHealth(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "wake-delivery") {
		t.Fatalf("runHealth error = %v, want failed subsystem", err)
	}
	if gotActionable != 7 {
		t.Fatalf("control-plane actionable count = %d, want process snapshot 7", gotActionable)
	}
}
