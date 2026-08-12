//go:build linux

package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLinuxSessionCgroupProvisionHelper(t *testing.T) {
	if os.Getenv("GT_TEST_SESSION_CGROUP_PROVISION_HELPER") != "1" {
		return
	}
	var barrier [1]byte
	if _, err := os.Stdin.Read(barrier[:]); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(envLinuxSessionCgroupRoot); err != nil {
		t.Fatal(err)
	}
	if err := ProvisionSessionCgroupRoot(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "GT_CGROUP_ROOT="+os.Getenv(envLinuxSessionCgroupRoot))
}

func TestLinuxSessionCgroupReceiptRecoveryHelper(t *testing.T) {
	if os.Getenv("GT_TEST_SESSION_CGROUP_RECEIPT_RECOVERY_HELPER") != "1" {
		return
	}
	root, err := resolveLinuxSessionCgroupRoot()
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireLinuxSessionCgroupRootLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := reconcileLinuxSessionCgroupReceipts(root, removeLinuxSessionCgroup); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxSessionCgroupReceiptRecoverySurvivesProcessLifetime(t *testing.T) {
	if os.Getenv(envLinuxSessionCgroupRoot) == "" {
		t.Skip("requires an explicitly delegated GT_SESSION_CGROUP_ROOT")
	}
	root, err := resolveLinuxSessionCgroupRoot()
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireLinuxSessionCgroupRootLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureLinuxSessionCgroupControllers(root); err != nil {
		release()
		t.Fatal(err)
	}
	stale, err := os.MkdirTemp(root, linuxSessionCgroupPrefix)
	if err != nil {
		release()
		t.Fatal(err)
	}
	active, err := os.MkdirTemp(root, linuxSessionCgroupPrefix)
	if err != nil {
		release()
		_ = removeLinuxSessionCgroup(stale, linuxSessionCgroupRemoveWait)
		t.Fatal(err)
	}
	worker := exec.Command("/bin/sh", "-c", "exec sleep 60")
	if err := worker.Start(); err != nil {
		release()
		_ = removeLinuxSessionCgroup(stale, linuxSessionCgroupRemoveWait)
		_ = removeLinuxSessionCgroup(active, linuxSessionCgroupRemoveWait)
		t.Fatal(err)
	}
	if err := writeLinuxCgroupControl(filepath.Join(active, "cgroup.procs"), strconv.Itoa(worker.Process.Pid)); err != nil {
		release()
		_ = worker.Process.Kill()
		_ = worker.Wait()
		_ = removeLinuxSessionCgroup(stale, linuxSessionCgroupRemoveWait)
		_ = removeLinuxSessionCgroup(active, linuxSessionCgroupRemoveWait)
		t.Fatal(err)
	}
	release()
	cleaned := false
	t.Cleanup(func() {
		_ = worker.Process.Kill()
		_ = worker.Wait()
		_ = removeLinuxSessionCgroup(active, linuxSessionCgroupRemoveWait)
		if !cleaned {
			_ = removeLinuxSessionCgroup(stale, linuxSessionCgroupRemoveWait)
		}
	})

	helper := exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCgroupReceiptRecoveryHelper$")
	helper.Env = append(os.Environ(), "GT_TEST_SESSION_CGROUP_RECEIPT_RECOVERY_HELPER=1")
	output, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("fresh-process cgroup receipt recovery: %v\n%s", err, output)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("fresh process retained empty cgroup receipt: %v", err)
	}
	cleaned = true
	if _, err := os.Stat(active); err != nil {
		t.Fatalf("fresh process touched active cgroup: %v", err)
	}
}

