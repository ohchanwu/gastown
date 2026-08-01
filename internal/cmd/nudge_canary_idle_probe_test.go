package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/delivery"
	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

var idlePaneProbeANSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func idlePaneProbeHasExactReply(styled, token string) bool {
	for _, line := range strings.Split(styled, "\n") {
		plain := strings.TrimSpace(idlePaneProbeANSI.ReplaceAllString(line, ""))
		if plain == token || strings.TrimPrefix(plain, "• ") == token {
			return true
		}
	}
	return false
}

func idlePaneProbeCompleted(styled, token string) bool {
	return !strings.Contains(styled, "esc to interrupt") && idlePaneProbeHasExactReply(styled, token)
}

func TestProbeIsolatedCodexIdlePane(t *testing.T) {
	if os.Getenv("GT_RUN_IDLE_PANE_PROBE") != "1" {
		t.Skip("set GT_RUN_IDLE_PANE_PROBE=1 for the private isolated probe")
	}
	evidencePath := os.Getenv("GT_IDLE_PANE_EVIDENCE")
	if !filepath.IsAbs(evidencePath) {
		t.Fatal("GT_IDLE_PANE_EVIDENCE must be an absolute path")
	}

	sandbox, err := newWakeCanarySandbox("", buildWakeCanaryCandidateGT(t))
	if err != nil {
		t.Fatalf("newWakeCanarySandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Cleanup() })
	if err := sandbox.linkCodexAuth(); err != nil {
		t.Fatalf("linkCodexAuth: %v", err)
	}
	probeInstruction, probeToken := wakeCanaryStartupChallenge("A2F7C9")
	var instructionObserved, instructionStranded, startupDialogObserved, modelTurnStarted, exactReplyObserved bool
	observeStages := func(content []byte) {
		styled := string(content)
		plain := idlePaneProbeANSI.ReplaceAllString(styled, "")
		for _, line := range strings.Split(plain, "\n") {
			if strings.Contains(line, probeInstruction) {
				instructionObserved = true
				if strings.HasPrefix(strings.TrimSpace(line), "›") {
					instructionStranded = true
				}
			}
		}
		startupDialogObserved = startupDialogObserved || strings.Contains(plain, "Do you trust the contents of this directory?") || strings.Contains(plain, "Hooks need review")
		modelTurnStarted = modelTurnStarted || strings.Contains(plain, "esc to interrupt")
		exactReplyObserved = exactReplyObserved || idlePaneProbeHasExactReply(styled, probeToken)
	}
	capture := func() ([]byte, error) {
		return exec.Command("tmux", "-L", sandbox.Socket, "capture-pane", "-p", "-e", "-t", sandbox.Session, "-S", "-").Output()
	}
	writeEvidence := func(content []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(evidencePath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(evidencePath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(evidencePath, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(evidencePath, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata := func() {
		t.Helper()
		cursor, cursorErr := exec.Command("tmux", "-L", sandbox.Socket, "display-message", "-p", "-t", sandbox.Session, "#{cursor_x}|#{cursor_y}|#{pane_width}|#{pane_height}").Output()
		visible, visibleErr := exec.Command("tmux", "-L", sandbox.Socket, "capture-pane", "-p", "-e", "-t", sandbox.Session).Output()
		agent, agentErr := sandbox.tmux.GetEnvironment(sandbox.Session, "GT_AGENT")
		cursorX, cursorY := -1, -1
		_, cursorParseErr := fmt.Sscanf(strings.TrimSpace(string(cursor)), "%d|%d", &cursorX, &cursorY)
		promptRow := -1
		promptRows := 0
		visibleLines := strings.Split(string(visible), "\n")
		for row, line := range visibleLines {
			if strings.Contains(line, "›") {
				promptRow = row
				promptRows++
			}
		}
		plainVisible := idlePaneProbeANSI.ReplaceAllString(string(visible), "")
		startupDialogActive := strings.Contains(plainVisible, "Do you trust the contents of this directory?") || strings.Contains(plainVisible, "Hooks need review")
		metadata := fmt.Sprintf("cursor_query_ok=%t\ncursor_parse_ok=%t\nvisible_query_ok=%t\nagent_query_ok=%t\nagent_is_codex=%t\ninstruction_observed=%t\ninstruction_stranded=%t\nruntime_receipt_observed=%t\nstartup_dialog_observed=%t\nstartup_dialog_active=%t\nmodel_turn_started=%t\nexact_reply_observed=%t\nvisible_lines=%d\nprompt_rows=%d\nprompt_row=%d\nprompt_on_cursor_row=%t\ncursor_x=%d\ncursor_y=%d\n", cursorErr == nil, cursorParseErr == nil, visibleErr == nil, agentErr == nil, agent == "codex", instructionObserved, instructionStranded, modelTurnStarted || exactReplyObserved, startupDialogObserved, startupDialogActive, modelTurnStarted, exactReplyObserved, len(visibleLines), promptRows, promptRow, promptRow == cursorY, cursorX, cursorY)
		if err := os.WriteFile(evidencePath+".meta", []byte(metadata), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(evidencePath+".meta", 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := session.StartSession(sandbox.tmux, session.SessionConfig{
		SessionID: sandbox.Session, WorkDir: sandbox.WorkDir, Role: "mayor",
		TownRoot: sandbox.TownRoot, AgentOverride: "codex", RuntimeConfigDir: sandbox.RuntimeConfigDir,
		ExtraEnv:         map[string]string{"GT_TOWN_ROOT": sandbox.TownRoot, "CODEX_HOME": sandbox.RuntimeConfigDir},
		StripEnvPrefixes: []string{"GT_DOLT_", "BD_", "BEADS_", "DOLT_"},
		Beacon:           session.BeaconConfig{Recipient: "isolated wake-canary mayor", Sender: "self", Topic: "canary"},
		Instructions:     probeInstruction,
		WaitForAgent:     true, WaitFatal: true, AcceptBypass: true, ReadyDelay: true, VerifySurvived: true,
	}); err != nil {
		if content, captureErr := capture(); captureErr == nil {
			writeEvidence(content)
		}
		t.Fatalf("StartSession: %v", err)
	}
	if content, captureErr := capture(); captureErr == nil {
		observeStages(content)
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	t.Cleanup(ticker.Stop)
	timeout := time.NewTimer(constants.ClaudeStartTimeout + 30*time.Second)
	t.Cleanup(func() { timeout.Stop() })
	stable := 0
	var lastCapture []byte
	for {
		select {
		case <-ticker.C:
			content, captureErr := capture()
			if captureErr != nil {
				stable = 0
				continue
			}
			lastCapture = content
			observeStages(content)
			styled := string(content)
			if idlePaneProbeCompleted(styled, probeToken) {
				stable++
			} else {
				stable = 0
			}
			if stable < 2 {
				continue
			}
			writeEvidence(content)
			writeMetadata()
			if err := waitForWakeCanaryIdle(sandbox.tmux, sandbox.Session); err != nil {
				t.Fatalf("WaitForIdle after completed response: %v", err)
			}
			info, err := os.Stat(evidencePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("private styled pane captured: path=%s bytes=%d mode=%o", evidencePath, info.Size(), info.Mode().Perm())
			return
		case <-timeout.C:
			if len(lastCapture) > 0 {
				writeEvidence(lastCapture)
				writeMetadata()
			}
			t.Fatal("isolated Codex probe did not reach stable completed output")
		}
	}
}

func TestIdlePaneProbeRecognizesReplyAlreadyPresentAtBaseline(t *testing.T) {
	const token = "IDLE_PANE_PROBE_COMPLETE_7F2A"

	if idlePaneProbeCompleted("Reply with exactly "+token+" and then wait.\n", token) {
		t.Fatal("instruction-only snapshot reported completion")
	}
	completed := "Reply with exactly " + token + " and then wait.\n" + token + "\n"
	if !idlePaneProbeCompleted(completed, token) {
		t.Fatal("exact reply already present at baseline was missed")
	}
	if !idlePaneProbeCompleted("\x1b[2m"+token+"\x1b[0m\n", token) {
		t.Fatal("styled exact reply was missed")
	}
	if !idlePaneProbeCompleted("• "+token+"\n", token) {
		t.Fatal("Codex-decorated exact reply was missed")
	}
	if idlePaneProbeCompleted(completed+"esc to interrupt\n", token) {
		t.Fatal("busy snapshot reported stable completion")
	}
}

func TestProbeIsolatedCodexFirstDelivery(t *testing.T) {
	if os.Getenv("GT_RUN_FIRST_DELIVERY_PROBE") != "1" {
		t.Skip("set GT_RUN_FIRST_DELIVERY_PROBE=1 for the private isolated probe")
	}
	evidencePath := os.Getenv("GT_FIRST_DELIVERY_EVIDENCE")
	if !filepath.IsAbs(evidencePath) {
		t.Fatal("GT_FIRST_DELIVERY_EVIDENCE must be an absolute path")
	}

	candidateGT := buildWakeCanaryCandidateGT(t)
	sandbox, err := newWakeCanarySandbox("", candidateGT)
	if err != nil {
		t.Fatalf("newWakeCanarySandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Cleanup() })
	if err := sandbox.linkCodexAuth(); err != nil {
		t.Fatalf("linkCodexAuth: %v", err)
	}
	hookProbeMarker := ""
	hookPayloadMeta := ""
	if os.Getenv("GT_FIRST_DELIVERY_WRAP_HOOK") == "1" {
		hookProbeMarker = filepath.Join(sandbox.TownRoot, ".runtime", "user-prompt-hook-invoked")
		hookPayload := filepath.Join(sandbox.TownRoot, ".runtime", "user-prompt-hook-input.json")
		hookPayloadMeta = filepath.Join(sandbox.TownRoot, ".runtime", "user-prompt-hook-input.meta")
		hookError := filepath.Join(sandbox.TownRoot, ".runtime", "user-prompt-hook-error")
		wrapper := filepath.Join(sandbox.TownRoot, "user-prompt-hook-probe")
		script := fmt.Sprintf(`#!/bin/sh
umask 077
mkdir -p %s
: > %s
if [ "$(tmux display-message -t "$TMUX_PANE" -p '#{session_name}' 2>/dev/null)" = %s ]; then
  export GT_PROBE_DIRECT_SESSION_MATCH=1
fi
tee %s | %s "$@" 2>%s
rc=$?
python3 - %s %s "$rc" %s %s %s %s <<'PY'
import json, os, sys
raw = open(sys.argv[1], 'rb').read()
stderr = open(sys.argv[4], 'rb').read()
try:
    payload = json.loads(raw)
    valid = True
except Exception:
    payload = {}
    valid = False
prompt = payload.get('prompt') if isinstance(payload.get('prompt'), str) else ''
prefix = '[gt-delivery-id:'
facts = {
    'candidate_exit_zero': sys.argv[3] == '0',
    'candidate_stderr_nonempty': bool(stderr),
    'control_at_zero': prompt.startswith(prefix),
    'control_present': prefix in prompt,
    'cwd_match': os.getcwd() == sys.argv[5],
    'delivery_receipt_error': b'delivery receipt error' in stderr,
    'direct_session_match': os.environ.get('GT_PROBE_DIRECT_SESSION_MATCH') == '1',
    'event_match': payload.get('hook_event_name') == 'UserPromptSubmit',
    'has_event_field': 'hook_event_name' in payload,
    'has_prompt_field': 'prompt' in payload,
    'json_valid': valid,
    'prompt_nonempty': bool(prompt),
    'gt_root_match': os.environ.get('GT_ROOT') == sys.argv[6],
    'gt_session_match': os.environ.get('GT_SESSION') == sys.argv[7],
    'gt_town_root_match': os.environ.get('GT_TOWN_ROOT') == sys.argv[6],
    'identity_error': b'identity' in stderr.lower(),
    'not_found_error': b'not found' in stderr.lower(),
    'tmux_env_present': bool(os.environ.get('TMUX')),
    'tmux_pane_present': bool(os.environ.get('TMUX_PANE')),
}
with open(sys.argv[2], 'w') as out:
    for key in sorted(facts):
        out.write(f'{key}={str(facts[key]).lower()}\n')
PY
exit "$rc"
`, filepath.Dir(hookProbeMarker), hookProbeMarker, sandbox.Session, hookPayload, candidateGT, hookError, hookPayload, hookPayloadMeta, hookError, sandbox.WorkDir, sandbox.TownRoot, sandbox.Session)
		if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		hooksPath := filepath.Join(sandbox.RuntimeConfigDir, "hooks.json")
		hooksData, err := os.ReadFile(hooksPath)
		if err != nil {
			t.Fatal(err)
		}
		oldCommand := candidateGT + " mail check --inject"
		newCommand := wrapper + " mail check --inject"
		updated := strings.Replace(string(hooksData), oldCommand, newCommand, 1)
		if updated == string(hooksData) {
			t.Fatal("UserPromptSubmit candidate command not found")
		}
		if err := os.WriteFile(hooksPath, []byte(updated), 0600); err != nil {
			t.Fatal(err)
		}
	}

	startupInstruction, startupResponse := wakeCanaryStartupChallenge("B3D8E4")
	if _, err := session.StartSession(sandbox.tmux, session.SessionConfig{
		SessionID: sandbox.Session, WorkDir: sandbox.WorkDir, Role: "mayor",
		TownRoot: sandbox.TownRoot, AgentOverride: "codex", RuntimeConfigDir: sandbox.RuntimeConfigDir,
		ExtraEnv:         map[string]string{"GT_TOWN_ROOT": sandbox.TownRoot, "CODEX_HOME": sandbox.RuntimeConfigDir},
		StripEnvPrefixes: []string{"GT_DOLT_", "BD_", "BEADS_", "DOLT_"},
		Beacon:           session.BeaconConfig{Recipient: "isolated wake-canary mayor", Sender: "self", Topic: "canary"},
		Instructions:     startupInstruction,
		WaitForAgent:     true, WaitFatal: true, AcceptBypass: true, ReadyDelay: true, VerifySurvived: true,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := waitForCanaryResponse(sandbox.tmux, sandbox.Session, "", startupResponse, constants.ClaudeStartTimeout); err != nil {
		t.Fatalf("startup response: %v", err)
	}

	var idleErr error
	idleCheckSkipped := os.Getenv("GT_FIRST_DELIVERY_CAPTURE_HOOK") == "1"
	if !idleCheckSkipped {
		idleErr = waitForWakeCanaryIdle(sandbox.tmux, sandbox.Session)
	}
	deliveryID := nudge.NewDeliveryID()
	receipt := tmux.SubmissionReceipt{Session: sandbox.Session, DeliveryID: deliveryID}
	var deliveryErr error
	if idleErr == nil {
		receipt, deliveryErr = sandbox.tmux.NudgeSessionWithReceipt(sandbox.Session, "Reply with exactly FIRST_DELIVERY_PROBE_COMPLETE.", tmux.NudgeOpts{TownRoot: sandbox.TownRoot, DeliveryID: deliveryID})
	}

	stage := "submitted"
	switch {
	case idleErr != nil:
		stage = "idle_timeout"
	case receipt.Typed && !receipt.Submitted:
		stage = "typed_receipt_missing"
	case !receipt.Typed:
		stage = "delivery_failed_before_type"
	}
	receiptPath := delivery.ReceiptPath(sandbox.TownRoot, sandbox.Session)
	_, receiptPathErr := os.Stat(receiptPath)
	receiptFiles := 0
	if entries, readErr := os.ReadDir(filepath.Dir(receiptPath)); readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".jsonl") {
				receiptFiles++
			}
		}
	}
	_, hookProbeErr := os.Stat(hookProbeMarker)
	hookFacts := ""
	if data, readErr := os.ReadFile(hookPayloadMeta); readErr == nil {
		hookFacts = string(data)
	}
	metadata := fmt.Sprintf("stage=%s\nidle_check_skipped=%t\nidle_timeout=%t\ntyped=%t\nsubmitted=%t\nsubmit_not_verified=%t\nsession_match=%t\ndelivery_match=%t\nhook_invoked=%t\nmatching_receipt_path_exists=%t\nreceipt_file_count=%d\n%s", stage, idleCheckSkipped, errors.Is(idleErr, tmux.ErrIdleTimeout), receipt.Typed, receipt.Submitted, errors.Is(deliveryErr, tmux.ErrSubmitNotVerified), receipt.Session == sandbox.Session, receipt.DeliveryID == deliveryID, hookProbeMarker != "" && hookProbeErr == nil, receiptPathErr == nil, receiptFiles, hookFacts)
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(evidencePath, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(evidencePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("private first-delivery metadata: path=%s bytes=%d mode=%o", evidencePath, info.Size(), info.Mode().Perm())

	if idleErr != nil || deliveryErr != nil || !receipt.Typed || !receipt.Submitted {
		t.Fatalf("first delivery stage=%s", stage)
	}
}
