package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

// writeFakeTmuxWithSession creates a fake tmux binary that reports the Deacon
// session as existing (has-session returns 0). Used for deacon idle guard tests
// where the session must be present so checkDeaconHeartbeat reaches the nudge path.
func writeFakeTmuxWithSession(t *testing.T, dir string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail

cmd=""
skip_next=0
for arg in "$@"; do
  if [[ "$skip_next" -eq 1 ]]; then
    skip_next=0
    continue
  fi
  if [[ "$arg" == "-u" ]]; then
    continue
  fi
  if [[ "$arg" == "-L" ]]; then
    skip_next=1
    continue
  fi
  cmd="$arg"
  break
done

if [[ -n "${TMUX_LOG:-}" ]]; then
  printf "%s %s\n" "$cmd" "$*" >> "$TMUX_LOG"
fi

if [[ "${1:-}" == "-V" ]]; then
  echo "tmux 3.3a"
  exit 0
fi

# Session exists: has-session returns 0 so the nudge path is reachable.
if [[ "$cmd" == "has-session" ]]; then
  exit 0
fi

# WaitForCommand treats shell commands as not ready and retries for minutes.
if [[ "$cmd" == "display-message" && "$*" == *"pane_current_command"* ]]; then
  echo "claude"
  exit 0
fi

exit 0
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
}

func assertNoNudgePollerLeak(t *testing.T, townRoot, sessionName string) {
	t.Helper()
	pidPath := filepath.Join(townRoot, ".runtime", "nudge_poller", sessionName+".pid")
	stopPath := filepath.Join(townRoot, ".runtime", "nudge_poller", sessionName+".pid.stop")
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			t.Errorf("read nudge poller pid: %v", err)
			return
		}
		pid, err := testNudgePollerPID(data)
		if err != nil {
			t.Errorf("parse nudge poller pid: %v", err)
			return
		}
		running, matches := testNudgePollerIdentity(pid, sessionName)
		if !running {
			return
		}
		if !matches {
			t.Errorf("refusing to stop pid %d: process identity does not match test nudge poller", pid)
			return
		}
		// The test binary is intentionally named daemon.test rather than gt, so
		// production StopPoller must reject it. Publish the same byte-exact,
		// generation-bound cooperative request directly for test-owned cleanup.
		if err := os.WriteFile(stopPath, data, 0o600); err != nil {
			t.Errorf("publish test nudge poller stop request %d: %v", pid, err)
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			running, matches = testNudgePollerIdentity(pid, sessionName)
			if !running || !matches {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		running, matches = testNudgePollerIdentity(pid, sessionName)
		if running && matches {
			t.Errorf("nudge poller %d still running after cooperative cleanup", pid)
			return
		}
		latest, err := os.ReadFile(pidPath)
		if err == nil {
			if !bytes.Equal(latest, data) {
				t.Errorf("nudge poller ownership changed during cleanup; preserving replacement")
				return
			}
			if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove stopped nudge poller ownership: %v", err)
			}
		} else if !os.IsNotExist(err) {
			t.Errorf("reread stopped nudge poller ownership: %v", err)
		}
		stopData, err := os.ReadFile(stopPath)
		if err == nil && bytes.Equal(stopData, data) {
			if err := os.Remove(stopPath); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove test nudge poller stop request: %v", err)
			}
		} else if err != nil && !os.IsNotExist(err) {
			t.Errorf("reread test nudge poller stop request: %v", err)
		}
	})
}

func testNudgePollerPID(data []byte) (int, error) {
	trimmed := strings.TrimSpace(string(data))
	if !strings.HasPrefix(trimmed, "{") {
		return strconv.Atoi(trimmed)
	}
	var record struct {
		PID int
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return 0, err
	}
	if record.PID <= 0 {
		return 0, fmt.Errorf("invalid structured poller PID %d", record.PID)
	}
	return record.PID, nil
}

func testNudgePollerIdentity(pid int, sessionName string) (running, matches bool) {
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return false, false
	}
	argv := strings.Fields(string(out))
	matches = len(argv) == 3 &&
		filepath.Base(argv[0]) == "daemon.test" &&
		argv[1] == "nudge-poller" &&
		argv[2] == sessionName
	return true, matches
}

// TestCheckDeaconHeartbeat_IdleGuard verifies that the nudge is suppressed when
// the Deacon heartbeat is stale but no active work is in flight (idle guard).
func TestCheckDeaconHeartbeat_IdleGuard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	tests := []struct {
		name             string
		heartbeatAge     time.Duration
		stores           map[string]beadsdk.Storage
		wantNudgeLog     bool
		wantIdleGuardLog bool
		desc             string
	}{
		{
			name:         "idle: stale heartbeat, no work — nudge suppressed",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantNudgeLog:     false,
			wantIdleGuardLog: true,
			desc:             "Idle guard must suppress nudge when no work is in flight",
		},
		{
			name:         "active work: stale heartbeat, in_progress bead — nudge sent",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"in_progress": {{ID: "sc-abc"}},
				}},
			},
			wantNudgeLog:     true,
			wantIdleGuardLog: false,
			desc:             "Nudge must fire when in_progress work exists",
		},
		{
			name:         "hooked only: stale heartbeat, patrol wisp — nudge suppressed",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"hooked": {{ID: "hq-wisp-34zi"}},
				}},
			},
			wantNudgeLog:     false,
			wantIdleGuardLog: true,
			desc:             "Patrol wisps in hooked state do not count as active work; nudge must be suppressed",
		},
		{
			name:         "store error: stale heartbeat, store fails — nudge sent conservatively",
			heartbeatAge: 10 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{err: fmt.Errorf("db offline")},
			},
			wantNudgeLog:     true,
			wantIdleGuardLog: false,
			desc:             "Nudge must fire conservatively when work state is unknown",
		},
		{
			name:         "very stale: heartbeat >= 20 min — escalation path, no nudge",
			heartbeatAge: 21 * time.Minute,
			stores: map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{}},
			},
			wantNudgeLog:     false,
			wantIdleGuardLog: false,
			desc:             "Very stale heartbeat takes escalation path, not nudge path; idle guard not reached",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			t.Setenv("GT_TOWN_ROOT", townRoot)
			t.Setenv("GT_ROOT", townRoot)
			assertNoNudgePollerLeak(t, townRoot, "hq-deacon")
			fakeBinDir := t.TempDir()
			tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
			if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
				t.Fatalf("create tmux log: %v", err)
			}

			writeFakeTmuxWithSession(t, fakeBinDir)
			t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMUX_LOG", tmuxLog)

			writeDeaconHeartbeat(t, townRoot, tc.heartbeatAge)

			d := newTestDaemonWithStores(t, townRoot, tc.stores)

			logBuf := &strings.Builder{}
			d.logger = log.New(logBuf, "", 0)

			d.checkDeaconHeartbeat()

			logOutput := logBuf.String()

			hasIdleGuardLog := strings.Contains(logOutput, "nudge skipped")
			if hasIdleGuardLog != tc.wantIdleGuardLog {
				t.Errorf("%s\nidle guard log present=%v, want=%v\nlog:\n%s",
					tc.desc, hasIdleGuardLog, tc.wantIdleGuardLog, logOutput)
			}

			hasNudgeLog := strings.Contains(logOutput, "nudging session")
			if hasNudgeLog != tc.wantNudgeLog {
				t.Errorf("%s\nnudge log present=%v, want=%v\nlog:\n%s",
					tc.desc, hasNudgeLog, tc.wantNudgeLog, logOutput)
			}
		})
	}
}
