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

	"github.com/steveyegge/gastown/internal/tmux"
)

// mockSessionChecker implements sessionChecker for testing.
type mockSessionChecker struct {
	healthResults  map[string]tmux.ZombieStatus // session -> status
	sessionsAlive  map[string]bool              // session -> exists
	generations    map[string]tmux.SessionGeneration
	killedSessions []string
}

func newMockChecker() *mockSessionChecker {
	return &mockSessionChecker{
		healthResults: make(map[string]tmux.ZombieStatus),
		sessionsAlive: make(map[string]bool),
		generations:   make(map[string]tmux.SessionGeneration),
	}
}

func newMockHealthChecker(mgr *Manager, checker *mockSessionChecker) *HealthChecker {
	hc := NewHealthChecker(mgr, checker)
	hc.checkerForGeneration = func(tmux.SessionGeneration) (sessionChecker, error) {
		return checker, nil
	}
	return hc
}

func (m *mockSessionChecker) CheckSessionHealth(session string, _ time.Duration) tmux.ZombieStatus {
	if s, ok := m.healthResults[session]; ok {
		return s
	}
	return tmux.SessionDead
}

func (m *mockSessionChecker) HasSession(name string) (bool, error) {
	return m.sessionsAlive[name], nil
}

func (m *mockSessionChecker) CaptureSessionGeneration(name string) (tmux.SessionGeneration, error) {
	return m.generations[name], nil
}

func (m *mockSessionChecker) KillSessionGenerationWithProcessesPortableContext(_ context.Context, generation tmux.SessionGeneration) error {
	m.killedSessions = append(m.killedSessions, generation.Name)
	return nil
}

func healthTestGeneration() tmux.SessionGeneration {
	return tmux.SessionGeneration{
		Name:           "hq-dog-alpha",
		SessionID:      "$1",
		PaneID:         "%0",
		Nonce:          "health-generation",
		Custody:        "health-custody",
		ServerPID:      4242,
		ServerIdentity: "health-server",
		Transport: tmux.SessionTransport{
			Bound: true, SocketName: "health-socket", SocketPath: "/tmp/tmux-health/health-socket",
		},
	}
}

// =============================================================================
// Healthy dogs
// =============================================================================

func TestHealth_IdleLegacyDogRequiresRecovery(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	setupLegacyDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateIdle, LastActive: now,
		CreatedAt: now, UpdatedAt: now,
	})

	mc := newMockChecker()
	hc := NewHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, false)

	if !r.NeedsAttention {
		t.Error("idle legacy dog without durable generation should need recovery")
	}
	if r.SessionStatus != "unknown" {
		t.Errorf("session_status = %q, want 'unknown'", r.SessionStatus)
	}
	if r.WorkDuration != 0 {
		t.Errorf("work_duration = %v, want 0", r.WorkDuration)
	}
}

func TestHealth_WorkingDog_Healthy(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	workStart := now.Add(-10 * time.Minute)
	generation := healthTestGeneration()
	setupLegacyDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		WorkStartedAt: workStart, LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.SessionHealthy
	hc := newMockHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, false)

	if r.NeedsAttention {
		t.Error("healthy working dog should not need attention")
	}
	if r.SessionStatus != "healthy" {
		t.Errorf("session_status = %q, want 'healthy'", r.SessionStatus)
	}
	if r.WorkDuration < 10*time.Minute {
		t.Errorf("work_duration = %v, want >= 10m", r.WorkDuration)
	}
}

// =============================================================================
// Zombies
// =============================================================================

func TestHealth_Zombie_SessionDead(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	setupLegacyDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		WorkStartedAt: now.Add(-1 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now,
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.SessionDead
	hc := NewHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, false)

	if !r.NeedsAttention {
		t.Error("zombie (SessionDead) should need attention")
	}
	if r.AutoCleared {
		t.Error("should not auto-clear when autoClear=false")
	}
}

func TestHealth_Zombie_AgentDead(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		WorkStartedAt: now.Add(-1 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now,
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.AgentDead
	hc := NewHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, false)

	if !r.NeedsAttention {
		t.Error("zombie (AgentDead) should need attention")
	}
	if r.AutoCleared {
		t.Error("should not auto-clear when autoClear=false")
	}
}

