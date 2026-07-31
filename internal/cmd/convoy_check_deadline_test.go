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
