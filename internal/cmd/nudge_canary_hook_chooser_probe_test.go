package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

func hookChooserTransitionAccepted(before, after string, dialog, progressAfterDialog bool) bool {
	if before == "" || after == "" || before == after {
		return false
	}
	return !dialog || progressAfterDialog
}

func hookChooserProgressAfterDialog(content string) bool {
	plain := idlePaneProbeANSI.ReplaceAllString(content, "")
	lastBlocker := -1
	lastProgress := -1
	for i, line := range strings.Split(plain, "\n") {
		normalized := strings.Join(strings.Fields(strings.ReplaceAll(line, "\u00a0", " ")), " ")
		lower := strings.ToLower(normalized)
		if normalized == "Hooks need review" || normalized == "› 1. Review hooks" ||
			normalized == "2. Trust all and continue" ||
			strings.Contains(lower, "hooks need review before they can run") ||
			normalized == "Press t to trust all; enter to review hooks; esc to close" {
			lastBlocker = i
		}
		if strings.Contains(normalized, "Trusting hooks") || strings.Contains(normalized, "esc to interrupt") {
			lastProgress = i
		}
	}
	return lastBlocker >= 0 && lastProgress > lastBlocker
}

func TestProbeIsolatedCodexHookChooserTarget(t *testing.T) {
	if os.Getenv("GT_RUN_HOOK_CHOOSER_PROBE") != "1" {
		t.Skip("set GT_RUN_HOOK_CHOOSER_PROBE=1 for the private isolated probe")
	}
	evidencePath := os.Getenv("GT_HOOK_CHOOSER_EVIDENCE")
	if !filepath.IsAbs(evidencePath) {
		t.Fatal("GT_HOOK_CHOOSER_EVIDENCE must be an absolute path")
	}

	sandbox, err := newWakeCanarySandbox("", buildWakeCanaryCandidateGT(t))
	if err != nil {
		t.Fatalf("newWakeCanarySandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Cleanup() })
	if err := sandbox.linkCodexAuth(); err != nil {
		t.Fatalf("linkCodexAuth: %v", err)
	}
	if _, err := session.StartSession(sandbox.tmux, session.SessionConfig{
		SessionID: sandbox.Session, WorkDir: sandbox.WorkDir, Role: "mayor",
		TownRoot: sandbox.TownRoot, AgentOverride: "codex", RuntimeConfigDir: sandbox.RuntimeConfigDir,
		ExtraEnv:         map[string]string{"GT_TOWN_ROOT": sandbox.TownRoot, "CODEX_HOME": sandbox.RuntimeConfigDir},
		StripEnvPrefixes: []string{"GT_DOLT_", "BD_", "BEADS_", "DOLT_"},
		Beacon:           session.BeaconConfig{Recipient: "isolated hook chooser probe", Sender: "self", Topic: "probe"},
		Instructions:     "Wait for the probe to finish.",
		WaitForAgent:     true, WaitFatal: true,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	tmuxValue := func(args ...string) (string, error) {
		cmdArgs := append([]string{"-L", sandbox.Socket}, args...)
		out, err := exec.Command("tmux", cmdArgs...).Output()
		return strings.TrimSpace(string(out)), err
	}
	capture := func() ([]byte, error) {
		return exec.Command("tmux", "-L", sandbox.Socket, "capture-pane", "-p", "-e", "-t", sandbox.Session, "-S", "-").Output()
	}
	type chooserState struct {
		SHA256              string `json:"sha256"`
		Dialog              bool   `json:"dialog"`
		ReviewSelected      bool   `json:"review_selected"`
		TrustSelected       bool   `json:"trust_selected"`
		TrustShortcut       bool   `json:"trust_shortcut"`
		Trusting            bool   `json:"trusting"`
		Busy                bool   `json:"busy"`
		ProgressAfterDialog bool   `json:"progress_after_dialog"`
	}
	hookReviewDialog := func(text string) bool {
		return strings.Contains(text, "Hooks need review") ||
			strings.Contains(strings.ToLower(text), "hooks need review before they can run") ||
			strings.Contains(strings.ToLower(text), "press t to trust all; enter to review hooks; esc to close")
	}
	state := func(content []byte) chooserState {
		sum := sha256.Sum256(content)
		text := string(content)
		return chooserState{
			SHA256:              hex.EncodeToString(sum[:]),
			Dialog:              hookReviewDialog(text),
			ReviewSelected:      strings.Contains(text, "› 1. Review hooks"),
			TrustSelected:       strings.Contains(text, "› 2. Trust all and continue"),
			TrustShortcut:       strings.Contains(strings.ToLower(text), "press t to trust all"),
			Trusting:            strings.Contains(text, "Trusting hooks"),
			Busy:                strings.Contains(text, "esc to interrupt"),
			ProgressAfterDialog: hookChooserProgressAfterDialog(text),
		}
	}

	startupDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(startupDeadline) {
		content, captureErr := capture()
		if captureErr == nil {
			text := string(content)
			if strings.Contains(text, "Do you trust the contents of this directory?") {
				if err := sandbox.tmux.AcceptWorkspaceTrustDialog(sandbox.Session); err != nil {
					t.Fatalf("accept standard workspace trust: %v", err)
				}
				break
			}
			if hookReviewDialog(text) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	var before []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		before, err = capture()
		text := string(before)
		legacyReady := strings.Contains(text, "Hooks need review") &&
			strings.Contains(text, "Review hooks") && strings.Contains(text, "Trust all and continue")
		currentReady := strings.Contains(strings.ToLower(text), "hooks need review before they can run") &&
			strings.Contains(strings.ToLower(text), "press t to trust all")
		if err == nil && (legacyReady || currentReady) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	beforeState := state(before)
	if !beforeState.Dialog {
		if err := os.MkdirAll(filepath.Dir(evidencePath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(evidencePath+".pane", before, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(evidencePath+".pane", 0600); err != nil {
			t.Fatal(err)
		}
		t.Fatal("isolated Codex hook chooser did not render")
	}

	const paneFormat = "#{session_name}|#{window_id}|#{window_index}|#{pane_id}|#{pane_index}|#{pane_active}|#{pane_in_mode}|#{pane_current_command}"
	socketPath, socketErr := tmuxValue("display-message", "-p", "#{socket_path}")
	resolvedBefore, resolveBeforeErr := tmuxValue("display-message", "-p", "-t", sandbox.Session, paneFormat)
	panesBefore, panesBeforeErr := tmuxValue("list-panes", "-t", sandbox.Session, "-F", paneFormat)
	clientsBefore, clientsBeforeErr := tmuxValue("list-clients", "-F", "#{client_name}")

	helperErr := sandbox.tmux.AcceptWorkspaceTrustDialog(sandbox.Session)
	after, captureAfterErr := capture()
	time.Sleep(time.Second)
	afterOneSecond, captureAfterOneSecondErr := capture()
	resolvedAfter, resolveAfterErr := tmuxValue("display-message", "-p", "-t", sandbox.Session, paneFormat)
	panesAfter, panesAfterErr := tmuxValue("list-panes", "-t", sandbox.Session, "-F", paneFormat)

	report := struct {
		SocketLabel      string       `json:"socket_label"`
		SocketPath       string       `json:"socket_path"`
		Session          string       `json:"session"`
		ResolvedBefore   string       `json:"resolved_before"`
		ResolvedAfter    string       `json:"resolved_after"`
		PanesBefore      string       `json:"panes_before"`
		PanesAfter       string       `json:"panes_after"`
		Detached         bool         `json:"detached"`
		Before           chooserState `json:"before"`
		After            chooserState `json:"after"`
		AfterOneSecond   chooserState `json:"after_one_second"`
		SocketQueryOK    bool         `json:"socket_query_ok"`
		ResolveBeforeOK  bool         `json:"resolve_before_ok"`
		ResolveAfterOK   bool         `json:"resolve_after_ok"`
		PanesBeforeOK    bool         `json:"panes_before_ok"`
		PanesAfterOK     bool         `json:"panes_after_ok"`
		ClientQueryOK    bool         `json:"client_query_ok"`
		HelperOK         bool         `json:"helper_ok"`
		CaptureAfterOK   bool         `json:"capture_after_ok"`
		CaptureAfter1sOK bool         `json:"capture_after_one_second_ok"`
	}{
		SocketLabel: sandbox.Socket, SocketPath: socketPath, Session: sandbox.Session,
		ResolvedBefore: resolvedBefore, ResolvedAfter: resolvedAfter,
		PanesBefore: panesBefore, PanesAfter: panesAfter, Detached: clientsBefore == "",
		Before: beforeState, After: state(after), AfterOneSecond: state(afterOneSecond),
		SocketQueryOK: socketErr == nil, ResolveBeforeOK: resolveBeforeErr == nil, ResolveAfterOK: resolveAfterErr == nil,
		PanesBeforeOK: panesBeforeErr == nil, PanesAfterOK: panesAfterErr == nil, ClientQueryOK: clientsBeforeErr == nil,
		HelperOK: helperErr == nil, CaptureAfterOK: captureAfterErr == nil, CaptureAfter1sOK: captureAfterOneSecondErr == nil,
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(evidencePath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath+".pane", afterOneSecond, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(evidencePath+".pane", 0600); err != nil {
		t.Fatal(err)
	}

	if helperErr != nil {
		t.Fatalf("AcceptWorkspaceTrustDialog: %v", helperErr)
	}
	if resolvedBefore == "" || resolvedBefore != resolvedAfter || panesBefore != panesAfter {
		t.Fatalf("tmux chooser target changed; private metadata: %s", evidencePath)
	}
	afterState := state(afterOneSecond)
	if !hookChooserTransitionAccepted(beforeState.SHA256, afterState.SHA256, afterState.Dialog, afterState.ProgressAfterDialog) {
		t.Fatalf("tmux accepted the chooser command without proving the modal cleared or Codex resumed; private metadata: %s", evidencePath)
	}
}

func TestHookChooserTransitionAccepted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		before     string
		after      string
		dialog     bool
		progress   bool
		wantAccept bool
	}{
		{name: "missing capture", before: "", after: "after", dialog: false, wantAccept: false},
		{name: "unchanged modal", before: "same", after: "same", dialog: true, wantAccept: false},
		{name: "pane churn with modal", before: "before", after: "after", dialog: true, wantAccept: false},
		{name: "explicit post-modal progress", before: "before", after: "after", dialog: true, progress: true, wantAccept: true},
		{name: "modal cleared", before: "before", after: "after", dialog: false, wantAccept: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hookChooserTransitionAccepted(tt.before, tt.after, tt.dialog, tt.progress); got != tt.wantAccept {
				t.Fatalf("hookChooserTransitionAccepted() = %v, want %v", got, tt.wantAccept)
			}
		})
	}
}

func TestHookChooserProgressAfterDialog(t *testing.T) {
	t.Parallel()
	currentModal := "Hooks\n⚠ 3 hooks need review before they can run.\nPress t to trust all; enter to review hooks; esc to close"
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "historical busy before active current modal",
			content: "• Working (esc to interrupt)\ncompleted response\n" + currentModal,
			want:    false,
		},
		{
			name:    "trusting after current modal",
			content: currentModal + "\nTrusting hooks",
			want:    true,
		},
		{
			name:    "busy after legacy modal",
			content: "Hooks need review\n› 1. Review hooks\n2. Trust all and continue\n• Working (esc to interrupt)",
			want:    true,
		},
		{name: "activity without modal", content: "• Working (esc to interrupt)", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hookChooserProgressAfterDialog(tt.content); got != tt.want {
				t.Fatalf("hookChooserProgressAfterDialog() = %v, want %v", got, tt.want)
			}
		})
	}
}
