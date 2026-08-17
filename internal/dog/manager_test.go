package dog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
)

// TestDogStateJSON verifies DogState JSON serialization.
func TestDogStateJSON(t *testing.T) {
	now := time.Now()
	state := &DogState{
		Name:       "alpha",
		State:      StateIdle,
		LastActive: now,
		Work:       "",
		Worktrees: map[string]string{
			"gastown": "/path/to/gastown",
			"beads":   "/path/to/beads",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Create temp file
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, ".dog.json")

	// Write and read back
	data, err := os.ReadFile(statePath)
	if err == nil {
		t.Logf("Data already exists: %s", data)
	}

	// Test state values
	if state.Name != "alpha" {
		t.Errorf("expected name 'alpha', got %q", state.Name)
	}
	if state.State != StateIdle {
		t.Errorf("expected state 'idle', got %q", state.State)
	}
	if len(state.Worktrees) != 2 {
		t.Errorf("expected 2 worktrees, got %d", len(state.Worktrees))
	}
}

func TestDogSessionGenerationJSONRoundTripAndLegacyCompatibility(t *testing.T) {
	legacy := []byte(`{"name":"alpha","state":"idle","last_active":"2026-08-17T00:00:00Z","created_at":"2026-08-17T00:00:00Z","updated_at":"2026-08-17T00:00:00Z"}`)
	var legacyState DogState
	if err := json.Unmarshal(legacy, &legacyState); err != nil {
		t.Fatalf("unmarshal legacy state: %v", err)
	}
	if legacyState.SessionGeneration != nil {
		t.Fatalf("legacy session generation = %+v, want nil", legacyState.SessionGeneration)
	}
	legacyRoundTrip, err := json.Marshal(&legacyState)
	if err != nil {
		t.Fatalf("marshal legacy state: %v", err)
	}
	if string(legacyRoundTrip) == "" || containsJSONField(legacyRoundTrip, "session_generation") {
		t.Fatalf("legacy round trip unexpectedly added session_generation: %s", legacyRoundTrip)
	}

	want := testDogTmuxGeneration("$9", "nonce-new")
	state := legacyState
	state.SessionGeneration = SessionGenerationFromTmux(want)
	data, err := json.Marshal(&state)
	if err != nil {
		t.Fatalf("marshal generation state: %v", err)
	}
	var got DogState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal generation state: %v", err)
	}
	if got.SessionGeneration == nil || !got.SessionGeneration.EqualTmux(want) {
		t.Fatalf("generation round trip = %+v, want %+v", got.SessionGeneration, want)
	}
}

func TestCompleteWorkWithTeardownIfMatchesKeepsFailedCloseoutUnavailable(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work-old")
	generation := testDogTmuxGeneration("$1", "nonce-old")
	persistDogSessionGenerationForTest(t, mgr, initial, generation)

	beforeMismatch, err := os.ReadFile(mgr.stateFilePath("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := mgr.CompleteWorkWithTeardownIfMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		testDogTmuxGeneration("$2", "nonce-new"),
		func(tmux.SessionGeneration) error {
			t.Fatal("mismatched generation ran teardown")
			return nil
		},
	)
	if err != nil || completed {
		t.Fatalf("mismatched generation closeout = %v, %v; want false, nil", completed, err)
	}
	afterMismatch, err := os.ReadFile(mgr.stateFilePath("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeMismatch) != string(afterMismatch) {
		t.Fatal("mismatched generation mutated durable state")
	}

	teardownErr := errors.New("teardown failed")
	completed, err = mgr.CompleteWorkWithTeardownIfMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		generation,
		func(got tmux.SessionGeneration) error {
			if !got.Equal(generation) {
				t.Fatalf("teardown generation = %+v, want %+v", got, generation)
			}
			return teardownErr
		},
	)
	if completed || !errors.Is(err, teardownErr) {
		t.Fatalf("failed closeout = %v, %v; want false, teardown error", completed, err)
	}

	dog, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.State != StateWorking || dog.Work != initial.Work ||
		dog.SessionGeneration == nil || !dog.SessionGeneration.EqualTmux(generation) {
		t.Fatalf("failed closeout mutated custody: %+v", dog)
	}
	idle, err := mgr.GetIdleDog()
	if err != nil {
		t.Fatal(err)
	}
	if idle != nil {
		t.Fatalf("failed closeout dog became assignable: %+v", idle)
	}

	completed, err = mgr.CompleteWorkWithTeardownIfMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		generation,
		func(tmux.SessionGeneration) error { return nil },
	)
	if err != nil || !completed {
		t.Fatalf("retry closeout = %v, %v; want true, nil", completed, err)
	}
	dog, err = mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.State != StateIdle || dog.Work != "" || dog.SessionGeneration != nil {
		t.Fatalf("completed closeout = %+v, want idle and custody-free", dog)
	}
}

