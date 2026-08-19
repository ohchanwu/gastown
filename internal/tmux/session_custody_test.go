package tmux

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestParseSessionGenerationClassifiesEmptyTargetFieldsAsAbsent(t *testing.T) {
	for _, output := range []string{"123", "123\t\t\t\t", "123\t\tserver-global-value\t\t"} {
		if _, err := parseSessionGeneration("gt-missing", output); !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("parseSessionGeneration(%q) error = %v, want ErrSessionNotFound", output, err)
		}
	}
	if _, err := parseSessionGeneration("gt-missing", "123\tinvalid session\tgeneration\t\t"); errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("malformed non-empty session ID classified as absence: %v", err)
	}
}

func TestParseSessionGenerationBindsCustodyToken(t *testing.T) {
	const custody = "11111111-2222-4333-8444-555555555555"
	generation, err := parseSessionGeneration("gt-test", "123\t$7\tfixture-generation\t"+custody)
	if err != nil {
		t.Fatal(err)
	}
	if generation.Custody != custody {
		t.Fatalf("custody = %q, want %q", generation.Custody, custody)
	}
	replacement := generation
	replacement.Custody = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if generation.Equal(replacement) {
		t.Fatal("session generations with different containment tokens compared equal")
	}
}

func TestParseSessionGenerationBindsPersistedPaneToken(t *testing.T) {
	generation, err := parseSessionGeneration("gt-test", "123\t$7\tfixture-generation\t\t19")
	if err != nil {
		t.Fatal(err)
	}
	if generation.PaneID != "%19" {
		t.Fatalf("pane ID = %q, want %%19", generation.PaneID)
	}
	replacement := generation
	replacement.PaneID = "%20"
	if generation.Equal(replacement) {
		t.Fatal("session generations with different immutable panes compared equal")
	}
}

func TestWrapSessionCommandWithCustodyIsPlatformHonest(t *testing.T) {
	const command = "exec env GT_ROLE=witness codex"
	wrapped, custody, err := WrapSessionCommandWithCustody("/tmp/gt", command)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		if wrapped != command || custody != "" {
			t.Fatalf("unsupported platform wrapper = %q, %q; want unchanged command and empty custody", wrapped, custody)
		}
		return
	}
	if custody == "" || wrapped == command || !strings.HasPrefix(wrapped, "exec ") || !strings.Contains(wrapped, "session-custody --id") || !strings.Contains(wrapped, custody) {
		t.Fatalf("linux wrapper = %q, custody %q", wrapped, custody)
	}
}

func TestEncodeSessionCustodyPathsRejectsUnboundedAndCanonicalizes(t *testing.T) {
	encoded, err := EncodeSessionCustodyPaths([]string{"/srv/witness", "/srv/witness", "/opt/runtime"})
	if err != nil {
		t.Fatal(err)
	}
	if encoded != `["/srv/witness","/opt/runtime"]` {
		t.Fatalf("encoded allowlist = %q", encoded)
	}
	for _, paths := range [][]string{nil, {"relative/path"}, {"/"}} {
		if _, err := EncodeSessionCustodyPaths(paths); err == nil {
			t.Fatalf("EncodeSessionCustodyPaths(%q) unexpectedly succeeded", paths)
		}
	}
}

func TestCanWrapCurrentExecutableRejectsTestBinary(t *testing.T) {
	if CanWrapCurrentExecutable("/tmp/witness.test") {
		t.Fatal("Go test binary was accepted as the hidden session-custody launcher")
	}
	if !CanWrapCurrentExecutable("/usr/local/bin/gt") {
		t.Fatal("installed gt binary was rejected as the hidden session-custody launcher")
	}
}