// =============================================================================
// Hung
// =============================================================================

func TestHealth_Hung_ReportOnly(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	generation := healthTestGeneration()
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		WorkStartedAt: now.Add(-2 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.AgentHung
	hc := newMockHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, false) // autoClear=false: report only

	if !r.NeedsAttention {
		t.Error("hung dog should need attention")
	}
	if r.AutoCleared {
		t.Error("hung dog should NOT be auto-cleared when autoClear=false")
	}
	if r.SessionStatus != "agent-hung" {
		t.Errorf("session_status = %q, want 'agent-hung'", r.SessionStatus)
	}
}

func TestHealth_Hung_AutoCleared(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	generation := healthTestGeneration()
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		WorkStartedAt: now.Add(-2 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.AgentHung
	mc.generations["hq-dog-alpha"] = generation
	hc := newMockHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, true) // autoClear=true: kill and reclaim

	if !r.NeedsAttention {
		t.Error("hung dog should need attention")
	}
	if !r.AutoCleared {
		t.Error("hung dog should be auto-cleared when autoClear=true")
	}
	if len(mc.killedSessions) != 1 || mc.killedSessions[0] != "hq-dog-alpha" {
		t.Errorf("killedSessions = %v, want [hq-dog-alpha]", mc.killedSessions)
	}

	// Verify state was cleared
	d2, _ := m.Get("alpha")
	if d2.State != StateIdle {
		t.Errorf("state = %q, want idle after auto-clear", d2.State)
	}
}

func TestHealthUsesPersistedEndpointAfterAmbientRootDrift(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Windows tmux workflows are unsupported; WSL runs the Linux path")
	}
	firstRoot, err := os.MkdirTemp("/tmp", "gt-dog-health-bound-a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(firstRoot) })
	secondRoot, err := os.MkdirTemp("/tmp", "gt-dog-health-bound-b-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(secondRoot) })
	socket := fmt.Sprintf("gt-dog-health-bound-%d", time.Now().UnixNano())
	target := tmux.NewTmuxWithSocketAndEnv(socket, []string{
		"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + firstRoot,
	})
	t.Cleanup(func() { _ = target.KillServer() })
	generation, err := target.NewSessionWithCommandAndEnvGeneration(
		"hq-dog-alpha", t.TempDir(), "sleep 30", map[string]string{"GT_PROCESS_NAMES": "sleep"},
	)
	if err != nil {
		t.Fatal(err)
	}

	mgr, _ := testManager(t)
	now := time.Now().UTC().Round(0)
	setupDogWithState(t, mgr, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-bound", WorkStartedAt: now,
		LastActive: now, CreatedAt: now, UpdatedAt: now,
		SessionGeneration: SessionGenerationFromTmux(generation),
	})
	ambient := tmux.NewTmuxWithSocketAndEnv(socket, []string{
		"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + secondRoot,
	})
	hc := NewHealthChecker(mgr, ambient)
	d, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}

	result := hc.Check(d, time.Hour, true)
	if result.SessionStatus != tmux.SessionHealthy.String() || result.NeedsAttention || result.AutoCleared {
		t.Fatalf("health through persisted endpoint = %+v, want healthy without clear", result)
	}
	if live, err := target.HasSession(generation.Name); err != nil || !live {
		t.Fatalf("persisted endpoint after health check: live=%v err=%v", live, err)
	}
	stored, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != StateWorking || stored.Work != "task-bound" || stored.SessionGeneration == nil {
		t.Fatalf("health check released live exact custody: %+v", stored)
	}
}

func TestHealthPreservesLegacyAssignmentWhenAmbientEndpointIsAbsent(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now().UTC().Round(0)
	setupLegacyDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-legacy", WorkStartedAt: now,
		LastActive: now, CreatedAt: now, UpdatedAt: now,
	})
	mc := newMockChecker()
	hc := NewHealthChecker(m, mc)
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}

	result := hc.Check(d, time.Hour, true)
	if result.SessionStatus != "unknown" || !result.NeedsAttention || result.AutoCleared {
		t.Fatalf("legacy health result = %+v, want recovery-required unknown state", result)
	}
	stored, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != StateWorking || stored.Work != "task-legacy" || stored.SessionGeneration != nil {
		t.Fatalf("ambient health absence released legacy custody: %+v", stored)
	}
}

