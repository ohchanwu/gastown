//go:build linux

package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	linuxSessionCgroupControlDir = "gastown-control"
	linuxSessionCgroupPoolDir    = "gastown-sessions"
)

// ProvisionSessionCgroupRoot creates the two-level cgroup layout used by the
// daemon and contained sessions. The systemd unit delegates the service cgroup;
// the daemon moves itself into a non-delegating control leaf so controllers can
// be enabled on the empty service root, then publishes the sibling session pool
// to every tmux session it starts.
func ProvisionSessionCgroupRoot() error {
	if configured := strings.TrimSpace(os.Getenv(envLinuxSessionCgroupRoot)); configured != "" {
		root, err := resolveLinuxSessionCgroupRoot()
		if err != nil {
			return err
		}
		return preflightLinuxSessionCgroupRoot(root)
	}
	serviceRoot, err := linuxCgroupDirectoryForPID(os.Getpid())
	if err != nil {
		return fmt.Errorf("locating delegated service cgroup: %w", err)
	}
	if serviceRoot == linuxCgroupMount {
		return errors.New("daemon is not inside a delegated systemd service cgroup")
	}
	controlRoot := filepath.Join(serviceRoot, linuxSessionCgroupControlDir)
	sessionRoot := filepath.Join(serviceRoot, linuxSessionCgroupPoolDir)
	for _, path := range []string{controlRoot, sessionRoot} {
		if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating session custody cgroup %s: %w", filepath.Base(path), err)
		}
	}
	if err := restoreLinuxProcessCgroup(controlRoot, os.Getpid()); err != nil {
		return fmt.Errorf("moving daemon into control cgroup: %w", err)
	}
	if err := ensureLinuxSessionCgroupControllers(serviceRoot); err != nil {
		return fmt.Errorf("enabling delegated service controllers: %w", err)
	}
	if err := preflightLinuxSessionCgroupRoot(sessionRoot); err != nil {
		return err
	}
	if err := os.Setenv(envLinuxSessionCgroupRoot, sessionRoot); err != nil {
		return fmt.Errorf("publishing delegated session cgroup root: %w", err)
	}
	return nil
}

func preflightLinuxSessionCgroupRoot(root string) error {
	if !filepath.IsAbs(root) || root == linuxCgroupMount || !strings.HasPrefix(root, linuxCgroupMount+string(os.PathSeparator)) {
		return errors.New("session cgroup root must be below /sys/fs/cgroup")
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("stat delegated session cgroup root: %w", err)
	}
	if !info.IsDir() {
		return errors.New("delegated session cgroup root is not a directory")
	}
	if err := ensureLinuxSessionCgroupControllers(root); err != nil {
		return fmt.Errorf("preflighting delegated session cgroup root: %w", err)
	}
	return nil
}
