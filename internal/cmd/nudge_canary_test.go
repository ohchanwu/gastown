package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

type wakeCanaryIdleWaiterStub struct {
	session string
	timeout time.Duration
	err     error
}

type wakeCanaryTurnWaiterStub struct {
	events          []string
	responseTimeout time.Duration
	idleTimeout     time.Duration
	responseErr     error
	idleErr         error
}

func (s *wakeCanaryTurnWaiterStub) WaitForResponse(_, _, _ string, timeout time.Duration) error {
	s.events = append(s.events, "response")
	s.responseTimeout = timeout
	return s.responseErr
}

func (s *wakeCanaryTurnWaiterStub) WaitForIdle(_ string, timeout time.Duration) error {
	s.events = append(s.events, "idle")
	s.idleTimeout = timeout
	return s.idleErr
}

func buildWakeCanaryCandidateGT(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	buildDir := t.TempDir()
	candidate := filepath.Join(buildDir, "gt")
	cmd := exec.Command("make", "build", "BUILD_DIR="+buildDir)
	cmd.Dir = repoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build candidate gt: %v: %s", err, output)
	}
	if output, err := exec.Command(candidate, "version").CombinedOutput(); err != nil {
		t.Fatalf("run candidate gt: %v: %s", err, output)
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

func TestConfirmWakeCanaryTurnWaitsForSteadyIdleBeforeNextDelivery(t *testing.T) {
	waiter := &wakeCanaryTurnWaiterStub{}

	code, err := confirmWakeCanaryTurn(waiter, session.MayorSessionName(), "before", "response")
	waiter.events = append(waiter.events, "next-delivery")

	if err != nil || code != "" {
		t.Fatalf("confirmWakeCanaryTurn = (%q, %v), want success", code, err)
	}
	if got, want := strings.Join(waiter.events, ","), "response,idle,next-delivery"; got != want {
		t.Fatalf("turn ordering = %q, want %q", got, want)
	}
	if waiter.responseTimeout != 30*time.Second || waiter.idleTimeout != constants.ClaudeStartTimeout {
		t.Fatalf("turn bounds = response %s idle %s", waiter.responseTimeout, waiter.idleTimeout)
	}
}

func TestConfirmWakeCanaryTurnDistinguishesResponseAndIdleFailures(t *testing.T) {
	responseErr := errors.New("response missing")
	idleErr := errors.New("turn still active")
	for _, tt := range []struct {
		name       string
		waiter     *wakeCanaryTurnWaiterStub
		wantCode   string
		wantErr    error
		wantEvents string
	}{
		{name: "response unconfirmed", waiter: &wakeCanaryTurnWaiterStub{responseErr: responseErr}, wantCode: "model-turn-unconfirmed", wantErr: responseErr, wantEvents: "response"},
		{name: "response observed but not idle", waiter: &wakeCanaryTurnWaiterStub{idleErr: idleErr}, wantCode: "model-turn-not-idle", wantErr: idleErr, wantEvents: "response,idle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, err := confirmWakeCanaryTurn(tt.waiter, session.MayorSessionName(), "before", "response")
			if code != tt.wantCode || !errors.Is(err, tt.wantErr) {
				t.Fatalf("confirmWakeCanaryTurn = (%q, %v), want (%q, %v)", code, err, tt.wantCode, tt.wantErr)
			}
			if got := strings.Join(tt.waiter.events, ","); got != tt.wantEvents {
				t.Fatalf("turn events = %q, want %q", got, tt.wantEvents)
			}
		})
	}
}

func TestWakeCanaryStartupChallengeRequiresFiniteReply(t *testing.T) {
	instruction, response := wakeCanaryStartupChallenge("abc123")
	if response != "321cba" {
		t.Fatalf("startup response = %q, want reversed nonce", response)
	}
	if want := "Reply with exactly the reverse of nonce abc123."; instruction != want {
		t.Fatalf("startup instruction = %q, want one finite request %q", instruction, want)
	}
	if strings.Contains(instruction, response) {
		t.Fatal("startup instruction contains the expected response before the model turn")
	}
}

