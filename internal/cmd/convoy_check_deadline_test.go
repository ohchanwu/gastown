package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckCompletedConvoysCancelsListingAtCommandDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
  list)
    exec sleep 0.50
    ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := checkCompletedConvoys(ctx, townRoot, true, true)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("canceled listing returned after %s, want under 300ms", elapsed.Round(time.Millisecond))
	}
}

func TestCloseConvoyCancellationStopsBeforePersist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	exportCalled := filepath.Join(binDir, "export.called")
	script := `#!/bin/sh
case "$1" in
  close) exec sleep 0.50 ;;
  export) touch "` + exportCalled + `"; exit 0 ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	closed, err := closeConvoyIfCompleteWithOutputContext(
		ctx,
		townRoot,
		"hq-cv-slow-close",
		"Slow close",
		[]trackedIssueInfo{{ID: "gt-done", Status: "closed"}},
		false,
		false,
	)
	elapsed := time.Since(start)

	if closed {
		t.Fatal("canceled close reported success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("canceled close returned after %s, want under 300ms", elapsed.Round(time.Millisecond))
	}
	if _, statErr := os.Stat(exportCalled); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("persist ran after close cancellation: %v", statErr)
	}
}

func TestCloseConvoyCancellationStopsNotificationAndLaterMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	gtCalled := filepath.Join(binDir, "gt.called")
	updateCalled := filepath.Join(binDir, "update.called")
	showStarted := filepath.Join(binDir, "show.started")
	bdScript := `#!/bin/sh
while [ "${1#--}" != "$1" ]; do shift; done
case "$1" in
  close|export) exit 0 ;;
  show) touch "` + showStarted + `"; exec sleep 5 ;;
  update) touch "` + updateCalled + `"; exit 0 ;;
  *) exit 1 ;;
esac
`
	gtScript := `#!/bin/sh
touch "` + gtCalled + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "gt"), []byte(gtScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, err := os.Stat(showStarted); err == nil {
					cancel()
					return
				}
			case <-deadline.C:
				cancel()
				return
			}
		}
	}()
	start := time.Now()
	_, err := closeConvoyIfCompleteWithOutputContext(
		ctx,
		townRoot,
		"hq-cv-slow-notify",
		"Slow notification",
		[]trackedIssueInfo{{ID: "gt-done", Status: "closed"}},
		false,
		false,
	)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("started notification returned after %s, want cancellation before its 5s sleep", elapsed.Round(time.Millisecond))
	}
	if _, statErr := os.Stat(showStarted); statErr != nil {
		t.Fatalf("notification lookup did not start: %v", statErr)
	}
	for _, path := range []string{gtCalled, updateCalled} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("later notification action ran after cancellation (%s): %v", filepath.Base(path), statErr)
		}
	}
}

func TestWriteConvoyListJSONWaitsForSlowSuccessfulLookup(t *testing.T) {
	const oldArtificialCutoff = 50 * time.Millisecond
	var output bytes.Buffer
	start := time.Now()
	err := writeConvoyListJSON(
		context.Background(),
		&output,
		[]convoyListIssue{{ID: "hq-cv-slow-list", Title: "Slow list", Status: "open"}},
		func(context.Context, convoyListIssue) ([]trackedIssueInfo, error) {
			time.Sleep(2 * oldArtificialCutoff)
			return []trackedIssueInfo{{ID: "gt-done", Status: "closed"}}, nil
		},
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("writeConvoyListJSON: %v", err)
	}
	if elapsed < 2*oldArtificialCutoff {
		t.Fatalf("lookup returned after %s, want slow fixture beyond %s", elapsed.Round(time.Millisecond), oldArtificialCutoff)
	}
	if !strings.Contains(output.String(), `"id": "hq-cv-slow-list"`) {
		t.Fatalf("slow successful lookup was not encoded: %s", output.String())
	}
}

func TestWriteConvoyListJSONReturnsCallerCancellationWithoutPartialOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slowStarted := make(chan struct{})
	go func() {
		<-slowStarted
		cancel()
	}()

	var output bytes.Buffer
	err := writeConvoyListJSON(
		ctx,
		&output,
		[]convoyListIssue{
			{ID: "hq-cv-fast-list", Title: "Fast list", Status: "open"},
			{ID: "hq-cv-pending-list", Title: "Pending list", Status: "open"},
		},
		func(ctx context.Context, convoy convoyListIssue) ([]trackedIssueInfo, error) {
			if convoy.ID == "hq-cv-pending-list" {
				close(slowStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []trackedIssueInfo{{ID: "gt-done", Status: "closed"}}, nil
		},
	)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if output.Len() != 0 {
		t.Fatalf("canceled list encoded partial JSON: %s", output.String())
	}
}
