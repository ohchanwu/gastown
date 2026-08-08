//go:build darwin

package tmux

import (
	"os"
	"strings"
	"testing"
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
