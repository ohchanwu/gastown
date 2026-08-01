package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

type wakeCanaryIdleWaiterStub struct {
	session string
	timeout time.Duration
	err     error
}

func buildWakeCanaryCandidateGT(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "gt")
	cmd := exec.Command("go", "build", "-o", candidate, "./cmd/gt")
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build candidate gt: %v: %s", err, output)
	}
	return candidate
}

func (s *wakeCanaryIdleWaiterStub) WaitForIdle(session string, timeout time.Duration) error {
	s.session = session
	s.timeout = timeout
	return s.err
}

func TestWaitForWakeCanaryIdleUsesStartupTurnBound(t *testing.T) {
	cause := errors.New("session busy")
	waiter := &wakeCanaryIdleWaiterStub{err: cause}

	err := waitForWakeCanaryIdle(waiter, session.MayorSessionName())
	if !errors.Is(err, cause) {
		t.Fatalf("waitForWakeCanaryIdle error = %v, want %v", err, cause)
	}
	if waiter.session != session.MayorSessionName() {
		t.Fatalf("WaitForIdle session = %q, want %q", waiter.session, session.MayorSessionName())
	}
	if waiter.timeout != constants.ClaudeStartTimeout {
		t.Fatalf("WaitForIdle timeout = %s, want %s", waiter.timeout, constants.ClaudeStartTimeout)
	}
}

func TestWakeCanaryStartupChallengeRequiresFiniteReply(t *testing.T) {
	instruction, response := wakeCanaryStartupChallenge("abc123")
	if response != "321cba" {
		t.Fatalf("startup response = %q, want reversed nonce", response)
	}
	if !strings.Contains(instruction, "abc123") || !strings.Contains(instruction, "Reply with exactly") || !strings.Contains(instruction, "then wait") {
		t.Fatal("startup instruction lacks a finite reply contract")
	}
	if strings.Contains(instruction, response) {
		t.Fatal("startup instruction contains the expected response before the model turn")
	}
}

func TestRunWakeCanaryPersistsSessionNotIdleBeforeLease(t *testing.T) {
	previousCommit := Commit
	Commit = "test-commit"
	t.Cleanup(func() { Commit = previousCommit })

	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-wake-canary-idle-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = tm.KillServer() })

	runtimeTownRoot := t.TempDir()
	evidenceRoot := t.TempDir()
	sessionName := session.MayorSessionName()
	if err := tm.NewSessionWithCommand(sessionName, t.TempDir(), "sleep 60"); err != nil {
		t.Fatalf("NewSessionWithCommand: %v", err)
	}

	release, err := tm.AcquireNudgeLease(runtimeTownRoot, sessionName)
	if err != nil {
		t.Fatalf("AcquireNudgeLease: %v", err)
	}
	t.Cleanup(release)

	type canaryResult struct {
		result    wakeCanaryResult
		statePath string
		err       error
	}
	done := make(chan canaryResult, 1)
	go func() {
		result, statePath, err := runWakeCanary(tm, runtimeTownRoot, evidenceRoot, sessionName, 1)
		done <- canaryResult{result: result, statePath: statePath, err: err}
	}()

	statePath := filepath.Join(evidenceRoot, constants.DirRuntime, "canary", "control-plane.json")
	deadline := time.NewTimer(5 * time.Second)
	t.Cleanup(func() { deadline.Stop() })
	poll := time.NewTicker(10 * time.Millisecond)
	t.Cleanup(poll.Stop)
	for {
		select {
		case <-poll.C:
			data, readErr := os.ReadFile(statePath)
			if readErr != nil {
				continue
			}
			var state wakeCanaryState
			if json.Unmarshal(data, &state) == nil && state.Result == "running" {
				goto waiting
			}
		case <-deadline.C:
			t.Fatal("runWakeCanary did not persist running state")
		}
	}

waiting:
	if err := tm.KillSession(sessionName); err != nil {
		t.Fatalf("KillSession: %v", err)
	}

	select {
	case got := <-done:
		if got.err == nil || !strings.Contains(got.err.Error(), "steady-state idle") {
			t.Fatalf("runWakeCanary error = %v, want steady-state idle failure", got.err)
		}
		if got.result.Submitted != 0 || got.result.Queued != 0 || got.result.Failed != 0 {
			t.Fatalf("runWakeCanary result = %+v, want zero delivery attempts", got.result)
		}
		data, readErr := os.ReadFile(got.statePath)
		if readErr != nil {
			t.Fatalf("read failed canary state: %v", readErr)
		}
		var state wakeCanaryState
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatalf("decode failed canary state: %v", err)
		}
		if state.Result != "failed" || state.FailureCode != "session-not-idle" {
			t.Fatalf("failed canary state = %+v, want session-not-idle", state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runWakeCanary did not stop after the session disappeared")
	}
}

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
	candidateGT := filepath.Join(parent, "candidate", "gt")
	sandbox, err := newWakeCanarySandbox(parent, candidateGT)
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
	var installed struct {
		Hooks struct {
			SessionStart []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"SessionStart"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(hooksData, &installed); err != nil {
		t.Fatalf("parse temporary Codex hooks: %v", err)
	}
	if len(installed.Hooks.SessionStart) != 1 || len(installed.Hooks.SessionStart[0].Hooks) != 1 {
		t.Fatal("temporary Codex hooks lack one SessionStart command")
	}
	if got, want := installed.Hooks.SessionStart[0].Hooks[0].Command, candidateGT+" prime --hook"; got != want {
		t.Fatalf("SessionStart command = %q, want exact candidate command %q", got, want)
	}
	if strings.Contains(string(hooksData), "cmd.test") {
		t.Fatal("temporary Codex hooks resolve to the Go test binary")
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
