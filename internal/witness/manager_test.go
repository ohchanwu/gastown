package witness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestManagerStartForegroundDeprecated(t *testing.T) {
	mgr := NewManager(&rig.Rig{Name: "testrig", Path: t.TempDir()})
	err := mgr.Start(true, "", nil)
	if err == nil {
		t.Fatal("expected foreground mode deprecation error")
	}
	if !strings.Contains(err.Error(), "foreground mode is deprecated") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakeSessionCleanup struct {
	generation tmux.SessionGeneration
	pane       tmux.PaneProcessGeneration
	logPath    string
	run        func(context.Context) error
	prepare    func(context.Context) (func(context.Context) (bool, error), error)
	close      func() error
}

func (cleanup *fakeSessionCleanup) Run(ctx context.Context) error {
	if cleanup.run != nil {
		return cleanup.run(ctx)
	}
	if os.Getenv("FAKE_CLEANUP_WAIT_CONTEXT") != "" {
		<-ctx.Done()
		return ctx.Err()
	}
	logFile, err := os.OpenFile(cleanup.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := logFile.WriteString("cleanup-run " + cleanup.generation.SessionID + " " + cleanup.generation.Nonce + " " + cleanup.pane.Identity + "\n")
	closeErr := logFile.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	if replacement := os.Getenv("FAKE_POLLER_REPLACEMENT"); replacement != "" {
		if err := os.WriteFile(os.Getenv("FAKE_POLLER_PATH"), []byte(replacement), 0o600); err != nil {
			return err
		}
	}
	if message := os.Getenv("FAKE_CLEANUP_ERROR"); message != "" {
		return errors.New(message)
	}
	return nil
}

func (cleanup *fakeSessionCleanup) PrepareCommit(ctx context.Context) (func(context.Context) (bool, error), error) {
	if cleanup.prepare != nil {
		return cleanup.prepare(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func(commitCtx context.Context) (bool, error) {
		// The fake models a commit point immediately before its historical Run
		// behavior. Tests can therefore distinguish post-commit cleanup errors
		// from preparation errors.
		return true, cleanup.Run(commitCtx)
	}, nil
}

func (cleanup *fakeSessionCleanup) Close() error {
	if cleanup.close != nil {
		return cleanup.close()
	}
	return nil
}

func newFakeTmuxManager(t *testing.T, behavior string) (*Manager, string, string) {
	t.Helper()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	fakeBin := t.TempDir()
	logPath := filepath.Join(fakeBin, "tmux.log")
	generationStatePath := filepath.Join(fakeBin, "generation-state")
	if err := os.WriteFile(generationStatePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_TMUX_LOG"
case "$*" in
  *"display-message"*"-p #{pid}"*)
    count=$(cat "$FAKE_GENERATION_STATE")
    count=$((count + 1))
    printf '%s' "$count" > "$FAKE_GENERATION_STATE"
    case "$FAKE_GENERATION_MODE" in
      first-fail) echo "generation query failed" >&2; exit 2 ;;
      first-malformed) echo "malformed-generation"; exit 0 ;;
      second-fail) if [ "$count" -ge 3 ]; then echo "second generation query failed" >&2; exit 2; fi ;;
      second-malformed) if [ "$count" -ge 3 ]; then echo "malformed-generation"; exit 0; fi ;;
      replacement) if [ "$count" -ge 3 ]; then nonce="fixture-generation-replacement"; else nonce="fixture-generation-original"; fi ;;
      poller-advance)
        if [ "$count" -eq 3 ]; then printf '%s' "$FAKE_POLLER_RECORD" > "$FAKE_POLLER_PATH"; fi
        ;;
    esac
    : "${nonce:=fixture-generation-original}"
    printf '%s\t$1\t%s\n' "$FAKE_TMUX_SERVER_PID" "$nonce"
    exit 0
    ;;
