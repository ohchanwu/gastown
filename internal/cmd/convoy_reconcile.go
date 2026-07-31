package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

const (
	reconcileActionClose    = "close"
	reconcileActionPreserve = "preserve"
	reconcileActionRepair   = "repair"
	reconcileActionReview   = "review"
)

type convoyReconcileEntry struct {
	ConvoyID       string   `json:"convoy_id"`
	Action         string   `json:"action"`
	Reason         string   `json:"reason"`
	TrackedCount   int      `json:"tracked_count"`
	TrackedIDs     []string `json:"tracked_ids"`
	RollbackStatus string   `json:"rollback_status,omitempty"`
}

type convoyReconcilePlan struct {
	Mode              string                 `json:"mode"`
	Scanned           int                    `json:"scanned"`
	LookupConcurrency int                    `json:"lookup_concurrency"`
	DeadlineSeconds   int                    `json:"deadline_seconds"`
	Entries           []convoyReconcileEntry `json:"entries"`
}

type convoyReconcileOutcome struct {
	ConvoyID       string `json:"convoy_id"`
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	RollbackStatus string `json:"rollback_status,omitempty"`
}

type convoyReconcileReceipt struct {
	Mode     string                   `json:"mode"`
	Outcomes []convoyReconcileOutcome `json:"outcomes"`
}

var convoyReconcileApply bool

var convoyReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "Audit open convoys and optionally close proven-complete trackers",
	Long: `Classify every open convoy using its tracked work set.

The default is a read-only dry-run. Use --apply to close only convoys whose
non-empty work sets are uniquely tracked and entirely terminal. The complete
sanitized JSON audit is printed before any mutation.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE:         runConvoyReconcile,
}

func init() {
	convoyReconcileCmd.Flags().BoolVar(&convoyReconcileApply, "apply", false, "Apply only the audited proven-complete closures")
	convoyCmd.AddCommand(convoyReconcileCmd)
}

func canonicalTrackedIDs(tracked []trackedIssueInfo) []string {
	seen := make(map[string]struct{}, len(tracked))
	ids := make([]string, 0, len(tracked))
	for _, issue := range tracked {
		if _, ok := seen[issue.ID]; ok {
			continue
		}
		seen[issue.ID] = struct{}{}
		ids = append(ids, issue.ID)
	}
	sort.Strings(ids)
	return ids
}

func planConvoyReconciliation(
	ctx context.Context,
	convoys []convoyListIssue,
	lookup func(context.Context, convoyListIssue) ([]trackedIssueInfo, error),
) convoyReconcilePlan {
	ordered := append([]convoyListIssue(nil), convoys...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	results, _ := lookupConvoysBounded(ctx, ordered, lookup)
	plan := convoyReconcilePlan{
		Mode:              "dry-run",
		Scanned:           len(ordered),
		LookupConcurrency: convoyLookupConcurrency,
		DeadlineSeconds:   int(convoyCheckDeadline.Seconds()),
		Entries:           make([]convoyReconcileEntry, 0, len(ordered)),
	}

	workSets := make(map[string][]int)
	for _, result := range results {
		ids := canonicalTrackedIDs(result.tracked)
		entry := convoyReconcileEntry{ConvoyID: result.convoy.ID, TrackedCount: len(ids), TrackedIDs: ids}
		switch {
		case !result.done || errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded):
			entry.Action, entry.Reason = reconcileActionRepair, "timeout"
		case result.err != nil:
			entry.Action, entry.Reason = reconcileActionRepair, "lookup_failed"
		case ensureKnownConvoyStatus(result.convoy.Status) != nil:
			entry.Action, entry.Reason = reconcileActionRepair, "invalid_state"
		case len(result.tracked) == 0:
			entry.Action, entry.Reason = reconcileActionReview, "empty_work_set"
		default:
			blocked := false
			for _, issue := range result.tracked {
				blocked = blocked || issue.Blocked
			}
			eligible, uncertain := trackedIssuesCompletion(result.tracked)
			switch {
			case uncertain:
				entry.Action, entry.Reason = reconcileActionRepair, "unresolved_reference"
			case blocked:
				entry.Action, entry.Reason = reconcileActionPreserve, "active_or_blocked"
			case eligible:
				entry.Action, entry.Reason, entry.RollbackStatus = reconcileActionClose, "all_tracked_terminal", convoyStatusOpen
			default:
				entry.Action, entry.Reason = reconcileActionPreserve, "active_or_blocked"
			}
		}
		plan.Entries = append(plan.Entries, entry)
		if len(ids) > 0 && result.done && result.err == nil {
			key := strings.Join(ids, "\x00")
			workSets[key] = append(workSets[key], len(plan.Entries)-1)
		}
	}

	for _, indexes := range workSets {
		if len(indexes) < 2 {
			continue
		}
		for _, index := range indexes {
			entry := &plan.Entries[index]
			entry.Action, entry.Reason, entry.RollbackStatus = reconcileActionPreserve, "duplicate_work_set", ""
		}
	}
	return plan
}

func applyConvoyReconciliation(
	plan convoyReconcilePlan,
	emit func(any) error,
	revalidate func(convoyReconcileEntry) string,
	mutate func(string) string,
) error {
	if err := emit(plan); err != nil {
		return errors.New("audit_write_failed")
	}
	if plan.Mode != "apply" {
		return nil
	}
	receipt := convoyReconcileReceipt{Mode: "apply", Outcomes: []convoyReconcileOutcome{}}
	incomplete := false
	for _, entry := range plan.Entries {
		if entry.Action != reconcileActionClose {
			continue
		}
		if reason := revalidate(entry); reason != "" {
			if reason != "state_changed" && reason != "revalidation_failed" {
				reason = "revalidation_failed"
			}
			receipt.Outcomes = append(receipt.Outcomes, convoyReconcileOutcome{ConvoyID: entry.ConvoyID, Status: "skipped", Reason: reason})
			incomplete = true
			continue
		}
		status := mutate(entry.ConvoyID)
		if status != "closed" && status != "close_failed" && status != "closed_export_failed" && status != "closed_notification_failed" {
			status = "close_failed"
		}
		outcome := convoyReconcileOutcome{ConvoyID: entry.ConvoyID, Status: status}
		if status == "closed" || status == "closed_export_failed" || status == "closed_notification_failed" {
			outcome.RollbackStatus = convoyStatusOpen
		} else {
			incomplete = true
		}
		if status == "closed_export_failed" || status == "closed_notification_failed" {
			incomplete = true
		}
		receipt.Outcomes = append(receipt.Outcomes, outcome)
	}
	if err := emit(receipt); err != nil {
		return errors.New("receipt_write_failed")
	}
	if incomplete {
		return errors.New("reconcile_apply_incomplete")
	}
	return nil
}

func revalidateConvoyReconcileEntry(ctx context.Context, townBeads string, original convoyReconcileEntry) string {
	convoys, err := listConvoyIssuesContext(ctx, townBeads, convoyStatusOpen, false)
	if err != nil {
		return "revalidation_failed"
	}
	cache := newConvoyIssueDetailsCacheContext(getIssueDetailsBatchContext)
	fresh := planConvoyReconciliation(ctx, convoys, func(ctx context.Context, convoy convoyListIssue) ([]trackedIssueInfo, error) {
		return getTrackedIssuesCached(ctx, townBeads, convoy.ID, cache)
	})
	return convoyReconcileRevalidationReason(original, fresh)
}

func convoyReconcileRevalidationReason(original convoyReconcileEntry, fresh convoyReconcilePlan) string {
	for _, entry := range fresh.Entries {
		if entry.ConvoyID == original.ConvoyID && entry.Action == reconcileActionClose && slices.Equal(entry.TrackedIDs, original.TrackedIDs) {
			return ""
		}
	}
	return "state_changed"
}

func runConvoyReconcile(cmd *cobra.Command, _ []string) error {
	townBeads, err := getTownBeadsDir()
	if err != nil {
		return errors.New("workspace_unavailable")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), convoyCheckDeadline)
	defer cancel()
	convoys, err := listConvoyIssuesContext(ctx, townBeads, convoyStatusOpen, false)
	if err != nil {
		return errors.New("listing_convoys: lookup_failed")
	}
	cache := newConvoyIssueDetailsCacheContext(getIssueDetailsBatchContext)
	plan := planConvoyReconciliation(ctx, convoys, func(ctx context.Context, convoy convoyListIssue) ([]trackedIssueInfo, error) {
		return getTrackedIssuesCached(ctx, townBeads, convoy.ID, cache)
	})
	if convoyReconcileApply {
		plan.Mode = "apply"
	}
	titles := make(map[string]string, len(convoys))
	for _, convoy := range convoys {
		titles[convoy.ID] = convoy.Title
	}
	return applyConvoyReconciliation(plan, func(value any) error {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	}, func(entry convoyReconcileEntry) string {
		return revalidateConvoyReconcileEntry(ctx, townBeads, entry)
	}, func(convoyID string) string {
		if err := runBdCommandContext(ctx, BdCmd("close", convoyID, "-r", "All tracked issues completed").Dir(townBeads).WithAutoCommit()); err != nil {
			return "close_failed"
		}
		if err := persistTownBeadsJSONLContext(ctx, townBeads); err != nil {
			return "closed_export_failed"
		}
		if err := notifyConvoyCompletionContextWithWarnings(ctx, townBeads, convoyID, titles[convoyID], false); err != nil {
			return "closed_notification_failed"
		}
		return "closed"
	})
}
