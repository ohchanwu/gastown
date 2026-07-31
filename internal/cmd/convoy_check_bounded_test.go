package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var hundredConvoyFixture = []convoyListIssue{
	{ID: "hq-cv-001", Title: "Convoy 001", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-002", Title: "Convoy 002", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-003", Title: "Convoy 003", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-004", Title: "Convoy 004", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-005", Title: "Convoy 005", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-006", Title: "Convoy 006", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-007", Title: "Convoy 007", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-008", Title: "Convoy 008", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-009", Title: "Convoy 009", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-010", Title: "Convoy 010", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-011", Title: "Convoy 011", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-012", Title: "Convoy 012", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-013", Title: "Convoy 013", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-014", Title: "Convoy 014", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-015", Title: "Convoy 015", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-016", Title: "Convoy 016", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-017", Title: "Convoy 017", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-018", Title: "Convoy 018", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-019", Title: "Convoy 019", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-020", Title: "Convoy 020", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-021", Title: "Convoy 021", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-022", Title: "Convoy 022", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-023", Title: "Convoy 023", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-024", Title: "Convoy 024", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-025", Title: "Convoy 025", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-026", Title: "Convoy 026", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-027", Title: "Convoy 027", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-028", Title: "Convoy 028", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-029", Title: "Convoy 029", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-030", Title: "Convoy 030", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-031", Title: "Convoy 031", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-032", Title: "Convoy 032", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-033", Title: "Convoy 033", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-034", Title: "Convoy 034", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-035", Title: "Convoy 035", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-036", Title: "Convoy 036", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-037", Title: "Convoy 037", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-038", Title: "Convoy 038", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-039", Title: "Convoy 039", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-040", Title: "Convoy 040", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-041", Title: "Convoy 041", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-042", Title: "Convoy 042", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-043", Title: "Convoy 043", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-044", Title: "Convoy 044", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-045", Title: "Convoy 045", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-046", Title: "Convoy 046", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-047", Title: "Convoy 047", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-048", Title: "Convoy 048", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-049", Title: "Convoy 049", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-050", Title: "Convoy 050", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-051", Title: "Convoy 051", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-052", Title: "Convoy 052", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-053", Title: "Convoy 053", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-054", Title: "Convoy 054", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-055", Title: "Convoy 055", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-056", Title: "Convoy 056", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-057", Title: "Convoy 057", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-058", Title: "Convoy 058", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-059", Title: "Convoy 059", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-060", Title: "Convoy 060", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-061", Title: "Convoy 061", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-062", Title: "Convoy 062", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-063", Title: "Convoy 063", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-064", Title: "Convoy 064", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-065", Title: "Convoy 065", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-066", Title: "Convoy 066", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-067", Title: "Convoy 067", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-068", Title: "Convoy 068", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-069", Title: "Convoy 069", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-070", Title: "Convoy 070", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-071", Title: "Convoy 071", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-072", Title: "Convoy 072", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-073", Title: "Convoy 073", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-074", Title: "Convoy 074", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-075", Title: "Convoy 075", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-076", Title: "Convoy 076", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-077", Title: "Convoy 077", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-078", Title: "Convoy 078", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-079", Title: "Convoy 079", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-080", Title: "Convoy 080", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-081", Title: "Convoy 081", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-082", Title: "Convoy 082", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-083", Title: "Convoy 083", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-084", Title: "Convoy 084", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-085", Title: "Convoy 085", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-086", Title: "Convoy 086", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-087", Title: "Convoy 087", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-088", Title: "Convoy 088", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-089", Title: "Convoy 089", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-090", Title: "Convoy 090", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-091", Title: "Convoy 091", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-092", Title: "Convoy 092", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-093", Title: "Convoy 093", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-094", Title: "Convoy 094", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-095", Title: "Convoy 095", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-096", Title: "Convoy 096", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-097", Title: "Convoy 097", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-098", Title: "Convoy 098", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-099", Title: "Convoy 099", Status: "open", Labels: []string{"gt:convoy"}},
	{ID: "hq-cv-100", Title: "Convoy 100", Status: "open", Labels: []string{"gt:convoy"}},
}

func TestCheckConvoysStableOrderAndFailClosedIsolation(t *testing.T) {
	convoys := []convoyListIssue{
		{ID: "hq-cv-first", Status: "open"},
		{ID: "hq-cv-error", Status: "open"},
		{ID: "hq-cv-second", Status: "open"},
		{ID: "hq-cv-unknown", Status: "open"},
		{ID: "hq-cv-active", Status: "open"},
	}
	delays := map[string]time.Duration{
		"hq-cv-first": 30 * time.Millisecond, "hq-cv-error": 1 * time.Millisecond,
		"hq-cv-second": 20 * time.Millisecond, "hq-cv-unknown": 2 * time.Millisecond,
		"hq-cv-active": 10 * time.Millisecond,
	}

	for run := 0; run < 5; run++ {
		var closed []string
		summary := checkConvoys(
			context.Background(), convoys, true,
			func(ctx context.Context, convoy convoyListIssue) ([]trackedIssueInfo, error) {
				timer := time.NewTimer(delays[convoy.ID])
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
				}
				switch convoy.ID {
				case "hq-cv-error":
					return nil, errors.New("private lookup detail")
				case "hq-cv-unknown":
					return []trackedIssueInfo{
						{ID: "gt-active-before-missing", Status: "open"},
						{ID: "gt-missing", Status: trackedStatusUnknown},
					}, nil
				case "hq-cv-active":
					return []trackedIssueInfo{{ID: "gt-active", Status: "open"}}, nil
				default:
					return []trackedIssueInfo{{ID: "gt-done", Status: "closed"}}, nil
				}
			},
			func(convoy convoyListIssue, _ []trackedIssueInfo, _ bool) error {
				closed = append(closed, convoy.ID)
				return nil
			},
		)

		if !reflect.DeepEqual(closed, []string{"hq-cv-first", "hq-cv-second"}) {
			t.Fatalf("run %d close order = %v", run, closed)
		}
		wantErrors := []convoyCheckError{
			{ConvoyID: "hq-cv-error", Code: convoyCheckErrorLookup},
			{ConvoyID: "hq-cv-unknown", Code: convoyCheckErrorUncertain},
		}
		if summary.Checked != 5 || summary.EligibleClosed != 2 || summary.SkippedUncertain != 2 || summary.TimedOut {
			t.Fatalf("run %d summary = %+v", run, summary)
		}
		if !reflect.DeepEqual(summary.Errors, wantErrors) {
			t.Fatalf("run %d errors = %#v, want %#v", run, summary.Errors, wantErrors)
		}
	}
}

