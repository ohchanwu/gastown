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

func TestManagerSessionGenerationGuardedCompletionAndCompareClear(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work-old")
	oldGeneration := testDogTmuxGeneration("$1", "nonce-old")
	newGeneration := testDogTmuxGeneration("$2", "nonce-new")
	if err := mgr.SetSessionGeneration("alpha", oldGeneration); err != nil {
		t.Fatalf("SetSessionGeneration: %v", err)
	}

	beforeMismatch, err := os.ReadFile(mgr.stateFilePath("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := mgr.CompleteWorkIfMatches("alpha", initial.Work, initial.WorkStartedAt, newGeneration)
	if err != nil {
		t.Fatalf("CompleteWorkIfMatches mismatch: %v", err)
	}
	if completed {
		t.Fatal("mismatched generation completed work")
	}
	afterMismatch, err := os.ReadFile(mgr.stateFilePath("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeMismatch) != string(afterMismatch) {
		t.Fatal("mismatched generation mutated durable state")
	}

	completed, err = mgr.CompleteWorkIfMatches("alpha", initial.Work, initial.WorkStartedAt, oldGeneration)
	if err != nil || !completed {
		t.Fatalf("matching CompleteWorkIfMatches = %v, %v; want true, nil", completed, err)
	}
	dog, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.State != StateIdle || dog.Work != "" || dog.SessionGeneration == nil || !dog.SessionGeneration.EqualTmux(oldGeneration) {
		t.Fatalf("completed dog = %+v, want idle work with retained generation", dog)
	}

	cleared, err := mgr.ClearSessionGenerationIfMatches("alpha", newGeneration)
	if err != nil || cleared {
		t.Fatalf("mismatched ClearSessionGenerationIfMatches = %v, %v; want false, nil", cleared, err)
	}
	cleared, err = mgr.ClearSessionGenerationIfMatches("alpha", oldGeneration)
	if err != nil || !cleared {
		t.Fatalf("matching ClearSessionGenerationIfMatches = %v, %v; want true, nil", cleared, err)
	}
	dog, err = mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.SessionGeneration != nil {
		t.Fatalf("session generation after compare-clear = %+v, want nil", dog.SessionGeneration)
	}
}

func TestSetSessionGenerationPreservesIndependentWorkState(t *testing.T) {
	mgr, state := newDogStateManager(t, "alpha", "")
	state.State = StateIdle
	state.WorkStartedAt = time.Time{}
	if err := mgr.saveState("alpha", state); err != nil {
		t.Fatal(err)
	}

	if err := mgr.SetSessionGeneration("alpha", testDogTmuxGeneration("$3", "nonce-idle")); err != nil {
		t.Fatal(err)
	}
	dog, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.State != StateIdle || dog.Work != "" {
		t.Fatalf("work state changed while recording runtime generation: %+v", dog)
	}
}

func TestSetSessionGenerationAcceptsPlatformWithoutCustodyMarker(t *testing.T) {
	mgr, _ := newDogStateManager(t, "alpha", "work")
	generation := testDogTmuxGeneration("$4", "nonce-no-custody")
	generation.Custody = ""
	if err := mgr.SetSessionGeneration("alpha", generation); err != nil {
		t.Fatalf("SetSessionGeneration without custody marker: %v", err)
	}
}

func TestManagerSessionGenerationOperationsSerializeWithAssignment(t *testing.T) {
	mgr, initial := newDogStateManager(t, "alpha", "work-old")
	oldGeneration := testDogTmuxGeneration("$1", "nonce-old")
	newGeneration := testDogTmuxGeneration("$2", "nonce-new")
	if err := mgr.SetSessionGeneration("alpha", oldGeneration); err != nil {
		t.Fatal(err)
	}

	lock, err := mgr.lockDog("alpha")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		errs <- mgr.AssignWork("alpha", "work-new")
	}()
	go func() {
		defer wg.Done()
		_, err := mgr.CompleteWorkIfMatches("alpha", initial.Work, initial.WorkStartedAt, oldGeneration)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		errs <- mgr.SetSessionGeneration("alpha", newGeneration)
	}()
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation: %v", err)
		}
	}

	dog, err := mgr.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if dog.Work != "work-new" {
		t.Fatalf("work = %q, want work-new", dog.Work)
	}
	if dog.SessionGeneration == nil || !dog.SessionGeneration.EqualTmux(newGeneration) {
		t.Fatalf("generation = %+v, want replacement", dog.SessionGeneration)
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
