//go:build linux

package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseLinuxUnifiedCgroupPath(t *testing.T) {
	path, err := parseLinuxUnifiedCgroupPath([]byte("0::/delegated/session\n"))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/delegated/session" {
		t.Fatalf("path = %q, want /delegated/session", path)
	}
	for _, invalid := range [][]byte{nil, []byte("2:cpu:/legacy\n"), []byte("0::relative\n")} {
		if _, err := parseLinuxUnifiedCgroupPath(invalid); err == nil {
			t.Fatalf("parseLinuxUnifiedCgroupPath(%q) succeeded", invalid)
		}
	}
}

func TestLinuxSessionCgroupEnforcesLimitsAndCleansUp(t *testing.T) {
	if os.Getenv(envLinuxSessionCgroupRoot) == "" {
		t.Skip("requires an explicitly delegated GT_SESSION_CGROUP_ROOT")
	}
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinWriter.Close()
	cmd := exec.Command("/bin/sh", "-c", `
read barrier
while :; do sleep 60 2>/dev/null & done
`)
	cmd.Stdin = stdinReader
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdinReader.Close()
	path, err := prepareLinuxSessionCgroup(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	removed := false
	t.Cleanup(func() {
		if !removed {
			if fileExists(filepath.Join(path, "cgroup.kill")) {
				_ = os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o600)
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = removeLinuxSessionCgroup(path, linuxSessionCgroupRemoveWait)
		}
	})
	for name, want := range map[string]string{
		"pids.max":   linuxSessionCgroupPIDsMax,
		"memory.max": linuxSessionCgroupMemoryMax,
		"cpu.max":    linuxSessionCgroupCPUMax,
	} {
		data, readErr := os.ReadFile(filepath.Join(path, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimSpace(string(data)) != want {
			t.Fatalf("%s = %q, want %q", name, strings.TrimSpace(string(data)), want)
		}
	}
	membership, err := linuxCgroupDirectoryForPID(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if membership != path {
		t.Fatalf("process cgroup = %q, want %q", membership, path)
	}
	if _, err := stdinWriter.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, readErr := os.ReadFile(filepath.Join(path, "pids.events"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		fields := strings.Fields(string(events))
		denials := 0
		for index := 0; index+1 < len(fields); index += 2 {
			if fields[index] == "max" {
				denials, _ = strconv.Atoi(fields[index+1])
			}
		}
		if denials > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pids.max overload produced no denial event: %q", events)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := removeLinuxSessionCgroup(path, linuxSessionCgroupRemoveWait); err != nil {
		t.Fatal(err)
	}
	removed = true
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bounded session cgroup remained after cleanup: %v", err)
	}
}
