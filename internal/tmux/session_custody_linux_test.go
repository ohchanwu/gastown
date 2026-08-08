//go:build linux

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			if err := os.WriteFile(os.Getenv("GT_TEST_SESSION_CUSTODY_WORKLOAD_PID"), []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if unmountErr := unix.Unmount(linuxProcRoot, unix.MNT_DETACH); unmountErr == nil {
		escape += ";mount-escaped"
	} else {
		escape += ";unmount-denied:" + unmountErr.Error()
	}
	if err := os.WriteFile(os.Getenv("GT_TEST_SESSION_CUSTODY_ESCAPE"), []byte(escape), 0o600); err != nil {
		t.Fatal(err)
	}
	socketResult := "unix=allowed"
	fd, socketErr := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if socketErr != nil {
		socketResult = "unix=" + socketErr.Error()
	} else {
		_ = unix.Close(fd)
	}
	if pair, pairErr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0); pairErr != nil {
		socketResult += ";socketpair=" + pairErr.Error()
	} else {
		socketResult += ";socketpair=allowed"
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
	}
	_, _, ioUringErr := unix.RawSyscall(unix.SYS_IO_URING_SETUP, 1, 0, 0)
	socketResult += ";io_uring=" + ioUringErr.Error()
	if inetFD, inetErr := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0); inetErr != nil {
		socketResult += ";inet=" + inetErr.Error()
	} else {
		socketResult += ";inet=allowed"
		_ = unix.Close(inetFD)
	}
	if err := os.WriteFile(os.Getenv("GT_TEST_SESSION_CUSTODY_SOCKET"), []byte(socketResult), 0o600); err != nil {
		t.Fatal(err)
	}
	grandchildPath := os.Getenv("GT_TEST_SESSION_CUSTODY_GRANDCHILD")
	script := fmt.Sprintf("(setsid sh -c 'grep ^NSpid: /proc/$$/status > %s; while :; do sleep 60; done' </dev/null >/dev/null 2>&1 &) &", config.ShellQuote(grandchildPath))
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
	command := os.Getenv("GT_TEST_SESSION_CUSTODY_COMMAND")
	if command == "" {
		command = config.ShellQuote(os.Args[0]) + " -test.run=^TestLinuxSessionCustodyWorkloadHelper$"
	}
	if err := RunSessionCustodyCommand(custody, command); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxDirectChildrenIncludesNonLeaderThreadChildren(t *testing.T) {
	type childResult struct {
		cmd *exec.Cmd
		tid int
		err error
	}
	started := make(chan childResult, 1)
	release := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cmd := exec.Command("sleep", "60")
		err := cmd.Start()
		started <- childResult{cmd: cmd, tid: unix.Gettid(), err: err}
		<-release
	}()
	result := <-started
	if result.err != nil {
		close(release)
		t.Fatal(result.err)
	}
	t.Cleanup(func() {
		_ = result.cmd.Process.Kill()
		_ = result.cmd.Wait()
		close(release)
	})
	if result.tid == os.Getpid() {
		t.Skip("runtime scheduled child launch on the process leader")
	}
	children, err := linuxDirectChildren(linuxProcRoot, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if child == result.cmd.Process.Pid {
			return
		}
	}
	t.Fatalf("non-leader thread %d child %d missing from %v", result.tid, result.cmd.Process.Pid, children)
}

