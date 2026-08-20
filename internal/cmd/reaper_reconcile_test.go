package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/reaper"
)

func TestSendReaperAnomalyMailChecksCustodyPerTarget(t *testing.T) {
	issue := &beads.Issue{ID: "hq-escalation"}
	anomaly := testReaperAnomaly("hq-child")
	targetA := "gastown/witness"
	targetB := "overseer"
	storedToA := &mail.Message{
		ID:       "hq-message-a",
		From:     "reaper",
		To:       targetA,
		Subject:  "[MEDIUM] Reaper anomaly",
		Type:     mail.TypeEscalation,
		ThreadID: issue.ID,
	}
	var sent []*mail.Message

	err := sendReaperAnomalyMail(issue, anomaly, []string{targetA, targetB},
		func(_, _ string) ([]*mail.Message, error) {
			return []*mail.Message{storedToA}, nil
		},
		func(msg *mail.Message) error {
			sent = append(sent, msg)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].To != targetB {
		t.Fatalf("sent = %#v, want one notice to %q", sent, targetB)
	}
}

func TestExpandReaperMailTargetsResolvesFanoutAndDeduplicates(t *testing.T) {
	targets, err := expandReaperMailTargets(
		[]string{"list:oncall", "@witnesses", "queue:triage", "gastown/witness"},
		func(address string) ([]string, error) {
			if address != "list:oncall" {
				t.Fatalf("list address = %q", address)
			}
			return []string{"mayor/", "gastown/witness"}, nil
		},
		func(address string) ([]string, error) {
			if address != "@witnesses" {
				t.Fatalf("group address = %q", address)
			}
			return []string{"gastown/witness", "other/witness"}, nil
		},
		func(address string) ([]string, error) {
			return nil, fmt.Errorf("unexpected channel lookup %q", address)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mayor/", "gastown/witness", "other/witness", "queue:triage"}
	if len(targets) != len(want) {
		t.Fatalf("targets = %#v, want %#v", targets, want)
	}
	for i := range want {
		if targets[i] != want[i] {
			t.Fatalf("targets = %#v, want %#v", targets, want)
		}
	}
}

func TestSendReaperAnomalyMailRetriesOnlyMissingFanoutRecipients(t *testing.T) {
	issue := &beads.Issue{ID: "hq-escalation"}
	anomaly := testReaperAnomaly("hq-child")
	targets, err := expandReaperMailTargets(
		[]string{"list:oncall"},
		func(string) ([]string, error) { return []string{"gastown/witness", "overseer"}, nil },
		func(string) ([]string, error) { return nil, errors.New("unexpected group lookup") },
		func(string) ([]string, error) { return nil, errors.New("unexpected channel lookup") },
	)
	if err != nil {
		t.Fatal(err)
	}
	stored := make(map[string]*mail.Message)
	attempts := make(map[string]int)
	list := func(_, _ string) ([]*mail.Message, error) {
		messages := make([]*mail.Message, 0, len(stored))
		for _, message := range stored {
			messages = append(messages, message)
		}
		return messages, nil
	}
	send := func(message *mail.Message) error {
		attempts[message.To]++
		if message.To == "overseer" && attempts[message.To] == 1 {
			return errors.New("injected partial fanout failure")
		}
		copy := *message
		copy.ID = "stored-" + message.To
		stored[message.To] = &copy
		return nil
	}

	if err := sendReaperAnomalyMail(issue, anomaly, targets, list, send); err == nil {
		t.Fatal("first fanout succeeded, want injected partial failure")
	}
	if err := sendReaperAnomalyMail(issue, anomaly, targets, list, send); err != nil {
		t.Fatalf("fanout retry: %v", err)
	}
	if err := sendReaperAnomalyMail(issue, anomaly, targets, list, send); err != nil {
		t.Fatalf("post-store marker retry: %v", err)
	}
	if attempts["gastown/witness"] != 1 || attempts["overseer"] != 2 {
		t.Fatalf("attempts = %#v, want witness=1 overseer=2", attempts)
	}
	if len(stored) != 2 {
		t.Fatalf("stored = %#v, want one notice per concrete recipient", stored)
	}
}

func TestSendReaperAnomalyMailRetriesOnlyMissingChannelSubscribers(t *testing.T) {
	issue := &beads.Issue{ID: "hq-escalation"}
	anomaly := testReaperAnomaly("hq-child")
	targets, err := expandReaperMailTargets(
		[]string{"channel:alerts"},
		func(string) ([]string, error) { return nil, errors.New("unexpected list lookup") },
		func(string) ([]string, error) { return nil, errors.New("unexpected group lookup") },
		func(address string) ([]string, error) {
			if address != "channel:alerts" {
				t.Fatalf("channel address = %q", address)
			}
			return []string{"gastown/witness", "overseer"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stored := make(map[string]*mail.Message)
	attempts := make(map[string]int)
	list := func(_, _ string) ([]*mail.Message, error) {
		messages := make([]*mail.Message, 0, len(stored))
		for _, message := range stored {
			messages = append(messages, message)
		}
		return messages, nil
	}
	send := func(message *mail.Message) error {
		attempts[message.To]++
		if message.To == "overseer" && attempts[message.To] == 1 {
			return errors.New("injected channel subscriber failure")
		}
		copy := *message
		copy.ID = "stored-" + message.To
		stored[message.To] = &copy
		return nil
	}

	if err := sendReaperAnomalyMail(issue, anomaly, targets, list, send); err == nil {
		t.Fatal("first channel fanout succeeded, want injected subscriber failure")
	}
	if err := sendReaperAnomalyMail(issue, anomaly, targets, list, send); err != nil {
		t.Fatalf("channel fanout retry: %v", err)
	}
	if err := sendReaperAnomalyMail(issue, anomaly, targets, list, send); err != nil {
		t.Fatalf("channel marker retry: %v", err)
	}
	if attempts["gastown/witness"] != 1 || attempts["overseer"] != 2 {
		t.Fatalf("attempts = %#v, want witness=1 overseer=2", attempts)
	}
}

func TestReconcileAnomalyScansFanoutMarkerRetryDoesNotDuplicateMail(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	lifecycle.markerErr = errors.New("injected marker failure")
	targets, err := expandReaperMailTargets(
		[]string{"list:oncall"},
		func(string) ([]string, error) { return []string{"gastown/witness", "overseer"}, nil },
		func(string) ([]string, error) { return nil, errors.New("unexpected group lookup") },
		func(string) ([]string, error) { return nil, errors.New("unexpected channel lookup") },
	)
	if err != nil {
		t.Fatal(err)
	}
	stored := make(map[string]*mail.Message)
	attempts := make(map[string]int)
	deps := lifecycle.deps()
	deps.send = func(issue *beads.Issue, anomaly reaper.Anomaly) error {
		return sendReaperAnomalyMail(issue, anomaly, targets,
			func(_, _ string) ([]*mail.Message, error) {
				messages := make([]*mail.Message, 0, len(stored))
				for _, message := range stored {
					messages = append(messages, message)
				}
				return messages, nil
			},
			func(message *mail.Message) error {
				attempts[message.To]++
				copy := *message
				copy.ID = "stored-" + message.To
				stored[message.To] = &copy
				return nil
			},
		)
	}
	scan := completeAnomalyScan(testReaperAnomaly("hq-child"))
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, deps); err == nil {
		t.Fatal("first reconcile succeeded, want marker failure")
	}
	lifecycle.markerErr = nil
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, deps); err != nil {
		t.Fatalf("marker retry: %v", err)
	}
	if attempts["gastown/witness"] != 1 || attempts["overseer"] != 1 {
		t.Fatalf("attempts = %#v, want one send per concrete recipient", attempts)
	}
}

func TestReconcileAnomalyScansAnnounceMarkerRetryDoesNotDuplicateMail(t *testing.T) {
	lifecycle := newIsolatedAnomalyLifecycle()
	lifecycle.markerErr = errors.New("injected marker failure")
	stored := make([]*mail.Message, 0, 1)
	attempts := 0
	deps := lifecycle.deps()
	deps.send = func(issue *beads.Issue, anomaly reaper.Anomaly) error {
		return sendReaperAnomalyMail(issue, anomaly, []string{"announce:alerts"},
			func(_, _ string) ([]*mail.Message, error) { return stored, nil },
			func(message *mail.Message) error {
				attempts++
				stored = append(stored, (&mail.BeadsMessage{
					ID: "hq-announce", Title: message.Subject, Description: message.Body,
					Assignee: message.To, Priority: 2, CreatedAt: time.Now(),
					Labels: []string{
						"gt:message", "gt:escalation", "from:reaper", "msg-type:escalation",
						"thread:" + issue.ID, "announce:alerts",
					},
				}).ToMessage())
				return nil
			},
		)
	}
	scan := completeAnomalyScan(testReaperAnomaly("hq-child"))
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, deps); err == nil {
		t.Fatal("first reconcile succeeded, want marker failure")
	}
	lifecycle.markerErr = nil
	if _, err := reconcileAnomalyScans([]reaper.AnomalyScan{scan}, deps); err != nil {
		t.Fatalf("announce marker retry: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("announce attempts = %d, want 1", attempts)
	}
	if len(stored) != 1 || !isStoredReaperNotice(stored[0], "announce:alerts", lifecycle.issues[0].ID) {
		t.Fatalf("stored announce notice = %#v, want valid retry custody", stored)
	}
}

func TestExpandReaperMailTargetsResolvesNestedFanoutAndRejectsCyclesOrEmpty(t *testing.T) {
	t.Run("nested fanout", func(t *testing.T) {
		targets, err := expandReaperMailTargets(
			[]string{"list:oncall", "mayor/"},
			func(address string) ([]string, error) {
				switch address {
				case "list:oncall":
					return []string{"@ops", "mayor/"}, nil
				case "list:backup":
					return []string{"other/witness"}, nil
				default:
					return nil, fmt.Errorf("unexpected list lookup %q", address)
				}
			},
			func(address string) ([]string, error) {
				if address != "@ops" {
					return nil, fmt.Errorf("unexpected group lookup %q", address)
				}
				return []string{"channel:alerts", "list:backup"}, nil
			},
			func(address string) ([]string, error) {
				if address != "channel:alerts" {
					return nil, fmt.Errorf("unexpected channel lookup %q", address)
				}
				return []string{"gastown/witness", "mayor/"}, nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"gastown/witness", "mayor/", "other/witness"}
		if fmt.Sprint(targets) != fmt.Sprint(want) {
			t.Fatalf("targets = %v, want %v", targets, want)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		_, err := expandReaperMailTargets(
			[]string{"list:a"},
			func(string) ([]string, error) { return []string{"@b"}, nil },
			func(string) ([]string, error) { return []string{"list:a"}, nil },
			func(string) ([]string, error) { return nil, errors.New("unexpected channel lookup") },
		)
		if err == nil || !strings.Contains(err.Error(), "cyclic fan-out") {
			t.Fatalf("cycle error = %v, want cyclic fan-out", err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		_, err := expandReaperMailTargets(
			[]string{"channel:empty"},
			func(string) ([]string, error) { return nil, errors.New("unexpected list lookup") },
			func(string) ([]string, error) { return nil, errors.New("unexpected group lookup") },
			func(string) ([]string, error) { return nil, nil },
		)
		if err == nil || !strings.Contains(err.Error(), "has no recipients") {
			t.Fatalf("empty error = %v, want no recipients", err)
		}
	})
}

func TestIsStoredReaperNoticeRequiresMatchingValidNotice(t *testing.T) {
	valid := mail.Message{
		ID:       "hq-message",
		From:     "reaper",
		To:       "gastown/witness",
		Subject:  "[MEDIUM] Reaper anomaly",
		Type:     mail.TypeEscalation,
		ThreadID: "hq-escalation",
	}
	tests := []struct {
		name    string
		message *mail.Message
		want    bool
	}{
		{name: "matching", message: &valid, want: true},
		{name: "nil"},
		{name: "missing ID", message: func() *mail.Message { msg := valid; msg.ID = ""; return &msg }()},
		{name: "wrong sender", message: func() *mail.Message { msg := valid; msg.From = "mayor"; return &msg }()},
		{name: "wrong recipient", message: func() *mail.Message { msg := valid; msg.To = "overseer"; return &msg }()},
		{name: "wrong type", message: func() *mail.Message { msg := valid; msg.Type = mail.TypeReply; return &msg }()},
		{name: "wrong thread", message: func() *mail.Message { msg := valid; msg.ThreadID = "other"; return &msg }()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStoredReaperNotice(tt.message, "gastown/witness", "hq-escalation"); got != tt.want {
				t.Fatalf("isStoredReaperNotice() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsStoredReaperNoticeAcceptsStoredQueueAndRejectsChannelOrigin(t *testing.T) {
	tests := []struct {
		name   string
		target string
		set    func(*mail.Message)
		want   bool
	}{
		{name: "queue", target: "queue:triage", set: func(message *mail.Message) { message.Queue = "triage" }, want: true},
		{name: "channel origin", target: "channel:alerts", set: func(message *mail.Message) { message.Channel = "alerts" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := &mail.Message{
				ID: "hq-message", From: "reaper", To: tt.target,
				Subject: "[MEDIUM] Reaper anomaly", Type: mail.TypeEscalation, ThreadID: "hq-escalation",
			}
			tt.set(message)
			if got := isStoredReaperNotice(message, tt.target, "hq-escalation"); got != tt.want {
				t.Fatalf("stored %s notice accepted = %v, want %v: %#v", tt.name, got, tt.want, message)
			}
		})
	}
}

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