func TestCheckConvoysCancellationKeepsEarlierProvenClosure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	convoys := []convoyListIssue{
		{ID: "hq-cv-complete", Status: "open"},
		{ID: "hq-cv-wait-1", Status: "open"},
		{ID: "hq-cv-wait-2", Status: "open"},
	}
	firstDone := make(chan struct{})
	var once sync.Once
	var closed []string

	summaryCh := make(chan convoyCheckSummary, 1)
	go func() {
		summaryCh <- checkConvoys(
			ctx, convoys, false,
			func(ctx context.Context, convoy convoyListIssue) ([]trackedIssueInfo, error) {
				if convoy.ID == "hq-cv-complete" {
					once.Do(func() { close(firstDone) })
					return []trackedIssueInfo{{ID: "gt-done", Status: "closed"}}, nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
			func(convoy convoyListIssue, _ []trackedIssueInfo, _ bool) error {
				closed = append(closed, convoy.ID)
				return nil
			},
		)
	}()
	<-firstDone
	time.Sleep(10 * time.Millisecond)
	cancel()
	summary := <-summaryCh

	if !reflect.DeepEqual(closed, []string{"hq-cv-complete"}) {
		t.Fatalf("closed = %v, want only proven complete convoy", closed)
	}
	if summary.Checked != 1 || summary.EligibleClosed != 1 || summary.SkippedUncertain != 2 || !summary.TimedOut {
		t.Fatalf("summary = %+v", summary)
	}
	wantErrors := []convoyCheckError{
		{ConvoyID: "hq-cv-wait-1", Code: convoyCheckErrorTimeout},
		{ConvoyID: "hq-cv-wait-2", Code: convoyCheckErrorTimeout},
	}
	if !reflect.DeepEqual(summary.Errors, wantErrors) {
		t.Fatalf("errors = %#v, want %#v", summary.Errors, wantErrors)
	}
}

func TestConvoyCheckSummaryMachineFields(t *testing.T) {
	summary := convoyCheckSummary{
		Checked: 3, EligibleClosed: 1, SkippedUncertain: 2, TimedOut: true,
		Errors: []convoyCheckError{{ConvoyID: "hq-cv-2", Code: convoyCheckErrorLookup}},
	}
	got, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"checked":3,"eligible_closed":1,"skipped_uncertain":2,"timed_out":true,"errors":[{"convoy_id":"hq-cv-2","code":"lookup_failed"}]}`
	if string(got) != want {
		t.Fatalf("machine output = %s, want %s", got, want)
	}
}

func TestConvoyIssueDetailsCacheLoadsSharedIssueOnce(t *testing.T) {
	var calls atomic.Int32
	cache := newConvoyIssueDetailsCache(func(ids []string) map[string]*issueDetails {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return map[string]*issueDetails{
			"gt-shared": {ID: "gt-shared", Status: "closed"},
		}
	})

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := cache.get(context.Background(), []string{"gt-shared"})
			if got["gt-shared"] == nil || got["gt-shared"].Status != "closed" {
				t.Errorf("cached detail = %#v", got["gt-shared"])
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("issue loader called %d times, want 1", calls.Load())
	}
}

func TestCheckAndCloseCompletedConvoysBoundsRelationshipQueries(t *testing.T) {
	if len(hundredConvoyFixture) != 100 {
		t.Fatalf("fixture has %d convoys, want 100", len(hundredConvoyFixture))
	}

	var active atomic.Int32
	var maxObserved atomic.Int32
	lookup := func(ctx context.Context, _ convoyListIssue) ([]trackedIssueInfo, error) {
		inFlight := active.Add(1)
		defer active.Add(-1)
		for {
			max := maxObserved.Load()
			if inFlight <= max || maxObserved.CompareAndSwap(max, inFlight) {
				break
			}
		}
		timer := time.NewTimer(50 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, nil
		}
	}

	start := time.Now()
	results, timedOut := lookupConvoysBounded(context.Background(), hundredConvoyFixture, lookup)
	elapsed := time.Since(start)
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	for i, result := range results {
		if !result.done || result.convoy.ID != hundredConvoyFixture[i].ID {
			t.Fatalf("result %d = %#v, want convoy %s", i, result, hundredConvoyFixture[i].ID)
		}
	}
	t.Logf("elapsed=%s max_in_flight=%d", elapsed.Round(time.Millisecond), maxObserved.Load())
	if elapsed >= 3*time.Second {
		t.Fatalf("100 50ms relationship lookups took %s, want under 3s", elapsed.Round(time.Millisecond))
	}
	if maxObserved.Load() != convoyLookupConcurrency {
		t.Fatalf("max relationship lookups = %d, want %d", maxObserved.Load(), convoyLookupConcurrency)
	}
}