func TestSetSessionGenerationIfAssignmentMatchesRejectsStaleStartup(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work-old")
	oldGeneration := testDogTmuxGeneration("$1", "nonce-old")
	newGeneration := testDogTmuxGeneration("$2", "nonce-new")
	staleGeneration := testDogTmuxGeneration("$3", "nonce-stale")

	set, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		nil,
		oldGeneration,
	)
	if err != nil || !set {
		t.Fatalf("initial generation CAS = %v, %v; want true, nil", set, err)
	}
	set, err = mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		nil,
		newGeneration,
	)
	if err != nil || set {
		t.Fatalf("stale prior-generation CAS = %v, %v; want false, nil", set, err)
	}

	completed, err := mgr.CompleteWorkWithTeardownIfMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		oldGeneration,
		func(tmux.SessionGeneration) error { return nil },
	)
	if err != nil || !completed {
		t.Fatalf("old assignment closeout = %v, %v", completed, err)
	}
	if err := mgr.AssignWork("alpha", "work-new"); err != nil {
		t.Fatal(err)
	}
	replacement, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	set, err = mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		replacement.Work,
		replacement.WorkStartedAt,
		nil,
		newGeneration,
	)
	if err != nil || !set {
		t.Fatalf("replacement generation CAS = %v, %v; want true, nil", set, err)
	}
	set, err = mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		&oldGeneration,
		staleGeneration,
	)
	if err != nil || set {
		t.Fatalf("stale assignment CAS = %v, %v; want false, nil", set, err)
	}
	dog, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.Work != "work-new" || dog.SessionGeneration == nil || !dog.SessionGeneration.EqualTmux(newGeneration) {
		t.Fatalf("stale startup mutated replacement: %+v", dog)
	}
}

func TestIdleDogWithSessionGenerationIsNotAssignable(t *testing.T) {
	mgr, state := newDogStateManager(t, "alpha", "")
	state.State = StateIdle
	state.WorkStartedAt = time.Time{}
	state.SessionGeneration = SessionGenerationFromTmux(testDogTmuxGeneration("$1", "nonce-stale"))
	if err := mgr.saveState("alpha", state); err != nil {
		t.Fatal(err)
	}

	if assigned, err := mgr.AssignWorkIfIdle("alpha", "work-new"); !errors.Is(err, ErrDogWorking) || assigned != nil {
		t.Fatalf("AssignWorkIfIdle = %+v, %v; want nil, ErrDogWorking", assigned, err)
	}
	idle, err := mgr.GetIdleDog()
	if err != nil {
		t.Fatal(err)
	}
	if idle != nil {
		t.Fatalf("GetIdleDog returned generation-owned dog: %+v", idle)
	}
}

func TestSetSessionGenerationIfAssignmentMatchesAcceptsPlatformWithoutCustodyMarker(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work")
	generation := testDogTmuxGeneration("$4", "nonce-no-custody")
	generation.Custody = ""
	changed, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		nil,
		generation,
	)
	if err != nil || !changed {
		t.Fatalf("SetSessionGenerationIfAssignmentMatches without custody marker = %v, %v", changed, err)
	}
}

func persistDogSessionGenerationForTest(
	t *testing.T,
	mgr *Manager,
	assignment *DogState,
	generation tmux.SessionGeneration,
) {
	t.Helper()
	changed, err := mgr.SetSessionGenerationIfAssignmentMatches(
		assignment.Name,
		assignment.Work,
		assignment.WorkStartedAt,
		nil,
		generation,
	)
	if err != nil || !changed {
		t.Fatalf("persisting test session generation = %v, %v", changed, err)
	}
}

func TestManagerSessionGenerationCASSerializesWithCloseout(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work-old")
	oldGeneration := testDogTmuxGeneration("$1", "nonce-old")
	newGeneration := testDogTmuxGeneration("$2", "nonce-new")
	set, err := mgr.SetSessionGenerationIfAssignmentMatches(
		"alpha",
		initial.Work,
		initial.WorkStartedAt,
		nil,
		oldGeneration,
	)
	if err != nil || !set {
		t.Fatalf("initial generation CAS = %v, %v", set, err)
	}

	lock, err := mgr.lockDog("alpha")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	type result struct {
		changed bool
		err     error
	}
	results := make(chan result, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		changed, err := mgr.CompleteWorkWithTeardownIfMatches(
			"alpha",
			initial.Work,
			initial.WorkStartedAt,
			oldGeneration,
			func(tmux.SessionGeneration) error { return nil },
		)
		results <- result{changed: changed, err: err}
	}()
	go func() {
		defer wg.Done()
		changed, err := mgr.SetSessionGenerationIfAssignmentMatches(
			"alpha",
			initial.Work,
			initial.WorkStartedAt,
			&oldGeneration,
			newGeneration,
		)
		results <- result{changed: changed, err: err}
	}()
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(results)
	changed := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent operation: %v", result.err)
		}
		if result.changed {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("successful transitions = %d, want exactly 1", changed)
	}

	dog, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.State == StateIdle {
		if dog.Work != "" || dog.SessionGeneration != nil {
			t.Fatalf("completed state = %+v, want idle and custody-free", dog)
		}
	} else if dog.State == StateWorking {
		if dog.Work != initial.Work || dog.SessionGeneration == nil || !dog.SessionGeneration.EqualTmux(newGeneration) {
			t.Fatalf("replacement generation state = %+v", dog)
		}
	} else {
		t.Fatalf("unexpected dog state: %+v", dog)
	}
}

