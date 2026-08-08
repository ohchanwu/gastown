//go:build darwin

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func TestRetainProcessTreeFailsClosedOnDarwin(t *testing.T) {
	processes, err := retainProcessTree(os.Getpid())
	if !errors.Is(err, ErrProcessReferenceUnsupported) {
		t.Fatalf("retainProcessTree() error = %v, want unsupported", err)
	}
	if len(processes) != 0 {
		t.Fatalf("retained processes = %d, want none", len(processes))
	}
}
