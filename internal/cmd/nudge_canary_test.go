package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

func TestRunWakeCanaryRejectsLiveMayorIdentity(t *testing.T) {
	evidenceRoot := t.TempDir()
	_, statePath, err := runWakeCanary(nil, t.TempDir(), evidenceRoot, "not-the-isolated-mayor", wakeCanaryTurns)
	if err == nil || !strings.Contains(err.Error(), "isolated") {
		t.Fatalf("runWakeCanary live Mayor error = %v, want isolated identity rejection", err)
	}
	data, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read failed canary state: %v", readErr)
	}
	if !strings.Contains(string(data), `"result": "failed"`) || !strings.Contains(string(data), `"failure_code": "identity-not-isolated"`) {
		t.Fatalf("failed canary state = %s", data)
	}
}

func TestIsolatedCanaryEnvStripsLiveRouting(t *testing.T) {
	got := filterIsolatedCanaryEnv([]string{
		"PATH=/usr/bin",
		"GT_DOLT_HOST=live",
		"BD_DB=live",
		"BEADS_DOLT_SERVER_PORT=3306",
		"DOLT_ROOT_PATH=/live",
	})
	if strings.Join(got, "\n") != "PATH=/usr/bin" {
		t.Fatalf("isolated environment retained live routing: %v", got)
	}
}

func TestNewWakeCanarySandboxIsPrivateAndIsolated(t *testing.T) {
	parent := t.TempDir()
	sandbox, err := newWakeCanarySandbox(parent)
	if err != nil {
		t.Fatalf("newWakeCanarySandbox: %v", err)
	}
	root := sandbox.TownRoot
	defer sandbox.Cleanup()

	if sandbox.Session != session.MayorSessionName() {
		t.Fatalf("canary session = %q, want dedicated mayor identity", sandbox.Session)
	}
	if _, err := os.Stat(filepath.Join(sandbox.TownRoot, ".beads")); err != nil {
		t.Fatalf("isolated Dolt database missing: %v", err)
	}
	hooksPath := filepath.Join(sandbox.RuntimeConfigDir, "hooks.json")
	hooksData, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("temporary Codex hooks missing: %v", err)
	}
	if !strings.Contains(string(hooksData), "UserPromptSubmit") || !strings.Contains(string(hooksData), "mail check --inject") {
		t.Fatalf("temporary Codex hooks lack receipt dispatcher: %s", hooksData)
	}
	if sandbox.Socket == "" || sandbox.Socket == "gastown" {
		t.Fatalf("canary socket is not isolated: %q", sandbox.Socket)
	}
	for _, path := range []string{sandbox.TownRoot, sandbox.WorkDir, sandbox.RuntimeConfigDir} {
		if filepath.Clean(path) == filepath.Clean(parent) || !filepath.IsAbs(path) {
			t.Fatalf("sandbox path is not a private child: %q", path)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("Stat(%s): %v", path, statErr)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("mode(%s) = %o, want 0700", path, info.Mode().Perm())
		}
	}

	if err := sandbox.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("sandbox still exists after cleanup: %v", err)
	}
}

func TestWriteWakeCanaryStateIsSanitizedAtomicAndPrivate(t *testing.T) {
	townRoot := t.TempDir()
	state := wakeCanaryState{
		SchemaVersion:         1,
		InstalledBinaryCommit: "abc123",
		AttemptedAt:           time.Now(),
		Result:                "passed",
		LatencyMS:             42,
	}
	path, err := writeWakeCanaryState(townRoot, state)
	if err != nil {
		t.Fatalf("writeWakeCanaryState: %v", err)
	}
	wantPath := filepath.Join(townRoot, ".runtime", "canary", "control-plane.json")
	if path != wantPath {
		t.Fatalf("state path = %q, want %q", path, wantPath)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 || got["schema_version"] != float64(1) || got["result"] != "passed" {
		t.Fatalf("state schema = %#v", got)
	}
	for _, forbidden := range []string{"session", "nonce", "message", "delivery_id"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("state leaked %s: %#v", forbidden, got)
		}
	}
}
