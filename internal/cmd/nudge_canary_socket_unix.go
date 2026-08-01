//go:build !windows

package cmd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

func removeWakeCanarySocketPath(socket string) error {
	if filepath.Base(socket) != socket || !strings.HasPrefix(socket, "gt-wake-canary-ndg-") {
		return fmt.Errorf("refusing ambiguous wake canary socket ownership: %q", socket)
	}
	socketDir := filepath.Clean(tmux.SocketDir())
	socketPath := filepath.Join(socketDir, socket)
	if filepath.Dir(socketPath) != socketDir {
		return fmt.Errorf("refusing wake canary socket outside owned directory: %q", socketPath)
	}
	before, err := os.Lstat(socketPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("lstat wake canary socket: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || before.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing non-socket wake canary path: %q", socketPath)
	}
	conn, dialErr := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("refusing live wake canary socket: %q", socketPath)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
		return fmt.Errorf("cannot prove wake canary socket stale: %w", dialErr)
	}
	after, err := os.Lstat(socketPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("recheck wake canary socket: %w", err)
	}
	if after.Mode()&os.ModeSocket == 0 || !os.SameFile(before, after) {
		return fmt.Errorf("wake canary socket changed during cleanup: %q", socketPath)
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove wake canary socket: %w", err)
	}
	return nil
}
