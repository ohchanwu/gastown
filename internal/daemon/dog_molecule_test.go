package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/reaper"
)

func TestParseWispID(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		wantID string
	}{
		{
			name:   "standard wisp output",
			input:  "✓ Spawned wisp: gt-wisp-abc123 — Reap stale wisps",
			wantID: "gt-wisp-abc123",
		},
		{
			name:   "wisp ID with ANSI codes",
			input:  "\033[32m✓\033[0m Spawned wisp: \033[1mgt-wisp-xyz789\033[0m — Title",
			wantID: "gt-wisp-xyz789",
		},
		{
			name:   "empty output",
			input:  "",
			wantID: "",
		},
		{
			name:   "no wisp ID in output",
			input:  "Error: something went wrong",
			wantID: "",
		},
		{
			name:   "wisp ID at end of line",
			input:  "Created gt-wisp-def456",
			wantID: "gt-wisp-def456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWispID(tt.input)
			if got != tt.wantID {
				t.Errorf("parseWispID(%q) = %q, want %q", tt.input, got, tt.wantID)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no ANSI", "hello", "hello"},
		{"color code", "\033[32mgreen\033[0m", "green"},
		{"bold", "\033[1mbold\033[0m", "bold"},
		{"multiple codes", "\033[32m✓\033[0m \033[1mtext\033[0m", "✓ text"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripANSI(tt.input)
			if got != tt.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseChildrenJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantIDs []string
		wantErr bool
	}{
		{
			name:    "bare array",
			input:   `[{"id":"a","title":"Probe","status":"open"}]`,
			wantIDs: []string{"a"},
		},
		{
			name:    "map wrapper from bd show",
			input:   `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"},{"id":"hq-wisp-b","title":"Report","status":"open"}]}`,
			wantIDs: []string{"hq-wisp-a", "hq-wisp-b"},
		},
		{
			name:    "empty map wrapper",
			input:   `{"hq-wisp-root":[]}`,
			wantIDs: []string{},
		},
		{
			name:    "schema metadata with children",
			input:   `{"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"}],"schema_version":1}`,
			wantIDs: []string{"hq-wisp-a"},
		},
		{
			name:    "schema metadata with empty children",
			input:   `{"hq-wisp-root":[],"schema_version":1}`,
			wantIDs: []string{},
		},
		{
			name:    "multiple child arrays are deterministic",
			input:   `{"hq-wisp-b":[{"id":"b-step","title":"Report","status":"open"}],"schema_version":1,"hq-wisp-a":[{"id":"a-step","title":"Probe","status":"open"}]}`,
			wantIDs: []string{"a-step", "b-step"},
		},
		{
			name:    "schema key is metadata even if array-valued",
			input:   `{"schema_version":[{"id":"metadata","title":"Ignore","status":"open"}],"hq-wisp-root":[{"id":"hq-wisp-a","title":"Probe","status":"open"}]}`,
			wantIDs: []string{"hq-wisp-a"},
		},
		{
			name:    "empty array",
			input:   `[]`,
			wantIDs: []string{},
		},
		{
			name:    "empty input",
			input:   `   `,
			wantErr: true,
		},
		{
			name:    "malformed bare array",
			input:   `[`,
			wantErr: true,
		},
		{
			name:    "malformed object envelope",
			input:   `{"hq-wisp-root":[`,
			wantErr: true,
		},
		{
			name:    "invalid json",
			input:   `not json`,
			wantErr: true,
		},
		{
			name:    "malformed child array",
			input:   `{"hq-wisp-root":[{"id":1}],"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "non-array child payload",
			input:   `{"hq-wisp-root":1,"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "metadata only is not silent skip-all",
			input:   `{"schema_version":1}`,
			wantErr: true,
		},
		{
			name:    "empty object is not silent skip-all",
			input:   `{}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChildrenJSON(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			gotIDs := make([]string, 0, len(got))
			for _, child := range got {
				gotIDs = append(gotIDs, child.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Errorf("got child IDs %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func TestDogMolGracefulDegradation(t *testing.T) {
	// A dogMol with empty rootID should be a no-op for all operations.
	dm := &dogMol{
		rootID:  "",
		stepIDs: make(map[string]string),
	}

	// These should not panic or error — graceful degradation.
	dm.closeStep("scan")
	dm.failStep("scan", "test failure")
	dm.close()
}

func TestDogMolFailStepStreamsLargeMultiDatabaseRecoveryReason(t *testing.T) {
	tmpDir := t.TempDir()
	bdPath := filepath.Join(tmpDir, "bd")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" > "${0%/*}/args"
if [ "$#" -ne 4 ] || [ "$1" != "close" ] || [ "$2" != "step-auto-close" ] || [ "$3" != "--reason-file" ] || [ "$4" != "-" ]; then
	exit 64
fi
cat > "${0%/*}/reason"
`
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	var failures []error
	for _, dbName := range []string{"alpha", "bravo"} {
		ids := make([]string, 40_000)
		for i := range ids {
			ids[i] = fmt.Sprintf("%s-task-%06d", dbName, i)
		}
		commitErr := &reaper.AutoCloseCommitOutcomeError{
			Cause: fmt.Errorf("%w: injected Dolt commit failure", reaper.ErrAutoCloseCommitOutcomeUnknown),
			Anomalies: []reaper.Anomaly{{
				Type:        "dolt_commit_failed",
				Scope:       dbName,
				AffectedIDs: ids,
				Remediation: "run CALL DOLT_COMMIT; do not rerun auto-close",
			}},
		}
		failures = append(failures, fmt.Errorf("%s: %w", dbName, commitErr))
	}
	reason := autoCloseFailureReason(failures)
	if len(reason) < 1<<20 {
		t.Fatalf("test reason is %d bytes, want at least 1 MiB", len(reason))
	}

	dm := &dogMol{
		rootID:   "root",
		stepIDs:  map[string]string{"auto-close": "step-auto-close"},
		bdPath:   bdPath,
		townRoot: tmpDir,
		logger:   log.New(io.Discard, "", 0),
	}
	dm.failStep("auto-close", reason)

	got, err := os.ReadFile(filepath.Join(tmpDir, "reason"))
	if err != nil {
		t.Fatalf("read transported recovery reason: %v", err)
	}
	if string(got) != reason {
		t.Fatalf("transported recovery reason is %d bytes, want exact %d-byte payload", len(got), len(reason))
	}
	args, err := os.ReadFile(filepath.Join(tmpDir, "args"))
	if err != nil {
		t.Fatalf("read subprocess argv: %v", err)
	}
	if got := strings.TrimSpace(string(args)); got != "close step-auto-close --reason-file -" {
		t.Fatalf("subprocess argv = %q, want bounded reason-file transport", got)
	}
	if len(args) > 128 {
		t.Fatalf("subprocess argv grew to %d bytes for %d-byte reason", len(args), len(reason))
	}
}

func TestDogMolClosePreservesStepWhenFailureReasonCannotPersist(t *testing.T) {
	tmpDir := t.TempDir()
	bdPath := filepath.Join(tmpDir, "bd")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "${0%/*}/calls"
if [ "$1" = "close" ] && [ "$2" = "step-auto-close" ] && [ "$#" -gt 2 ]; then
	exit 23
fi
if [ "$1" = "show" ]; then
	printf '%s\n' '{"root":[{"id":"step-auto-close","title":"Auto-close stale issues","status":"open"}]}'
fi
`
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	dm := &dogMol{
		rootID:   "root",
		stepIDs:  map[string]string{"auto-close": "step-auto-close"},
		bdPath:   bdPath,
		townRoot: tmpDir,
		logger:   log.New(io.Discard, "", 0),
	}
	dm.failStep("auto-close", errors.New("commit outcome unknown").Error())
	dm.close()

	calls, err := os.ReadFile(filepath.Join(tmpDir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if call == "close step-auto-close" {
			t.Fatal("close backstop silently closed step after failure reason was not persisted")
		}
		if call == "close root" {
			t.Fatal("root was closed while a child failure reason remained unpersisted")
		}
	}
}
