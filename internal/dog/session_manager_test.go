package dog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

type testSessionGenerationController struct {
	capture func(string) (tmux.SessionGeneration, error)
	kill    func(context.Context, tmux.SessionGeneration) error
}

func (c testSessionGenerationController) CaptureSessionGeneration(name string) (tmux.SessionGeneration, error) {
	return c.capture(name)
}

func (testSessionGenerationController) SendKeysRawGeneration(tmux.SessionGeneration, string) error {
	return nil
}

func (c testSessionGenerationController) KillSessionGenerationWithProcessesPortableContext(ctx context.Context, generation tmux.SessionGeneration) error {
	return c.kill(ctx, generation)
}

func useInjectedSessionGenerationController(sm *SessionManager) {
	sm.controllerForGeneration = func(tmux.SessionGeneration) (sessionGenerationController, error) {
		return testSessionGenerationController{
			capture: sm.captureSessionGeneration,
			kill:    sm.killSessionGeneration,
		}, nil
	}
}

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
	sm.killSessionGeneration = func(_ context.Context, generation tmux.SessionGeneration) error {
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

func TestDogStartHoldsLifecycleLockThroughGenerationPersistence(t *testing.T) {
	mgr, state := newDogStateManager(t, "alpha", "work")
	tm := tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-start-lock-%d", os.Getpid()))
	t.Cleanup(func() { _ = tm.KillServer() })
	sm := NewSessionManager(tm, mgr.townRoot, mgr)
	generation := testDogTmuxGeneration("$locked", "nonce-locked")
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	sm.startSession = func(_ *tmux.Tmux, _ session.SessionConfig) (*session.StartResult, error) {
		close(startEntered)
		<-releaseStart
		return &session.StartResult{SessionGeneration: generation}, nil
	}

	startDone := make(chan error, 1)
	go func() { startDone <- sm.Start("alpha", SessionStartOptions{WorkDesc: state.Work}) }()
	<-startEntered

	type clearResult struct {
		cleared bool
		err     error
	}
	clearDone := make(chan clearResult, 1)
	go func() {
		cleared, err := mgr.ClearWorkIfMatches("alpha", state.Work, state.WorkStartedAt)
		clearDone <- clearResult{cleared: cleared, err: err}
	}()
	select {
	case result := <-clearDone:
		t.Fatalf("closeout crossed in-progress session generation publication: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseStart)
	if err := <-startDone; err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := <-clearDone
	if result.err != nil || result.cleared {
		t.Fatalf("post-start stale closeout = %+v, want false and nil", result)
	}
	got, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionGeneration == nil || !got.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("published generation lost: %+v", got)
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
	sm.killSessionGeneration = func(_ context.Context, generation tmux.SessionGeneration) error {
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
	sm.killSessionGeneration = func(context.Context, tmux.SessionGeneration) error {
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
	sm.killSessionGeneration = func(_ context.Context, got tmux.SessionGeneration) error {
		killed++
		if !got.Equal(generation) {
			t.Fatalf("killed generation = %+v, want %+v", got, generation)
		}
		return nil
	}
	useInjectedSessionGenerationController(sm)

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
	teardownErr := errors.Join(tmux.ErrSessionCleanupUnreconciled, errors.New("detached descendant survived"))
	sm.killSessionGeneration = func(context.Context, tmux.SessionGeneration) error { return teardownErr }
	useInjectedSessionGenerationController(sm)

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
	sm.killSessionGeneration = func(context.Context, tmux.SessionGeneration) error { killed++; return nil }
	useInjectedSessionGenerationController(sm)

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

func TestDogStopIfMatchesUsesPersistedEndpointAfterAmbientRootDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	firstRoot, err := os.MkdirTemp("/tmp", "gt-dog-stop-bound-a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(firstRoot) })
	secondRoot, err := os.MkdirTemp("/tmp", "gt-dog-stop-bound-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(secondRoot) })
	socket := fmt.Sprintf("gt-dog-stop-bound-%d", time.Now().UnixNano())
	target := tmux.NewTmuxWithSocketAndEnv(socket, []string{
		"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + firstRoot,
	})
	t.Cleanup(func() { _ = target.KillServer() })
	generation, err := target.NewSessionWithCommandAndEnvGeneration(
		"hq-dog-alpha", t.TempDir(), "sleep 30", nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	mgr, initial := newDogStateManager(t, "alpha", "work")
	set, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha", initial.Work, initial.WorkStartedAt, nil, generation,
	)
	if err != nil || !set {
		t.Fatalf("persist generation = %v, %v", set, err)
	}
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	ambient := tmux.NewTmuxWithSocketAndEnv(socket, []string{
		"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + secondRoot,
	})
	sm := NewSessionManager(ambient, mgr.townRoot, mgr)

	if err := sm.StopIfMatches(snapshot, true); err != nil {
		t.Fatalf("StopIfMatches through persisted endpoint: %v", err)
	}
	if live, err := target.HasSession(generation.Name); err != nil || live {
		t.Fatalf("persisted endpoint after stop: live=%v err=%v", live, err)
	}
	stored, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != StateIdle || stored.Work != "" || stored.SessionGeneration != nil {
		t.Fatalf("stopped dog = %+v, want idle and custody-free", stored)
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
	sm.killSessionGeneration = func(context.Context, tmux.SessionGeneration) error { killed++; return nil }

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

func TestDogStopIfMatchesRejectsNameOnlyTransportAfterAmbientAbsence(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "legacy-transport-work")
	generation := testDogTmuxGeneration("$legacy-transport", "nonce-legacy-transport")
	generation.Transport = tmux.SessionTransport{Bound: true, SocketName: "legacy-alias"}
	set, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha", initial.Work, initial.WorkStartedAt, nil, generation,
	)
	if err != nil || !set {
		t.Fatalf("persist legacy transport = %v, %v", set, err)
	}
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}

	sm := NewSessionManager(tmux.NewTmuxWithSocket("wrong-ambient-socket"), mgr.townRoot, mgr)
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) {
		return tmux.SessionGeneration{}, tmux.ErrSessionNotFound
	}
	killed := 0
	sm.killSessionGeneration = func(context.Context, tmux.SessionGeneration) error { killed++; return nil }

	err = sm.StopIfMatches(snapshot, true)
	if !errors.Is(err, tmux.ErrSessionTransportUnbound) {
		t.Fatalf("StopIfMatches() error = %v, want unbound transport", err)
	}
	if killed != 0 {
		t.Fatalf("legacy name-only transport received %d cleanup call(s)", killed)
	}
	current, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != StateWorking || current.Work != initial.Work || current.SessionGeneration == nil {
		t.Fatalf("unbound transport custody changed: %+v", current)
	}
}

func TestDogStopIfMatchesPluginFinalizerFailurePreservesAssignment(t *testing.T) {
	mgr, state := newDogStateManager(t, "alpha", "plugin:reaper")
	wantErr := errors.New("mail archive unavailable")
	mgr.assignmentFinalizer = func(string, string, time.Time) error { return wantErr }
	snapshot, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	sm := NewSessionManager(tmux.NewTmuxWithSocket(fmt.Sprintf("gt-dog-stop-mail-%d", os.Getpid())), mgr.townRoot, mgr)
	sm.captureSessionGeneration = func(string) (tmux.SessionGeneration, error) {
		return tmux.SessionGeneration{}, tmux.ErrSessionNotFound
	}

	if err := sm.StopIfMatches(snapshot, true); !errors.Is(err, wantErr) {
		t.Fatalf("StopIfMatches() error = %v, want assignment finalizer failure", err)
	}
	got, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateWorking || got.Work != state.Work || !got.WorkStartedAt.Equal(state.WorkStartedAt) {
		t.Fatalf("StopIfMatches released plugin assignment after archive failure: %+v", got)
	}
	if replacement, err := mgr.AssignWorkIfIdle("alpha", "plugin:replacement"); replacement != nil || !errors.Is(err, ErrDogWorking) {
		t.Fatalf("replacement assignment = %+v, %v; want blocked", replacement, err)
	}
}
