package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/reaper"
)

func TestReconcileAnomalyScansFiveIdenticalPatrolsPersistOneEscalationAndMail(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	scan := reaper.AnomalyScan{
		Scope:    "hq",
		Complete: true,
		Anomalies: []reaper.Anomaly{{
			Type:        "dangling_parent_ref",
			Scope:       "hq",
			AffectedIDs: []string{"hq-child"},
			Remediation: "repair_parent_links",
			Message:     "volatile patrol prose",
			Count:       1,
		}},
	}

	for patrol := 0; patrol < 5; patrol++ {
		if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
			t.Fatalf("patrol %d: %v", patrol+1, err)
		}
	}

	open := 0
	for _, issue := range lifecycle.issues {
		if issue.Status == "open" {
			open++
		}
	}
	if open != 1 || len(lifecycle.issues) != 1 {
		t.Fatalf("escalations = %#v, want one open occurrence", lifecycle.issues)
	}
	if len(lifecycle.durableMail) != 1 {
		t.Fatalf("durable mail = %#v, want one record", lifecycle.durableMail)
	}
}

func TestReconcileAnomalyScansDuplicateInputPersistsOneEscalationAndMail(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	anomaly := testReaperAnomaly("hq-child")
	scan := completeAnomalyScan(anomaly)
	scan.Anomalies = append(scan.Anomalies, anomaly)
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.issues) != 1 || len(lifecycle.durableMail) != 1 {
		t.Fatalf("duplicate input state issues=%d mail=%v, want one occurrence and one mail", len(lifecycle.issues), lifecycle.durableMail)
	}
}

func TestReconcileAnomalyScansPersistsChangedResolvedRecurrenceAndIncompleteTransitions(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	anomaly := reaper.Anomaly{
		Type:        "dangling_parent_ref",
		Scope:       "hq",
		AffectedIDs: []string{"hq-child-a"},
		Remediation: "repair_parent_links",
	}
	reconcile := func(scan reaper.AnomalyScan) {
		t.Helper()
		if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
			t.Fatal(err)
		}
	}

	reconcile(reaper.AnomalyScan{Scope: "hq", Complete: true, Anomalies: []reaper.Anomaly{anomaly}})
	first := lifecycle.issues[0]
	anomaly.AffectedIDs = []string{"hq-child-a", "hq-child-b"}
	reconcile(reaper.AnomalyScan{Scope: "hq", Complete: true, Anomalies: []reaper.Anomaly{anomaly}})
	second := lifecycle.issues[1]
	if first.Status != "closed" || second.Status != "open" {
		t.Fatalf("changed transition = %#v", lifecycle.issues)
	}
	if got := beads.ParseEscalationFields(second.Description).PreviousOccurrence; got != first.ID {
		t.Fatalf("changed previous occurrence = %q, want %q", got, first.ID)
	}

	reconcile(reaper.AnomalyScan{Scope: "hq", Complete: true})
	if second.Status != "closed" {
		t.Fatalf("resolved status = %q, want closed", second.Status)
	}
	reconcile(reaper.AnomalyScan{Scope: "hq", Complete: true, Anomalies: []reaper.Anomaly{anomaly}})
	third := lifecycle.issues[2]
	if got := beads.ParseEscalationFields(third.Description).PreviousOccurrence; got != second.ID {
		t.Fatalf("recurrence previous occurrence = %q, want %q", got, second.ID)
	}
	reconcile(reaper.AnomalyScan{Scope: "hq", Complete: false})
	if third.Status != "open" {
		t.Fatalf("incomplete scope changed recurrence status to %q", third.Status)
	}
}

func TestReconcileAnomalyScansCreateFailurePreservesActiveOccurrence(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	old := testReaperAnomaly("hq-child-a")
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{completeAnomalyScan(old)}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}

	lifecycle.createErr = errors.New("create unavailable")
	changed := testReaperAnomaly("hq-child-a", "hq-child-b")
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{completeAnomalyScan(changed)}, lifecycle.deps()); err == nil {
		t.Fatal("reconcile succeeded, want create failure")
	}
	if got := lifecycle.openIssueIDs(); fmt.Sprint(got) != "[hq-esc-1]" {
		t.Fatalf("open occurrences after create failure = %v, want original", got)
	}

	lifecycle.createErr = nil
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{completeAnomalyScan(changed)}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.openIssueIDs(); fmt.Sprint(got) != "[hq-esc-2]" {
		t.Fatalf("open occurrences after retry = %v, want replacement", got)
	}
	if got := beads.ParseEscalationFields(lifecycle.issues[1].Description).PreviousOccurrence; got != "hq-esc-1" {
		t.Fatalf("previous occurrence = %q, want hq-esc-1", got)
	}
}

