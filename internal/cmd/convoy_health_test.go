package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/health"
)

func TestRunConvoyCheckRecordsHealthEvidence(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)

	oldCheck, oldWrite := checkCompletedConvoysForCommand, writeConvoyCheckHealthEvidence
	checkCompletedConvoysForCommand = func(context.Context, string, bool, bool) (convoyCheckSummary, error) {
		return convoyCheckSummary{Checked: 3, SkippedUncertain: 2, TimedOut: true}, nil
	}
	var gotRoot string
	var got health.ConvoyCheckState
	writeConvoyCheckHealthEvidence = func(root string, state health.ConvoyCheckState) (string, error) {
		gotRoot, got = root, state
		return filepath.Join(root, ".runtime", "health", "convoy-check.json"), nil
	}
	t.Cleanup(func() {
		checkCompletedConvoysForCommand = oldCheck
		writeConvoyCheckHealthEvidence = oldWrite
	})

	err := runConvoyCheck(nil, nil)
	if err == nil {
		t.Fatal("runConvoyCheck returned nil for timed-out summary")
	}
	if gotRoot != townRoot || got.SchemaVersion != 1 || !got.TimedOut || got.SkippedUncertain != 2 || got.CheckedAt.IsZero() {
		t.Fatalf("recorded root/state = %q, %#v", gotRoot, got)
	}
}
