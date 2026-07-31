package doctor

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/health"
)

func TestControlPlaneHealthCheckMakesDoctorReportError(t *testing.T) {
	check := newControlPlaneHealthCheck("candidate", func(string, string) (health.ControlPlaneVerdict, error) {
		return health.ControlPlaneVerdict{Failures: []health.ControlPlaneFailure{
			{Subsystem: "mayor-mail", Diagnostic: "gt mail inbox mayor/"},
			{Subsystem: "wake-delivery", Diagnostic: "gt nudge --help"},
			{Subsystem: "dolt", Diagnostic: "gt dolt status"},
			{Subsystem: "convoy", Diagnostic: "gt convoy check --dry-run --json"},
			{Subsystem: "wake-canary", Diagnostic: "gt nudge-canary --help"},
		}}, nil
	})
	d := NewDoctor()
	d.Register(check)

	report := d.Run(&CheckContext{TownRoot: t.TempDir()})
	if !report.HasErrors() || report.Summary.Errors != 1 {
		t.Fatalf("report = %#v, want one Doctor error", report.Summary)
	}
	result := report.Checks[0]
	if result.Message != "5 control-plane subsystem(s) unhealthy: convoy, dolt, mayor-mail, wake-canary, wake-delivery" || len(result.Details) != 5 {
		t.Fatalf("result = %#v", result)
	}
	if result.FixHint != "run gt health --json for per-subsystem diagnostics" {
		t.Fatalf("FixHint = %q", result.FixHint)
	}
	var output bytes.Buffer
	report.PrintSummaryOnly(&output, false, 0)
	for _, visible := range []string{"convoy, dolt, mayor-mail, wake-canary, wake-delivery", "gt health --json"} {
		if !strings.Contains(output.String(), visible) {
			t.Fatalf("non-verbose Doctor output missing %q: %s", visible, output.String())
		}
	}
	rendered := strings.Join(append([]string{result.Message}, result.Details...), "\n")
	for _, private := range []string{"subject", "body", "terminal", "/private"} {
		if strings.Contains(rendered, private) {
			t.Fatalf("Doctor result leaked %q: %s", private, rendered)
		}
	}
}

func TestControlPlaneHealthCheckFailsClosedOnEvidenceReadError(t *testing.T) {
	check := newControlPlaneHealthCheck("candidate", func(string, string) (health.ControlPlaneVerdict, error) {
		return health.ControlPlaneVerdict{}, errors.New("/private/path")
	})
	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	if result.Status != StatusError || result.Message != "control-plane evidence unavailable" || result.FixHint != "run gt health --json for a sanitized diagnostic" {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(result.Message+result.FixHint, "/private") {
		t.Fatalf("result leaked collector error: %#v", result)
	}
}

func TestControlPlaneHealthCheckQualifiesHealthyNonDoltVerdict(t *testing.T) {
	check := newControlPlaneHealthCheck("candidate", func(string, string) (health.ControlPlaneVerdict, error) {
		return health.ControlPlaneVerdict{Healthy: true}, nil
	})
	result := check.Run(&CheckContext{TownRoot: t.TempDir()})
	want := "non-Dolt delivery/convoy/canary evidence is healthy; Dolt covered by dedicated checks"
	if result.Status != StatusOK || result.Message != want {
		t.Fatalf("result = %#v, want %q", result, want)
	}
	if result.Message == "control-plane evidence is healthy" {
		t.Fatal("Doctor emitted an unqualified all-control-plane healthy claim")
	}
}
