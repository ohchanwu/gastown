//go:build linux && !integration

package cmd

import (
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