func TestHealthClearExactDogRuntimeRejectsNameOnlyTransport(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now().UTC()
	generation := healthTestGeneration()
	generation.Transport = tmux.SessionTransport{Bound: true, SocketName: "legacy-alias"}
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-legacy", WorkStartedAt: now.Add(-time.Hour),
		LastActive: now, CreatedAt: now, UpdatedAt: now,
		SessionGeneration: SessionGenerationFromTmux(generation),
	})
	mc := newMockChecker()
	hc := NewHealthChecker(m, mc)
	d, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}

	err = hc.clearExactDogRuntime(d, false)
	if !errors.Is(err, tmux.ErrSessionTransportUnbound) {
		t.Fatalf("clearExactDogRuntime() error = %v, want unbound transport", err)
	}
	if len(mc.killedSessions) != 0 {
		t.Fatalf("unbound transport received cleanup: %v", mc.killedSessions)
	}
	current, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.State != StateWorking || current.Work != "task-legacy" || current.SessionGeneration == nil {
		t.Fatalf("unbound health cleanup changed custody: %+v", current)
	}
}

func TestHealthAutoClearPluginFinalizerFailurePreservesAssignment(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	generation := healthTestGeneration()
	state := &DogState{
		Name: "alpha", State: StateWorking, Work: "plugin:reaper",
		WorkStartedAt: now.Add(-2 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	}
	setupDogWithState(t, m, "alpha", state)
	wantErr := errors.New("mail archive unavailable")
	m.assignmentFinalizer = func(string, string, time.Time) error { return wantErr }

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.SessionDead
	hc := newMockHealthChecker(m, mc)
	d, _ := m.Get("alpha")
	result := hc.Check(d, 30*time.Minute, true)

	if result.AutoCleared {
		t.Fatal("health auto-clear released plugin assignment after archive failure")
	}
	if !strings.Contains(result.Recommendation, wantErr.Error()) {
		t.Fatalf("recommendation = %q, want archive failure", result.Recommendation)
	}
	got, err := m.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateWorking || got.Work != state.Work || !got.WorkStartedAt.Equal(state.WorkStartedAt) {
		t.Fatalf("health auto-clear released plugin assignment: %+v", got)
	}
	if replacement, err := m.AssignWorkIfIdle("alpha", "plugin:replacement"); replacement != nil || !errors.Is(err, ErrDogWorking) {
		t.Fatalf("replacement assignment = %+v, %v; want blocked", replacement, err)
	}
}

// =============================================================================
// Auto-clear zombies
// =============================================================================

func TestHealth_AutoClear_SessionDead(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	generation := healthTestGeneration()
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		WorkStartedAt: now.Add(-1 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.SessionDead
	hc := newMockHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, true)

	if !r.AutoCleared {
		t.Error("zombie (SessionDead) should be auto-cleared")
	}

	// Verify state was actually cleared
	d2, _ := m.Get("alpha")
	if d2.State != StateIdle {
		t.Errorf("state = %q, want idle after auto-clear", d2.State)
	}
	if d2.Work != "" {
		t.Errorf("work = %q, want empty after auto-clear", d2.Work)
	}
}

func TestHealth_AutoClear_AgentDead(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	generation := healthTestGeneration()
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		WorkStartedAt: now.Add(-1 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.AgentDead
	mc.generations["hq-dog-alpha"] = generation
	hc := newMockHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, true)

	if !r.AutoCleared {
		t.Error("zombie (AgentDead) should be auto-cleared")
	}

	// Verify session was killed
	if len(mc.killedSessions) != 1 || mc.killedSessions[0] != "hq-dog-alpha" {
		t.Errorf("killedSessions = %v, want [hq-dog-alpha]", mc.killedSessions)
	}

	// Verify state was cleared
	d2, _ := m.Get("alpha")
	if d2.State != StateIdle {
		t.Errorf("state = %q, want idle after auto-clear", d2.State)
	}
}

// =============================================================================
// Orphan sessions
// =============================================================================

func TestHealth_Orphan_IdleWithSession(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	generation := healthTestGeneration()
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateIdle, LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	})

	mc := newMockChecker()
	mc.sessionsAlive["hq-dog-alpha"] = true
	hc := newMockHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, false)

	if !r.NeedsAttention {
		t.Error("orphan session should need attention")
	}
	if r.SessionStatus != "orphan" {
		t.Errorf("session_status = %q, want 'orphan'", r.SessionStatus)
	}
}

