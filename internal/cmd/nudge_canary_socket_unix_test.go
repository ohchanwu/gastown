//go:build !windows

package cmd

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

func TestWakeCanaryCleanupRemovesOnlyOwnedSocket(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	suffix := time.Now().UnixNano()
	ownedSocket := fmt.Sprintf("gt-wake-canary-ndg-cleanup-%d", suffix)
	unrelatedSocket := fmt.Sprintf("gt-unrelated-live-%d", suffix)
	owned := tmux.NewTmuxWithSocket(ownedSocket)
	unrelated := tmux.NewTmuxWithSocket(unrelatedSocket)
	ownedPath := filepath.Join(tmux.SocketDir(), ownedSocket)
	unrelatedPath := filepath.Join(tmux.SocketDir(), unrelatedSocket)
	t.Cleanup(func() {
		_ = owned.KillServer()
		_ = unrelated.KillServer()
		_ = os.Remove(ownedPath)
		_ = os.Remove(unrelatedPath)
	})
	if err := owned.NewSessionWithCommand("gt-canary-owned", t.TempDir(), "sleep 60"); err != nil {
		t.Fatalf("create owned tmux server: %v", err)
	}
	if err := unrelated.NewSessionWithCommand("gt-unrelated-live", t.TempDir(), "sleep 60"); err != nil {
		t.Fatalf("create unrelated tmux server: %v", err)
	}

	unrelatedUnixPath := filepath.Join(tmux.SocketDir(), fmt.Sprintf("gt-unrelated-path-%d", suffix))
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: unrelatedUnixPath, Net: "unix"})
	if err != nil {
		t.Fatalf("create unrelated Unix socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close unrelated Unix socket: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(unrelatedUnixPath) })

	sandboxRoot := t.TempDir()
	sandbox := &wakeCanarySandbox{
		TownRoot: sandboxRoot,
		Socket:   ownedSocket,
		Session:  "gt-canary-owned",
		tmux:     owned,
	}
	if err := sandbox.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Lstat(ownedPath); !os.IsNotExist(err) {
		t.Fatalf("owned stale socket still exists: %v", err)
	}
	if alive, err := unrelated.HasSession("gt-unrelated-live"); err != nil || !alive {
		t.Fatalf("unrelated tmux server survived: alive=%t err=%v", alive, err)
	}
	if info, err := os.Lstat(unrelatedUnixPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("unrelated Unix socket survived: mode_socket=%t err=%v", err == nil && info.Mode()&os.ModeSocket != 0, err)
	}
}

func TestWakeCanaryCleanupRefusesAmbiguousSocketOwnership(t *testing.T) {
	name := fmt.Sprintf("gt-canary-ambiguous-%d", time.Now().UnixNano())
	target := filepath.Join(filepath.Dir(tmux.SocketDir()), name)
	if err := os.WriteFile(target, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(target) })

	sandbox := &wakeCanarySandbox{TownRoot: t.TempDir(), Socket: filepath.Join("..", name)}
	if err := sandbox.Cleanup(); err == nil {
		t.Fatal("Cleanup with ambiguous socket ownership = nil, want error")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "unrelated" {
		t.Fatalf("ambiguous target changed: preserved=%t err=%v", string(data) == "unrelated", err)
	}
}

func TestProbeWakeCanaryOwnedSocketCleanup(t *testing.T) {
	if os.Getenv("GT_RUN_WAKE_CANARY_CLEANUP_PROBE") != "1" {
		t.Skip("set GT_RUN_WAKE_CANARY_CLEANUP_PROBE=1 for the private isolated probe")
	}
	evidencePath := os.Getenv("GT_WAKE_CANARY_CLEANUP_EVIDENCE")
	if !filepath.IsAbs(evidencePath) {
		t.Fatal("GT_WAKE_CANARY_CLEANUP_EVIDENCE must be an absolute path")
	}
	countCanarySockets := func() int {
		entries, err := os.ReadDir(tmux.SocketDir())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "gt-wake-canary-") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode()&os.ModeSocket != 0 {
				count++
			}
		}
		return count
	}

	baseline := countCanarySockets()
	sandbox, err := newWakeCanarySandbox("", buildWakeCanaryCandidateGT(t))
	if err != nil {
		t.Fatalf("newWakeCanarySandbox: %v", err)
	}
	t.Cleanup(func() { _ = sandbox.Cleanup() })
	root := sandbox.TownRoot
	socketPath := filepath.Join(tmux.SocketDir(), sandbox.Socket)
	if err := sandbox.tmux.NewSessionWithCommand(sandbox.Session, sandbox.WorkDir, "sleep 60"); err != nil {
		t.Fatalf("create canary-owned server: %v", err)
	}
	if info, err := os.Lstat(socketPath); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("canary socket created: mode_socket=%t err=%v", err == nil && info.Mode()&os.ModeSocket != 0, err)
	}
	if err := sandbox.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	_, socketErr := os.Lstat(socketPath)
	_, rootErr := os.Stat(root)
	after := countCanarySockets()
	metadata := fmt.Sprintf("baseline_count=%d\nafter_count=%d\ncounts_equal=%t\nexact_socket_absent=%t\nsandbox_root_absent=%t\nzero_deliveries=true\n", baseline, after, baseline == after, os.IsNotExist(socketErr), os.IsNotExist(rootErr))
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(evidencePath, 0600); err != nil {
		t.Fatal(err)
	}
	if baseline != after || !os.IsNotExist(socketErr) || !os.IsNotExist(rootErr) {
		t.Fatalf("cleanup proof failed: counts_equal=%t socket_absent=%t root_absent=%t", baseline == after, os.IsNotExist(socketErr), os.IsNotExist(rootErr))
	}
}
