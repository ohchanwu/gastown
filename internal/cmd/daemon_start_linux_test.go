//go:build linux

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxDaemonStartUsesDelegatedSystemdUnitAndNeverSpawnsGT(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemctl.log")
	gtLogPath := filepath.Join(dir, "gt.log")
	systemctl := filepath.Join(dir, "systemctl")
	gt := filepath.Join(dir, "gt")
	if err := os.WriteFile(systemctl, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$SYSTEMCTL_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gt, []byte("#!/bin/sh\nprintf called >\"$GT_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SYSTEMCTL_LOG", logPath)
	t.Setenv("GT_LOG", gtLogPath)
	process, err := startDaemonBackground(dir, gt)
	if err != nil {
		t.Fatal(err)
	}
	if process != nil {
		t.Fatalf("Linux systemd start returned detached process %#v", process)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "--user start gastown-daemon.service" {
		t.Fatalf("systemctl arguments = %q", data)
	}
	if _, err := os.Stat(gtLogPath); !os.IsNotExist(err) {
		t.Fatalf("ordinary gt daemon run was spawned: %v", err)
	}
}

func TestLinuxDaemonStartFailsBeforeSpawnWhenSystemdRejects(t *testing.T) {
	dir := t.TempDir()
	gtLogPath := filepath.Join(dir, "gt.log")
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf 'unit lacks delegation' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	gt := filepath.Join(dir, "gt")
	if err := os.WriteFile(gt, []byte("#!/bin/sh\nprintf called >\"$GT_LOG\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GT_LOG", gtLogPath)
	if process, err := startDaemonBackground(dir, gt); err == nil || process != nil || !strings.Contains(err.Error(), "unit lacks delegation") {
		t.Fatalf("startDaemonBackground = process %#v error %v", process, err)
	}
	if _, err := os.Stat(gtLogPath); !os.IsNotExist(err) {
		t.Fatalf("ordinary gt daemon run was spawned after systemd failure: %v", err)
	}
}