func TestHealth_Orphan_AutoCleared(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	generation := healthTestGeneration()
	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateIdle, LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(generation),
	})

	mc := newMockChecker()
	mc.sessionsAlive["hq-dog-alpha"] = true
	mc.generations["hq-dog-alpha"] = generation
	hc := newMockHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, true) // autoClear=true: kill orphan session

	if !r.NeedsAttention {
		t.Error("orphan session should need attention")
	}
	if !r.AutoCleared {
		t.Error("orphan session should be auto-cleared when autoClear=true")
	}
	if len(mc.killedSessions) != 1 || mc.killedSessions[0] != "hq-dog-alpha" {
		t.Errorf("killedSessions = %v, want [hq-dog-alpha]", mc.killedSessions)
	}
}

// =============================================================================
// WorkDuration computation
// =============================================================================

func TestHealth_WorkDuration_ZeroStartedAt(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	// Working dog with zero WorkStartedAt (legacy state file)
	setupLegacyDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateWorking, Work: "task-1",
		LastActive: now, CreatedAt: now, UpdatedAt: now,
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-alpha"] = tmux.SessionHealthy
	hc := NewHealthChecker(m, mc)

	d, _ := m.Get("alpha")
	r := hc.Check(d, 30*time.Minute, false)

	if r.WorkDuration != 0 {
		t.Errorf("work_duration = %v, want 0 for zero WorkStartedAt", r.WorkDuration)
	}
}

// =============================================================================
// CheckAll
// =============================================================================

func TestHealth_CheckAll_MultipleDogs(t *testing.T) {
	m, _ := testManager(t)
	now := time.Now()
	alphaGeneration := healthTestGeneration()
	betaGeneration := healthTestGeneration()
	betaGeneration.Name = "hq-dog-beta"
	betaGeneration.SessionID = "$2"
	betaGeneration.Nonce = "health-generation-beta"

	setupDogWithState(t, m, "alpha", &DogState{
		Name: "alpha", State: StateIdle, LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(alphaGeneration),
	})
	setupDogWithState(t, m, "beta", &DogState{
		Name: "beta", State: StateWorking, Work: "task-1",
		WorkStartedAt: now.Add(-1 * time.Hour), LastActive: now,
		CreatedAt: now, UpdatedAt: now, SessionGeneration: SessionGenerationFromTmux(betaGeneration),
	})

	mc := newMockChecker()
	mc.healthResults["hq-dog-beta"] = tmux.SessionDead // zombie
	hc := newMockHealthChecker(m, mc)

	results, err := hc.CheckAll(30*time.Minute, false)
	if err != nil {
		t.Fatalf("CheckAll() error = %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("CheckAll() returned %d results, want 2", len(results))
	}

	attention := NeedsAttentionCount(results)
	if attention != 1 {
		t.Errorf("NeedsAttentionCount = %d, want 1", attention)
	}
}

// =============================================================================
// NeedsAttentionCount
// =============================================================================

func TestNeedsAttentionCount(t *testing.T) {
	results := []DogHealthResult{
		{Name: "a", NeedsAttention: false},
		{Name: "b", NeedsAttention: true},
		{Name: "c", NeedsAttention: true},
		{Name: "d", NeedsAttention: false},
	}

	if got := NeedsAttentionCount(results); got != 2 {
		t.Errorf("NeedsAttentionCount = %d, want 2", got)
	}

	if got := NeedsAttentionCount(nil); got != 0 {
		t.Errorf("NeedsAttentionCount(nil) = %d, want 0", got)
	}
}
