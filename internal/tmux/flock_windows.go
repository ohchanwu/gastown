//go:build windows

package tmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// windowsFlockMu serializes acquireFlockLock calls on Windows where flock(2) is unavailable.
// In-process locking is sufficient for Windows since tmux is not available there anyway.
var windowsFlockMu sync.Mutex

// acquireFlockLock provides in-process locking on Windows (flock(2) is unavailable).
// Since tmux is not supported on Windows, this is only reached in tests; it uses
// a global mutex rather than per-path locking for simplicity.
func acquireFlockLock(lockPath string, timeout time.Duration) (func(), error) {
	return acquireFlockLockContext(context.Background(), lockPath, timeout)
}

func acquireFlockLockContext(ctx context.Context, lockPath string, timeout time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(lockPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating lock dir: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("securing lock dir: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		if windowsFlockMu.TryLock() {
			return func() { windowsFlockMu.Unlock() }, nil
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout after %s waiting for lock", timeout)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