func TestRunWakeCanaryPersistsSessionNotIdleBeforeLease(t *testing.T) {
	previousCommit := Commit
	Commit = "test-commit"
	t.Cleanup(func() { Commit = previousCommit })

	socket := "gt-wake-canary-" + nudge.NewDeliveryID()
	tm := tmux.NewTmuxWithSocket(socket)
	socketPath := filepath.Join(tmux.SocketDir(), socket)
	runtimeTownRoot := t.TempDir()
	sessionName := session.MayorSessionName()
	t.Cleanup(func() {
		if _, err := os.Lstat(socketPath); !os.IsNotExist(err) {
			t.Errorf("test tmux socket still exists after cleanup: %v", err)
		}
	})
	t.Cleanup(func() {
		sandbox := &wakeCanarySandbox{TownRoot: runtimeTownRoot, Socket: socket, Session: sessionName, tmux: tm}
		if err := sandbox.Cleanup(); err != nil {
			t.Errorf("cleanup test wake canary sandbox: %v", err)
		}
	})

	evidenceRoot := t.TempDir()
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

func TestWakeCanaryLaunchBypassesHookTrustOnlyForCanary(t *testing.T) {
	sandbox := &wakeCanarySandbox{
		TownRoot:         t.TempDir(),
		WorkDir:          t.TempDir(),
		RuntimeConfigDir: t.TempDir(),
		Session:          session.MayorSessionName(),
	}
	settings := config.NewTownSettings()
	settings.Agents["codex"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.RoleAgents = map[string]string{constants.RoleMayor: "codex"}
	if err := config.SaveTownSettings(config.TownSettingsPath(sandbox.TownRoot), settings); err != nil {
		t.Fatalf("save canary town settings: %v", err)
	}
	cfg, err := wakeCanarySessionConfig(sandbox, "complete the finite startup challenge")
	if err != nil {
		t.Fatalf("build canary session config: %v", err)
	}
	runtimeConfig, _, err := config.ResolveAgentConfigWithOverride(cfg.TownRoot, cfg.RigPath, cfg.AgentOverride)
	if err != nil {
		t.Fatalf("resolve canary runtime: %v", err)
	}
	env := config.AgentEnv(config.AgentEnvConfig{
		Role: cfg.Role, Rig: cfg.RigName, AgentName: cfg.AgentName, TownRoot: cfg.TownRoot,
		RuntimeConfigDir: cfg.RuntimeConfigDir, Agent: cfg.AgentOverride, SessionName: cfg.SessionID,
	})
	env = session.MergeRuntimeLivenessEnv(env, runtimeConfig)

	if got := env["GT_AGENT"]; got != "codex" {
		t.Fatalf("canary GT_AGENT = %q, want receipt-compatible codex identity", got)
	}
	if got := env["GT_PROCESS_NAMES"]; got != "codex" {
		t.Fatalf("canary GT_PROCESS_NAMES = %q, want codex", got)
	}

	prompt := session.BuildStartupPrompt(cfg.Beacon, cfg.Instructions)
	ordinaryCommand, err := config.BuildAgentStartupCommandWithAgentOverride(
		cfg.Role, cfg.RigName, cfg.TownRoot, cfg.RigPath, prompt, "codex",
	)
	if err != nil {
		t.Fatalf("build ordinary startup command: %v", err)
	}

	const flag = "--dangerously-bypass-hook-trust"
	if !strings.Contains(cfg.Command, " "+flag+" ") {
		t.Fatalf("canary startup command lacks %s: %q", flag, cfg.Command)
	}
	if strings.Contains(cfg.Command, "GT_AGENT=") {
		t.Fatalf("pure canary agent command unexpectedly overrides GT_AGENT: %q", cfg.Command)
	}
	if strings.Contains(ordinaryCommand, flag) {
		t.Fatalf("ordinary startup command unexpectedly contains %s: %q", flag, ordinaryCommand)
	}
}

func TestWakeCanarySessionConfigUsesIsolatedConfiguredMayorPreset(t *testing.T) {
	callerDir := t.TempDir()
	callerSettings := config.NewRigSettings()
	callerSettings.Agents = map[string]*config.RuntimeConfig{
		"codex": {Provider: "codex", Command: "caller-codex-wrapper", Args: []string{"--caller"}},
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(callerDir), callerSettings); err != nil {
		t.Fatalf("save caller rig settings: %v", err)
	}
	t.Chdir(callerDir)

	townRoot := t.TempDir()
	workDir := filepath.Join(townRoot, "mayor", "rig")
	if err := os.MkdirAll(workDir, 0700); err != nil {
		t.Fatalf("create isolated workdir: %v", err)
	}
	townSettings := config.NewTownSettings()
	townSettings.Agents["night-mayor"] = &config.RuntimeConfig{
		Provider: "codex", Command: "configured-codex", Args: []string{"--model", "mayor"},
	}
	townSettings.RoleAgents = map[string]string{constants.RoleMayor: "night-mayor"}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), townSettings); err != nil {
		t.Fatalf("save isolated town settings: %v", err)
	}
	sandbox := &wakeCanarySandbox{
		TownRoot: townRoot, WorkDir: workDir, RuntimeConfigDir: filepath.Join(townRoot, ".codex"),
		Session: session.MayorSessionName(),
	}
	cfg, err := wakeCanarySessionConfig(sandbox, "complete the finite startup challenge")
	if err != nil {
		t.Fatalf("build canary session config: %v", err)
	}

	runtimeConfig, _, err := config.ResolveAgentConfigWithOverride(cfg.TownRoot, cfg.RigPath, cfg.AgentOverride)
	if err != nil {
		t.Fatalf("resolve StartSession runtime: %v", err)
	}
	if cfg.AgentOverride != "night-mayor" || runtimeConfig.Provider != "codex" {
		t.Fatalf("StartSession runtime = %q/%q, want night-mayor/codex", cfg.AgentOverride, runtimeConfig.Provider)
	}
	if !strings.Contains(cfg.Command, "configured-codex") || !strings.Contains(cfg.Command, "--model mayor") {
		t.Fatalf("canary command does not preserve configured preset: %q", cfg.Command)
	}
	if cfg.RigPath != sandbox.WorkDir {
		t.Fatalf("canary rig path = %q, want sandbox workdir %q", cfg.RigPath, sandbox.WorkDir)
	}
	if strings.Contains(cfg.Command, "caller-codex-wrapper") {
		t.Fatalf("canary command inherited caller wrapper: %q", cfg.Command)
	}
}

