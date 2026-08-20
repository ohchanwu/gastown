package mail

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
)

type threadSearchStore struct {
	beadsdk.Storage
	issues []*beadsdk.Issue
	err    error
	calls  int
	query  string
	filter beadsdk.IssueFilter
}

func (s *threadSearchStore) SearchIssues(_ context.Context, query string, filter beadsdk.IssueFilter) ([]*beadsdk.Issue, error) {
	s.calls++
	s.query = query
	s.filter = filter
	return s.issues, s.err
}

func TestMailboxStoreListByThread(t *testing.T) {
	base := time.Date(2026, time.August, 20, 0, 0, 0, 0, time.UTC)
	store := &threadSearchStore{issues: []*beadsdk.Issue{
		{ID: "msg-z", Title: "message z", Assignee: "gastown/Toast", Status: beadsdk.StatusClosed, CreatedAt: base, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
		{ID: "msg-hooked", Title: "hooked message", Assignee: "gastown/Toast", Status: beadsdk.Status("hooked"), CreatedAt: base.Add(time.Minute), Labels: []string{"gt:message", "thread:thread-target", "from:mayor/", "read"}},
		{ID: "msg-wisp", Title: "ephemeral message", Assignee: "gastown/Toast", Status: beadsdk.StatusOpen, CreatedAt: base.Add(2 * time.Minute), Ephemeral: true, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
		{ID: "msg-a", Title: "message a", Assignee: "gastown/Toast", Status: beadsdk.StatusOpen, CreatedAt: base, Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"}},
	}}
	t.Setenv("PATH", t.TempDir())
	m := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store)

	messages, err := m.ListByThread("thread-target")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if store.calls != 1 || store.query != "" {
		t.Fatalf("SearchIssues calls/query = %d/%q, want 1/empty", store.calls, store.query)
	}
	wantFilter := beadsdk.IssueFilter{
		Labels: []string{"gt:message", "thread:thread-target"},
		Limit:  0,
	}
	if !reflect.DeepEqual(store.filter, wantFilter) {
		t.Fatalf("SearchIssues filter = %#v, want %#v", store.filter, wantFilter)
	}
	if len(messages) != 4 {
		t.Fatalf("ListByThread returned %d messages, want 4", len(messages))
	}
	wantIDs := []string{"msg-a", "msg-z", "msg-hooked", "msg-wisp"}
	for i, want := range wantIDs {
		if messages[i].ID != want {
			t.Fatalf("message[%d].ID = %q, want %q", i, messages[i].ID, want)
		}
	}
	if !messages[1].Read || !messages[2].Read {
		t.Fatalf("closed/read-labelled messages not preserved: %#v", messages)
	}
	if !messages[3].Wisp {
		t.Fatal("ephemeral message was not preserved")
	}
}

func TestMailboxStoreListByThreadRejectsInvalidMessage(t *testing.T) {
	tests := []struct {
		name  string
		issue *beadsdk.Issue
	}{
		{name: "nil"},
		{name: "missing route", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Labels: []string{"gt:message", "thread:thread-target", "from:mayor/"},
		}},
		{name: "wrong thread", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "gastown/Toast",
			Labels: []string{"gt:message", "thread:thread-other", "from:mayor/"},
		}},
		{name: "queue assignee mismatch", issue: &beadsdk.Issue{
			ID: "msg-1", Title: "message", Assignee: "queue:other",
			Labels: []string{"gt:message", "thread:thread-target", "from:mayor/", "queue:triage"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &threadSearchStore{issues: []*beadsdk.Issue{tt.issue}}
			t.Setenv("PATH", t.TempDir())
			m := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store)
			if _, err := m.ListByThread("thread-target"); err == nil {
				t.Fatal("ListByThread succeeded, want invalid stored message error")
			}
		})
	}
}

func TestMailboxStoreListByThreadAcceptsStoredQueueAndChannelRoutes(t *testing.T) {
	store := &threadSearchStore{issues: []*beadsdk.Issue{
		{
			ID: "msg-queue", Title: "queue message", Assignee: "queue:triage",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "queue:triage"},
		},
		{
			ID: "msg-channel", Title: "channel message", Assignee: "channel:alerts",
			Labels: []string{"gt:message", "thread:thread-target", "from:reaper", "channel:alerts"},
		},
	}}
	t.Setenv("PATH", t.TempDir())
	messages, err := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store).ListByThread("thread-target")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if len(messages) != 2 || messages[0].Channel != "alerts" || messages[1].Queue != "triage" {
		t.Fatalf("messages = %#v, want channel and queue routes", messages)
	}
}

func TestMailboxStoreListByThreadEmpty(t *testing.T) {
	store := &threadSearchStore{issues: []*beadsdk.Issue{}}
	t.Setenv("PATH", t.TempDir())
	messages, err := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store).ListByThread("thread-missing")
	if err != nil {
		t.Fatalf("ListByThread: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("ListByThread returned %d messages, want 0", len(messages))
	}
}

func TestMailboxStoreListByThreadReturnsStoreFailureWithoutCLIFallback(t *testing.T) {
	wantErr := errors.New("search failed")
	store := &threadSearchStore{err: wantErr}
	t.Setenv("PATH", t.TempDir())
	m := NewMailboxBeadsWithStore("gastown/Toast", t.TempDir(), store)

	_, err := m.ListByThread("thread-target")
	if err == nil {
		t.Fatal("ListByThread succeeded, want store error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("ListByThread error = %v, want wrapped %v", err, wantErr)
	}
	if got := err.Error(); got != "store list thread: search failed" {
		t.Fatalf("ListByThread error = %q, want store context", got)
	}
	if store.calls != 1 {
		t.Fatalf("SearchIssues called %d times, want 1", store.calls)
	}
}
