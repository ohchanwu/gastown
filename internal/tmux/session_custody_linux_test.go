//go:build linux

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/config"
	"golang.org/x/sys/unix"
)

func TestLinuxSessionCustodyWorkloadHelper(t *testing.T) {
	if os.Getenv("GT_TEST_SESSION_CUSTODY_WORKLOAD") == "" {
		return
	}
	supervisorPID, err := strconv.Atoi(os.Getenv("GT_TEST_SESSION_CUSTODY_SUPERVISOR"))
	if err != nil || supervisorPID <= 0 {
		t.Fatalf("invalid supervisor PID: %v", err)
	}
	escape := "open-denied"
	if parentNS, openErr := os.Open(fmt.Sprintf("/proc/%d/ns/pid", supervisorPID)); openErr == nil {
		_, _, setnsErr := syscall.RawSyscall(syscall.SYS_SETNS, parentNS.Fd(), uintptr(unix.CLONE_NEWPID), 0)
		_ = parentNS.Close()
		escape = "setns-denied:" + setnsErr.Error()
		if setnsErr == 0 {
			escape = "escaped"
		}
	}
	if err := os.WriteFile(os.Getenv("GT_TEST_SESSION_CUSTODY_ESCAPE"), []byte(escape), 0o600); err != nil {
		t.Fatal(err)
	}
	grandchildPath := os.Getenv("GT_TEST_SESSION_CUSTODY_GRANDCHILD")
	script := fmt.Sprintf("(setsid sh -c 'grep ^NSpid: /proc/self/status > %s; while :; do sleep 60; done' </dev/null >/dev/null 2>&1 &) &", config.ShellQuote(grandchildPath))
	if output, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("starting detached grandchild: %v\n%s", err, output)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestLinuxSessionCustodySupervisorHelper(t *testing.T) {
	custody := os.Getenv("GT_TEST_SESSION_CUSTODY_HELPER")
	if custody == "" {
		return
	}
	t.Setenv(EnvSessionCustody, custody)
	t.Setenv("GT_TEST_SESSION_CUSTODY_SUPERVISOR", strconv.Itoa(os.Getpid()))
	command := config.ShellQuote(os.Args[0]) + " -test.run=^TestLinuxSessionCustodyWorkloadHelper$"
	if err := RunSessionCustodyCommand(custody, command); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchLinuxCustodyFallsBackButLaterCleanupFailsClosed(t *testing.T) {
	var attempts []bool
	child, pidfd, contained, err := launchLinuxCustodyCommand("sleep 60", func(command string, namespaced bool) (*exec.Cmd, int, error) {
		attempts = append(attempts, namespaced)
		if namespaced {
			return nil, -1, unix.EPERM
		}
		return startLinuxCustodyCommand(command, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
		_ = child.Wait()
		_ = unix.Close(pidfd)
	})
	if contained {
		t.Fatal("fallback launch reported generation-bound containment")
	}
	if len(attempts) != 2 || !attempts[0] || attempts[1] {
		t.Fatalf("launch attempts = %v, want [true false]", attempts)
	}
	if _, err := retainSessionCustody(uuid.NewString(), os.Getpid()); !errors.Is(err, ErrSessionCustodyUnsupported) {
		t.Fatalf("retainSessionCustody() error = %v, want ErrSessionCustodyUnsupported", err)
	}
}

func TestLinuxSessionCustodyRetainsExitedNamespaceInit(t *testing.T) {
	child, pidfd, contained, err := launchLinuxCustodyCommand("sleep 0.1; exit 7", startLinuxCustodyCommand)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = child.Wait()
		_ = unix.Close(pidfd)
	}()
	if !contained {
		t.Skip("nested PID namespaces unavailable")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		stat, statErr := readLinuxProcessStat(child.Process.Pid)
		if statErr == nil && linuxProcessStateTerminal(stat.state) {
			break
		}
		if statErr != nil {
			t.Fatalf("reading namespace init: %v", statErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("namespace init did not become terminal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	handle, err := retainSessionCustody(uuid.NewString(), os.Getpid())
	if err != nil {
		t.Fatalf("retaining exited namespace init: %v", err)
	}
	defer handle.Close()
	if err := handle.Freeze(context.Background()); err != nil {
		t.Fatalf("preparing exited namespace init: %v", err)
	}
}

func TestLinuxSessionCustodyContainsDoubleForkAndDeniesParentNamespace(t *testing.T) {
	custody := uuid.NewString()
	testDir := t.TempDir()
	escapePath := testDir + "/escape"
	grandchildPath := testDir + "/grandchild"
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCustodySupervisorHelper$")
	cmd.Env = append(os.Environ(),
		"GT_TEST_SESSION_CUSTODY_HELPER="+custody,
		"GT_TEST_SESSION_CUSTODY_WORKLOAD=1",
		"GT_TEST_SESSION_CUSTODY_ESCAPE="+escapePath,
		"GT_TEST_SESSION_CUSTODY_GRANDCHILD="+grandchildPath,
	)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out reaping custody supervisor %d", cmd.Process.Pid)
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		escape, escapeErr := os.ReadFile(escapePath)
		grandchild, grandchildErr := os.ReadFile(grandchildPath)
		if escapeErr == nil && grandchildErr == nil {
			if strings.TrimSpace(string(escape)) == "escaped" {
				t.Fatal("contained workload joined the supervisor PID namespace")
			}
			fields := strings.Fields(string(grandchild))
			if len(fields) < 2 {
				t.Fatalf("malformed grandchild NSpid status: %q", grandchild)
			}
			grandchildPID, parseErr := strconv.Atoi(fields[1])
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			handle, err := retainSessionCustody(custody, cmd.Process.Pid)
			if err != nil {
				t.Fatalf("retaining PID-namespace custody: %v\n%s", err, output.String())
			}
			defer handle.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := handle.Freeze(ctx); err != nil {
				t.Fatal(err)
			}
			committed, err := handle.Kill(ctx)
			if !committed || err != nil {
				t.Fatalf("Kill() = committed %v, err %v", committed, err)
			}
			for i := 0; i < 200; i++ {
				stat, statErr := readLinuxProcessStat(grandchildPID)
				if errorsIsProcessGoneOrTerminal(stat, statErr) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("detached grandchild survived namespace-init kill")
		}
		select {
		case err := <-done:
			t.Fatalf("custody supervisor exited before ready: %v\n%s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("custody workload did not become ready\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func errorsIsProcessGoneOrTerminal(stat linuxProcessStat, err error) bool {
	return errors.Is(err, errProcessNotFound) || err == nil && linuxProcessStateTerminal(stat.state)
}