func TestReconcileAnomalyScansCloseFailureLeavesReplacementActiveForRetry(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	old := testReaperAnomaly("hq-child-a")
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{completeAnomalyScan(old)}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}

	lifecycle.closeErr = errors.New("close unavailable")
	changed := testReaperAnomaly("hq-child-a", "hq-child-b")
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{completeAnomalyScan(changed)}, lifecycle.deps()); err == nil {
		t.Fatal("reconcile succeeded, want close failure")
	}
	if got := lifecycle.openIssueIDs(); fmt.Sprint(got) != "[hq-esc-1 hq-esc-2]" {
		t.Fatalf("open occurrences after close failure = %v, want original and replacement", got)
	}
	if len(lifecycle.durableMail) != 2 {
		t.Fatalf("durable mail = %v, want one per occurrence", lifecycle.durableMail)
	}

	lifecycle.closeErr = nil
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{completeAnomalyScan(changed)}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	if got := lifecycle.openIssueIDs(); fmt.Sprint(got) != "[hq-esc-2]" {
		t.Fatalf("open occurrences after retry = %v, want replacement", got)
	}
	if len(lifecycle.durableMail) != 2 {
		t.Fatalf("retry duplicated durable mail: %v", lifecycle.durableMail)
	}
}

func TestReconcileAnomalyScansDurableMailFailureRetriesPendingOccurrence(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	lifecycle.mailWriteErr = errors.New("mail store unavailable")
	scan := completeAnomalyScan(testReaperAnomaly("hq-child-a"))
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err == nil {
		t.Fatal("reconcile succeeded, want durable mail failure")
	}
	if got := lifecycle.openIssueIDs(); fmt.Sprint(got) != "[hq-esc-1]" {
		t.Fatalf("open occurrences after mail failure = %v, want created occurrence", got)
	}
	if len(lifecycle.durableMail) != 0 {
		t.Fatalf("durable mail = %v, want none", lifecycle.durableMail)
	}

	lifecycle.mailWriteErr = nil
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.issues) != 1 || len(lifecycle.durableMail) != 1 {
		t.Fatalf("retry state issues=%d mail=%v, want one occurrence and one mail", len(lifecycle.issues), lifecycle.durableMail)
	}
}

func TestReconcileAnomalyScansAsyncNotificationFailureDoesNotDuplicateStoredMail(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	lifecycle.notifyErr = errors.New("wake not confirmed")
	scan := completeAnomalyScan(testReaperAnomaly("hq-child-a"))
	_, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps())
	if err == nil || !strings.Contains(err.Error(), "notification") {
		t.Fatalf("error = %v, want sanitized notification failure", err)
	}
	if len(lifecycle.durableMail) != 1 {
		t.Fatalf("durable mail = %v, want stored mail", lifecycle.durableMail)
	}
	if !beads.ParseEscalationFields(lifecycle.issues[0].Description).AnomalyMailStored {
		t.Fatal("mail-stored marker = false after durable write")
	}

	lifecycle.notifyErr = nil
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.durableMail) != 1 {
		t.Fatalf("retry duplicated stored mail: %v", lifecycle.durableMail)
	}
}

func TestReconcileAnomalyScansMailMarkerSurvivesThreadRetention(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	scan := completeAnomalyScan(testReaperAnomaly("hq-child-a"))
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	lifecycle.durableMail = nil // Simulate normal retention after the marker is durable.
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.durableMail) != 0 {
		t.Fatalf("unchanged replay recreated retained mail: %v", lifecycle.durableMail)
	}
}

func TestReconcileAnomalyScansMailMarkerFailureHealsWithoutDuplicateMail(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	lifecycle.markerErr = errors.New("marker unavailable")
	scan := completeAnomalyScan(testReaperAnomaly("hq-child-a"))
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err == nil {
		t.Fatal("reconcile succeeded, want marker failure")
	}
	if len(lifecycle.durableMail) != 1 {
		t.Fatalf("durable mail = %v, want stored mail before marker failure", lifecycle.durableMail)
	}
	if beads.ParseEscalationFields(lifecycle.issues[0].Description).AnomalyMailStored {
		t.Fatal("mail-stored marker = true after failed marker write")
	}

	lifecycle.markerErr = nil
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, lifecycle.deps()); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.durableMail) != 1 {
		t.Fatalf("marker retry duplicated mail: %v", lifecycle.durableMail)
	}
	if !beads.ParseEscalationFields(lifecycle.issues[0].Description).AnomalyMailStored {
		t.Fatal("mail-stored marker = false after retry")
	}
}

