package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFindStrandedConvoysCancelsSlowTrackedLookups(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
while [ "${1#--}" != "$1" ]; do shift; done
case "$1" in
  list)
    printf '%s\n' '[{"id":"hq-cv-a","title":"A","status":"open","issue_type":"convoy","labels":["gt:convoy"]},{"id":"hq-cv-b","title":"B","status":"open","issue_type":"convoy","labels":["gt:convoy"]}]'
    ;;
  sql)
    sleep 0.50
    printf '%s\n' '[]'
    ;;
  show)
    printf '%s\n' '[{"dependencies":[]}]'
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
	started := time.Now()
	stranded, err := findStrandedConvoysContext(ctx, townRoot)
	elapsed := time.Since(started)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
	if stranded != nil {
		t.Fatalf("timed-out scan returned partial results: %#v", stranded)
	}
	if elapsed >= 350*time.Millisecond {
		t.Fatalf("canceled scan returned after %s, want under 350ms", elapsed.Round(time.Millisecond))
	}
}

func TestFindStrandedConvoysFailsClosedWithoutRawLookupError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	binDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
while [ "${1#--}" != "$1" ]; do shift; done
case "$1" in
  list) printf '%s\n' '[{"id":"hq-cv-a","title":"A","status":"open","issue_type":"convoy","labels":["gt:convoy"]}]' ;;
  *) printf 'private lookup detail\n' >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stranded, err := findStrandedConvoysContext(context.Background(), townRoot)
	if err == nil || err.Error() != "convoy stranded scan incomplete" {
		t.Fatalf("error = %v, want sanitized incomplete scan", err)
	}
	if strings.Contains(err.Error(), "private lookup detail") {
		t.Fatalf("error exposed raw lookup detail: %v", err)
	}
	if stranded != nil {
		t.Fatalf("failed scan returned partial results: %#v", stranded)
	}
}