func TestReconcileLinuxSessionCgroupReceiptsRemovesOnlyEmptyOwnedEntries(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, linuxSessionCgroupPrefix+"empty")
	active := filepath.Join(root, linuxSessionCgroupPrefix+"active")
	foreign := filepath.Join(root, "foreign-empty")
	for _, path := range []string{empty, active, foreign} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(empty, "cgroup.procs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(active, "cgroup.procs"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var removed []string
	err := reconcileLinuxSessionCgroupReceipts(root, func(path string, _ time.Duration) error {
		removed = append(removed, path)
		return os.RemoveAll(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != empty {
		t.Fatalf("reconciled cgroups = %q, want only %q", removed, empty)
	}
	for _, path := range []string{active, foreign} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("reconciliation touched preserved path %q: %v", path, err)
		}
	}
}

func TestProvisionSessionCgroupRootMatchesSystemdDelegationShape(t *testing.T) {
	delegated := os.Getenv(envLinuxSessionCgroupRoot)
	if delegated == "" {
		t.Skip("requires an explicitly delegated GT_SESSION_CGROUP_ROOT")
	}
	if err := ensureLinuxSessionCgroupControllers(delegated); err != nil {
		t.Fatal(err)
	}
	service, err := os.MkdirTemp(delegated, "gastown-daemon-test-")
	if err != nil {
		t.Fatal(err)
	}
	var command *exec.Cmd
	t.Cleanup(func() {
		if command != nil && command.Process != nil && command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		for _, path := range []string{
			filepath.Join(service, linuxSessionCgroupControlDir),
			filepath.Join(service, linuxSessionCgroupPoolDir),
			service,
		} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Errorf("removing owned test cgroup %s: %v", path, err)
			}
		}
	})
	command = exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCgroupProvisionHelper$")
	command.Env = append(os.Environ(), "GT_TEST_SESSION_CGROUP_PROVISION_HELPER=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := writeLinuxCgroupControl(filepath.Join(service, "cgroup.procs"), strconv.Itoa(command.Process.Pid)); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("provision helper: %v\n%s", err, output.String())
	}
	wantRoot := filepath.Join(service, linuxSessionCgroupPoolDir)
	got, err := linuxCustodyOutputMarker(output.String(), "GT_CGROUP_ROOT=")
	if err != nil {
		t.Fatalf("provision helper omitted root marker: %v\n%s", err, output.String())
	}
	if got != wantRoot {
		t.Fatalf("provisioned root = %q, want %q", got, wantRoot)
	}
	for _, root := range []string{service, wantRoot} {
		controllers, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
		if err != nil {
			t.Fatal(err)
		}
		for _, controller := range linuxSessionCgroupControllers {
			if !containsString(strings.Fields(string(controllers)), controller) {
				t.Fatalf("%s lacks delegated %s controller: %q", root, controller, controllers)
			}
		}
	}
}

func TestClearLinuxSessionCgroupReceiptRetainsFailedRemoval(t *testing.T) {
	receipt := "/sys/fs/cgroup/gastown-session-test"
	removeErr := errors.New("busy")
	calls := 0
	remove := func(path string, timeout time.Duration) error {
		calls++
		if path != receipt || timeout != linuxSessionCgroupRemoveWait {
			t.Fatalf("remove(%q, %v)", path, timeout)
		}
		if calls == 1 {
			return removeErr
		}
		return nil
	}
	if err := clearLinuxSessionCgroupReceipt(&receipt, remove); !errors.Is(err, removeErr) {
		t.Fatalf("first removal error = %v, want %v", err, removeErr)
	}
	if receipt == "" {
		t.Fatal("failed cgroup removal cleared retry receipt")
	}
	if err := clearLinuxSessionCgroupReceipt(&receipt, remove); err != nil {
		t.Fatal(err)
	}
	if receipt != "" {
		t.Fatalf("successful cgroup removal retained receipt %q", receipt)
	}
}

func TestClearLinuxSessionCgroupReceiptEmptyReceiptRemainsNoOpWithoutRemover(t *testing.T) {
	receipt := ""
	if err := clearLinuxSessionCgroupReceipt(&receipt, nil); err != nil {
		t.Fatalf("empty receipt clear = %v, want legacy no-op", err)
	}
}

