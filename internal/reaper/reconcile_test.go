package reaper

import (
	"reflect"
	"testing"
	"time"
)

func TestAnomalyFingerprintUsesOnlyCanonicalStableFields(t *testing.T) {
	anomaly := Anomaly{
		Type:        "dangling_parent_ref",
		Scope:       "hq",
		AffectedIDs: []string{"hq-b", "hq-a", "hq-a"},
		Remediation: "repair_parent_links",
		Message:     "first display message",
		Count:       3,
	}

	got, err := FingerprintAnomaly(anomaly)
	if err != nil {
		t.Fatal(err)
	}
	const want = "reaper-anomaly:v1:6788c54f2720348df5037c016a87e5404c571d58a53c1c895686785dd5f76bf4"
	if got != want {
		t.Fatalf("fingerprint = %q, want %q", got, want)
	}

	anomaly.Message = "different display prose"
	anomaly.Count = 999
	anomaly.AffectedIDs = []string{"hq-a", "hq-b"}
	gotAfterVolatileChanges, err := FingerprintAnomaly(anomaly)
	if err != nil {
		t.Fatal(err)
	}
	if gotAfterVolatileChanges != want {
		t.Fatalf("fingerprint after volatile changes = %q, want %q", gotAfterVolatileChanges, want)
	}
}

func TestAnomalyFingerprintRejectsUnsanitizedStableFields(t *testing.T) {
	valid := Anomaly{Type: "dangling_parent_ref", Scope: "hq", AffectedIDs: []string{"hq-a"}, Remediation: "repair_parent_links"}
	tests := []struct {
		name   string
		mutate func(*Anomaly)
	}{
		{name: "type newline", mutate: func(a *Anomaly) { a.Type = "dangling\nparent" }},
		{name: "scope traversal", mutate: func(a *Anomaly) { a.Scope = "../hq" }},
		{name: "remediation prose", mutate: func(a *Anomaly) { a.Remediation = "repair parent links" }},
		{name: "affected ID newline", mutate: func(a *Anomaly) { a.AffectedIDs = []string{"hq-a\nhq-b"} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			anomaly := valid
			tc.mutate(&anomaly)
			if _, err := FingerprintAnomaly(anomaly); err == nil {
				t.Fatalf("FingerprintAnomaly(%#v) succeeded, want validation error", anomaly)
			}
		})
	}
}

func TestReconcileAnomaliesFiveIdenticalSnapshotsCreateOneOccurrence(t *testing.T) {
	anomaly := Anomaly{
		Type:        "dangling_parent_ref",
		Scope:       "hq",
		AffectedIDs: []string{"hq-a"},
		Remediation: "repair_parent_links",
	}
	scan := AnomalyScan{Scope: "hq", Complete: true, Anomalies: []Anomaly{anomaly}}
	var occurrences []AnomalyOccurrence
	creates := 0

	for patrol := 0; patrol < 5; patrol++ {
		actions, err := ReconcileAnomalies([]AnomalyScan{scan}, occurrences)
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range actions {
			if action.Kind != ReconcileCreate {
				t.Fatalf("patrol %d action = %#v, want create or no-op", patrol+1, action)
			}
			creates++
			occurrences = append(occurrences, AnomalyOccurrence{
				ID:          "hq-esc-1",
				Fingerprint: action.Fingerprint,
				Family:      action.Family,
				Scope:       action.Anomaly.Scope,
				Status:      "open",
			})
		}
	}

	if creates != 1 {
		t.Fatalf("create actions = %d, want 1", creates)
	}
}

func TestReconcileAnomaliesChangedAffectedSetReplacesOccurrence(t *testing.T) {
	oldAnomaly := Anomaly{
		Type:        "dangling_parent_ref",
		Scope:       "hq",
		AffectedIDs: []string{"hq-a"},
		Remediation: "repair_parent_links",
	}
	initial, err := ReconcileAnomalies([]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{oldAnomaly}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 1 || initial[0].Kind != ReconcileCreate {
		t.Fatalf("initial actions = %#v, want one create", initial)
	}
	oldOccurrence := AnomalyOccurrence{
		ID:          "hq-esc-old",
		Fingerprint: initial[0].Fingerprint,
		Family:      initial[0].Family,
		Scope:       "hq",
		Status:      "open",
	}
	changedAnomaly := oldAnomaly
	changedAnomaly.AffectedIDs = []string{"hq-a", "hq-b"}

	actions, err := ReconcileAnomalies(
		[]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{changedAnomaly}}},
		[]AnomalyOccurrence{oldOccurrence},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("actions = %#v, want create then resolve", actions)
	}
	if actions[0].Kind != ReconcileCreate || actions[0].PreviousOccurrence != oldOccurrence.ID {
		t.Fatalf("first action = %#v, want linked replacement", actions[0])
	}
	if actions[1].Kind != ReconcileResolve || actions[1].OccurrenceID != oldOccurrence.ID {
		t.Fatalf("second action = %#v, want old occurrence resolution", actions[1])
	}
}

