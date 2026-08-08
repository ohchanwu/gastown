//go:build linux

package tmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLinuxSessionCustodyHelperProcess(t *testing.T) {
	custody := os.Getenv("GT_TEST_SESSION_CUSTODY_HELPER")
	if custody == "" {
		return
	}
	if err := activateLinuxSessionCustody(custody); err != nil {
		fmt.Fprintf(os.Stderr, "SESSION_CUSTODY_SKIP:%v\n", err)
		return
	}
	child := exec.Command("sh", "-c", `(sleep 60 & echo $! > "$GT_TEST_GRANDCHILD_PATH") & wait`)
	child.Env = os.Environ()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if data, err := os.ReadFile(os.Getenv("GT_TEST_GRANDCHILD_PATH")); err == nil && strings.TrimSpace(string(data)) != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached grandchild PID was not published")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(os.Getenv("GT_TEST_CUSTODY_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	select {}
}

func TestLinuxSessionCustodyContainsDoubleForkAndIgnoresSIGCONT(t *testing.T) {
	custody := uuid.NewString()
	readyPath := t.TempDir() + "/ready"
	grandchildPath := t.TempDir() + "/grandchild"
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCustodyHelperProcess$")
	cmd.Env = append(os.Environ(),
		"GT_TEST_SESSION_CUSTODY_HELPER="+custody,
		"GT_TEST_CUSTODY_READY="+readyPath,
		"GT_TEST_GRANDCHILD_PATH="+grandchildPath,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		select {
		case <-done:
			if strings.Contains(output.String(), "SESSION_CUSTODY_SKIP:") {
				t.Skipf("cgroup v2 delegation unavailable: %s", strings.TrimSpace(output.String()))
			}
			t.Fatalf("custody helper exited before ready: %v\n%s", waitErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("custody helper did not become ready\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	handle, err := retainSessionCustody(custody, cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := handle.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	grandchildData, err := os.ReadFile(grandchildPath)
	if err != nil {
		t.Fatal(err)
	}
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(string(grandchildData)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(grandchildPID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	events, err := readCgroupControl(handle.(*linuxSessionCustody).dir, "cgroup.events")
	if err != nil {
		t.Fatal(err)
	}
	if frozen, ok := cgroupEventValue(events, "frozen"); !ok || frozen != "1" {
		t.Fatalf("SIGCONT reopened frozen session cgroup: events=%q", events)
	}
	committed, err := handle.Kill(ctx)
	if !committed || err != nil {
		t.Fatalf("Kill() = committed %v, err %v", committed, err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("contained helper survived cgroup.kill")
	}
}
