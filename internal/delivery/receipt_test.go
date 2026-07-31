package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPromptSubmittedReceiptIsAtomicPrivateAndMatchable(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-codex"
	const deliveryID = "ndg-0123456789abcdef"
	baseline := time.Now().Add(-time.Second)
	prompt := ControlMessage(deliveryID, "private message body")

	receiptPath := ReceiptPath(townRoot, session)
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	written, err := RecordPromptSubmitted(townRoot, session, "codex", prompt, time.Now())
	if err != nil {
		t.Fatalf("RecordPromptSubmitted: %v", err)
	}
	if !written {
		t.Fatal("RecordPromptSubmitted = false, want matching control message")
	}

	receipt, ok, err := FindSubmittedAfter(townRoot, session, deliveryID, baseline)
	if err != nil {
		t.Fatalf("FindSubmittedAfter: %v", err)
	}
	if !ok || !receipt.Submitted || receipt.Session != session || receipt.DeliveryID != deliveryID || receipt.Runtime != "codex" || !receipt.SubmittedAt.After(baseline) {
		t.Fatalf("receipt = %#v, ok=%v", receipt, ok)
	}

	info, err := os.Stat(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("receipt mode = %o, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private message body") || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("receipt JSONL leaked prompt or lacked newline: %q", data)
	}
	entries, err := os.ReadDir(filepath.Dir(receiptPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp.") {
			t.Fatalf("receipt directory contains temporary artifact: %s", entry.Name())
		}
	}
	lockInfo, err := os.Stat(receiptPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if lockInfo.Mode().Perm() != 0600 {
		t.Fatalf("receipt lock mode = %o, want 0600", lockInfo.Mode().Perm())
	}
}

func TestRecordPromptSubmittedIgnoresPromptWithoutControlID(t *testing.T) {
	written, err := RecordPromptSubmitted(t.TempDir(), "gt-test-codex", "codex", "ordinary user prompt", time.Now())
	if err != nil || written {
		t.Fatalf("RecordPromptSubmitted = %v, %v; want false, nil", written, err)
	}
}

func TestFindSubmittedAfterRejectsEqualBaselineTimestamp(t *testing.T) {
	townRoot := t.TempDir()
	const session = "gt-test-equal-receipt"
	const deliveryID = "ndg-equal-timestamp"
	baseline := time.Now().UTC()

	written, err := RecordPromptSubmitted(townRoot, session, "codex", ControlMessage(deliveryID, "private"), baseline)
	if err != nil || !written {
		t.Fatalf("RecordPromptSubmitted() = %v, %v", written, err)
	}
	if receipt, ok, err := FindSubmittedAfter(townRoot, session, deliveryID, baseline); err != nil || ok {
		t.Fatalf("FindSubmittedAfter(equal) = %#v, %v, %v; want no match", receipt, ok, err)
	}
}
