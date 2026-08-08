//go:build linux && !integration

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
	"golang.org/x/sys/unix"
)

const (
	envSessionBrokerReexecHelper = "GT_TEST_CMD_REEXEC_HELPER"
	envSessionBrokerReexecStage  = "GT_TEST_CMD_REEXEC_STAGE"
	envSessionBrokerRawClient    = "GT_TEST_CMD_BROKER_RAW_CLIENT"
)

func runSessionBrokerReexecHelper() bool {
	if os.Getenv(envSessionBrokerReexecHelper) != "1" {
		return false
	}
	if os.Getenv(envSessionBrokerReexecStage) == "" {
		if err := os.Setenv(envSessionBrokerReexecStage, "1"); err != nil {
			panic(err)
		}
		if err := unix.Exec("/proc/self/exe", os.Args, os.Environ()); err != nil {
			panic(err)
		}
	}
	_ = os.Unsetenv(envSessionBrokerReexecHelper)
	_ = os.Unsetenv(envSessionBrokerReexecStage)
	return true
}

func runSessionBrokerRawClientHelper() (int, bool) {
	if os.Getenv(envSessionBrokerRawClient) != "1" {
		return 0, false
	}
	handled, code, err := tmux.RunSessionBrokerClient(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "raw broker client: %v\n", err)
		return 1, true
	}
	if !handled {
		fmt.Fprintln(os.Stderr, "raw broker client: inherited endpoint was not handled")
		return 1, true
	}
	return code, true
}

func TestSessionBrokerRawUnsafeFramesNeverStartWorker(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	clientEndpoint := os.NewFile(uintptr(pair[1]), "raw-broker-client")
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	proofPath := filepath.Join(t.TempDir(), "worker-started")
	workerPath := filepath.Join(t.TempDir(), "worker")
	worker := "#!/bin/sh\ntouch " + config.ShellQuote(proofPath) + "\nexit 0\n"
	if err := os.WriteFile(workerPath, []byte(worker), 0o700); err != nil {
		t.Fatal(err)
	}
	go func() {
		serverDone <- tmux.ServeSessionBroker(ctx, workerPath, pair[0], func(args []string) error {
			return IsBrokerSafeCommand(rootCmd, args)
		})
	}()
	t.Cleanup(func() {
		cancel()
		_ = clientEndpoint.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("ServeSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("ServeSessionBroker() did not stop")
		}
	})

	nullFiles := make([]*os.File, 3)
	for index := range nullFiles {
		nullFiles[index], err = os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer nullFiles[index].Close()
	}
	unsafeRequests := [][]string{
		{"session-custody-init"},
		{"session-custody", "--id", "00000000-0000-0000-0000-000000000000", "--", "true"},
		{"shell", "-c", "touch " + proofPath},
		{"doctor", "--fix"},
		{"up", "--restore"},
		{"tmux", "send-keys", "hq-mayor", "payload"},
	}
	for _, args := range unsafeRequests {
		command := exec.Command(os.Args[0], args...)
		command.Env = append(os.Environ(), envSessionBrokerRawClient+"=1")
		command.ExtraFiles = []*os.File{nullFiles[0], nullFiles[1], nullFiles[2], clientEndpoint}
		output, err := command.CombinedOutput()
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) || exitError.ExitCode() != 126 {
			t.Fatalf("raw unsafe request %q error = %v, output = %q; want exit 126", args, err, output)
		}
	}
	if _, err := os.Stat(proofPath); !os.IsNotExist(err) {
		t.Fatalf("unsafe raw frame started worker; stat error = %v", err)
	}
}

func TestContainedGTInvocationsUseBrokerBeforeCobra(t *testing.T) {
	testDir := t.TempDir()
	safeOutputPath := filepath.Join(testDir, "safe-output")
	deniedOutputPath := filepath.Join(testDir, "denied-output")
	unsafeProofPath := filepath.Join(testDir, "unsafe-proof")
	deniedCodePath := filepath.Join(testDir, "denied-code")
	deniedCodeTempPath := deniedCodePath + ".tmp"
	reexecGT := envSessionBrokerReexecHelper + "=1 " + config.ShellQuote(os.Args[0])
	command := strings.Join([]string{
		reexecGT + " prime --help >" + config.ShellQuote(safeOutputPath) + " 2>&1",
		reexecGT + " shell -c " + config.ShellQuote("touch "+unsafeProofPath) + " >" + config.ShellQuote(deniedOutputPath) + " 2>&1",
		"printf '%s' $? >" + config.ShellQuote(deniedCodeTempPath),
		"mv " + config.ShellQuote(deniedCodeTempPath) + " " + config.ShellQuote(deniedCodePath),
		"while :; do sleep 60; done",
	}, "; ")
	custody := uuid.NewString()
	process := exec.Command(
		os.Args[0],
		"session-custody",
		"--id", custody,
		"--", command,
	)
	process.Env = append(os.Environ(),
		"GT_TEST_CMD_EXECUTE_HELPER=1",
		tmux.EnvSessionCustody+"="+custody,
	)
	var output synchronizedBrokerTestOutput
	process.Stdout = &output
	process.Stderr = &output
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if reaped {
			return
		}
		_ = process.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("timed out reaping custody test process %d", process.Process.Pid)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		safeOutput, safeErr := os.ReadFile(safeOutputPath)
		deniedOutput, deniedErr := os.ReadFile(deniedOutputPath)
		deniedCode, codeErr := os.ReadFile(deniedCodePath)
		if safeErr == nil && deniedErr == nil && codeErr == nil {
			if !strings.Contains(string(safeOutput), "Usage:\n  gt prime") {
				t.Fatalf("brokered safe command output = %q", safeOutput)
			}
			code, err := strconv.Atoi(strings.TrimSpace(string(deniedCode)))
			if err != nil {
				t.Fatal(err)
			}
			if code != 126 {
				t.Fatalf("unsafe broker command exit code = %d, want 126; output %q", code, deniedOutput)
			}
			if !strings.Contains(string(deniedOutput), "not broker-safe") {
				t.Fatalf("unsafe broker denial output = %q", deniedOutput)
			}
			if _, err := os.Stat(unsafeProofPath); !os.IsNotExist(err) {
				t.Fatalf("unsafe contained gt invocation executed; stat error = %v", err)
			}
			return
		}
		select {
		case err := <-done:
			reaped = true
			if strings.Contains(output.String(), "PID namespace unavailable") {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("custody test process exited before proof: %v\n%s", err, output.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("contained gt broker proof timed out\n%s", output.String())
}

type synchronizedBrokerTestOutput struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (output *synchronizedBrokerTestOutput) Write(payload []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.builder.Write(payload)
}

func (output *synchronizedBrokerTestOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.builder.String()
}
