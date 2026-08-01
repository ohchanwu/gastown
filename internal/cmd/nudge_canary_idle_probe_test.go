package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

func TestProbeIsolatedCodexIdlePane(t *testing.T) {
	if os.Getenv("GT_RUN_IDLE_PANE_PROBE") != "1" {
		t.Skip("set GT_RUN_IDLE_PANE_PROBE=1 for the private isolated probe")
	}
	evidencePath := os.Getenv("GT_IDLE_PANE_EVIDENCE")
	if !filepath.IsAbs(evidencePath) {
		t.Fatal("GT_IDLE_PANE_EVIDENCE must be an absolute path")
	}

	sandbox, err := newWakeCanarySandbox("")
	if err != nil {
		t.Fatalf("newWakeCanarySandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Cleanup() })
	if err := sandbox.linkCodexAuth(); err != nil {
		t.Fatalf("linkCodexAuth: %v", err)
	}

	const probeToken = "IDLE_PANE_PROBE_COMPLETE_7F2A"
	if _, err := session.StartSession(sandbox.tmux, session.SessionConfig{
		SessionID: sandbox.Session, WorkDir: sandbox.WorkDir, Role: "mayor",
		TownRoot: sandbox.TownRoot, AgentOverride: "codex", RuntimeConfigDir: sandbox.RuntimeConfigDir,
		ExtraEnv:         map[string]string{"GT_TOWN_ROOT": sandbox.TownRoot, "CODEX_HOME": sandbox.RuntimeConfigDir},
		StripEnvPrefixes: []string{"GT_DOLT_", "BD_", "BEADS_", "DOLT_"},
		Beacon:           session.BeaconConfig{Recipient: "isolated idle-pane probe", Sender: "self", Topic: "probe"},
		Instructions:     "Reply with exactly " + probeToken + " and then wait.",
		WaitForAgent:     true, WaitFatal: true, AcceptBypass: true, ReadyDelay: true, VerifySurvived: true,
	}); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	capture := func() ([]byte, error) {
		return exec.Command("tmux", "-L", sandbox.Socket, "capture-pane", "-p", "-e", "-t", sandbox.Session, "-S", "-").Output()
	}
	baseline, err := capture()
	if err != nil {
		t.Fatalf("capture baseline: %v", err)
	}
	baselineTokenCount := strings.Count(string(baseline), probeToken)
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

	ticker := time.NewTicker(200 * time.Millisecond)
	t.Cleanup(ticker.Stop)
	timeout := time.NewTimer(180 * time.Second)
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
			styled := string(content)
			busy := strings.Contains(styled, "esc to interrupt")
			if !busy && strings.Count(styled, probeToken) > baselineTokenCount {
				stable++
			} else {
				stable = 0
			}
			if stable < 2 {
				continue
			}
			writeEvidence(content)
			info, err := os.Stat(evidencePath)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("private styled pane captured: path=%s bytes=%d mode=%o", evidencePath, info.Size(), info.Mode().Perm())
			return
		case <-timeout.C:
			if len(lastCapture) > 0 {
				writeEvidence(lastCapture)
			}
			t.Fatal("isolated Codex probe did not reach stable completed output")
		}
	}
}
