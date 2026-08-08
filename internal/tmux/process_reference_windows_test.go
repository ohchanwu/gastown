//go:build windows

package tmux

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWindowsRetainedProcessHelper(t *testing.T) {
	if os.Getenv("GT_WINDOWS_RETAINED_PROCESS_HELPER") == "" {
		return
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestWindowsRetainedProcessFreezesThawsAndKillsNativeProcess(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestWindowsRetainedProcessHelper$")
	cmd.Env = append(os.Environ(), "GT_WINDOWS_RETAINED_PROCESS_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out reaping Windows retained-process fixture %d", cmd.Process.Pid)
		}
	})

	var process retainedProcess
	deadline := time.Now().Add(2 * time.Second)
	for {
		process, err = acquireWindowsRetainedProcess(cmd.Process.Pid)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() {
		if err := process.Close(); err != nil {
			t.Errorf("closing Windows retained reference: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := process.Freeze(ctx); err != nil {
		t.Fatal(err)
	}
	if err := process.Thaw(); err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(processSignalKill); err != nil {
		t.Fatal(err)
	}
	for {
		alive, err := process.Alive()
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if err := waitForContext(ctx, 10*time.Millisecond); err != nil {
			t.Fatal(err)
		}
	}
}