esac
` + behavior
	if err := os.WriteFile(filepath.Join(fakeBin, "tmux"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_LOG", logPath)
	t.Setenv("FAKE_GENERATION_STATE", generationStatePath)
	t.Setenv("FAKE_TMUX_SERVER_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	mgr := NewManagerWithTmux(
		&rig.Rig{Name: "testrig", Path: rigPath},
		tmux.NewTmuxWithSocket("gt-wsf-"+strconv.FormatInt(time.Now().UnixNano(), 10)),
	)
	mgr.capturePaneGeneration = func(tmux.SessionGeneration) (tmux.PaneProcessGeneration, error) {
		return tmux.PaneProcessGeneration{PID: os.Getpid(), Identity: "fixture-pane-generation"}, nil
	}
	mgr.prepareSessionCleanup = func(
		generation tmux.SessionGeneration,
		pane tmux.PaneProcessGeneration,
	) (sessionGenerationCleanup, error) {
		return &fakeSessionCleanup{generation: generation, pane: pane, logPath: logPath}, nil
	}
	t.Setenv("FAKE_SESSION_NAME", mgr.SessionName())
	pollerDir := filepath.Join(townRoot, ".runtime", "nudge_poller")
	if err := os.MkdirAll(pollerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pollerPath := filepath.Join(pollerDir, mgr.SessionName()+".pid")
	if err := os.WriteFile(pollerPath, []byte("invalid ownership"), 0o600); err != nil {
		t.Fatal(err)
	}
	return mgr, pollerPath, logPath
}

func startTestSleeper(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out reaping test sleeper PID %d", cmd.Process.Pid)
		}
	})
	return strconv.Itoa(cmd.Process.Pid)
}

func assertFakePollerPreserved(t *testing.T, pollerPath, logPath string) {
	t.Helper()
	data, err := os.ReadFile(pollerPath)
	if err != nil || string(data) != "invalid ownership" {
		t.Fatalf("poller custody changed: data=%q err=%v", data, err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logData), "kill-session") {
		t.Fatalf("session kill attempted after unknown state:\n%s", logData)
	}
}

func TestManagerStartPreservesZombieWhenLivenessIsUnknown(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "transport failure" >&2; exit 2 ;;
esac
exit 2
`)

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "checking witness liveness") {
		t.Fatalf("Start() error = %v, want checked liveness failure", err)
	}
	assertFakePollerPreserved(t, pollerPath, logPath)
}

func TestManagerStartPreservesZombieWhenSessionIdentityIsUnknown(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) echo "transport failure" >&2; exit 2 ;;
esac
exit 2
`)
	t.Setenv("FAKE_GENERATION_MODE", "first-fail")

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "reading witness session identity") {
		t.Fatalf("Start() error = %v, want checked session-identity failure", err)
	}
	assertFakePollerPreserved(t, pollerPath, logPath)
}

func TestManagerStartPreservesZombieWhenSessionIdentityIsInvalid(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf 'other-session:$1\n'; exit 0 ;;
esac
exit 2
`)
	t.Setenv("FAKE_GENERATION_MODE", "first-malformed")

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected field count") {
		t.Fatalf("Start() error = %v, want invalid session-identity failure", err)
	}
	assertFakePollerPreserved(t, pollerPath, logPath)
}

func TestManagerStartPreservesZombieWhenSecondLivenessIsUnknown(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*)
    if [ "$(cat "$FAKE_TMUX_STATE")" = "0" ]; then
      echo "GT_PROCESS_NAMES=definitely-not-running"
      exit 0
    fi
    echo "second liveness query failed" >&2
    exit 2
    ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) printf '1' > "$FAKE_TMUX_STATE"; exit 0 ;;