func TestLaunchLinuxCustodyFallsBackButLaterCleanupFailsClosed(t *testing.T) {
	var attempts []bool
	launch, contained, err := launchLinuxCustodyCommand("sleep 60", func(command string, namespaced bool) (*linuxCustodyLaunch, error) {
		attempts = append(attempts, namespaced)
		if namespaced {
			return nil, unix.EPERM
		}
		return startLinuxCustodyCommand(command, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = closeLinuxCustodyLaunch(launch, true)
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

func TestLaunchLinuxCustodyDoesNotFallbackAfterNonNamespaceStartFailure(t *testing.T) {
	startErr := errors.New("broken trusted init executable")
	var attempts []bool
	launch, contained, err := launchLinuxCustodyCommand("printf unsafe", func(_ string, namespaced bool) (*linuxCustodyLaunch, error) {
		attempts = append(attempts, namespaced)
		return nil, startErr
	})
	if launch != nil || contained {
		t.Fatalf("launch = %v, contained %v", launch, contained)
	}
	if !errors.Is(err, startErr) {
		t.Fatalf("launch error = %v, want %v", err, startErr)
	}
	if len(attempts) != 1 || !attempts[0] {
		t.Fatalf("launch attempts = %v, want one contained attempt", attempts)
	}
}

func TestLaunchLinuxCustodyValidationFailureNeverExecutesWorkload(t *testing.T) {
	sideEffect := filepath.Join(t.TempDir(), "ran")
	command := "printf 'ran\\n' >> " + config.ShellQuote(sideEffect)
	validationErr := errors.New("forced post-start validation failure")
	var attempts []bool
	launch, contained, err := launchLinuxCustodyCommandValidated(
		command,
		func(command string, namespaced bool) (*linuxCustodyLaunch, error) {
			attempts = append(attempts, namespaced)
			// Exercise the trusted readiness protocol without requiring nested
			// namespaces; the injected validator supplies the failure boundary.
			return startLinuxCustodyInitCommand(command, false)
		},
		func(string, int, int) (string, uint64, error) {
			return "", 0, validationErr
		},
	)
	if launch != nil || contained {
		t.Fatalf("launch = %v, contained %v", launch, contained)
	}
	if !errors.Is(err, validationErr) {
		t.Fatalf("launch error = %v, want %v", err, validationErr)
	}
	if len(attempts) != 1 || !attempts[0] {
		t.Fatalf("launch attempts = %v, want one contained attempt", attempts)
	}
	if data, readErr := os.ReadFile(sideEffect); !os.IsNotExist(readErr) {
		t.Fatalf("workload ran before validation: data %q, error %v", data, readErr)
	}
}

func TestLinuxSessionCustodyRetainsEmptyNamespaceInit(t *testing.T) {
	launch, contained, err := launchLinuxCustodyCommand("sleep 0.1; exit 7", startLinuxCustodyCommand)
	if errors.Is(err, ErrSessionCustodyUnsupported) {
		t.Skipf("nested PID namespace setup unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = closeLinuxCustodyLaunch(launch, true)
	}()
	if !contained {
		t.Skip("nested PID namespaces unavailable")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		children, childrenErr := linuxDirectChildren(linuxProcRoot, launch.child.Process.Pid)
		if childrenErr == nil && len(children) == 0 {
			break
		}
		if childrenErr != nil {
			t.Fatalf("reading namespace init children: %v", childrenErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("namespace init did not reap exited workload; children %v", children)
		}
		time.Sleep(10 * time.Millisecond)
	}
	handle, err := retainSessionCustody(uuid.NewString(), os.Getpid())
	if err != nil {
		t.Fatalf("retaining empty namespace init: %v", err)
	}
	defer handle.Close()
	if err := handle.Freeze(context.Background()); err != nil {
		t.Fatalf("preparing empty namespace init: %v", err)
	}
}

func TestLinuxSessionCustodyContainsDoubleForkAndDeniesParentNamespace(t *testing.T) {
	custody := uuid.NewString()
	testDir := t.TempDir()
	escapePath := testDir + "/escape"
	grandchildPath := testDir + "/grandchild"
	workloadPIDPath := testDir + "/workload-pid"
	socketPath := testDir + "/socket"
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCustodySupervisorHelper$")
	cmd.Env = append(os.Environ(),
		"GT_TEST_SESSION_CUSTODY_HELPER="+custody,
		"GT_TEST_SESSION_CUSTODY_WORKLOAD=1",
		"GT_TEST_SESSION_CUSTODY_ESCAPE="+escapePath,
		"GT_TEST_SESSION_CUSTODY_GRANDCHILD="+grandchildPath,
		"GT_TEST_SESSION_CUSTODY_WORKLOAD_PID="+workloadPIDPath,
		"GT_TEST_SESSION_CUSTODY_SOCKET="+socketPath,
	)
	var output strings.Builder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	supervisorReaped := false
	t.Cleanup(func() {
		if !supervisorReaped {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Errorf("timed out reaping custody supervisor %d", cmd.Process.Pid)
			}
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		escape, escapeErr := os.ReadFile(escapePath)
		grandchild, grandchildErr := os.ReadFile(grandchildPath)
		workloadPID, workloadPIDErr := os.ReadFile(workloadPIDPath)
		socketResult, socketErr := os.ReadFile(socketPath)
		if escapeErr == nil && grandchildErr == nil && workloadPIDErr == nil && socketErr == nil {
			if strings.Contains(strings.TrimSpace(string(escape)), "escaped") {
				t.Fatal("contained workload joined the supervisor PID namespace")
			}
			fields := strings.Fields(string(grandchild))
			if len(fields) < 2 {
				t.Fatalf("malformed grandchild NSpid status: %q", grandchild)
			}
			workloadFields := strings.Fields(string(workloadPID))
			if len(workloadFields) < 2 || workloadFields[len(workloadFields)-1] == "1" {
				t.Fatalf("workload must run below trusted namespace init, got %q", workloadPID)
			}
			socketEvidence := strings.ToLower(string(socketResult))
			for _, field := range []string{"unix=operation not permitted", "socketpair=operation not permitted", "io_uring=operation not permitted"} {
				if !strings.Contains(socketEvidence, field) {
					t.Fatalf("contained workload bypassed socket broker containment: %q", socketResult)
				}
			}
			if !strings.Contains(socketEvidence, "inet=allowed") {
				t.Fatalf("session custody blocked ordinary TCP sockets: %q", socketResult)
			}
			grandchildNamespacePID, parseErr := strconv.Atoi(fields[len(fields)-1])
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			handle, err := retainSessionCustody(custody, cmd.Process.Pid)
			if err != nil {
				t.Fatalf("retaining PID-namespace custody: %v\n%s", err, output.String())
			}
			defer handle.Close()
			linuxHandle, ok := handle.(*linuxSessionCustody)
			if !ok {
				t.Fatalf("retained custody type = %T", handle)
			}
			grandchildPID, err := findLinuxHostPIDForNamespacePID(linuxHandle.initNamespace, grandchildNamespacePID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := handle.Freeze(ctx); err != nil {
				t.Fatal(err)
			}
			committed, err := handle.Kill(ctx)
			if !committed || err != nil {
				t.Fatalf("Kill() = committed %v, err %v", committed, err)
			}
			select {
			case <-done:
				supervisorReaped = true
			case <-time.After(2 * time.Second):
				t.Fatal("first custody cleanup left the pane supervisor alive")
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
			supervisorReaped = true
			if strings.Contains(output.String(), ErrSessionCustodyUnsupported.Error()) {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("custody supervisor exited before ready: %v\n%s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("custody workload did not become ready\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLinuxSessionGenerationCleanupStopsSupervisorOnFirstRun(t *testing.T) {
	tm := newTestTmux(t)
	session := fmt.Sprintf("gt-custody-stop-%d", time.Now().UnixNano())
	custody := uuid.NewString()
	command := "exec " + config.ShellQuote(os.Args[0]) + " -test.run=^TestLinuxSessionCustodySupervisorHelper$"
	env := map[string]string{
		EnvSessionCustody:                 custody,
		"GT_TEST_SESSION_CUSTODY_HELPER":  custody,
		"GT_TEST_SESSION_CUSTODY_COMMAND": "while :; do sleep 60; done",
	}
	if err := tm.NewSessionWithCommandAndEnv(session, "", command, env); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	var cleanup *SessionGenerationCleanup
	deadline := time.Now().Add(3 * time.Second)
	for {
		cleanup, err = tm.PrepareSessionGenerationProcessCleanup(generation, pane)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			if errors.Is(err, ErrSessionCustodyUnsupported) {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("preparing real session cleanup: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer cleanup.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cleanup.Run(ctx); err != nil {
		t.Fatalf("first exact session cleanup: %v", err)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("first exact session cleanup left the tmux session alive")
	}
}

func TestLinuxSessionCustodySupervisorDeathKillsTrustedInit(t *testing.T) {
	custody := uuid.NewString()
	tempDir := t.TempDir()
	readyPath := filepath.Join(tempDir, "ready")
	lifeFDPath := filepath.Join(tempDir, "life-fd")
	outputPath := filepath.Join(tempDir, "supervisor-output")
	outputFile, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outputFile.Close() })
	readOutput := func() string {
		output, _ := os.ReadFile(outputPath)
		return string(output)
	}
	command := "if [ -e /proc/$$/fd/5 ]; then readlink /proc/$$/fd/5 > " + config.ShellQuote(lifeFDPath) + "; else printf 'closed\\n' > " + config.ShellQuote(lifeFDPath) + "; fi; printf 'ready\\n' > " + config.ShellQuote(readyPath) + "; while :; do sleep 60; done"
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCustodySupervisorHelper$")
	cmd.Env = append(os.Environ(),
		"GT_TEST_SESSION_CUSTODY_HELPER="+custody,
		"GT_TEST_SESSION_CUSTODY_COMMAND="+command,
	)
	// Use a regular file instead of an exec.Cmd-managed output pipe. Wait waits
	// for pipe-copy goroutines after the supervisor exits, and those goroutines
	// stay blocked while the very descendants this test is checking retain the
	// inherited output descriptors.
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Errorf("timed out reaping custody supervisor %d", cmd.Process.Pid)
			}
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	var handle sessionCustodyHandle
	for {
		if _, err := os.ReadFile(readyPath); err == nil {
			handle, err = retainSessionCustody(custody, cmd.Process.Pid)
			if err == nil {
				break
			}
		}
		select {
		case err := <-done:
			reaped = true
			if strings.Contains(readOutput(), ErrSessionCustodyUnsupported.Error()) {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("custody supervisor exited before ready: %v\n%s", err, readOutput())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("custody supervisor did not become ready\n%s", readOutput())
		}
		time.Sleep(10 * time.Millisecond)
	}
	lifeFD, err := os.ReadFile(lifeFDPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(lifeFD)); got != "closed" {
		t.Fatalf("workload inherited trusted-init liveness descriptor: %s", got)
	}
	defer handle.Close()
	linuxHandle := handle.(*linuxSessionCustody)
	children, err := linuxDirectChildren(linuxProcRoot, linuxHandle.initPID)
	if err != nil || len(children) == 0 {
		t.Fatalf("trusted init children = %v, error %v", children, err)
	}
	workloadPID := children[0]
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		reaped = true
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reaping killed custody supervisor")
	}
	for _, target := range []struct {
		name string
		pid  int
	}{{"trusted init", linuxHandle.initPID}, {"workload", workloadPID}} {
		for i := 0; i < 200; i++ {
			stat, statErr := readLinuxProcessStat(target.pid)
			if errorsIsProcessGoneOrTerminal(stat, statErr) {
				break
			}
			if i == 199 {
				t.Fatalf("%s PID %d survived supervisor death", target.name, target.pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func errorsIsProcessGoneOrTerminal(stat linuxProcessStat, err error) bool {
	return errors.Is(err, errProcessNotFound) || err == nil && linuxProcessStateTerminal(stat.state)
}

func findLinuxHostPIDForNamespacePID(namespace uint64, namespacePID int) (int, error) {
	entries, err := os.ReadDir(linuxProcRoot)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		candidateNamespace, err := linuxPIDNamespaceInode(linuxProcRoot, pid)
		if err != nil || candidateNamespace != namespace {
			continue
		}
		pids, err := linuxNamespacePIDs(linuxProcRoot, pid)
		if err == nil && len(pids) > 0 && pids[len(pids)-1] == namespacePID {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("namespace %d has no process %d", namespace, namespacePID)
}