func TestClearLinuxSessionCgroupReceiptWithContextRetainsReceiptAfterDeadline(t *testing.T) {
	receipt := "/sys/fs/cgroup/gastown-session-test"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	removeCalls := 0
	err := clearLinuxSessionCgroupReceiptWithContext(
		ctx,
		&receipt,
		func(context.Context, string, time.Duration) error {
			removeCalls++
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("removal error = %v, want canceled context", err)
	}
	if removeCalls != 0 || receipt == "" {
		t.Fatalf("expired removal = calls %d, receipt %q; want retained ownership without removal", removeCalls, receipt)
	}
}

func TestRemoveLinuxSessionCgroupWithContextSkipsRemovalAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	removeCalls := 0
	err := removeLinuxSessionCgroupWithContextAndRemove(
		ctx,
		filepath.Join(linuxCgroupMount, linuxSessionCgroupPrefix+"cancelled"),
		linuxSessionCgroupRemoveWait,
		func(string) error {
			removeCalls++
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("removal error = %v, want canceled context", err)
	}
	if removeCalls != 0 {
		t.Fatalf("removal calls = %d, want no destructive attempt after cancellation", removeCalls)
	}
}

func TestClearLinuxSessionCgroupReceiptRetainsReceiptWhenCancellationLandsDuringRemoval(t *testing.T) {
	receipt := "/sys/fs/cgroup/gastown-session-test"
	ctx, cancel := context.WithCancel(context.Background())
	removeCalls := 0
	err := clearLinuxSessionCgroupReceiptWithContext(
		ctx,
		&receipt,
		func(context.Context, string, time.Duration) error {
			removeCalls++
			cancel()
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("removal error = %v, want cancellation that landed during removal", err)
	}
	if removeCalls != 1 || receipt == "" {
		t.Fatalf("canceled removal = calls %d, receipt %q; want retained custody", removeCalls, receipt)
	}
}

func TestRemoveLinuxSessionCgroupWithContextReportsCancellationDuringRemoval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	removeCalls := 0
	err := removeLinuxSessionCgroupWithContextAndRemove(
		ctx,
		filepath.Join(linuxCgroupMount, linuxSessionCgroupPrefix+"cancel-in-flight"),
		linuxSessionCgroupRemoveWait,
		func(string) error {
			removeCalls++
			cancel()
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("removal error = %v, want cancellation that landed during removal", err)
	}
	if removeCalls != 1 {
		t.Fatalf("removal calls = %d, want exactly one in-flight attempt", removeCalls)
	}
}

func TestRemoveLinuxSessionCgroupWithContextStopsRetryAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	removeCalls := 0
	removeErr := errors.New("busy")
	err := removeLinuxSessionCgroupWithContextAndRemove(
		ctx,
		filepath.Join(linuxCgroupMount, linuxSessionCgroupPrefix+"cancel-retry"),
		linuxSessionCgroupRemoveWait,
		func(string) error {
			removeCalls++
			cancel()
			return removeErr
		},
	)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, removeErr) {
		t.Fatalf("removal error = %v, want busy evidence joined with cancellation", err)
	}
	if removeCalls != 1 {
		t.Fatalf("removal calls = %d, want no second destructive attempt after cancellation", removeCalls)
	}
}

func TestParseLinuxUnifiedCgroupPath(t *testing.T) {
	path, err := parseLinuxUnifiedCgroupPath([]byte("0::/delegated/session\n"))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/delegated/session" {
		t.Fatalf("path = %q, want /delegated/session", path)
	}
	for _, invalid := range [][]byte{nil, []byte("2:cpu:/legacy\n"), []byte("0::relative\n")} {
		if _, err := parseLinuxUnifiedCgroupPath(invalid); err == nil {
			t.Fatalf("parseLinuxUnifiedCgroupPath(%q) succeeded", invalid)
		}
	}
}

func TestLinuxSessionCgroupEnforcesLimitsAndCleansUp(t *testing.T) {
	if os.Getenv(envLinuxSessionCgroupRoot) == "" {
		t.Skip("requires an explicitly delegated GT_SESSION_CGROUP_ROOT")
	}
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinWriter.Close()
	cmd := exec.Command("/bin/sh", "-c", `
read barrier
while :; do sleep 60 2>/dev/null & done
`)
	cmd.Stdin = stdinReader
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdinReader.Close()
	path, err := prepareLinuxSessionCgroup(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	removed := false
	t.Cleanup(func() {
		if !removed {
			if fileExists(filepath.Join(path, "cgroup.kill")) {
				_ = os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o600)
			}
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = removeLinuxSessionCgroup(path, linuxSessionCgroupRemoveWait)
		}
	})
	for name, want := range map[string]string{
		"pids.max":   linuxSessionCgroupPIDsMax,
		"memory.max": linuxSessionCgroupMemoryMax,
		"cpu.max":    linuxSessionCgroupCPUMax,
	} {
		data, readErr := os.ReadFile(filepath.Join(path, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.TrimSpace(string(data)) != want {
			t.Fatalf("%s = %q, want %q", name, strings.TrimSpace(string(data)), want)
		}
	}
	membership, err := linuxCgroupDirectoryForPID(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if membership != path {
		t.Fatalf("process cgroup = %q, want %q", membership, path)
	}
	if _, err := stdinWriter.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		events, readErr := os.ReadFile(filepath.Join(path, "pids.events"))
		if readErr != nil {
			t.Fatal(readErr)
		}
		fields := strings.Fields(string(events))
		denials := 0
		for index := 0; index+1 < len(fields); index += 2 {
			if fields[index] == "max" {
				denials, _ = strconv.Atoi(fields[index+1])
			}
		}
		if denials > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pids.max overload produced no denial event: %q", events)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := removeLinuxSessionCgroup(path, linuxSessionCgroupRemoveWait); err != nil {
		t.Fatal(err)
	}
	removed = true
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("bounded session cgroup remained after cleanup: %v", err)
	}
}