esac
exit 2
`)
	statePath := filepath.Join(t.TempDir(), "tmux-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_STATE", statePath)

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "rechecking witness liveness") {
		t.Fatalf("Start() error = %v, want second checked liveness failure", err)
	}
	assertFakePollerPreserved(t, pollerPath, logPath)
}

func TestManagerStartCarriesPaneGenerationCapturedBeforeLivenessDecision(t *testing.T) {
	mgr, _, _ := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*)
    printf '1' > "$FAKE_PANE_STATE"
    echo "GT_PROCESS_NAMES=definitely-not-running"
    exit 0
    ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	statePath := filepath.Join(t.TempDir(), "pane-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_PANE_STATE", statePath)
	mgr.capturePaneGeneration = func(tmux.SessionGeneration) (tmux.PaneProcessGeneration, error) {
		state, err := os.ReadFile(statePath)
		if err != nil {
			return tmux.PaneProcessGeneration{}, err
		}
		if string(state) == "0" {
			return tmux.PaneProcessGeneration{PID: 123, Identity: "pre-liveness-pane"}, nil
		}
		return tmux.PaneProcessGeneration{PID: 123, Identity: "replacement-pane"}, nil
	}
	prepareErr := errors.New("stop after pane token assertion")
	var preparedPane tmux.PaneProcessGeneration
	mgr.prepareSessionCleanup = func(
		_ tmux.SessionGeneration,
		pane tmux.PaneProcessGeneration,
	) (sessionGenerationCleanup, error) {
		preparedPane = pane
		return nil, prepareErr
	}

	err := mgr.Start(false, "", nil)
	if !errors.Is(err, prepareErr) {
		t.Fatalf("Start() error = %v, want prepare sentinel", err)
	}
	if preparedPane.Identity != "pre-liveness-pane" {
		t.Fatalf("prepared pane = %+v, want token captured before liveness changed state", preparedPane)
	}
}

func TestManagerStartPreservesZombieWhenSecondIdentityQueryFails(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*)
    if [ "$(cat "$FAKE_TMUX_STATE")" = "0" ]; then
      printf '1' > "$FAKE_TMUX_STATE"
      printf '%s:$1\n' "$FAKE_SESSION_NAME"
      exit 0
    fi
    echo "second identity query failed" >&2
    exit 2
    ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	t.Setenv("FAKE_GENERATION_MODE", "second-fail")
	statePath := filepath.Join(t.TempDir(), "tmux-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_STATE", statePath)

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "rechecking witness session identity") {
		t.Fatalf("Start() error = %v, want second checked identity failure", err)
	}
	assertFakePollerPreserved(t, pollerPath, logPath)
}

func TestManagerStartPreservesZombieWhenSecondIdentityIsInvalid(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*)
    if [ "$(cat "$FAKE_TMUX_STATE")" = "0" ]; then
      printf '1' > "$FAKE_TMUX_STATE"
      printf '%s:$1\n' "$FAKE_SESSION_NAME"
    fi
    exit 0
    ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	t.Setenv("FAKE_GENERATION_MODE", "second-malformed")
	statePath := filepath.Join(t.TempDir(), "tmux-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_STATE", statePath)

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "rechecking witness session identity") {
		t.Fatalf("Start() error = %v, want invalid second session identity", err)
	}
	assertFakePollerPreserved(t, pollerPath, logPath)
}

func TestManagerStartPreservesZombieWhenSecondIdentityIsMalformed(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*)
    if [ "$(cat "$FAKE_TMUX_STATE")" = "0" ]; then
      printf '1' > "$FAKE_TMUX_STATE"
      printf '%s:$1\n' "$FAKE_SESSION_NAME"
    else
      printf '%s:not-an-id\n' "$FAKE_SESSION_NAME"
    fi
    exit 0
    ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	t.Setenv("FAKE_GENERATION_MODE", "second-malformed")
	statePath := filepath.Join(t.TempDir(), "tmux-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_STATE", statePath)

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "rechecking witness session identity") {
		t.Fatalf("Start() error = %v, want malformed second session identity", err)
	}
	assertFakePollerPreserved(t, pollerPath, logPath)
}

func TestManagerStartPreservesSameSecondReplacement(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*)
    if [ "$(cat "$FAKE_TMUX_STATE")" = "0" ]; then
      printf '%s:$1\n' "$FAKE_SESSION_NAME"
    else
      printf '%s:$2\n' "$FAKE_SESSION_NAME"
    fi
    exit 0
    ;;
  *"show-environment"*"GT_PROCESS_NAMES"*)
    printf '1' > "$FAKE_TMUX_STATE"
    echo "GT_PROCESS_NAMES=definitely-not-running"
    exit 0
    ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"session_created"*) echo "123"; exit 0 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
  *"set-option"*|*"set-hook"*) exit 0 ;;
  *"kill-session"*) echo "replacement session targeted" >&2; exit 2 ;;
esac
exit 0
`)
	t.Setenv("FAKE_GENERATION_MODE", "replacement")
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "tmux-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_STATE", statePath)
	t.Setenv("FAKE_SESSION_NAME", mgr.SessionName())

	err := mgr.Start(false, "", nil)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Start() error = %v, want replacement preserved", err)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "kill-session") {
		t.Fatalf("same-second replacement was killed:\n%s", logData)
	}
}

