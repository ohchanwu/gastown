package dog

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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
		return &session.StartResult{SessionGeneration: want}, nil
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
		return &session.StartResult{SessionGeneration: want}, nil
	}
	sm.persistSessionGeneration = func(string, string, time.Time, *tmux.SessionGeneration, tmux.SessionGeneration) (bool, error) {
		return false, persistErr
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

func TestDogStartMissingCreationReceiptFailsClosed(t *testing.T) {
	mgr, _ := newDogStateManager(t, "alpha", "work")
	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-generation-capture-%d", os.Getpid()))
	t.Cleanup(func() { _ = tm.KillServer() })
	sm := NewSessionManager(tm, mgr.townRoot, mgr)
	killed := 0
	sm.startSession = func(_ *tmux.Tmux, _ session.SessionConfig) (*session.StartResult, error) {
		return &session.StartResult{}, nil
	}
	sm.killSessionGeneration = func(tmux.SessionGeneration) error {
		killed++
		return nil
	}

	err := sm.Start("alpha", SessionStartOptions{})
	if !errors.Is(err, ErrSessionStartCleanupIncomplete) || !strings.Contains(err.Error(), "creation receipt") {
		t.Fatalf("Start error = %v, want missing-receipt cleanup blocker", err)
	}
	if killed != 0 {
		t.Fatalf("unproven session received %d destructive kill(s)", killed)
	}
}

func TestDogStartPreservesAssignmentWhenLowerLifecycleCleanupIsUnreconciled(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work")
	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-generation-unreconciled-%d", os.Getpid()))
	t.Cleanup(func() { _ = tm.KillServer() })
	sm := NewSessionManager(tm, mgr.townRoot, mgr)
	sm.startSession = func(_ *tmux.Tmux, _ session.SessionConfig) (*session.StartResult, error) {
		return nil, errors.Join(errors.New("startup failed"), tmux.ErrSessionCleanupUnreconciled)
	}

	err := sm.Start("alpha", SessionStartOptions{WorkDesc: "work"})
	if !errors.Is(err, ErrSessionStartCleanupIncomplete) ||
		!errors.Is(err, tmux.ErrSessionCleanupUnreconciled) {
		t.Fatalf("Start error = %v, want cleanup-incomplete assignment hold", err)
	}
	after, getErr := mgr.Get("alpha")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.State != StateWorking || after.Work != initial.Work ||
		!after.WorkStartedAt.Equal(initial.WorkStartedAt) {
		t.Fatalf("unreconciled startup mutated assignment: %+v", after)
	}
}

func TestDogStopClearsOnlyAfterExactGenerationTeardown(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work")
	generation := testDogTmuxGeneration("$20", "nonce-stop")
	set, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		nil,
		generation,
	)
	if err != nil || !set {
		t.Fatalf("persist generation = %v, %v", set, err)
	}
	sm := NewSessionManager(tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-stop-%d", os.Getpid())), mgr.townRoot, mgr)
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) { return generation, nil }
	killed := 0
	sm.killSessionGeneration = func(got tmux.SessionGeneration) error {
		killed++
		if !got.Equal(generation) {
			t.Fatalf("killed generation = %+v, want %+v", got, generation)
		}
		return nil
	}

	if err := sm.Stop("alpha", true); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if killed != 1 {
		t.Fatalf("exact teardown calls = %d, want 1", killed)
	}
	stored, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != StateIdle || stored.Work != "" || stored.SessionGeneration != nil {
		t.Fatalf("stopped dog = %+v, want idle and custody-free", stored)
	}
}

func TestDogStopTeardownFailurePreservesAssignment(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work")
	generation := testDogTmuxGeneration("$21", "nonce-stop-fail")
	set, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		nil,
		generation,
	)
	if err != nil || !set {
		t.Fatalf("persist generation = %v, %v", set, err)
	}
	sm := NewSessionManager(tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-stop-fail-%d", os.Getpid())), mgr.townRoot, mgr)
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) { return generation, nil }
	teardownErr := errors.New("exact kill failed")
	sm.killSessionGeneration = func(tmux.SessionGeneration) error { return teardownErr }

	if err := sm.Stop("alpha", true); !errors.Is(err, teardownErr) {
		t.Fatalf("Stop error = %v, want teardown failure", err)
	}
	stored, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != StateWorking || stored.Work != initial.Work ||
		stored.SessionGeneration == nil || !stored.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("failed stop mutated custody: %+v", stored)
	}
}

func TestDogStopIfMatchesRejectsSameNameReplacement(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work-old")
	oldGeneration := testDogTmuxGeneration("$old", "nonce-old")
	set, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha", initial.Work, initial.WorkStartedAt, nil, oldGeneration,
	)
	if err != nil || !set {
		t.Fatalf("persist old generation = %v, %v", set, err)
	}
	oldSnapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}

	replacementGeneration := testDogTmuxGeneration("$replacement", "nonce-replacement")
	fl, err := mgr.lockDog("alpha")
	if err != nil {
		t.Fatal(err)
	}
	replacementState, err := mgr.loadState("alpha")
	if err != nil {
		_ = fl.Unlock()
		t.Fatal(err)
	}
	replacementState.Work = "work-replacement"
	replacementState.WorkStartedAt = initial.WorkStartedAt.Add(time.Second)
	replacementState.SessionGeneration = SessionGenerationFromTmux(replacementGeneration)
	if err := mgr.saveState("alpha", replacementState); err != nil {
		_ = fl.Unlock()
		t.Fatal(err)
	}
	_ = fl.Unlock()

	sm := NewSessionManager(tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-stop-replacement-%d", os.Getpid())), mgr.townRoot, mgr)
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) { return replacementGeneration, nil }
	killed := 0
	sm.killSessionGeneration = func(tmux.SessionGeneration) error { killed++; return nil }

	if err := sm.StopIfMatches(oldSnapshot, true); !errors.Is(err, tmux.ErrSessionGenerationChanged) {
		t.Fatalf("StopIfMatches error = %v, want ErrSessionGenerationChanged", err)
	}
	if killed != 0 {
		t.Fatalf("same-name replacement received %d kill(s)", killed)
	}
	current, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.Work != replacementState.Work || current.SessionGeneration == nil || !current.SessionGeneration.EqualTmux(replacementGeneration) {
		t.Fatalf("replacement custody changed: %+v", current)
	}
}

func TestDogStopIfMatchesPreservesLiveLegacySession(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "legacy-work")
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	liveGeneration := testDogTmuxGeneration("$legacy", "nonce-legacy")
	sm := NewSessionManager(tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-stop-legacy-%d", os.Getpid())), mgr.townRoot, mgr)
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) { return liveGeneration, nil }
	killed := 0
	sm.killSessionGeneration = func(tmux.SessionGeneration) error { killed++; return nil }

	err = sm.StopIfMatches(snapshot, true)
	if err == nil || !strings.Contains(err.Error(), "live legacy dog session") {
		t.Fatalf("StopIfMatches error = %v, want live legacy preservation", err)
	}
	if killed != 0 {
		t.Fatalf("live legacy session received %d kill(s)", killed)
	}
	current, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != StateWorking || current.Work != initial.Work || current.SessionGeneration != nil {
		t.Fatalf("legacy custody changed: %+v", current)
	}
}
