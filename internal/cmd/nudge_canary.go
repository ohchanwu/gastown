package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/hooks"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/witness"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	nudgeCanaryConfirm bool
)

const wakeCanaryTurns = 20

type wakeCanaryResult struct {
	ID          string    `json:"id"`
	Session     string    `json:"session"`
	Turns       int       `json:"turns"`
	Submitted   int       `json:"submitted"`
	Queued      int       `json:"queued"`
	Failed      int       `json:"failed"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

type wakeCanaryState struct {
	SchemaVersion         int       `json:"schema_version"`
	InstalledBinaryCommit string    `json:"installed_binary_commit"`
	AttemptedAt           time.Time `json:"attempted_at"`
	Result                string    `json:"result"`
	LatencyMS             int64     `json:"latency_ms"`
	FailureCode           string    `json:"failure_code"`
}

type wakeCanarySandbox struct {
	TownRoot         string
	WorkDir          string
	RuntimeConfigDir string
	Socket           string
	Session          string
	tmux             *tmux.Tmux
}

func newWakeCanarySandbox(parent string) (*wakeCanarySandbox, error) {
	townRoot, err := os.MkdirTemp(parent, "gt-wake-canary-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(townRoot) }
	if err := os.Chmod(townRoot, 0700); err != nil {
		cleanup()
		return nil, err
	}
	workDir := filepath.Join(townRoot, "mayor", "rig")
	runtimeConfigDir := filepath.Join(townRoot, ".codex")
	for _, dir := range []string{workDir, runtimeConfigDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			cleanup()
			return nil, err
		}
	}
	cmd := exec.Command("bd", "init", "--non-interactive", "--quiet", "--prefix", "canary", "--skip-agents", "--skip-hooks")
	cmd.Dir = townRoot
	cmd.Env = isolatedCanaryEnv()
	if err := cmd.Run(); err != nil {
		cleanup()
		return nil, fmt.Errorf("initializing isolated Dolt database: %w", err)
	}
	if err := hooks.InstallForRole("codex", townRoot, townRoot, "mayor", ".codex", "hooks.json", false); err != nil {
		cleanup()
		return nil, fmt.Errorf("installing temporary Codex hooks: %w", err)
	}
	if err := atomicfile.WriteFile(filepath.Join(runtimeConfigDir, "config.toml"), []byte("[features]\nhooks = true\n"), 0600); err != nil {
		cleanup()
		return nil, fmt.Errorf("writing temporary Codex settings: %w", err)
	}
	id := nudge.NewDeliveryID()
	socket := "gt-wake-canary-" + id
	return &wakeCanarySandbox{
		TownRoot: townRoot, WorkDir: workDir, RuntimeConfigDir: runtimeConfigDir,
		Socket: socket, Session: session.MayorSessionName(),
		tmux: tmux.NewTmuxWithSocketAndEnv(socket, isolatedCanaryEnv()),
	}, nil
}

func isolatedCanaryEnv() []string {
	return append(filterIsolatedCanaryEnv(os.Environ()), "BD_NON_INTERACTIVE=1", "CI=1")

}

func filterIsolatedCanaryEnv(environ []string) []string {
	env := make([]string, 0, len(environ))
	for _, entry := range environ {
		if strings.HasPrefix(entry, "GT_") || strings.HasPrefix(entry, "BD_") ||
			strings.HasPrefix(entry, "BEADS_") || strings.HasPrefix(entry, "DOLT_") {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func (s *wakeCanarySandbox) linkCodexAuth() error {
	sourceRoot := os.Getenv("CODEX_HOME")
	if sourceRoot == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		sourceRoot = filepath.Join(home, ".codex")
	}
	source := filepath.Join(sourceRoot, "auth.json")
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("Codex authentication unavailable: %w", err)
	}
	if err := os.Symlink(source, filepath.Join(s.RuntimeConfigDir, "auth.json")); err != nil {
		return fmt.Errorf("linking temporary Codex authentication: %w", err)
	}
	return nil
}

func (s *wakeCanarySandbox) Cleanup() error {
	if s.tmux != nil {
		_ = s.tmux.KillSessionWithProcesses(s.Session)
		_ = s.tmux.KillServer()
	}
	return os.RemoveAll(s.TownRoot)
}

func init() {
	rootCmd.AddCommand(nudgeCanaryCmd)
	nudgeCanaryCmd.Flags().BoolVar(&nudgeCanaryConfirm, "confirm-live", false, "Confirm 20 wake turns in an isolated Codex Mayor session")
}

var nudgeCanaryCmd = &cobra.Command{
	Use:         "nudge-canary",
	Short:       "Run an opt-in receipt-verified Mayor wake canary",
	GroupID:     GroupComm,
	Annotations: map[string]string{AnnotationPolecatSafe: "true"},
	Args:        cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		if !nudgeCanaryConfirm {
			return fmt.Errorf("isolated Mayor canary requires --confirm-live")
		}
		evidenceRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		sandbox, err := newWakeCanarySandbox("")
		if err != nil {
			return err
		}
		defer sandbox.Cleanup()
		if err := sandbox.linkCodexAuth(); err != nil {
			return err
		}
		if _, err := session.StartSession(sandbox.tmux, session.SessionConfig{
			SessionID: sandbox.Session, WorkDir: sandbox.WorkDir, Role: "mayor",
			TownRoot: sandbox.TownRoot, AgentOverride: "codex", RuntimeConfigDir: sandbox.RuntimeConfigDir,
			ExtraEnv:         map[string]string{"GT_TOWN_ROOT": sandbox.TownRoot, "CODEX_HOME": sandbox.RuntimeConfigDir},
			StripEnvPrefixes: []string{"GT_DOLT_", "BD_", "BEADS_", "DOLT_"},
			Beacon:           session.BeaconConfig{Recipient: "isolated wake-canary mayor", Sender: "self", Topic: "canary"},
			Instructions:     "This is an isolated wake canary. For each high-priority Witness mail notification, reply with exactly the requested reversed nonce and no other text.",
			WaitForAgent:     true, WaitFatal: true, AcceptBypass: true, ReadyDelay: true, VerifySurvived: true,
		}); err != nil {
			return fmt.Errorf("starting isolated Codex Mayor: %w", err)
		}
		result, statePath, err := runWakeCanary(sandbox.tmux, sandbox.TownRoot, evidenceRoot, sandbox.Session, wakeCanaryTurns)
		fmt.Printf("Mayor wake canary: %d/%d submitted, %d queued, %d failed\nState: %s\n", result.Submitted, result.Turns, result.Queued, result.Failed, statePath)
		return err
	},
}

func runWakeCanary(t *tmux.Tmux, runtimeTownRoot, evidenceRoot, sessionName string, turns int) (wakeCanaryResult, string, error) {
	result := wakeCanaryResult{
		ID: "wake-" + nudge.NewDeliveryID(), Session: sessionName, Turns: turns, StartedAt: time.Now(),
	}
	state := wakeCanaryState{SchemaVersion: 1, InstalledBinaryCommit: resolveCommitHash(), AttemptedAt: result.StartedAt, Result: "running"}
	statePath, err := writeWakeCanaryState(evidenceRoot, state)
	if err != nil {
		return result, statePath, err
	}
	fail := func(code string, cause error) (wakeCanaryResult, string, error) {
		state.Result = "failed"
		state.FailureCode = code
		state.LatencyMS = time.Since(result.StartedAt).Milliseconds()
		_ = writeWakeCanaryStateAt(statePath, state)
		return result, statePath, cause
	}
	if sessionName != session.MayorSessionName() {
		return fail("identity-not-isolated", fmt.Errorf("wake canary requires an isolated Mayor identity"))
	}
	if t == nil || !t.IsIsolated() {
		return fail("tmux-not-isolated", fmt.Errorf("wake canary requires an isolated tmux socket"))
	}
	if state.InstalledBinaryCommit == "" {
		return fail("binary-commit-unknown", fmt.Errorf("wake canary requires an installed binary commit"))
	}
	releaseLease, err := t.AcquireNudgeLease(runtimeTownRoot, sessionName)
	if err != nil {
		return fail("nudge-lock-contended", fmt.Errorf("acquiring exclusive canary nudge lease: %w", err))
	}
	defer releaseLease()
	if err := t.ArmClientAttachmentLatch(sessionName); err != nil {
		return fail("client-latch-failed", fmt.Errorf("arming client attachment latch: %w", err))
	}
	router := mail.NewRouterWithTownRootAndTmux(runtimeTownRoot, runtimeTownRoot, t)

	for turn := 1; turn <= turns; turn++ {
		attached, attachmentErr := t.ClientAttachmentObserved(sessionName)
		if attachmentErr != nil {
			result.Failed++
			return fail("client-proof-failed", attachmentErr)
		}
		if attached {
			result.Failed++
			return fail("human-client-attached", fmt.Errorf("wake canary tmux session has an attached client"))
		}
		nonce := strings.TrimPrefix(nudge.NewDeliveryID(), "ndg-")
		response := reverseString(nonce)
		before, captureErr := t.CapturePaneAll(sessionName)
		if captureErr != nil {
			result.Failed++
			return fail("transcript-baseline-failed", captureErr)
		}
		outcome, sendErr := witness.DeliverMayorNotification(router,
			fmt.Sprintf("Wake canary %d/%d: reverse nonce %s", turn, turns, nonce),
			"Reply in the active model turn with the nonce reversed.")
		if sendErr != nil {
			result.Failed++
			return fail("mail-send-failed", sendErr)
		}
		if outcome == witness.MayorNotificationQueued {
			result.Queued++
			return fail("notification-queued", errors.New("Mayor notification queued"))
		}
		if outcome == witness.MayorNotificationWakeFailed {
			result.Failed++
			return fail("notification-failed", errors.New("Mayor notification wake failed"))
		}
		if err := waitForCanaryResponse(t, sessionName, before, response, 30*time.Second); err != nil {
			result.Failed++
			return fail("model-turn-unconfirmed", err)
		}
		attached, attachmentErr = t.ClientAttachmentObserved(sessionName)
		if attachmentErr != nil {
			result.Failed++
			return fail("client-proof-failed", attachmentErr)
		}
		if attached {
			result.Failed++
			return fail("human-client-attached", fmt.Errorf("canary tmux client attached during model turn"))
		}
		result.Submitted++
	}

	result.CompletedAt = time.Now()
	if result.Submitted != turns || result.Queued != 0 || result.Failed != 0 {
		return fail("count-mismatch", fmt.Errorf("wake canary failed: %d/%d submitted, %d queued, %d failed", result.Submitted, turns, result.Queued, result.Failed))
	}
	state.Result = "passed"
	state.FailureCode = ""
	state.LatencyMS = time.Since(result.StartedAt).Milliseconds()
	if err := writeWakeCanaryStateAt(statePath, state); err != nil {
		return result, statePath, err
	}
	return result, statePath, nil
}

func reverseString(value string) string {
	runes := []rune(value)
	for left, right := 0, len(runes)-1; left < right; left, right = left+1, right-1 {
		runes[left], runes[right] = runes[right], runes[left]
	}
	return string(runes)
}

func waitForCanaryResponse(t *tmux.Tmux, sessionName, baseline, response string, timeout time.Duration) error {
	if strings.Contains(baseline, response) {
		return fmt.Errorf("canary response was present before notification")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pane, err := t.CapturePaneAll(sessionName)
		if err != nil {
			return err
		}
		if strings.Contains(pane, response) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("model turn did not contain the nonce response within %s", timeout)
}

func writeWakeCanaryState(townRoot string, state wakeCanaryState) (string, error) {
	path := filepath.Join(townRoot, constants.DirRuntime, "canary", "control-plane.json")
	return path, writeWakeCanaryStateAt(path, state)
}

func writeWakeCanaryStateAt(path string, state wakeCanaryState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0600)
}