func TestManagerStartPreservesPollerGenerationCreatedBeforeStop(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*)
    if [ "$(cat "$FAKE_TMUX_STATE")" = "0" ]; then
      printf '1' > "$FAKE_TMUX_STATE"
    else
      printf '%s' "$FAKE_POLLER_RECORD" > "$FAKE_POLLER_PATH"
    fi
    printf '%s:$1\n' "$FAKE_SESSION_NAME"
    exit 0
    ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
  *"set-option"*|*"set-hook"*) exit 0 ;;
  *"kill-session"*) echo "session targeted after poller advanced" >&2; exit 2 ;;
esac
exit 0
`)
	t.Setenv("FAKE_GENERATION_MODE", "poller-advance")
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "tmux-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacementRecord := []byte(`{"PID":999999,"Identity":{"StartTime":"new-start","Command":"gt nudge-poller ` + mgr.SessionName() + `","Generation":"new-generation","Transport":"test"},"Session":"` + mgr.SessionName() + `","Legacy":false}` + "\n")
	t.Setenv("FAKE_TMUX_STATE", statePath)
	t.Setenv("FAKE_SESSION_NAME", mgr.SessionName())
	t.Setenv("FAKE_POLLER_PATH", pollerPath)
	t.Setenv("FAKE_POLLER_RECORD", string(replacementRecord))

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "poller ownership advanced") {
		t.Fatalf("Start() error = %v, want advanced poller generation refusal", err)
	}
	got, readErr := os.ReadFile(pollerPath)
	if readErr != nil || string(got) != string(replacementRecord) {
		t.Fatalf("replacement poller record = %q, err %v", got, readErr)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "kill-session") {
		t.Fatalf("session kill followed poller generation advance:\n%s", logData)
	}
}

func TestManagerStartProcessCleanupTargetsCapturedGeneration(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*)
    if [ -f "$FAKE_CLEANUP_STATE" ]; then
      # The reusable session name now resolves to a replacement. The captured
      # immutable session ID still resolves to the old pane generation.
      case "$*" in
        *"-t \$1:^"*) echo "$FAKE_PANE_PID"; exit 0 ;;
      esac
      echo "replacement session targeted by name" >&2
      exit 2
    fi
    echo "$FAKE_PANE_PID"
    exit 0
    ;;
  *"list-panes"*) exit 0 ;;
  *"set-option"*) exit 0 ;;
  *"set-hook"*)
    printf 'replacement-generation' > "$FAKE_POLLER_PATH"
    printf 'cleanup' > "$FAKE_CLEANUP_STATE"
    exit 0
    ;;
  *"kill-session"*) echo "expected captured-generation kill failure" >&2; exit 2 ;;
esac
exit 0
`)
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	cleanupStatePath := filepath.Join(t.TempDir(), "cleanup-state")
	t.Setenv("FAKE_POLLER_PATH", pollerPath)
	t.Setenv("FAKE_CLEANUP_STATE", cleanupStatePath)
	t.Setenv("FAKE_PANE_PID", startTestSleeper(t))
	t.Setenv("FAKE_POLLER_REPLACEMENT", "replacement-generation")
	t.Setenv("FAKE_CLEANUP_ERROR", "expected captured-generation cleanup failure")

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "killing zombie session") {
		t.Fatalf("Start() error = %v, want captured-generation cleanup failure", err)
	}
	got, readErr := os.ReadFile(pollerPath)
	if readErr != nil || string(got) != "replacement-generation" {
		t.Fatalf("replacement poller record = %q, err %v", got, readErr)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	logText := string(logData)
	if !strings.Contains(logText, "cleanup-run $1 fixture-generation-original fixture-pane-generation") {
		t.Fatalf("captured tmux generation was not passed to cleanup:\n%s", logText)
	}
	if strings.Contains(logText, "kill-session -t "+mgr.SessionName()) {
		t.Fatalf("replacement session name was targeted:\n%s", logText)
	}
}