func TestReconcileAnomaliesExistingReplacementResolvesSupersededFamilyOccurrence(t *testing.T) {
	old := Anomaly{Type: "dangling_parent_ref", Scope: "hq", AffectedIDs: []string{"hq-a"}, Remediation: "repair_parent_links"}
	changed := old
	changed.AffectedIDs = []string{"hq-a", "hq-b"}
	oldFingerprint, _ := FingerprintAnomaly(old)
	newFingerprint, _ := FingerprintAnomaly(changed)
	family, _ := fingerprintAnomalyFamily(changed)
	actions, err := ReconcileAnomalies(
		[]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{changed}}},
		[]AnomalyOccurrence{
			{ID: "hq-esc-old", Fingerprint: oldFingerprint, Family: family, Scope: "hq", Status: "open"},
			{ID: "hq-esc-new", Fingerprint: newFingerprint, Family: family, Scope: "hq", Status: "open"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ReconcileResolve || actions[0].OccurrenceID != "hq-esc-old" {
		t.Fatalf("actions = %#v, want superseded family occurrence resolution", actions)
	}
}

func TestReconcileAnomaliesCompleteEmptyScopeResolvesOccurrence(t *testing.T) {
	occurrence := AnomalyOccurrence{
		ID:          "hq-esc-active",
		Fingerprint: "reaper-anomaly:v1:active",
		Family:      "reaper-anomaly-family:v1:family",
		Scope:       "hq",
		Status:      "open",
	}

	actions, err := ReconcileAnomalies(
		[]AnomalyScan{{Scope: "hq", Complete: true}},
		[]AnomalyOccurrence{occurrence},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ReconcileResolve || actions[0].OccurrenceID != occurrence.ID {
		t.Fatalf("actions = %#v, want resolution of %s", actions, occurrence.ID)
	}
}

func TestReconcileAnomaliesIncompleteScopePreservesOccurrence(t *testing.T) {
	occurrence := AnomalyOccurrence{
		ID:          "hq-esc-active",
		Fingerprint: "reaper-anomaly:v1:active",
		Family:      "reaper-anomaly-family:v1:family",
		Scope:       "hq",
		Status:      "open",
	}

	actions, err := ReconcileAnomalies(
		[]AnomalyScan{{Scope: "hq", Complete: false}},
		[]AnomalyOccurrence{occurrence},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %#v, want incomplete scope preserved", actions)
	}
}

func TestReconcileAnomaliesRecurrenceLinksLatestClosedOccurrence(t *testing.T) {
	anomaly := Anomaly{
		Type:        "dangling_parent_ref",
		Scope:       "hq",
		AffectedIDs: []string{"hq-a"},
		Remediation: "repair_parent_links",
	}
	initial, err := ReconcileAnomalies([]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{anomaly}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	older := AnomalyOccurrence{
		ID:          "hq-esc-older",
		Fingerprint: initial[0].Fingerprint,
		Family:      initial[0].Family,
		Scope:       "hq",
		Status:      "closed",
		CreatedAt:   time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC),
	}
	latest := older
	latest.ID = "hq-esc-latest"
	latest.CreatedAt = time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC)

	actions, err := ReconcileAnomalies(
		[]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{anomaly}}},
		[]AnomalyOccurrence{latest, older},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ReconcileCreate {
		t.Fatalf("actions = %#v, want one recurrence create", actions)
	}
	if actions[0].PreviousOccurrence != latest.ID {
		t.Fatalf("previous occurrence = %q, want %q", actions[0].PreviousOccurrence, latest.ID)
	}
}

func TestReconcileAnomaliesResolvesDuplicateActiveFingerprint(t *testing.T) {
	anomaly := Anomaly{
		Type:        "dangling_parent_ref",
		Scope:       "hq",
		AffectedIDs: []string{"hq-a"},
		Remediation: "repair_parent_links",
	}
	initial, err := ReconcileAnomalies([]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{anomaly}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	older := AnomalyOccurrence{
		ID:          "hq-esc-older",
		Fingerprint: initial[0].Fingerprint,
		Family:      initial[0].Family,
		Scope:       "hq",
		Status:      "open",
		CreatedAt:   time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC),
	}
	latest := older
	latest.ID = "hq-esc-latest"
	latest.CreatedAt = time.Date(2026, 7, 31, 1, 0, 0, 0, time.UTC)

	actions, err := ReconcileAnomalies(
		[]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{anomaly}}},
		[]AnomalyOccurrence{latest, older},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ReconcileResolve || actions[0].OccurrenceID != older.ID {
		t.Fatalf("actions = %#v, want duplicate resolution of %s", actions, older.ID)
	}
}

func TestReconcileAnomaliesOrdersActionsDeterministically(t *testing.T) {
	first := Anomaly{Type: "dangling_parent_ref", Scope: "hq", AffectedIDs: []string{"hq-b"}, Remediation: "repair_parent_links"}
	second := Anomaly{Type: "open_wisp_spike", Scope: "hq", AffectedIDs: []string{"hq-a"}, Remediation: "inspect_wisp_lifecycle"}

	forward, err := ReconcileAnomalies([]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{first, second}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := ReconcileAnomalies([]AnomalyScan{{Scope: "hq", Complete: true, Anomalies: []Anomaly{second, first}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, reverse) {
		t.Fatalf("forward actions = %#v, reverse actions = %#v", forward, reverse)
	}
}
