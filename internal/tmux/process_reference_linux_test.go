//go:build linux

package tmux

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLinuxRetainedProcessTreeFreezesAndKillsNativeChild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out reaping native retained-process fixture %d", cmd.Process.Pid)
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		relations, err := linuxProcessRelations(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if len(relations) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native fixture did not start its child")
		}
		time.Sleep(10 * time.Millisecond)
	}

	processes, err := retainProcessTree(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeRetainedProcesses(processes); err != nil {
			t.Errorf("closing native retained references: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	processes, err = stabilizeProcessTree(ctx, cmd.Process.Pid, processes)
	if err != nil {
		t.Fatal(err)
	}
	if len(processes) < 2 {
		t.Fatalf("stable native custody retained %d processes, want root and child", len(processes))
	}
	for _, process := range processes {
		stat, err := readLinuxProcessStat(process.PID())
		if err != nil {
			t.Fatal(err)
		}
		if stat.state != 'T' && stat.state != 't' {
			t.Fatalf("retained process %d state = %q, want frozen", process.PID(), stat.state)
		}
	}
	if err := cleanupFrozenRetainedProcesses(ctx, processes, waitForContext); err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		alive, err := process.Alive()
		if err != nil {
			t.Fatal(err)
		}
		if alive {
			t.Fatalf("retained process %d survived native frozen cleanup", process.PID())
		}
	}
}
