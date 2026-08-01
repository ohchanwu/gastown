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
	if err := cleanupSinceBaseline(path); err == nil {
		t.Fatal("cleanup accepted a non-private baseline receipt")
	}
}
