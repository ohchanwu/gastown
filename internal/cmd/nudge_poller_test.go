package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestPollerCustomPromptBusyDoesNotClaimQueue(t *testing.T) {
	socket := fmt.Sprintf("gt-test-poller-prompt-%d", time.Now().UnixNano())
	transport := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() {
		socketPath, _ := exec.Command("tmux", "-L", socket, "display-message", "-p", "#{socket_path}").Output()
		_ = transport.KillServer()
		if path := strings.TrimSpace(string(socketPath)); filepath.IsAbs(path) {
			_ = os.Remove(path)
		}
	})
	sessionName := "gt-test-custom-codex-mayor"
	if err := transport.NewSessionWithCommand(sessionName, t.TempDir(), "sleep 60"); err != nil {
		if _, lookupErr := exec.LookPath("tmux"); lookupErr != nil {
			t.Skip("tmux unavailable")
		}
		t.Fatalf("NewSessionWithCommand: %v", err)
	}
	if err := transport.SetEnvironment(sessionName, "GT_AGENT", "custom-codex-mayor"); err != nil {
		t.Fatalf("SetEnvironment GT_AGENT: %v", err)
	}
	if err := transport.SetEnvironment(sessionName, "GT_READY_PROMPT_PREFIX", "› "); err != nil {
		t.Fatalf("SetEnvironment GT_READY_PROMPT_PREFIX: %v", err)
	}

	hasPrompt, _ := resolvePollerSessionMetadata(transport, sessionName)
	if !hasPrompt {
		t.Fatal("poller ignored resolved session prompt metadata")
	}

	claimed := false
	claim, err := claimPollerNudgeWhenIdle(
		hasPrompt,
		func() error { return errors.New("session busy") },
		func() (*nudge.ClaimedNudge, error) {
			claimed = true
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if claim != nil || claimed {
		t.Fatal("custom-prompt busy cycle claimed the queue")
	}
}

func TestShouldSkipDrainUntilIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		hasPromptDetection bool
		waitErr            error
		want               bool
	}{
		{"prompt aware idle", true, nil, false},
		{"prompt aware busy", true, errors.New("timeout"), true},
		{"no prompt detection busy", false, errors.New("timeout"), false},
		{"no prompt detection idle", false, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDrainUntilIdle(tt.hasPromptDetection, tt.waitErr); got != tt.want {
				t.Errorf("shouldSkipDrainUntilIdle(%v, %v) = %v, want %v", tt.hasPromptDetection, tt.waitErr, got, tt.want)
			}
		})
	}
}