func TestWriteWakeCanaryStateIsSanitizedAtomicAndPrivate(t *testing.T) {
	townRoot := t.TempDir()
	state := wakeCanaryState{
		SchemaVersion:         2,
		InstalledBinaryCommit: "abc123",
		MayorPreset:           "codex-mayor",
		MayorProvider:         "codex",
		PolecatPreset:         "codex-polecat",
		PolecatProvider:       "codex",
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
	if len(got) != 10 || got["schema_version"] != float64(2) || got["result"] != "passed" ||
		got["mayor_preset"] != "codex-mayor" || got["polecat_preset"] != "codex-polecat" {
		t.Fatalf("state schema = %#v", got)
	}
	for _, forbidden := range []string{"session", "nonce", "message", "delivery_id"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("state leaked %s: %#v", forbidden, got)
		}
	}
}

func TestResolveWakeCanaryRolesUsesConfiguredPresetsAndProviders(t *testing.T) {
	townRoot := t.TempDir()
	settings := config.NewTownSettings()
	settings.Agents["codex-mayor"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.Agents["codex-polecat"] = &config.RuntimeConfig{Provider: "codex", Command: "codex"}
	settings.RoleAgents = map[string]string{
		constants.RoleMayor:   "codex-mayor",
		constants.RolePolecat: "codex-polecat",
	}
	if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	got := resolveWakeCanaryRoles(townRoot, "")
	if got.MayorPreset != "codex-mayor" || got.MayorProvider != "codex" {
		t.Fatalf("Mayor role evidence = %+v, want codex-mayor/codex", got)
	}
	if got.PolecatPreset != "codex-polecat" || got.PolecatProvider != "codex" {
		t.Fatalf("polecat role evidence = %+v, want codex-polecat/codex", got)
	}
}
