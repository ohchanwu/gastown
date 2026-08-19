package deps

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseBeadsVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"bd version 0.55.4 (dev: main@3e1378e122c6)", "0.55.4"},
		{"bd version 0.55.4", "0.55.4"},
		{"bd version 1.2.3", "1.2.3"},
		{"bd version 10.20.30 (release)", "10.20.30"},
		{"some other output", ""},
		{"", ""},
	}

	for _, tt := range tests {
		result := parseBeadsVersion(tt.input)
		if result != tt.expected {
			t.Errorf("parseBeadsVersion(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b     string
		expected int
	}{
		{"0.55.4", "0.55.4", 0},
		{"0.55.4", "0.54.0", 1},
		{"0.54.0", "0.55.4", -1},
		{"1.0.0", "0.99.99", 1},
		{"0.55.5", "0.55.4", 1},
		{"0.55.4", "0.55.5", -1},
	}

	for _, tt := range tests {
		result := CompareVersions(tt.a, tt.b)
		if result != tt.expected {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tt.a, tt.b, result, tt.expected)
		}
	}
}

func TestCheckBeads(t *testing.T) {
	// This test depends on whether bd is installed in the test environment
	status, version := CheckBeads()

	// We expect bd to be installed in dev environment
	if status == BeadsNotFound {
		t.Skip("bd not installed, skipping integration test")
	}

	if status == BeadsOK && version == "" {
		t.Error("CheckBeads returned BeadsOK but empty version")
	}

	t.Logf("CheckBeads: status=%d, version=%s", status, version)
}

func TestCheckBeadsEnforcesReasonFileVersionFloor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX bd shim")
	}
	tmpDir := t.TempDir()
	bdPath := filepath.Join(tmpDir, "bd")
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tc := range []struct {
		version string
		status  BeadsStatus
	}{
		{version: "1.0.3", status: BeadsTooOld},
		{version: "1.0.4", status: BeadsOK},
	} {
		if err := os.WriteFile(bdPath, []byte("#!/bin/sh\necho 'bd version "+tc.version+"'\n"), 0755); err != nil {
			t.Fatal(err)
		}
		status, version := CheckBeads()
		if status != tc.status || version != tc.version {
			t.Fatalf("bd %s: CheckBeads() = (%v, %q), want (%v, %q)", tc.version, status, version, tc.status, tc.version)
		}
	}
}
