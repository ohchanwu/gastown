package witness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestWitnessSessionCustodyPathsIgnoresTestOverrides(t *testing.T) {
	witnessRoot := t.TempDir()
	witnessDir := filepath.Join(witnessRoot, "witness")
	witnessSettingsDir := filepath.Join(witnessRoot, "settings")
	for _, path := range []string{witnessDir, witnessSettingsDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testFileRoot := t.TempDir()
	testExecutable := filepath.Join(testFileRoot, "gt")
	if err := os.WriteFile(testExecutable, []byte("test executable"), 0o700); err != nil {
		t.Fatal(err)
	}

	encoded, err := witnessSessionCustodyPaths(
		witnessDir,
		"",
		witnessSettingsDir,
		&config.RuntimeConfig{Command: "/bin/sh"},
		map[string]string{"GT_TEST_FLOW_GT": testExecutable, "PATH": os.Getenv("PATH")},
	)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	if err := json.Unmarshal([]byte(encoded), &paths); err != nil {
		t.Fatal(err)
	}
	var hasFile, hasParent bool
	for _, path := range paths {
		hasFile = hasFile || path == testExecutable
		hasParent = hasParent || path == testFileRoot
	}
	if hasFile || hasParent {
		t.Fatalf("test override expanded production custody: paths=%q file=%t parent=%t", paths, hasFile, hasParent)
	}
}

func TestWitnessSessionCustodyPathsRejectsConfiguredSymlinkToRoot(t *testing.T) {
	witnessDir := t.TempDir()
	witnessSettingsDir := t.TempDir()
	rootLink := filepath.Join(t.TempDir(), "configured-root")
	if err := os.Symlink(string(filepath.Separator), rootLink); err != nil {
		t.Fatal(err)
	}
	_, err := witnessSessionCustodyPaths(
		witnessDir,
		"",
		witnessSettingsDir,
		&config.RuntimeConfig{Command: "/bin/sh"},
		map[string]string{"CODEX_HOME": rootLink, "PATH": os.Getenv("PATH")},
	)
	if err == nil {
		t.Fatal("configured custody symlink widened to the host root")
	}
}

func TestWitnessSessionCustodyPathsCanonicalizesConfiguredEnvironment(t *testing.T) {
	witnessDir := t.TempDir()
	witnessSettingsDir := t.TempDir()
	realHome := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(realHome)
	if err != nil {
		t.Fatal(err)
	}
	homeLink := filepath.Join(t.TempDir(), "configured-home")
	if err := os.Symlink(realHome, homeLink); err != nil {
		t.Fatal(err)
	}
	envVars := map[string]string{"CODEX_HOME": homeLink, "PATH": os.Getenv("PATH")}
	encoded, err := witnessSessionCustodyPaths(
		witnessDir,
		"",
		witnessSettingsDir,
		&config.RuntimeConfig{Command: "/bin/sh"},
		envVars,
	)
	if err != nil {
		t.Fatal(err)
	}
	if envVars["CODEX_HOME"] != canonicalHome {
		t.Fatalf("contained CODEX_HOME = %q, want canonical %q", envVars["CODEX_HOME"], canonicalHome)
	}
	var paths []string
	if err := json.Unmarshal([]byte(encoded), &paths); err != nil {
		t.Fatal(err)
	}
	var hasCanonical, hasLink bool
	for _, path := range paths {
		hasCanonical = hasCanonical || path == canonicalHome
		hasLink = hasLink || path == homeLink
	}
	if !hasCanonical || hasLink {
		t.Fatalf("canonical custody paths = %q, want %q without %q", paths, canonicalHome, homeLink)
	}
}

func TestAppendWitnessCustodyExecutableIncludesExactOptDirectory(t *testing.T) {
	paths := appendWitnessCustodyExecutable(nil, "/opt/example/bin/codex", "")
	if len(paths) != 1 || paths[0] != "/opt/example/bin" {
		t.Fatalf("exact /opt executable directory = %q, want [/opt/example/bin]", paths)
	}
}

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
	generation   tmux.SessionGeneration
	pane         tmux.PaneProcessGeneration
	logPath      string
	run          func(context.Context) error
	prepare      func(context.Context) (func(context.Context) (bool, error), error)
	close        func() error
	closeContext func(context.Context) error
}