func containsJSONField(data []byte, field string) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return false
	}
	_, ok := object[field]
	return ok
}

func testDogTmuxGeneration(sessionID, nonce string) tmux.SessionGeneration {
	return tmux.SessionGeneration{
		Name:           "hq-dog-alpha",
		SessionID:      sessionID,
		Nonce:          nonce,
		Custody:        "custody-alpha",
		ServerPID:      4242,
		ServerIdentity: "server-start-alpha",
	}
}

func newDogStateManager(t *testing.T, name, work string) (*Manager, *DogState) {
	t.Helper()
	mgr := NewManager(t.TempDir(), &config.RigsConfig{Rigs: map[string]config.RigEntry{}})
	if err := os.MkdirAll(mgr.dogDir(name), 0755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(0)
	state := &DogState{
		Name:          name,
		State:         StateWorking,
		Work:          work,
		WorkStartedAt: now,
		LastActive:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := mgr.saveState(name, state); err != nil {
		t.Fatal(err)
	}
	return mgr, state
}

// TestManagerCreation verifies Manager initialization.
func TestManagerCreation(t *testing.T) {
	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs: map[string]config.RigEntry{
			"gastown": {
				GitURL: "git@github.com:test/gastown.git",
			},
			"beads": {
				GitURL: "git@github.com:test/beads.git",
			},
		},
	}

	m := NewManager("/tmp/test-town", rigsConfig)

	if filepath.ToSlash(m.townRoot) != "/tmp/test-town" {
		t.Errorf("expected townRoot '/tmp/test-town', got %q", m.townRoot)
	}
	if filepath.ToSlash(m.kennelPath) != "/tmp/test-town/deacon/dogs" {
		t.Errorf("expected kennelPath '/tmp/test-town/deacon/dogs', got %q", m.kennelPath)
	}
}

// TestDogDir verifies dogDir path construction.
func TestDogDir(t *testing.T) {
	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs:    map[string]config.RigEntry{},
	}
	m := NewManager("/home/user/gt", rigsConfig)

	path := m.dogDir("alpha")
	expected := "/home/user/gt/deacon/dogs/alpha"
	if filepath.ToSlash(path) != expected {
		t.Errorf("expected %q, got %q", expected, path)
	}
}

// TestStateConstants verifies state constants.
func TestStateConstants(t *testing.T) {
	tests := []struct {
		state    State
		expected string
	}{
		{StateIdle, "idle"},
		{StateWorking, "working"},
	}

	for _, tc := range tests {
		if string(tc.state) != tc.expected {
			t.Errorf("expected %q, got %q", tc.expected, string(tc.state))
		}
	}
}

// TestValidateDogName verifies name validation rejects dangerous names.
func TestValidateDogName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"alpha", false},
		{"dog-1", false},
		{"my_dog", false},
		{"", true},
		{"/", true},
		{"foo/bar", true},
		{"foo\\bar", true},
		{"..", true},
		{".", true},
		{"../../etc", true},
		{"a..b", true},
	}

	for _, tc := range tests {
		err := validateDogName(tc.name)
		if tc.wantErr && err == nil {
			t.Errorf("validateDogName(%q): expected error, got nil", tc.name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateDogName(%q): unexpected error: %v", tc.name, err)
		}
		if tc.wantErr && err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("validateDogName(%q): expected ErrInvalidName, got %v", tc.name, err)
		}
	}
}

// TestRemoveEmptyName verifies Remove("") returns ErrInvalidName, not ErrDogNotFound.
func TestRemoveEmptyName(t *testing.T) {
	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs:    map[string]config.RigEntry{},
	}
	m := NewManager(t.TempDir(), rigsConfig)

	err := m.Remove("")
	if err == nil {
		t.Fatal("Remove('') should return an error")
	}
	if !errors.Is(err, ErrInvalidName) {
		t.Errorf("Remove(''): expected ErrInvalidName, got %v", err)
	}
}

// TestAddTraversalName verifies Add rejects path traversal names.
func TestAddTraversalName(t *testing.T) {
	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs:    map[string]config.RigEntry{},
	}
	m := NewManager(t.TempDir(), rigsConfig)

	_, err := m.Add("../../etc")
	if err == nil {
		t.Fatal("Add('../../etc') should return an error")
	}
	if !errors.Is(err, ErrInvalidName) {
		t.Errorf("Add('../../etc'): expected ErrInvalidName, got %v", err)
	}
}
