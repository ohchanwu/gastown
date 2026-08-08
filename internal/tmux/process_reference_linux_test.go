//go:build linux

package tmux

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestLinuxRetainedProcessTreatsZombieAsExited(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -STOP $$; exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	deadline := time.Now().Add(2 * time.Second)
	for {
		stat, err := readLinuxProcessStat(cmd.Process.Pid)
		if err == nil && (stat.state == 'T' || stat.state == 't') {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not stop before custody acquisition: stat=%#v err=%v", stat, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	retained, err := acquireLinuxRetainedProcess(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		stat, err := readLinuxProcessStat(cmd.Process.Pid)
		if err == nil && linuxProcessStateTerminal(stat.state) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child did not become a zombie: stat=%#v err=%v", stat, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := retained.Freeze(ctx); !errors.Is(err, errProcessNotFound) {
		t.Fatalf("Freeze() error = %v, want terminal process not found", err)
	}
	if alive, err := retained.Alive(); err != nil || alive {
		t.Fatalf("Alive() = %v, %v; want terminal process reported exited", alive, err)
	}
}

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
