//go:build windows

package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

func TestScheduleDogCloseoutHostFallbackFailsClosedOnNativeWindows(t *testing.T) {
	_, started, err := scheduleDogCloseoutHostFallback(
		tmux.NewTmuxWithSocket("unused"),
		"hq-dog-finalizer-alpha-test",
		`C:\Program Files\gt.exe`,
		`C:\Town Root`,
		[]string{"dog", "done", "alpha"},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "WSL") {
		t.Fatalf("scheduleDogCloseoutHostFallback() error = %v, want WSL support boundary", err)
	}
	if started {
		t.Fatal("unsupported native Windows closeout reported a started finalizer")
	}
}
