// Package deps manages external dependencies for Gas Town.
package deps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// MinBeadsVersion is the minimum compatible beads version for this Gas Town release.
// Update this when Gas Town requires new beads features.
const MinBeadsVersion = "1.0.4"

// BeadsInstallPath is the go install path for beads.
const BeadsInstallPath = "github.com/steveyegge/beads/cmd/bd@latest"

// BeadsStatus represents the state of the beads installation.
type BeadsStatus int

const (
	BeadsOK       BeadsStatus = iota // bd found, version compatible
	BeadsNotFound                    // bd not in PATH
	BeadsTooOld                      // bd found but version too old
	BeadsUnknown                     // bd found but couldn't parse version
)

// CheckBeads checks if bd is installed and compatible.
// Returns status and the installed version (if found).
func CheckBeads() (BeadsStatus, string) {
	// Check if bd exists in PATH
	path, err := exec.LookPath("bd")
	if err != nil {
		return BeadsNotFound, ""
	}
	_ = path // bd found

	// Get version (with timeout to prevent hanging on broken bd installs).
	// 10s is generous but necessary: under heavy CI load (parallel test
	// packages), even a trivial shell script can take >3s to start.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", "version")
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		return BeadsUnknown, ""
	}

	version := parseBeadsVersion(string(output))
	if version == "" {
		return BeadsUnknown, ""
	}

	// Compare versions
	if CompareVersions(version, MinBeadsVersion) < 0 {
		return BeadsTooOld, version
	}

	return BeadsOK, version
}

// EnsureBeads checks for bd and installs it if missing or outdated.
// Returns nil if bd is available and compatible.
// If autoInstall is true, will attempt to install bd when missing.
func EnsureBeads(autoInstall bool) error {
	status, version := CheckBeads()

	switch status {
	case BeadsOK:
		return nil

	case BeadsNotFound:
		if !autoInstall {
			return fmt.Errorf("beads (bd) not found in PATH\n\nInstall with: go install %s", BeadsInstallPath)
		}
		return installBeads()

	case BeadsTooOld:
		return fmt.Errorf("beads version %s is too old (minimum: %s)\n\nUpgrade with: go install %s",
			version, MinBeadsVersion, BeadsInstallPath)

	case BeadsUnknown:
		return fmt.Errorf("beads (bd) version could not be determined\n\nTry reinstalling: go install %s", BeadsInstallPath)
	}

	return nil
}

// installBeads runs go install to install the latest beads.
// GOBIN is set to ~/.local/bin so the binary lands in the canonical
// location rather than the default $GOPATH/bin (~/go/bin/).
func installBeads() error {
	fmt.Printf("   beads (bd) not found. Installing...\n")

	cmd := exec.Command("go", "install", BeadsInstallPath)
	util.SetDetachedProcessGroup(cmd)
	cmd.Env = appendGOBIN(cmd.Environ())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to install beads: %s\n%s", err, string(output))
	}

	// Verify installation
	status, version := CheckBeads()
	if status == BeadsNotFound {
		return fmt.Errorf("beads installed but not in PATH - ensure $GOPATH/bin is in your PATH")
	}
	if status == BeadsTooOld {
		return fmt.Errorf("installed beads %s but minimum required is %s", version, MinBeadsVersion)
	}

	fmt.Printf("   ✓ Installed beads %s\n", version)
	return nil
}

// appendGOBIN returns env with GOBIN set to ~/.local/bin so that
// `go install` places binaries in the canonical location instead of
// the default $GOPATH/bin (which creates a stale shadow copy).
func appendGOBIN(env []string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return env // fall back to default
	}
	gobin := filepath.Join(home, ".local", "bin")
	// Replace existing GOBIN if present, otherwise append.
	for i, e := range env {
		if strings.HasPrefix(e, "GOBIN=") {
			env[i] = "GOBIN=" + gobin
			return env
		}
	}
	return append(env, "GOBIN="+gobin)
}

// parseBeadsVersion extracts the version from bd's text or JSON output.
func parseBeadsVersion(output string) string {
	trimmed := strings.TrimSpace(output)
	data := []byte(trimmed)
	if json.Valid(data) {
		return parseBeadsVersionJSON(data)
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return ""
	}

	// Match canonical text like "bd version 0.52.0" or "bd version 0.52.0 (dev: ...)".
	re := regexp.MustCompile(`^bd version (\d+\.\d+\.\d+)(?: \([^\r\n]*\))?$`)
	matches := re.FindStringSubmatch(trimmed)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func parseBeadsVersionJSON(data []byte) string {
	root, ok := decodeJSONObject(data)
	if !ok {
		return ""
	}
	var schemaVersion int
	if raw, exists := root["schema_version"]; !exists || json.Unmarshal(raw, &schemaVersion) != nil || schemaVersion != 1 {
		return ""
	}

	var version string
	hasVersion := false
	if raw, exists := root["version"]; exists {
		if json.Unmarshal(raw, &version) != nil {
			return ""
		}
		hasVersion = true
	}
	if raw, exists := root["data"]; exists {
		envelope, ok := decodeJSONObject(raw)
		versionRaw, hasEnvelopeVersion := envelope["version"]
		if !ok || !hasEnvelopeVersion {
			return ""
		}
		var envelopeVersion string
		if json.Unmarshal(versionRaw, &envelopeVersion) != nil || hasVersion && version != envelopeVersion {
			return ""
		}
		version = envelopeVersion
		hasVersion = true
	}
	if hasVersion && regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version) {
		return version
	}
	return ""
}

func decodeJSONObject(data []byte) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}

	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, false
		}
		if _, duplicate := object[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		object[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, false
	}
	return object, true
}
