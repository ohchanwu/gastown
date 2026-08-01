package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEnrichConvoyWorkersScansOnce(t *testing.T) {
	results := []convoyLookupResult{
		{done: true, tracked: []trackedIssueInfo{{ID: "gt-a", Status: "open"}, {ID: "gt-closed", Status: "closed"}}},
		{done: true, tracked: []trackedIssueInfo{{ID: "gt-b", Status: "in_progress"}, {ID: "gt-a", Status: "open"}}},
	}

	calls := 0
	var gotIDs []string
	err := enrichConvoyWorkersContext(context.Background(), "/town", results,
		func(_ context.Context, townRoot string, ids []string) (map[string]*workerInfo, error) {
			calls++
			if townRoot != "/town" {
				t.Fatalf("town root = %q", townRoot)
			}
			gotIDs = append([]string(nil), ids...)
			return map[string]*workerInfo{
				"gt-a": {Worker: "rig/a"},
				"gt-b": {Worker: "rig/b"},
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("worker inventory calls = %d, want 1", calls)
	}
	if want := []string{"gt-a", "gt-b"}; !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("worker inventory IDs = %v, want %v", gotIDs, want)
	}
	if results[0].tracked[0].Worker != "rig/a" || results[1].tracked[0].Worker != "rig/b" {
		t.Fatalf("workers not attached to ordered results: %#v", results)
	}
}

func TestGetWorkersForIssuesContextCancelsAndCapsRigQueries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}

	townRoot := t.TempDir()
	for i := 0; i < 6; i++ {
		for _, suffix := range []string{"polecats", filepath.Join("mayor", "rig", ".beads")} {
			if err := os.MkdirAll(filepath.Join(townRoot, "rig-"+string(rune('a'+i)), suffix), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	binDir := t.TempDir()
	startLog := filepath.Join(t.TempDir(), "starts")
	script := "#!/bin/sh\nprintf 'start\\n' >> \"$GT_WORKER_START_LOG\"\nexec sleep 2\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_WORKER_START_LOG", startLog)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := getWorkersForIssuesContext(ctx, townRoot, []string{"gt-work"})
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if data, err := os.ReadFile(startLog); err == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker scan did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	startedCancel := time.Now()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if elapsed := time.Since(startedCancel); elapsed >= 750*time.Millisecond {
		t.Fatalf("canceled worker scan returned after %s", elapsed.Round(time.Millisecond))
	}
	data, err := os.ReadFile(startLog)
	if err != nil {
		t.Fatal(err)
	}
	if starts := strings.Count(string(data), "start\n"); starts < 1 || starts > convoyLookupConcurrency {
		t.Fatalf("started rig queries = %d, want 1..%d", starts, convoyLookupConcurrency)
	}
}