func (cleanup *fakeSessionCleanup) Run(ctx context.Context) error {
	if cleanup.run != nil {
		return cleanup.run(ctx)
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

func (cleanup *fakeSessionCleanup) CloseContext(ctx context.Context) error {
	if cleanup.closeContext != nil {
		return cleanup.closeContext(ctx)
	}
	return cleanup.Close()
}

func TestManagerRetryPendingCleanupsUsesOneSharedDeadline(t *testing.T) {
	t.Parallel()

	legacyCloseCalls := 0
	contextCloseCalls := make([]int, 3)
	cleanups := make([]sessionGenerationCleanup, 3)
	for i := range cleanups {
		index := i
		cleanups[i] = &fakeSessionCleanup{
			close: func() error {
				legacyCloseCalls++
				return errors.New("legacy unbounded cleanup path called")
			},
			closeContext: func(ctx context.Context) error {
				contextCloseCalls[index]++
				if index == 0 {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			},
		}
	}

	mgr := NewManagerWithTmux(
		&rig.Rig{Name: "testrig", Path: t.TempDir()},
		tmux.NewTmux(),
	)
	mgr.cleanupTimeout = 20 * time.Millisecond
	mgr.cleanupRegistry.pending = append(mgr.cleanupRegistry.pending, cleanups...)

	ctx, cancel := context.WithTimeout(context.Background(), mgr.cleanupTimeout)
	defer cancel()
	started := time.Now()
	err := mgr.retryPendingCleanups(ctx)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retryPendingCleanups() error = %v, want deadline exceeded", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("retryPendingCleanups() elapsed = %v, want one bounded cleanup pass", elapsed)
	}
	if legacyCloseCalls != 0 {
		t.Fatalf("legacy Close calls = %d, want 0", legacyCloseCalls)
	}
	if want := []int{1, 0, 0}; !reflect.DeepEqual(contextCloseCalls, want) {
		t.Fatalf("context Close calls = %v, want %v", contextCloseCalls, want)
	}

	mgr.cleanupRegistry.mu.Lock()
	retained := append([]sessionGenerationCleanup(nil), mgr.cleanupRegistry.pending...)
	mgr.cleanupRegistry.mu.Unlock()
	if len(retained) != len(cleanups) || !reflect.DeepEqual(retained, cleanups) {
		t.Fatalf("retained cleanup owners = %d entries, want all %d original current and unprocessed owners in order", len(retained), len(cleanups))
	}
}

func TestManagerRetainsCommittedCleanupOwnerAcrossManagerLifetimes(t *testing.T) {
	closeErr := errors.New("injected committed cleanup close failure")
	closeCalls := 0
	cleanup := &fakeSessionCleanup{
		prepare: func(context.Context) (func(context.Context) (bool, error), error) {
			return func(context.Context) (bool, error) { return true, nil }, nil
		},
		close: func() error {
			closeCalls++
			if closeCalls == 1 {
				return closeErr
			}
			return nil
		},
	}
	commit, err := cleanup.PrepareCommit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	committed, err := commit(context.Background())
	if err != nil || !committed {
		t.Fatalf("commit = %v, err %v; want committed success", committed, err)
	}

	sharedRig := &rig.Rig{Name: "testrig", Path: t.TempDir()}
	first := NewManagerWithTmux(sharedRig, tmux.NewTmux())
	second := NewManagerWithTmux(sharedRig, tmux.NewTmux())
	if err := first.closeOrRetainCleanup(context.Background(), cleanup); !errors.Is(err, closeErr) {
		t.Fatalf("initial Close error = %v, want %v", err, closeErr)
	}
	first.cleanupRegistry.mu.Lock()
	pending := len(first.cleanupRegistry.pending)
	first.cleanupRegistry.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending cleanup owners = %d, want 1", pending)
	}
	if err := second.Start(true, "", nil); err == nil || !strings.Contains(err.Error(), "foreground mode is deprecated") {
		t.Fatalf("fresh Manager Start after cleanup retry = %v, want foreground stop boundary", err)
	}
	if closeCalls != 2 {
		t.Fatalf("Close calls = %d, want 2", closeCalls)
	}
	first.cleanupRegistry.mu.Lock()
	pending = len(first.cleanupRegistry.pending)
	first.cleanupRegistry.mu.Unlock()
	if pending != 0 {
		t.Fatalf("successful cleanup retry retained %d owners", pending)
	}
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
    printf '%s\t$1\t%s\t\t19\n' "$FAKE_TMUX_SERVER_PID" "$nonce"
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
	mgr.zombieKillGrace = 0
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
	assertNoFakeSessionKill(t, logPath)
}

func assertNoFakeSessionKill(t *testing.T, logPath string) {
	t.Helper()
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
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "tmux-state")
	if err := os.WriteFile(statePath, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_STATE", statePath)

	err := mgr.Start(false, "", nil)
	if err == nil || !strings.Contains(err.Error(), "rechecking witness liveness") {
		t.Fatalf("Start() error = %v, want second checked liveness failure", err)
	}
	assertNoFakeSessionKill(t, logPath)
}

func TestManagerStartRejectsPaneGenerationChangedAfterLivenessDecision(t *testing.T) {
	mgr, pollerPath, _ := newFakeTmuxManager(t, `
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
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
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
	prepared := false
	mgr.prepareSessionCleanup = func(
		_ tmux.SessionGeneration,
		pane tmux.PaneProcessGeneration,
	) (sessionGenerationCleanup, error) {
		prepared = true
		return nil, errors.New("cleanup must not prepare after pane replacement")
	}

	err := mgr.Start(false, "", nil)
	if !errors.Is(err, tmux.ErrSessionGenerationChanged) {
		t.Fatalf("Start() error = %v, want exact pane-generation refusal", err)
	}
	if prepared {
		t.Fatal("cleanup prepared after pane generation changed")
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
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
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
	assertNoFakeSessionKill(t, logPath)
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
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
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
	assertNoFakeSessionKill(t, logPath)
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
	if err := os.Remove(pollerPath); err != nil {
		t.Fatal(err)
	}
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
	assertNoFakeSessionKill(t, logPath)
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
	if !errors.Is(err, tmux.ErrSessionGenerationChanged) {
		t.Fatalf("Start() error = %v, want exact-generation refusal", err)
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
	observedBudget := make(chan time.Duration, 1)
	mgr.prepareSessionCleanup = func(generation tmux.SessionGeneration, pane tmux.PaneProcessGeneration) (sessionGenerationCleanup, error) {
		return &fakeSessionCleanup{
			generation: generation,
			pane:       pane,
			prepare: func(ctx context.Context) (func(context.Context) (bool, error), error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					return nil, errors.New("cleanup context has no deadline")
				}
				observedBudget <- time.Until(deadline)
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}, nil
	}
	err := mgr.Start(false, "", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() error = %v, want cleanup deadline", err)
	}
	if budget := <-observedBudget; budget > mgr.cleanupTimeout {
		t.Fatalf("cleanup transaction budget = %v, want at most %v", budget, mgr.cleanupTimeout)
	}
}

func TestManagerStartPreservesPollerAfterUnreconciledCommittedCleanup(t *testing.T) {
	mgr, pollerPath, _ := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=definitely-not-running"; exit 0 ;;
  *"show-environment"*) echo "unknown variable" >&2; exit 1 ;;
  *"display-message"*"pane_current_command"*) echo "other"; exit 0 ;;
  *"display-message"*"pane_pid"*) echo "999999"; exit 0 ;;
  *"list-panes"*) exit 0 ;;
esac
exit 2
`)
	record := []byte(`{"PID":999999,"Identity":{"StartTime":"old-start","Command":"gt nudge-poller ` + mgr.SessionName() + `","Generation":"old-generation","Transport":"test"},"Session":"` + mgr.SessionName() + `","Legacy":false}` + "\n")
	if err := os.WriteFile(pollerPath, record, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr.prepareSessionCleanup = func(
		generation tmux.SessionGeneration,
		pane tmux.PaneProcessGeneration,
	) (sessionGenerationCleanup, error) {
		return &fakeSessionCleanup{
			generation: generation,
			pane:       pane,
			prepare: func(context.Context) (func(context.Context) (bool, error), error) {
				return func(context.Context) (bool, error) {
					return true, tmux.ErrSessionCleanupUnreconciled
				}, nil
			},
		}, nil
	}

	err := mgr.Start(false, "", nil)
	if !errors.Is(err, tmux.ErrSessionCleanupUnreconciled) ||
		!errors.Is(err, nudge.ErrPollerPreservedAfterCommittedCleanup) {
		t.Fatalf("Start() error = %v, want committed ambiguous cleanup and poller-preservation evidence", err)
	}
	got, readErr := os.ReadFile(pollerPath)
	if readErr != nil || string(got) != string(record) {
		t.Fatalf("preserved poller record = %q, err %v; want %q", got, readErr, record)
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
	pollerCaptureDone := make(chan error, 1)
	go func() {
		_, err := nudge.CapturePollerGeneration(townRoot, mgr.SessionName())
		pollerCaptureDone <- err
	}()
	select {
	case err := <-pollerCaptureDone:
		t.Fatalf("poller lifecycle lock released before cleanup Close: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	close(releaseClose)
	if err := <-result; !errors.Is(err, precommitErr) {
		t.Fatalf("Start() error = %v, want pre-commit failure", err)
	}
	if err := <-pollerCaptureDone; err != nil {
		t.Fatalf("capturing poller after cleanup Close: %v", err)
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

func TestManagerStopAbsentSessionPreservesInvalidPollerOwnership(t *testing.T) {
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

	generation, err := nudge.CapturePollerGeneration(townRoot, mgr.SessionName())
	if err != nil {
		t.Fatal(err)
	}
	err = mgr.stopPollerForAbsentSession(townRoot, mgr.SessionName(), generation)
	if err == nil {
		t.Fatal("stopPollerForAbsentSession() succeeded with invalid poller ownership")
	}
}

func TestManagerStopUsesExactTransactionForLiveSession(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=other"; exit 0 ;;
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
	if err := mgr.Stop(); err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "cleanup-run $1 fixture-generation-original fixture-pane-generation") {
		t.Fatalf("Stop skipped exact generation cleanup:\n%s", logData)
	}
}

func TestManagerStopUsesExplicitFallbackWhenPlatformCustodyIsUnavailable(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=other"; exit 0 ;;
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
	mgr.prepareSessionCleanup = func(tmux.SessionGeneration, tmux.PaneProcessGeneration) (sessionGenerationCleanup, error) {
		return nil, tmux.ErrSessionCustodyUnsupported
	}
	var explicitCalled bool
	mgr.prepareExplicitCleanup = func(generation tmux.SessionGeneration, pane tmux.PaneProcessGeneration) (sessionGenerationCleanup, error) {
		explicitCalled = true
		return &fakeSessionCleanup{generation: generation, pane: pane, logPath: logPath}, nil
	}

	if err := mgr.Stop(); err != nil {
		t.Fatal(err)
	}
	if !explicitCalled {
		t.Fatal("explicit Stop did not use the reviewed platform fallback")
	}
}

func TestManagerZombieReplacementNeverUsesExplicitFallback(t *testing.T) {
	mgr, pollerPath, _ := newFakeTmuxManager(t, `
case "$*" in
  *"has-session"*) exit 0 ;;
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
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
	mgr.prepareSessionCleanup = func(tmux.SessionGeneration, tmux.PaneProcessGeneration) (sessionGenerationCleanup, error) {
		return nil, tmux.ErrSessionCustodyUnsupported
	}
	var explicitCalled bool
	mgr.prepareExplicitCleanup = func(tmux.SessionGeneration, tmux.PaneProcessGeneration) (sessionGenerationCleanup, error) {
		explicitCalled = true
		return &fakeSessionCleanup{}, nil
	}

	err := mgr.Start(false, "", nil)
	if !errors.Is(err, tmux.ErrSessionCustodyUnsupported) {
		t.Fatalf("Start() error = %v, want fail-closed custody error", err)
	}
	if explicitCalled {
		t.Fatal("automatic zombie replacement used explicit Stop fallback")
	}
}

func TestManagerFailedStartupRollbackUsesExactTransaction(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=other"; exit 0 ;;
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
	generation, err := mgr.tmux.CaptureSessionGeneration(mgr.SessionName())
	if err != nil {
		t.Fatal(err)
	}
	paneGeneration, err := mgr.capturePaneGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	pollerGeneration, err := nudge.CapturePollerGeneration(filepath.Dir(mgr.rig.Path), mgr.SessionName())
	if err != nil {
		t.Fatal(err)
	}
	startupErr := errors.New("agent startup failed")
	err = mgr.rollbackFailedStart(
		filepath.Dir(mgr.rig.Path),
		mgr.SessionName(),
		generation,
		paneGeneration,
		pollerGeneration,
		startupErr,
		nil,
	)
	if !errors.Is(err, startupErr) {
		t.Fatalf("rollbackFailedStart() error = %v, want startup cause", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "cleanup-run $1 fixture-generation-original fixture-pane-generation") {
		t.Fatalf("startup rollback skipped exact generation cleanup:\n%s", logData)
	}
}

func TestManagerFailedStartupRollbackUsesExplicitFallback(t *testing.T) {
	mgr, pollerPath, logPath := newFakeTmuxManager(t, `
case "$*" in
  *"list-sessions"*) printf '%s:$1\n' "$FAKE_SESSION_NAME"; exit 0 ;;
  *"show-environment"*"GT_PROCESS_NAMES"*) echo "GT_PROCESS_NAMES=other"; exit 0 ;;
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
	generation, err := mgr.tmux.CaptureSessionGeneration(mgr.SessionName())
	if err != nil {
		t.Fatal(err)
	}
	paneGeneration, err := mgr.capturePaneGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	pollerGeneration, err := nudge.CapturePollerGeneration(filepath.Dir(mgr.rig.Path), mgr.SessionName())
	if err != nil {
		t.Fatal(err)
	}
	mgr.prepareSessionCleanup = func(tmux.SessionGeneration, tmux.PaneProcessGeneration) (sessionGenerationCleanup, error) {
		return nil, tmux.ErrProcessReferenceUnsupported
	}
	var explicitCalled bool
	mgr.prepareExplicitCleanup = func(generation tmux.SessionGeneration, pane tmux.PaneProcessGeneration) (sessionGenerationCleanup, error) {
		explicitCalled = true
		return &fakeSessionCleanup{generation: generation, pane: pane, logPath: logPath}, nil
	}

	startupErr := errors.New("agent startup failed")
	err = mgr.rollbackFailedStart(
		filepath.Dir(mgr.rig.Path),
		mgr.SessionName(),
		generation,
		paneGeneration,
		pollerGeneration,
		startupErr,
		nil,
	)
	if !errors.Is(err, startupErr) {
		t.Fatalf("rollbackFailedStart() error = %v, want startup cause", err)
	}
	if !explicitCalled {
		t.Fatal("attempt-owned failed-start rollback did not use explicit fallback")
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

	if !strings.HasPrefix(got, "exec ") {
		t.Errorf("expected direct runtime exec command, got %q", got)
	}
	for _, inline := range []string{"GT_ROLE=", "BD_ACTOR=", "GT_DOLT_HOST=", "GT_DOLT_PORT="} {
		if strings.Contains(got, inline) {
			t.Errorf("startup command repeated session environment %q inline: %q", inline, got)
		}
	}
}

func TestBuildWitnessStartCommandWithEnvReturnsResolvedPresetEnvironment(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	baseEnv := map[string]string{
		"GT_ROLE":      "gastown/witness",
		"GT_DOLT_HOST": "127.0.0.1",
		"GT_DOLT_PORT": "41000",
	}

	command, resolvedEnv, err := buildWitnessStartCommandWithEnv(
		rigPath, "gastown", townRoot, "gastown-witness", "opencode", nil, "", baseEnv,
	)
	if err != nil {
		t.Fatalf("buildWitnessStartCommandWithEnv: %v", err)
	}
	for _, inline := range []string{"GT_DOLT_HOST=", "GT_DOLT_PORT=", "OPENCODE_PERMISSION="} {
		if strings.Contains(command, inline) {
			t.Fatalf("startup command repeated authoritative environment %q inline: %q", inline, command)
		}
	}
	if got := resolvedEnv["GT_DOLT_HOST"]; got != "127.0.0.1" {
		t.Fatalf("resolved GT_DOLT_HOST = %q", got)
	}
	if got := resolvedEnv["GT_DOLT_PORT"]; got != "41000" {
		t.Fatalf("resolved GT_DOLT_PORT = %q", got)
	}
	if got := resolvedEnv["GT_AGENT"]; got != "opencode" {
		t.Fatalf("resolved GT_AGENT = %q, want opencode", got)
	}
	if got := resolvedEnv["OPENCODE_PERMISSION"]; got != `{"*":"allow"}` {
		t.Fatalf("resolved OPENCODE_PERMISSION = %q", got)
	}
	if _, mutated := baseEnv["OPENCODE_PERMISSION"]; mutated {
		t.Fatal("startup environment resolution mutated caller map")
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

func TestBuildWitnessStartCommand_DoesNotInlineConfigDir(t *testing.T) {
	t.Parallel()
	got, err := buildWitnessStartCommand("/town/rig", "gastown", "/town", "", "", nil, "/home/user/.claude-accounts/work")
	if err != nil {
		t.Fatalf("buildWitnessStartCommand: %v", err)
	}

	if strings.Contains(got, "CLAUDE_CONFIG_DIR=") {
		t.Errorf("startup command repeated authoritative tmux environment inline: %q", got)
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
	if !strings.Contains(got, "codex") || strings.Contains(got, "GT_ROLE=") {
		t.Errorf("expected direct codex exec without inline session environment, got %q", got)
	}
}
