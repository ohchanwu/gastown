//go:build !windows

package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func processGenerationIdentity(pid int) (string, error) {
	alive, err := processAliveChecked(pid)
	if err != nil {
		return "", err
	}
	if !alive {
		return "", errProcessNotFound
	}
	identity, err := readProcessStartIdentity(pid)
	if err != nil {
		alive, aliveErr := processAliveChecked(pid)
		if aliveErr != nil {
			return "", aliveErr
		}
		if !alive {
			return "", errProcessNotFound
		}
		return "", fmt.Errorf("reading process start identity: %w", err)
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return "", errors.New("process start identity is empty")
	}
	return identity, nil
}

func processAliveChecked(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("invalid PID %d", pid)
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func killProcessGroup(pgid int) {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// getParentPID returns the parent process ID (PPID) for a given PID.
// Returns empty string if the process doesn't exist or PPID can't be determined.
func getParentPID(pid string) string {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", pid).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getProcessGroupID returns the process group ID (PGID) for a given PID.
// Returns empty string if the process doesn't exist or PGID can't be determined.
func getProcessGroupID(pid string) string {
	out, err := exec.Command("ps", "-o", "pgid=", "-p", pid).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getProcessGroupMembers returns all PIDs in a process group.
// This finds processes that share the same PGID, including those that reparented to init.
func getProcessGroupMembers(pgid string) []string {
	// Use ps to find all processes with this PGID
	// On macOS: ps -axo pid,pgid
	// On Linux: ps -eo pid,pgid
	out, err := exec.Command("ps", "-axo", "pid,pgid").Output()
	if err != nil {
		return nil
	}

	var members []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimSpace(fields[1]) == pgid {
			members = append(members, strings.TrimSpace(fields[0]))
		}
	}
	return members
}
