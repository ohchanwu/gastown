//go:build linux

package tmux

import (
	"fmt"

	"golang.org/x/sys/unix"
)

const (
	linuxSessionNoFileLimit = 1024
	linuxSessionFileLimit   = 1 << 30
	linuxSessionASLimit     = 4 << 30
)

func setLinuxSessionRlimit(resource int, limit uint64) error {
	var current unix.Rlimit
	if err := unix.Getrlimit(resource, &current); err != nil {
		return err
	}
	if current.Max < limit {
		limit = current.Max
	}
	bounded := &unix.Rlimit{Cur: limit, Max: limit}
	if err := unix.Setrlimit(resource, bounded); err != nil {
		return err
	}
	return nil
}

func applyLinuxSessionResourceLimits() error {
	limits := []struct {
		name     string
		resource int
		value    uint64
	}{
		{"open files", unix.RLIMIT_NOFILE, linuxSessionNoFileLimit},
		{"file size", unix.RLIMIT_FSIZE, linuxSessionFileLimit},
		{"address space", unix.RLIMIT_AS, linuxSessionASLimit},
		{"core size", unix.RLIMIT_CORE, 0},
	}
	for _, limit := range limits {
		if err := setLinuxSessionRlimit(limit.resource, limit.value); err != nil {
			return fmt.Errorf("setting session %s resource limit: %w", limit.name, err)
		}
	}
	return nil
}
