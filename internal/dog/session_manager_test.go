package dog

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestDogStartCapturesAndPersistsSessionGeneration(t *testing.T) {
	mgr, _ := newDogStateManager(t, "alpha", "work")
	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-generation-%d", os.Getpid()))
	t.Cleanup(func() { _ = tm.KillServer() })
	sm := NewSessionManager(tm, mgr.townRoot, mgr)
	want := testDogTmuxGeneration("$11", "nonce-started")
	started := 0
	killed := 0
	sm.startSession = func(_ *tmux.Tmux, _ session.SessionConfig) (*session.StartResult, error) {
		started++
		return &session.StartResult{}, nil
	}
	sm.captureSessionGeneration = func(name string) (tmux.SessionGeneration, error) {
		if name != want.Name {
			t.Fatalf("capture name = %q, want %q", name, want.Name)
		}
		return want, nil
	}
	sm.killSessionGeneration = func(generation tmux.SessionGeneration) error {
		killed++
		return nil
	}

	if err := sm.Start("alpha", SessionStartOptions{WorkDesc: "work"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started != 1 || killed != 0 {
		t.Fatalf("started=%d killed=%d, want 1 and 0", started, killed)
	}
	dog, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.SessionGeneration == nil || !dog.SessionGeneration.EqualTmux(want) {
		t.Fatalf("persisted generation = %+v, want %+v", dog.SessionGeneration, want)
	}
}

func TestDogStartPersistenceFailureKillsOnlyCapturedGeneration(t *testing.T) {
	mgr, _ := newDogStateManager(t, "alpha", "work")
	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-generation-fail-%d", os.Getpid()))
	t.Cleanup(func() { _ = tm.KillServer() })
	sm := NewSessionManager(tm, mgr.townRoot, mgr)
	want := testDogTmuxGeneration("$12", "nonce-persist-fail")
	persistErr := errors.New("persist failed")
	var killed []tmux.SessionGeneration
	sm.startSession = func(_ *tmux.Tmux, _ session.SessionConfig) (*session.StartResult, error) {
		return &session.StartResult{}, nil
	}
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) {
		return want, nil
	}
	sm.persistSessionGeneration = func(string, tmux.SessionGeneration) error {
		return persistErr
	}
	sm.killSessionGeneration = func(generation tmux.SessionGeneration) error {
		killed = append(killed, generation)
		return nil
	}

	err := sm.Start("alpha", SessionStartOptions{WorkDesc: "work"})
	if !errors.Is(err, persistErr) {
		t.Fatalf("Start error = %v, want persistence failure", err)
	}
	if len(killed) != 1 || !killed[0].Equal(want) {
		t.Fatalf("killed generations = %+v, want exact captured generation", killed)
	}
}

func TestDogStartCaptureFailurePreservesUnprovenSession(t *testing.T) {
	mgr, _ := newDogStateManager(t, "alpha", "work")
	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-generation-capture-%d", os.Getpid()))
	t.Cleanup(func() { _ = tm.KillServer() })
	sm := NewSessionManager(tm, mgr.townRoot, mgr)
	captureErr := errors.New("capture failed")
	killed := 0
	sm.startSession = func(_ *tmux.Tmux, _ session.SessionConfig) (*session.StartResult, error) {
		return &session.StartResult{}, nil
	}
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) {
		return tmux.SessionGeneration{}, captureErr
	}
	sm.killSessionGeneration = func(tmux.SessionGeneration) error {
		killed++
		return nil
	}

	err := sm.Start("alpha", SessionStartOptions{})
	if !errors.Is(err, captureErr) || !strings.Contains(err.Error(), "capture") {
		t.Fatalf("Start error = %v, want capture failure", err)
	}
	if killed != 0 {
		t.Fatalf("unproven session received %d destructive kill(s)", killed)
	}
}
