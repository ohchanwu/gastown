//go:build darwin

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadProcessStartIdentityDarwinIsStableAndPrecise(t *testing.T) {
	first, err := readProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	second, err := readProcessStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("process start identity changed: %q != %q", first, second)
	}
	parts := strings.Split(first, ":")
	if len(parts) != 2 || len(parts[1]) != 6 {
		t.Fatalf("process start identity %q lacks microsecond precision", first)
	}
}

func TestKillSessionGenerationWithProcessesFailsBeforeMutationOnDarwin(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-retained-darwin-%d", time.Now().UnixNano()))
	session := "gt-retained-darwin"
	defer func() { _ = tm.KillServer() }()
	if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
		t.Fatal(err)
	}
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	err = tm.KillSessionGenerationWithProcessesContext(context.Background(), generation)
	if !errors.Is(err, ErrProcessReferenceUnsupported) {
		t.Fatalf("KillSessionGenerationWithProcessesContext() error = %v, want unsupported", err)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("session mutated after unsupported retained-process capture")
	}
}

func TestKillSessionGenerationWithProcessesPortablePreservesUnrelatedSessionsOnDarwin(t *testing.T) {
	tm := NewTmuxWithSocket(fmt.Sprintf("gt-portable-darwin-%d", time.Now().UnixNano()))
	defer func() { _ = tm.KillServer() }()

	const target = "gt-portable-target"
	unrelated := []string{"gt-portable-unrelated-a", "gt-portable-unrelated-b"}
	for _, session := range append([]string{target}, unrelated...) {
		if err := tm.NewSessionWithCommand(session, "", "sleep 60"); err != nil {
			t.Fatalf("create session %s: %v", session, err)
		}
	}

	generation, err := tm.CaptureSessionGeneration(target)
	if err != nil {
		t.Fatal(err)
	}
	targetPane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	targetPIDText, err := tm.GetPanePID(target)
	if err != nil {
		t.Fatal(err)
	}
	targetPID, err := strconv.Atoi(targetPIDText)
	if err != nil {
		t.Fatal(err)
	}
	if targetPane.PID != targetPID {
		t.Fatalf("generation-bound pane PID = %d, want target pane PID %d", targetPane.PID, targetPID)
	}
	processes, err := captureExplicitProcessGenerations(targetPane.PID)
	if err != nil {
		t.Fatal(err)
	}
	capturedPIDs := make([]int, 0, len(processes))
	for _, process := range processes {
		capturedPIDs = append(capturedPIDs, process.pid)
	}
	for _, session := range unrelated {
		unrelatedPIDText, err := tm.GetPanePID(session)
		if err != nil {
			t.Fatal(err)
		}
		unrelatedPID, err := strconv.Atoi(unrelatedPIDText)
		if err != nil {
			t.Fatal(err)
		}
		for _, pid := range capturedPIDs {
			if pid == unrelatedPID {
				t.Fatalf("target process capture %v included unrelated pane PID %d for %s", capturedPIDs, pid, session)
			}
		}
	}
	if err := tm.KillSessionGenerationWithProcessesPortableContext(context.Background(), generation); err != nil {
		t.Fatalf("KillSessionGenerationWithProcessesPortableContext() error = %v", err)
	}

	running, err := tm.HasSession(target)
	if err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("target session remained live")
	}
	for _, session := range unrelated {
		running, err := tm.HasSession(session)
		if err != nil {
			t.Fatal(err)
		}
		if !running {
			t.Fatalf("portable exact cleanup removed unrelated session %s", session)
		}
	}
}

func TestRetainProcessTreeFailsClosedOnDarwin(t *testing.T) {
	processes, err := retainProcessTree(os.Getpid())
	if !errors.Is(err, ErrProcessReferenceUnsupported) {
		t.Fatalf("retainProcessTree() error = %v, want unsupported", err)
	}
	if len(processes) != 0 {
		t.Fatalf("retained processes = %d, want none", len(processes))
	}
}
