package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/nudge"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestPollerEnvironmentSurvivesCLIRegistryInitialization(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	socket := fmt.Sprintf("gt-test-poller-registry-%d", time.Now().UnixNano())
	transport := tmux.NewTmuxWithSocketAndEnv(socket, []string{"PATH=" + os.Getenv("PATH")})
	sessionName := "hq-mayor"
	if err := transport.NewSessionWithCommand(sessionName, t.TempDir(), "sleep 60"); err != nil {
		t.Fatalf("create isolated poller target: %v", err)
	}
	socketPath, err := exec.Command("tmux", "-L", socket, "display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		t.Fatalf("resolve isolated poller socket: %v", err)
	}
	oldSocket := tmux.GetDefaultSocket()
	t.Cleanup(func() {
		tmux.SetDefaultSocket(oldSocket)
		_ = transport.KillServer()
		if path := strings.TrimSpace(string(socketPath)); filepath.IsAbs(path) {
			_ = os.Remove(path)
		}
	})

	t.Setenv("GT_TOWN_SOCKET", "")
	t.Setenv("GT_TMUX_SOCKET", "")
	for _, entry := range transport.PollerEnvironment() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && (name == "GT_TOWN_SOCKET" || name == "GT_TMUX_SOCKET") {
			t.Setenv(name, value)
		}
	}
	if err := session.InitRegistry(t.TempDir()); err != nil {
		t.Fatalf("initialize CLI registry from poller environment: %v", err)
	}

	exists, err := tmux.NewTmux().HasSession(sessionName)
	if err != nil || !exists {
		t.Fatalf("poller target after CLI registry initialization: exists=%t err=%v", exists, err)
	}
}

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

func TestCooperativeStopWatcherBeatsShutdownDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var requested atomic.Bool
	started := time.Now()
	stopped := watchCooperativePollerStop(ctx, 10*time.Millisecond, requested.Load)
	time.AfterFunc(30*time.Millisecond, func() { requested.Store(true) })

	select {
	case <-stopped:
		if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
			t.Fatalf("cooperative stop observed after %s", elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cooperative stop watcher missed the bounded stop request")
	}
}

func TestPollerHasSessionRetriesFalseError(t *testing.T) {
	var calls int
	got, err := pollerHasSessionWith(func() (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("transient tmux error")
		}
		return true, nil
	})
	if err != nil || !got || calls != 2 {
		t.Fatalf("result=%v err=%v calls=%d", got, err, calls)
	}
}

func TestPollerCancellationWhileLeaseIsBusyDoesNotClaimQueue(t *testing.T) {
	townRoot := t.TempDir()
	const sessionName = "gt-test-lease-busy"
	owner := tmux.NewTmuxWithSocket("lease-owner")
	releaseOwner, err := owner.AcquireNudgeLease(townRoot, sessionName)
	if err != nil {
		t.Fatalf("owner lease: %v", err)
	}
	defer releaseOwner()

	waiter := tmux.NewTmuxWithSocket("lease-waiter")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	claims := 0
	claim, release, err := claimPollerNudgeWhenIdleContext(
		ctx,
		false,
		func(context.Context) error { return nil },
		func(ctx context.Context) (func(), error) {
			return waiter.AcquireNudgeLeaseContext(ctx, townRoot, sessionName)
		},
		func() (*nudge.ClaimedNudge, error) {
			claims++
			return nil, nil
		},
	)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) || claim != nil || claims != 0 {
		t.Fatalf("canceled lease cycle = claim %#v release %v err %v claims %d", claim, release != nil, err, claims)
	}
}

func TestBusyPollerStopsBeforeReplacementWithoutClaimingQueue(t *testing.T) {
	socket := fmt.Sprintf("gt-test-busy-poller-stop-%d", time.Now().UnixNano())
	transport := tmux.NewTmuxWithSocket(socket)
	t.Cleanup(func() { _ = transport.KillServer() })

	townRoot := t.TempDir()
	const sessionName = "gt-test-busy-poller"
	if err := transport.NewSessionWithCommandAndEnv(sessionName, townRoot, "sleep 60", map[string]string{
		"GT_AGENT":               "codex",
		"GT_READY_PROMPT_PREFIX": "› ",
	}); err != nil {
		t.Fatalf("create busy session: %v", err)
	}
	queued := nudge.QueuedNudge{
		DeliveryID:      "ndg-busy-stop",
		Sender:          "mayor",
		Message:         "must remain queued",
		Priority:        nudge.PriorityUrgent,
		DurableUntilAck: true,
	}
	if err := nudge.Enqueue(townRoot, sessionName, queued); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	pidDir := filepath.Join(townRoot, ".runtime", "nudge_poller")
	if err := os.MkdirAll(pidDir, 0700); err != nil {
		t.Fatal(err)
	}
	pidPath := filepath.Join(pidDir, sessionName+".pid")
	generation := []byte("busy-poller-generation")
	if err := os.WriteFile(pidPath, generation, 0600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- runNudgePollerLoop(context.Background(), townRoot, sessionName, transport, 10*time.Millisecond, 10*time.Second)
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("busy poller exited before stop: %v", err)
	default:
	}

	kills := 0
	err := polecat.StopPollerBeforeReplacement(func() error {
		if err := os.WriteFile(pidPath+".stop", generation, 0600); err != nil {
			return err
		}
		select {
		case err := <-done:
			return err
		case <-time.After(time.Second):
			return errors.New("busy poller did not stop within replacement bound")
		}
	}, func() error {
		kills++
		return transport.KillSession(sessionName)
	})
	if err != nil {
		t.Fatalf("busy replacement: %v", err)
	}
	if kills != 1 {
		t.Fatalf("replacement kills = %d, want 1", kills)
	}
	if pending, err := nudge.Pending(townRoot, sessionName); err != nil || pending != 1 {
		t.Fatalf("pending after busy stop = %d, %v; want one untouched record", pending, err)
	}
}