func TestReconcileAnomalyScansFailureDiagnosticsExcludeDependencyAndAnomalyDetails(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	lifecycle.createErr = errors.New("private subprocess detail")
	anomaly := testReaperAnomaly("private-affected-id")
	anomaly.Message = "private anomaly prose"
	_, err := reconcileAnomalyScans([]reaper.AnomalyScan{completeAnomalyScan(anomaly)}, lifecycle.deps())
	if err == nil {
		t.Fatal("reconcile succeeded, want create failure")
	}
	for _, private := range []string{"private subprocess detail", "private-affected-id", "private anomaly prose"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("error %q contains private detail %q", err, private)
		}
	}
}

func TestReaperReconcileAnomaliesCommandIsRegistered(t *testing.T) {
	cmd, _, err := reaperCmd.Find([]string{"reconcile-anomalies"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Name() != "reconcile-anomalies" {
		t.Fatalf("command = %q, want reconcile-anomalies", cmd.Name())
	}
}

type isolatedAnomalyLifecycle struct {
	now          time.Time
	issues       []*beads.Issue
	durableMail  []string
	createErr    error
	closeErr     error
	mailWriteErr error
	notifyErr    error
	markerErr    error
}

func newIsolatedAnomalyLifecycle() *isolatedAnomalyLifecycle {
	return &isolatedAnomalyLifecycle{now: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
}

func (l *isolatedAnomalyLifecycle) deps() anomalyReconcileDeps {
	return anomalyReconcileDeps{
		now: func() time.Time { return l.now },
		list: func() ([]*beads.Issue, error) {
			return append([]*beads.Issue(nil), l.issues...), nil
		},
		create: func(title string, fields *beads.EscalationFields) (*beads.Issue, error) {
			if l.createErr != nil {
				return nil, l.createErr
			}
			issue := &beads.Issue{
				ID:          fmt.Sprintf("hq-esc-%d", len(l.issues)+1),
				Title:       title,
				Description: beads.FormatEscalationDescription(title, fields),
				Status:      "open",
				CreatedAt:   l.now.Format(time.RFC3339),
				Labels:      []string{"gt:escalation"},
			}
			l.issues = append(l.issues, issue)
			return issue, nil
		},
		close: func(id, _, _ string) error {
			if l.closeErr != nil {
				return l.closeErr
			}
			for _, issue := range l.issues {
				if issue.ID == id {
					issue.Status = "closed"
					return nil
				}
			}
			return fmt.Errorf("unknown escalation %s", id)
		},
		send: func(issue *beads.Issue, anomaly reaper.Anomaly) error {
			for _, record := range l.durableMail {
				if strings.HasPrefix(record, issue.ID+":") {
					return nil
				}
			}
			if l.mailWriteErr != nil {
				return l.mailWriteErr
			}
			l.durableMail = append(l.durableMail, issue.ID+":"+anomaly.Type)
			return nil
		},
		wait: func() error { return l.notifyErr },
		mark: func(issue *beads.Issue) error {
			if l.markerErr != nil {
				return l.markerErr
			}
			fields := beads.ParseEscalationFields(issue.Description)
			fields.AnomalyMailStored = true
			issue.Description = beads.FormatEscalationDescription(issue.Title, fields)
			return nil
		},
	}
}

func (l *isolatedAnomalyLifecycle) openIssueIDs() []string {
	var ids []string
	for _, issue := range l.issues {
		if issue.Status == "open" {
			ids = append(ids, issue.ID)
		}
	}
	return ids
}

func testReaperAnomaly(ids ...string) reaper.Anomaly {
	return reaper.Anomaly{
		Type: "dangling_parent_ref", Scope: "hq", AffectedIDs: ids, Remediation: "repair_parent_links",
	}
}

func completeAnomalyScan(anomaly reaper.Anomaly) reaper.AnomalyScan {
	return reaper.AnomalyScan{Scope: anomaly.Scope, Complete: true, Anomalies: []reaper.Anomaly{anomaly}}
}
