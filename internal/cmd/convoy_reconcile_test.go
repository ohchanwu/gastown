package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlanConvoyReconciliationClassifiesAndOrdersDeterministically(t *testing.T) {
	convoys := []convoyListIssue{
		{ID: "hq-cv-unresolved", Status: "open"},
		{ID: "hq-cv-duplicate-b", Status: "open"},
		{ID: "hq-cv-active", Status: "open"},
		{ID: "hq-cv-blocked", Status: "open"},
		{ID: "hq-cv-empty", Status: "open"},
		{ID: "hq-cv-complete", Status: "open"},
		{ID: "hq-cv-duplicate-a", Status: "open"},
		{ID: "hq-cv-lookup", Status: "open"},
	}
	tracked := map[string][]trackedIssueInfo{
		"hq-cv-unresolved":  {{ID: "gt-missing", Status: trackedStatusUnknown}},
		"hq-cv-duplicate-b": {{ID: "gt-shared-2", Status: "closed"}, {ID: "gt-shared-1", Status: "closed"}},
		"hq-cv-active":      {{ID: "gt-active", Status: "in_progress"}},
		"hq-cv-blocked":     {{ID: "gt-blocked", Status: "closed", Blocked: true}},
		"hq-cv-complete":    {{ID: "gt-terminal", Status: "closed"}},
		"hq-cv-duplicate-a": {{ID: "gt-shared-1", Status: "closed"}, {ID: "gt-shared-2", Status: "closed"}},
	}

	plan := planConvoyReconciliation(context.Background(), convoys, func(_ context.Context, convoy convoyListIssue) ([]trackedIssueInfo, error) {
		if convoy.ID == "hq-cv-lookup" {
			return nil, errors.New("private path and subprocess output")
		}
		return tracked[convoy.ID], nil
	})

	want := convoyReconcilePlan{Mode: "dry-run", Scanned: 8, LookupConcurrency: 4, DeadlineSeconds: 30, Entries: []convoyReconcileEntry{
		{ConvoyID: "hq-cv-active", Action: "preserve", Reason: "active_or_blocked", TrackedCount: 1, TrackedIDs: []string{"gt-active"}},
		{ConvoyID: "hq-cv-blocked", Action: "preserve", Reason: "active_or_blocked", TrackedCount: 1, TrackedIDs: []string{"gt-blocked"}},
		{ConvoyID: "hq-cv-complete", Action: "close", Reason: "all_tracked_terminal", TrackedCount: 1, TrackedIDs: []string{"gt-terminal"}, RollbackStatus: "open"},
		{ConvoyID: "hq-cv-duplicate-a", Action: "preserve", Reason: "duplicate_work_set", TrackedCount: 2, TrackedIDs: []string{"gt-shared-1", "gt-shared-2"}},
		{ConvoyID: "hq-cv-duplicate-b", Action: "preserve", Reason: "duplicate_work_set", TrackedCount: 2, TrackedIDs: []string{"gt-shared-1", "gt-shared-2"}},
		{ConvoyID: "hq-cv-empty", Action: "review", Reason: "empty_work_set", TrackedIDs: []string{}},
		{ConvoyID: "hq-cv-lookup", Action: "repair", Reason: "lookup_failed", TrackedIDs: []string{}},
		{ConvoyID: "hq-cv-unresolved", Action: "repair", Reason: "unresolved_reference", TrackedCount: 1, TrackedIDs: []string{"gt-missing"}},
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %#v, want %#v", plan, want)
	}

	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || strings.Contains(string(encoded), "private path") || strings.Contains(string(encoded), "subprocess output") {
		t.Fatalf("machine audit leaked unsanitized lookup error: %s", encoded)
	}
}

func TestApplyConvoyReconciliationAuditsBeforeMutationAndClosesOnlyEligible(t *testing.T) {
	plan := convoyReconcilePlan{Mode: "apply", Scanned: 2, Entries: []convoyReconcileEntry{
		{ConvoyID: "hq-cv-close", Action: "close", Reason: "all_tracked_terminal", TrackedIDs: []string{"gt-done"}, RollbackStatus: "open"},
		{ConvoyID: "hq-cv-keep", Action: "preserve", Reason: "active_or_blocked", TrackedIDs: []string{"gt-open"}},
	}}
	var events []string
	err := applyConvoyReconciliation(plan,
		func(value any) error {
			switch got := value.(type) {
			case convoyReconcilePlan:
				events = append(events, "audit:"+got.Entries[0].ConvoyID)
			case convoyReconcileReceipt:
				events = append(events, "receipt:"+got.Outcomes[0].Status)
			}
			return nil
		},
		func(convoyReconcileEntry) string { return "" },
		func(convoyID string) string {
			events = append(events, "close:"+convoyID)
			return "closed"
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"audit:hq-cv-close", "close:hq-cv-close", "receipt:closed"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestApplyConvoyReconciliationDryRunNeverMutates(t *testing.T) {
	plan := convoyReconcilePlan{Mode: "dry-run", Scanned: 1, Entries: []convoyReconcileEntry{{
		ConvoyID: "hq-cv-close", Action: "close", Reason: "all_tracked_terminal", TrackedIDs: []string{"gt-done"}, RollbackStatus: "open",
	}}}
	closed := false
	if err := applyConvoyReconciliation(plan, func(any) error { return nil }, func(convoyReconcileEntry) string { return "" }, func(string) string {
		closed = true
		return "closed"
	}); err != nil {
		t.Fatal(err)
	}
	if closed {
		t.Fatal("dry-run mutated convoy state")
	}
}

func TestApplyConvoyReconciliationAuditFailurePreventsMutation(t *testing.T) {
	plan := convoyReconcilePlan{Mode: "apply", Scanned: 1, Entries: []convoyReconcileEntry{{
		ConvoyID: "hq-cv-close", Action: "close", Reason: "all_tracked_terminal", TrackedCount: 1, TrackedIDs: []string{"gt-done"}, RollbackStatus: "open",
	}}}
	closed := false
	err := applyConvoyReconciliation(plan, func(any) error {
		return errors.New("audit unavailable")
	}, func(convoyReconcileEntry) string { return "" }, func(string) string {
		closed = true
		return "closed"
	})
	if err == nil || closed {
		t.Fatalf("audit failure err = %v, closed = %v; want error without mutation", err, closed)
	}
}

func TestApplyConvoyReconciliationSanitizesCloseFailure(t *testing.T) {
	plan := convoyReconcilePlan{Mode: "apply", Scanned: 1, Entries: []convoyReconcileEntry{{
		ConvoyID: "hq-cv-close", Action: "close", Reason: "all_tracked_terminal", TrackedCount: 1, TrackedIDs: []string{"gt-done"}, RollbackStatus: "open",
	}}}
	var receipt convoyReconcileReceipt
	err := applyConvoyReconciliation(plan, func(value any) error {
		if got, ok := value.(convoyReconcileReceipt); ok {
			receipt = got
		}
		return nil
	}, func(convoyReconcileEntry) string { return "" }, func(string) string {
		return "close_failed"
	})
	if got, want := fmt.Sprint(err), "reconcile_apply_incomplete"; got != want {
		t.Fatalf("close error = %q, want %q", got, want)
	}
	if want := (convoyReconcileOutcome{ConvoyID: "hq-cv-close", Status: "close_failed"}); len(receipt.Outcomes) != 1 || receipt.Outcomes[0] != want {
		t.Fatalf("receipt = %#v, want outcome %#v", receipt, want)
	}
}

func TestApplyConvoyReconciliationRecordsCloseBeforeExportFailure(t *testing.T) {
	plan := convoyReconcilePlan{Mode: "apply", Scanned: 1, Entries: []convoyReconcileEntry{{
		ConvoyID: "hq-cv-close", Action: "close", Reason: "all_tracked_terminal", TrackedCount: 1, TrackedIDs: []string{"gt-done"}, RollbackStatus: "open",
	}}}
	var receipt convoyReconcileReceipt
	err := applyConvoyReconciliation(plan, func(value any) error {
		if got, ok := value.(convoyReconcileReceipt); ok {
			receipt = got
		}
		return nil
	}, func(convoyReconcileEntry) string { return "" }, func(string) string {
		return "closed_export_failed"
	})
	if got, want := fmt.Sprint(err), "reconcile_apply_incomplete"; got != want {
		t.Fatalf("apply error = %q, want %q", got, want)
	}
	if want := (convoyReconcileOutcome{ConvoyID: "hq-cv-close", Status: "closed_export_failed", RollbackStatus: "open"}); len(receipt.Outcomes) != 1 || receipt.Outcomes[0] != want {
		t.Fatalf("receipt = %#v, want outcome %#v", receipt, want)
	}
}

func TestApplyConvoyReconciliationRevalidatesImmediatelyBeforeClose(t *testing.T) {
	plan := convoyReconcilePlan{Mode: "apply", Scanned: 1, Entries: []convoyReconcileEntry{{
		ConvoyID: "hq-cv-close", Action: "close", Reason: "all_tracked_terminal", TrackedCount: 1, TrackedIDs: []string{"gt-done"}, RollbackStatus: "open",
	}}}
	closed := false
	var receipt convoyReconcileReceipt
	err := applyConvoyReconciliation(plan, func(value any) error {
		if got, ok := value.(convoyReconcileReceipt); ok {
			receipt = got
		}
		return nil
	}, func(convoyReconcileEntry) string {
		return "state_changed"
	}, func(string) string {
		closed = true
		return "closed"
	})
	if got, want := fmt.Sprint(err), "reconcile_apply_incomplete"; got != want || closed {
		t.Fatalf("apply err = %q, closed = %v; want %q without mutation", got, closed, want)
	}
	if want := (convoyReconcileOutcome{ConvoyID: "hq-cv-close", Status: "skipped", Reason: "state_changed"}); len(receipt.Outcomes) != 1 || receipt.Outcomes[0] != want {
		t.Fatalf("receipt = %#v, want outcome %#v", receipt, want)
	}
}

func TestConvoyReconcileRevalidationFailsClosedOnStateOrDuplicateChange(t *testing.T) {
	original := convoyReconcileEntry{ConvoyID: "hq-cv-close", Action: "close", Reason: "all_tracked_terminal", TrackedIDs: []string{"gt-done"}}
	tests := []struct {
		name  string
		fresh convoyReconcileEntry
		want  string
	}{
		{name: "unchanged", fresh: convoyReconcileEntry{ConvoyID: "hq-cv-close", Action: "close", TrackedIDs: []string{"gt-done"}}},
		{name: "issue reopened", fresh: convoyReconcileEntry{ConvoyID: "hq-cv-close", Action: "preserve", Reason: "active_or_blocked", TrackedIDs: []string{"gt-done"}}, want: "state_changed"},
		{name: "duplicate appeared", fresh: convoyReconcileEntry{ConvoyID: "hq-cv-close", Action: "preserve", Reason: "duplicate_work_set", TrackedIDs: []string{"gt-done"}}, want: "state_changed"},
		{name: "work set changed", fresh: convoyReconcileEntry{ConvoyID: "hq-cv-close", Action: "close", TrackedIDs: []string{"gt-done", "gt-new"}}, want: "state_changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fresh := convoyReconcilePlan{Entries: []convoyReconcileEntry{tt.fresh}}
			if got := convoyReconcileRevalidationReason(original, fresh); got != tt.want {
				t.Fatalf("revalidation reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlanConvoyReconciliationBoundsHundredLookups(t *testing.T) {
	convoys := make([]convoyListIssue, 100)
	for i := range convoys {
		convoys[i] = convoyListIssue{ID: fmt.Sprintf("hq-cv-%03d", i), Status: "open"}
	}
	var active atomic.Int32
	var maximum atomic.Int32
	started := time.Now()
	plan := planConvoyReconciliation(context.Background(), convoys, func(ctx context.Context, _ convoyListIssue) ([]trackedIssueInfo, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
			return []trackedIssueInfo{{ID: "gt-done", Status: "closed"}}, nil
		}
	})
	if len(plan.Entries) != 100 {
		t.Fatalf("entries = %d, want 100", len(plan.Entries))
	}
	if got := maximum.Load(); got > convoyLookupConcurrency {
		t.Fatalf("maximum concurrent lookups = %d, want <= %d", got, convoyLookupConcurrency)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("100-convoy reconciliation took %s", elapsed)
	}
}

func TestConvoyReconcileCommandDefaultsToDryRunAndRequiresApply(t *testing.T) {
	flag := convoyReconcileCmd.Flags().Lookup("apply")
	if flag == nil {
		t.Fatal("convoy reconcile must expose --apply")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--apply default = %q, want false", flag.DefValue)
	}
	found := false
	for _, command := range convoyCmd.Commands() {
		if command.Name() == "reconcile" {
			found = true
		}
	}
	if !found {
		t.Fatal("convoy reconcile command is not registered")
	}
}
