package doctor

import (
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

func TestStalledPolecatCheck_Properties(t *testing.T) {
	check := NewStalledPolecatCheck()

	if check.Name() != "stalled-polecats" {
		t.Errorf("Name() = %q, want %q", check.Name(), "stalled-polecats")
	}

	if check.Description() == "" {
		t.Error("Description() should not be empty")
	}

	if !check.CanFix() {
		t.Error("CanFix() should be true — stalled polecats can have branches pushed")
	}

	if check.Category() != CategoryCleanup {
		t.Errorf("Category() = %q, want %q", check.Category(), CategoryCleanup)
	}
}

func TestStalledPolecatCheck_EmptyTownRoot(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir}

	check := NewStalledPolecatCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK for empty town root", result.Status)
	}
}

func TestStalledPolecatCheck_NoPolecats(t *testing.T) {
	tmpDir := t.TempDir()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}

	check := NewStalledPolecatCheck()
	result := check.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %v, want OK when no polecats dir exists", result.Status)
	}
}

func TestStalledPolecatCheck_FixNoStalled(t *testing.T) {
	check := NewStalledPolecatCheck()
	// Fix with no stalled polecats should be a no-op
	if err := check.Fix(&CheckContext{TownRoot: t.TempDir()}); err != nil {
		t.Errorf("Fix() with no stalled polecats returned error: %v", err)
	}
}

func TestStalledPolecatCheck_ResolveClonePath_NoDir(t *testing.T) {
	check := NewStalledPolecatCheck()
	path := check.resolveClonePath(t.TempDir(), "testrig", "furiosa")
	if path != "" {
		t.Errorf("resolveClonePath() = %q, want empty for nonexistent", path)
	}
}

func TestShouldReportUnpushedPolecat(t *testing.T) {
	tests := []struct {
		name          string
		unpushedCount int
		cleanupStatus polecat.CleanupStatus
		want          bool
	}{
		{name: "ordinary unpushed work is at risk", unpushedCount: 1, cleanupStatus: polecat.CleanupUnpushed, want: true},
		{name: "preserved work is deliberately retained", unpushedCount: 1, cleanupStatus: polecat.CleanupStatus("preserved"), want: false},
		{name: "no unpushed work", cleanupStatus: polecat.CleanupUnknown, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldReportUnpushedPolecat(tt.unpushedCount, tt.cleanupStatus); got != tt.want {
				t.Fatalf("shouldReportUnpushedPolecat() = %v, want %v", got, tt.want)
			}
		})
	}
}