func TestManagerStartZombieUsesProcessAwareCleanup(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"session_created"*) echo "123"; exit 0 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*)
    if [ -f "$FAKE_CLEANUP_STATE" ]; then
      echo "$FAKE_PANE_PID"
      exit 0
    fi
    echo "$FAKE_PANE_PID"
    exit 0
    ;;
  *"list-panes"*) exit 0 ;;
  *"set-option"*) exit 0 ;;
  *"set-hook"*) touch "$FAKE_CLEANUP_STATE"; exit 0 ;;
  *"kill-session"*) echo "expected kill failure" >&2; exit 2 ;;
esac
exit 0
`)
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	cleanupStatePath := filepath.Join(t.TempDir(), "cleanup-state")
	t.Setenv("FAKE_CLEANUP_STATE", cleanupStatePath)
	t.Setenv("FAKE_PANE_PID", startTestSleeper(t))
	t.Setenv("FAKE_CLEANUP_ERROR", "expected cleanup failure")

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "killing zombie session") {
		t.Fatalf("Start() error = %v, want replacement kill failure", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "cleanup-run $1 fixture-generation-original fixture-pane-generation") {
		t.Fatalf("zombie replacement skipped process-aware cleanup:\n%s", logText)
	}
}

func TestManagerStartBoundsCleanupTransaction(t *testing.T) {
	mgr, pollerPath, _ := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	mgr.cleanupTimeout = 50 * time.Millisecond
	t.Setenv("FAKE_CLEANUP_WAIT_CONTEXT", "1")
	started := time.Now()
	err := mgr.Start(false, "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() error = %v, want cleanup deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("bounded cleanup took %v", elapsed)
	}
}

func TestManagerStartHoldsDeliveryLeaseThroughFailedCleanup(t *testing.T) {
	mgr, pollerPath, _ := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	townRoot := filepath.Dir(mgr.rig.Path)
	queued := nudge.QueuedNudge{
		DeliveryID:      "ndg-cleanup-delivery-lease",
		Sender:          "mayor",
		Message:         "must remain recoverable",
		Priority:        nudge.PriorityUrgent,
		DurableUntilAck: true,
	}
	if err := nudge.Enqueue(townRoot, mgr.SessionName(), queued); err != nil {
		t.Fatal(err)
	}
	cleanupEntered := make(chan struct{})
	releaseCleanup := make(chan struct{})
	cleanupErr := errors.New("preserve zombie")
	mgr.prepareSessionCleanup = func(
		generation tmux.SessionGeneration,
		pane tmux.PaneProcessGeneration,
	) (sessionGenerationCleanup, error) {
		return &fakeSessionCleanup{
			generation: generation,
			pane:       pane,
			run: func(context.Context) error {
				close(cleanupEntered)
				<-releaseCleanup
				return cleanupErr
			},
		}, nil
	}
	startResult := make(chan error, 1)
	go func() { startResult <- mgr.Start(false, "", nil) }()
	select {
	case <-cleanupEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not start")
	}

	competitor := tmux.NewTmuxWithSocket("gt-witness-delivery-lease-competitor")
	leaseCtx, cancelLease := context.WithTimeout(context.Background(), 75*time.Millisecond)
	releaseCompetingLease, leaseErr := competitor.AcquireNudgeLeaseContext(leaseCtx, townRoot, mgr.SessionName())
	cancelLease()
	if releaseCompetingLease != nil {
		releaseCompetingLease()
	}
	close(releaseCleanup)
	startErr := <-startResult
	if !errors.Is(startErr, cleanupErr) {
		t.Fatalf("Start() error = %v, want cleanup failure", startErr)
	}
	if !errors.Is(leaseErr, context.DeadlineExceeded) {
		t.Fatalf("competing delivery lease error = %v, want deadline during cleanup", leaseErr)
	}

	recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), time.Second)
	releaseRecoveryLease, err := competitor.AcquireNudgeLeaseContext(recoveryCtx, townRoot, mgr.SessionName())
	cancelRecovery()
	if err != nil {
		t.Fatalf("reacquiring delivery lease after cleanup: %v", err)
	}
	defer releaseRecoveryLease()
	claim, err := nudge.ClaimDue(townRoot, mgr.SessionName())
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil || claim.Nudge.DeliveryID != queued.DeliveryID {
		t.Fatalf("recoverable claim = %#v, want %s", claim, queued.DeliveryID)
	}
	if err := claim.Nack("test-recovery", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestManagerStartHoldsDeliveryLeaseThroughPrecommitThaw(t *testing.T) {
	mgr, pollerPath, _ := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	precommitErr := errors.New("pre-commit freeze validation failed")
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	mgr.prepareSessionCleanup = func(
		generation tmux.SessionGeneration,
		pane tmux.PaneProcessGeneration,
	) (sessionGenerationCleanup, error) {
		return &fakeSessionCleanup{
			generation: generation,
			pane:       pane,
			prepare: func(context.Context) (func(context.Context) (bool, error), error) {
				return nil, precommitErr
			},
			close: func() error {
				close(closeEntered)
				<-releaseClose
				return nil
			},
		}, nil
	}
	result := make(chan error, 1)
	go func() { result <- mgr.Start(false, "", nil) }()
	select {
	case <-closeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("pre-commit cleanup did not enter Close")
	}
	townRoot := filepath.Dir(mgr.rig.Path)
	competitor := tmux.NewTmuxWithSocket("gt-witness-thaw-lease-competitor")
	leaseCtx, cancelLease := context.WithTimeout(context.Background(), 75*time.Millisecond)
	releaseCompetingLease, leaseErr := competitor.AcquireNudgeLeaseContext(leaseCtx, townRoot, mgr.SessionName())
	cancelLease()
	if releaseCompetingLease != nil {
		releaseCompetingLease()
	}
	if !errors.Is(leaseErr, context.DeadlineExceeded) {
		t.Fatalf("competing delivery lease error = %v, want deadline while containment thaws", leaseErr)
	}
	close(releaseClose)
	if err := <-result; !errors.Is(err, precommitErr) {
		t.Fatalf("Start() error = %v, want pre-commit failure", err)
	}
}

func TestManagerStartAbortsWhenDeliveryMakesGenerationAliveBeforeMutation(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*)
    count=$(cat "$FAKE_LIVENESS_STATE")
    count=$((count + 1))
    printf '%s' "$count" > "$FAKE_LIVENESS_STATE"
    if [ "$count" -ge 3 ]; then
      echo "GT_PROCESS_NAMES=other"
    else
      echo "GT_PROCESS_NAMES=definitely-not-running"
    fi
    exit 0
    ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	livenessState := filepath.Join(t.TempDir(), "liveness-state")
	if err := os.WriteFile(livenessState, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LIVENESS_STATE", livenessState)

	err := mgr.Start(false, "", nil)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Start() error = %v, want under-lease liveness abort", err)
	}
	logData, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logData), "cleanup-run") || strings.Contains(string(logData), "kill-session") {
		t.Fatalf("destructive cleanup followed under-lease liveness recovery:\n%s", logData)
	}
}

func TestManagerStopSessionPreservesSessionWhenPollerOwnershipIsInvalid(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := NewManager(&rig.Rig{Name: "testrig", Path: rigPath})
	pollerDir := filepath.Join(townRoot, ".runtime", "nudge_poller")
	if err := os.MkdirAll(pollerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pollerDir, mgr.SessionName()+".pid"), []byte("invalid ownership"), 0o600); err != nil {
		t.Fatal(err)
	}

	replaced := false
	err := mgr.stopSession(townRoot, mgr.SessionName(), func() error { replaced = true; return nil })
	if err == nil || replaced {
		t.Fatalf("stopSession err=%v replaced=%v, want ownership error without replacement", err, replaced)
	}
}

func TestManagerStopChecksPollerOwnershipWhenSessionIsAbsent(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := NewManagerWithTmux(
		&rig.Rig{Name: "testrig", Path: rigPath},
		tmux.NewTmuxWithSocket("gt-wsa-"+strconv.FormatInt(time.Now().UnixNano(), 10)),
	)
	pollerDir := filepath.Join(townRoot, ".runtime", "nudge_poller")
	if err := os.MkdirAll(pollerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pollerDir, mgr.SessionName()+".pid"), []byte("invalid ownership"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := mgr.Stop()
	if err == nil || errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop() error = %v, want poller ownership failure before session-absent result", err)
	}
}

func TestManagerStopChecksSessionStateBeforePollerCustody(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := NewManagerWithTmux(
		&rig.Rig{Name: "testrig", Path: rigPath},
		tmux.NewTmuxWithSocket("gt-wse-"+strconv.FormatInt(time.Now().UnixNano(), 10)),
	)
	pollerDir := filepath.Join(townRoot, ".runtime", "nudge_poller")
	if err := os.MkdirAll(pollerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pollerPath := filepath.Join(pollerDir, mgr.SessionName()+".pid")
	if err := os.WriteFile(pollerPath, []byte("invalid ownership"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	err := mgr.Stop()
	if err == nil || !strings.Contains(err.Error(), "checking witness session") {
		t.Fatalf("Stop() error = %v, want tmux state error before poller custody", err)
	}
	if data, readErr := os.ReadFile(pollerPath); readErr != nil || string(data) != "invalid ownership" {
		t.Fatalf("poller custody changed after unknown session state: data=%q err=%v", data, readErr)
	}
}

func TestManagerStartFailsClosedWhenSessionStateIsUnknown(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := NewManagerWithTmux(
		&rig.Rig{Name: "testrig", Path: rigPath},
		tmux.NewTmuxWithSocket("gt-wss-"+strconv.FormatInt(time.Now().UnixNano(), 10)),
	)
	t.Setenv("PATH", t.TempDir())

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "checking witness session") {
		t.Fatalf("Start() error = %v, want unknown tmux state failure", err)
	}
}

func TestPrepareWitnessDirCreatesMissingAddedRigWitnessDir(t *testing.T) {
	t.Parallel()

	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	rigBeadsDir := filepath.Join(rigPath, ".beads")
	mayorBeadsDir := filepath.Join(rigPath, "mayor", "rig", ".beads")

	if err := os.MkdirAll(rigBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir rig beads: %v", err)
	}
	if err := os.MkdirAll(mayorBeadsDir, 0755); err != nil {
		t.Fatalf("mkdir mayor beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigBeadsDir, "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
		t.Fatalf("write rig beads redirect: %v", err)
	}

	mgr := NewManager(&rig.Rig{Name: "gastown", Path: rigPath})
	witnessDir, err := mgr.prepareWitnessDir(townRoot)
	if err != nil {
		t.Fatalf("prepareWitnessDir: %v", err)
	}

	wantWitnessDir := filepath.Join(rigPath, "witness")
	if witnessDir != wantWitnessDir {
		t.Fatalf("witnessDir = %q, want %q", witnessDir, wantWitnessDir)
	}
	if info, err := os.Stat(witnessDir); err != nil || !info.IsDir() {
		t.Fatalf("witness dir was not created: info=%v err=%v", info, err)
	}

	redirectData, err := os.ReadFile(filepath.Join(witnessDir, ".beads", "redirect"))
	if err != nil {
		t.Fatalf("read witness redirect: %v", err)
	}
	if got, want := string(redirectData), "../mayor/rig/.beads\n"; got != want {
		t.Fatalf("redirect = %q, want %q", got, want)
	}
}

func TestBuildWitnessStartCommand_UsesRoleConfig(t *testing.T) {
	t.Parallel()
	roleCfg := &beads.RoleConfig{
		StartCommand: "exec run --town {town} --rig {rig} --role {role}",
	}

	got, err := buildWitnessStartCommand("/town/rig", "gastown", "/town", "", "", roleCfg, "")
	if err != nil {
		t.Fatalf("buildWitnessStartCommand: %v", err)
	}

	want := "exec env -u CLAUDECODE NODE_OPTIONS='' run --town /town --rig gastown --role witness"
	if got != want {
		t.Errorf("buildWitnessStartCommand = %q, want %q", got, want)
	}
}

func TestBuildWitnessStartCommand_DefaultsToRuntime(t *testing.T) {
	t.Parallel()
	got, err := buildWitnessStartCommand("/town/rig", "gastown", "/town", "", "", nil, "")
	if err != nil {
		t.Fatalf("buildWitnessStartCommand: %v", err)
	}

	if !strings.Contains(got, "GT_ROLE=gastown/witness") {
		t.Errorf("expected GT_ROLE=gastown/witness in command, got %q", got)
	}
	if !strings.Contains(got, "BD_ACTOR=gastown/witness") {
		t.Errorf("expected BD_ACTOR=gastown/witness in command, got %q", got)
	}
}

// TestRoleConfigEnvVars_ExpandsQualifiedGTRole verifies that the TOML env vars
// expand GT_ROLE to a qualified value (e.g., "gastown/witness" not "witness").
func TestRoleConfigEnvVars_ExpandsQualifiedGTRole(t *testing.T) {
	t.Parallel()
	roleCfg := &beads.RoleConfig{
		EnvVars: map[string]string{
			"GT_ROLE":  "{rig}/witness",
			"GT_SCOPE": "rig",
		},
	}

	got := roleConfigEnvVars(roleCfg, "/town", "gastown")
	if got["GT_ROLE"] != "gastown/witness" {
		t.Errorf("GT_ROLE = %q, want %q", got["GT_ROLE"], "gastown/witness")
	}
	if got["GT_SCOPE"] != "rig" {
		t.Errorf("GT_SCOPE = %q, want %q", got["GT_SCOPE"], "rig")
	}
}

// TestRoleConfigEnvVars_NilConfig verifies nil roleConfig returns nil.
func TestRoleConfigEnvVars_NilConfig(t *testing.T) {
	t.Parallel()
	got := roleConfigEnvVars(nil, "/town", "gastown")
	if got != nil {
		t.Errorf("expected nil for nil roleConfig, got %v", got)
	}
}

func TestBuildWitnessStartCommand_IncludesConfigDir(t *testing.T) {
	t.Parallel()
	got, err := buildWitnessStartCommand("/town/rig", "gastown", "/town", "", "", nil, "/home/user/.claude-accounts/work")
	if err != nil {
		t.Fatalf("buildWitnessStartCommand: %v", err)
	}

	if !strings.Contains(got, "CLAUDE_CONFIG_DIR=/home/user/.claude-accounts/work") {
		t.Errorf("expected CLAUDE_CONFIG_DIR in command, got %q", got)
	}
}

func TestBuildWitnessStartCommand_AgentOverrideWins(t *testing.T) {
	t.Parallel()
	roleCfg := &beads.RoleConfig{
		StartCommand: "exec run --role {role}",
	}

	got, err := buildWitnessStartCommand("/town/rig", "gastown", "/town", "", "codex", roleCfg, "")
	if err != nil {
		t.Fatalf("buildWitnessStartCommand: %v", err)
	}
	if strings.Contains(got, "exec run") {
		t.Fatalf("expected agent override to bypass role start_command, got %q", got)
	}
	if !strings.Contains(got, "GT_ROLE=gastown/witness") {
		t.Errorf("expected GT_ROLE=gastown/witness in command, got %q", got)
	}
}
