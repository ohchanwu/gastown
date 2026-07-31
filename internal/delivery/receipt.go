// Package delivery defines runtime-neutral delivery evidence.
package delivery

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/atomicfile"
)

const (
	receiptSchemaVersion = 1
	promptSubmittedEvent = "prompt_submitted"
	controlPrefix        = "[gt-delivery-id:"
)

// SubmissionReceipt distinguishes text typed into a composer from a turn the
// target runtime accepted.
type SubmissionReceipt struct {
	Session     string
	DeliveryID  string
	Runtime     string
	Typed       bool
	Submitted   bool
	TypedAt     time.Time
	SubmittedAt time.Time
}

type receiptEvent struct {
	SchemaVersion int       `json:"schema_version"`
	Event         string    `json:"event"`
	DeliveryID    string    `json:"delivery_id"`
	Session       string    `json:"session"`
	Runtime       string    `json:"runtime"`
	SubmittedAt   time.Time `json:"submitted_at"`
}

// ControlMessage binds an opaque delivery ID to the prompt observed by the
// runtime hook. The hook records only the ID, never the message body.
func ControlMessage(deliveryID, message string) string {
	return controlPrefix + deliveryID + "] " + message
}

func deliveryIDFromPrompt(prompt string) (string, bool) {
	if !strings.HasPrefix(prompt, controlPrefix) {
		return "", false
	}
	end := strings.IndexByte(prompt[len(controlPrefix):], ']')
	if end < 1 {
		return "", false
	}
	id := prompt[len(controlPrefix) : len(controlPrefix)+end]
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", false
		}
	}
	return id, true
}

// ReceiptPath returns the private per-session JSONL receipt path.
func ReceiptPath(townRoot, session string) string {
	safe := strings.NewReplacer("/", "_", string(filepath.Separator), "_").Replace(session)
	return filepath.Join(townRoot, ".runtime", "delivery_receipts", safe+".jsonl")
}

// RecordPromptSubmitted atomically appends a value-blind Codex hook receipt.
// It returns false when the submitted prompt is not a delivery control message.
func RecordPromptSubmitted(townRoot, session, runtimeName, prompt string, submittedAt time.Time) (bool, error) {
	deliveryID, ok := deliveryIDFromPrompt(prompt)
	if !ok {
		return false, nil
	}
	if err := recordSubmitted(townRoot, session, runtimeName, deliveryID, promptSubmittedEvent, submittedAt); err != nil {
		return false, err
	}
	return true, nil
}

func recordSubmitted(townRoot, session, runtimeName, deliveryID, eventName string, submittedAt time.Time) error {
	if townRoot == "" || session == "" || runtimeName != "codex" || submittedAt.IsZero() {
		return fmt.Errorf("invalid prompt submission receipt identity")
	}

	path := ReceiptPath(townRoot, session)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating receipt directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("tightening receipt directory: %w", err)
	}

	fileLock := flock.New(path + ".lock")
	if err := fileLock.Lock(); err != nil {
		return err
	}
	defer func() { _ = fileLock.Unlock() }()
	if err := os.Chmod(path+".lock", 0600); err != nil {
		return fmt.Errorf("tightening receipt lock: %w", err)
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading receipt log: %w", err)
	}
	event := receiptEvent{
		SchemaVersion: receiptSchemaVersion,
		Event:         eventName,
		DeliveryID:    deliveryID,
		Session:       session,
		Runtime:       runtimeName,
		SubmittedAt:   submittedAt,
	}
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data := append(append(existing, line...), '\n')
	if err := atomicfile.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("appending receipt: %w", err)
	}
	return nil
}

// FindSubmittedAfter finds a matching runtime receipt newer than baseline.
func FindSubmittedAfter(townRoot, session, deliveryID string, baseline time.Time) (SubmissionReceipt, bool, error) {
	f, err := os.Open(ReceiptPath(townRoot, session))
	if err != nil {
		if os.IsNotExist(err) {
			return SubmissionReceipt{}, false, nil
		}
		return SubmissionReceipt{}, false, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event receiptEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event.SchemaVersion != receiptSchemaVersion || event.Event != promptSubmittedEvent || event.Session != session || event.DeliveryID != deliveryID || !event.SubmittedAt.After(baseline) {
			continue
		}
		return SubmissionReceipt{
			Session: session, DeliveryID: deliveryID, Runtime: event.Runtime,
			Typed: event.Event == promptSubmittedEvent, Submitted: true, SubmittedAt: event.SubmittedAt,
		}, true, nil
	}
	if err := scanner.Err(); err != nil {
		return SubmissionReceipt{}, false, err
	}
	return SubmissionReceipt{}, false, nil
}
