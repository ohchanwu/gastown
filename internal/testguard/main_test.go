package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/doltserver"
)

func TestWriteBaselineCreatesPrivateReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	want := []doltserver.DoltListener{{PID: 101, Port: 4401}}
	if err := writeBaseline(path, want); err != nil {
		t.Fatalf("writeBaseline: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("receipt mode = %04o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []doltserver.DoltListener
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("receipt = %#v, want %#v", got, want)
	}
}

func TestCleanupSinceBaselineRejectsNonPrivateReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte("[]"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cleanupSinceBaseline(path, 101, t.TempDir()); err == nil {
		t.Fatal("cleanup accepted a non-private baseline receipt")
	}
}

func TestRequiredBaselineListenerRejectsMissingLauncher(t *testing.T) {
	baseline := []doltserver.DoltListener{{PID: 101, Port: 4401}}
	if _, err := requiredBaselineListener(baseline, 202); err == nil {
		t.Fatal("accepted a baseline that omitted the known launcher process")
	}
}

func TestPathWithinOrEqualAcceptsLauncherRootOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gastown-test-dolt.run")
	if !pathWithinOrEqual(root, root) {
		t.Fatal("launcher root did not own itself")
	}
	if !pathWithinOrEqual(root, filepath.Join(root, "tmp", "child")) {
		t.Fatal("launcher root did not own its descendant")
	}
	if pathWithinOrEqual(root, root+"-sibling") {
		t.Fatal("launcher root owned a sibling path")
	}
}
