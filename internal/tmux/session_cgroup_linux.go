//go:build linux

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	linuxCgroupMount             = "/sys/fs/cgroup"
	envLinuxSessionCgroupRoot    = "GT_SESSION_CGROUP_ROOT"
	linuxSessionCgroupPrefix     = "gastown-session-"
	linuxSessionCgroupPIDsMax    = "128"
	linuxSessionCgroupMemoryMax  = "1073741824"
	linuxSessionCgroupCPUMax     = "200000 100000"
	linuxSessionCgroupRemoveWait = 3 * time.Second
)

var linuxSessionCgroupControllers = []string{"cpu", "memory", "pids"}

func parseLinuxUnifiedCgroupPath(data []byte) (string, error) {
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			path := strings.TrimPrefix(line, "0::")
			if path == "" || !filepath.IsAbs(path) {
				return "", errors.New("unified cgroup path is invalid")
			}
			return filepath.Clean(path), nil
		}
	}
	return "", errors.New("unified cgroup v2 membership is unavailable")
}

func linuxCgroupDirectoryForPID(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid cgroup process PID")
	}
	data, err := os.ReadFile(filepath.Join(linuxProcRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", fmt.Errorf("reading process cgroup membership: %w", err)
	}
	relative, err := parseLinuxUnifiedCgroupPath(data)
	if err != nil {
		return "", err
	}
	path := filepath.Clean(filepath.Join(linuxCgroupMount, relative))
	if path != linuxCgroupMount && !strings.HasPrefix(path, linuxCgroupMount+string(os.PathSeparator)) {
		return "", errors.New("process cgroup escaped the unified hierarchy")
	}
	return path, nil
}

func resolveLinuxSessionCgroupRoot() (string, error) {
	root := strings.TrimSpace(os.Getenv(envLinuxSessionCgroupRoot))
	if root == "" {
		return linuxCgroupDirectoryForPID(os.Getpid())
	}
	if !filepath.IsAbs(root) {
		return "", errors.New("GT_SESSION_CGROUP_ROOT must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("resolving delegated session cgroup root: %w", err)
	}
	if resolved == linuxCgroupMount || !strings.HasPrefix(resolved, linuxCgroupMount+string(os.PathSeparator)) {
		return "", errors.New("GT_SESSION_CGROUP_ROOT must name a delegated directory below /sys/fs/cgroup")
	}
	return resolved, nil
}

func ensureLinuxSessionCgroupControllers(root string) error {
	availableData, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("reading delegated cgroup controllers: %w", err)
	}
	available := strings.Fields(string(availableData))
	for _, controller := range linuxSessionCgroupControllers {
		if !containsString(available, controller) {
			return fmt.Errorf("delegated cgroup root lacks %s controller", controller)
		}
	}
	enabledPath := filepath.Join(root, "cgroup.subtree_control")
	enabledData, err := os.ReadFile(enabledPath)
	if err != nil {
		return fmt.Errorf("reading delegated cgroup controller state: %w", err)
	}
	enabled := strings.Fields(string(enabledData))
	var missing []string
	for _, controller := range linuxSessionCgroupControllers {
		if !containsString(enabled, controller) {
			missing = append(missing, "+"+controller)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	processes, err := os.ReadFile(filepath.Join(root, "cgroup.procs"))
	if err != nil {
		return fmt.Errorf("checking delegated cgroup root occupancy: %w", err)
	}
	if strings.TrimSpace(string(processes)) != "" {
		return errors.New("session cgroup root is not delegated: controllers are disabled while the root contains processes")
	}
	if err := os.WriteFile(enabledPath, []byte(strings.Join(missing, " ")), 0o600); err != nil {
		return fmt.Errorf("enabling delegated session cgroup controllers: %w", err)
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func writeLinuxCgroupControl(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	return nil
}

func prepareLinuxSessionCgroup(pid int) (_ string, retErr error) {
	root, err := resolveLinuxSessionCgroupRoot()
	if err != nil {
		return "", err
	}
	if err := ensureLinuxSessionCgroupControllers(root); err != nil {
		return "", err
	}
	path, err := os.MkdirTemp(root, linuxSessionCgroupPrefix)
	if err != nil {
		return "", fmt.Errorf("creating bounded session cgroup: %w", err)
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.Remove(path))
		}
	}()
	limits := []struct{ name, value string }{
		{"pids.max", linuxSessionCgroupPIDsMax},
		{"memory.max", linuxSessionCgroupMemoryMax},
		{"cpu.max", linuxSessionCgroupCPUMax},
	}
	for _, limit := range limits {
		if err := writeLinuxCgroupControl(filepath.Join(path, limit.name), limit.value); err != nil {
			return "", err
		}
	}
	if swapPath := filepath.Join(path, "memory.swap.max"); fileExists(swapPath) {
		if err := writeLinuxCgroupControl(swapPath, "0"); err != nil {
			return "", err
		}
	}
	if err := writeLinuxCgroupControl(filepath.Join(path, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return "", fmt.Errorf("moving trusted init into bounded session cgroup: %w", err)
	}
	return path, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func removeLinuxSessionCgroup(path string, timeout time.Duration) error {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(filepath.Base(clean), linuxSessionCgroupPrefix) ||
		!strings.HasPrefix(clean, linuxCgroupMount+string(os.PathSeparator)) {
		return errors.New("refusing to remove unrecognized session cgroup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var lastErr error
	for {
		err := os.Remove(clean)
		if err == nil || os.IsNotExist(err) {
			return nil
		}
		lastErr = err
		if err := waitForContext(ctx, processExitPollInterval); err != nil {
			return errors.Join(fmt.Errorf("removing bounded session cgroup: %w", lastErr), err)
		}
	}
}
