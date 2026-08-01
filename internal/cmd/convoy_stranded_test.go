package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestIsReadyIssue_BlockingAndStatus(t *testing.T) {
	tests := []struct {
		name string
		in   trackedIssueInfo
		want bool
	}{
		{
			name: "closed issue never ready",
			in: trackedIssueInfo{
				Status:  "closed",
				Blocked: false,
			},
			want: false,
		},
		{
			name: "unknown issue never ready",
			in: trackedIssueInfo{
				Status:  trackedStatusUnknown,
				Blocked: false,
			},
			want: false,
		},
		{
			name: "blank status never ready",
			in: trackedIssueInfo{
				Status:  " ",
				Blocked: false,
			},
			want: false,
		},
		{
			name: "blocked open issue not ready",
			in: trackedIssueInfo{
				Status:  "open",
				Blocked: true,
			},
			want: false,
		},
		{
			name: "open unassigned issue ready",
			in: trackedIssueInfo{
				Status:  "open",
				Blocked: false,
			},
			want: true,
		},
		{
			name: "non-open unassigned issue treated ready for recovery",
			in: trackedIssueInfo{
				Status:  "in_progress",
				Blocked: false,
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isReadyIssue(tc.in, nil)
			if got != tc.want {
				t.Fatalf("isReadyIssue() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsReadyIssueFailsClosedOnTmuxInfrastructureError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf 'private tmux infrastructure failure\\n' >&2\nexit 2\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := isReadyIssue(trackedIssueInfo{
		ID:       "gt-work",
		Status:   "open",
		Assignee: "gastown/polecats/capable",
	}, nil)
	if got {
		t.Fatal("tmux infrastructure failure marked assigned work ready")
	}
}

func TestIsReadyIssueContextCancelsTmuxProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nexec sleep 2\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	ready, err := isReadyIssueContext(ctx, trackedIssueInfo{
		ID:       "gt-work",
		Status:   "open",
		Assignee: "gastown/polecats/capable",
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if ready {
		t.Fatal("canceled tmux probe marked assigned work ready")
	}
	if elapsed := time.Since(started); elapsed >= 750*time.Millisecond {
		t.Fatalf("canceled tmux probe returned after %s", elapsed.Round(time.Millisecond))
	}
}

func TestIsReadyIssueContextAcceptsConfirmedMissingSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf \"can't find session: missing\\n\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ready, err := isReadyIssueContext(context.Background(), trackedIssueInfo{
		ID:       "gt-work",
		Status:   "open",
		Assignee: "gastown/polecats/capable",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("confirmed missing assignee session was not ready")
	}
}

func TestApplyFreshIssueDetails_SetsBlockedFlag(t *testing.T) {
	dep := trackedDependency{
		ID:     "gt-123",
		Status: "open",
	}
	details := &issueDetails{
		ID:             "gt-123",
		Status:         "open",
		BlockedByCount: 1,
	}

	applyFreshIssueDetails(&dep, details)

	if !dep.Blocked {
		t.Fatalf("applyFreshIssueDetails() should set Blocked=true when details are blocked")
	}
}

func TestApplyFreshIssueDetails_BlankStatusBecomesUnknown(t *testing.T) {
	dep := trackedDependency{ID: "gt-123"}
	details := &issueDetails{ID: "gt-123", Status: "  "}

	applyFreshIssueDetails(&dep, details)

	if dep.Status != trackedStatusUnknown {
		t.Fatalf("dep.Status = %q, want %q", dep.Status, trackedStatusUnknown)
	}
}

func TestIssueDetailsIsBlocked(t *testing.T) {
	tests := []struct {
		name string
		in   issueDetails
		want bool
	}{
		{
			name: "blocked_by_count marks blocked",
			in: issueDetails{
				BlockedByCount: 2,
			},
			want: true,
		},
		{
			name: "blocked_by list marks blocked",
			in: issueDetails{
				BlockedBy: []string{"gt-1"},
			},
			want: true,
		},
		{
			name: "open blocks dependency marks blocked",
			in: issueDetails{
				Dependencies: []issueDependency{
					{DependencyType: "blocks", Status: "open"},
				},
			},
			want: true,
		},
		{
			name: "closed blocks dependency does not mark blocked",
			in: issueDetails{
				Dependencies: []issueDependency{
					{DependencyType: "blocks", Status: "closed"},
				},
			},
			want: false,
		},
		{
			name: "non-blocking dependency does not mark blocked",
			in: issueDetails{
				Dependencies: []issueDependency{
					{DependencyType: "parent-child", Status: "open"},
				},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.IsBlocked()
			if got != tc.want {
				t.Fatalf("IsBlocked() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsSlingableBead(t *testing.T) {
	// Set up a fake town root with routes.jsonl
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	routesContent := `{"prefix": "gt-", "path": "gastown/mayor/rig"}
{"prefix": "bd-", "path": "beads/mayor/rig"}
{"prefix": "hq-", "path": "."}
`
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routesContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		beadID string
		want   bool
	}{
		{"rig bead is slingable", "gt-wisp-abc", true},
		{"another rig bead is slingable", "bd-wisp-xyz", true},
		{"town-level bead not slingable", "hq-wisp-abc", false},
		{"town-level convoy not slingable", "hq-cv-kl6ns", false},
		{"unknown prefix not slingable", "zz-wisp-abc", false},
		{"no prefix assumes slingable", "nohyphen", true},
		{"empty ID assumes slingable", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isSlingableBead(townRoot, tc.beadID)
			if got != tc.want {
				t.Fatalf("isSlingableBead(%q) = %v, want %v", tc.beadID, got, tc.want)
			}
		})
	}
}
