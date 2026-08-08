package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/config"
)

// ErrSessionCustodyUnsupported means that the pane was not launched inside a
// verifiable OS-owned containment boundary. Destructive zombie replacement
// must fail closed rather than fall back to ancestry scanning.
var ErrSessionCustodyUnsupported = errors.New("generation-bound session containment is unavailable")

type sessionCustodyHandle interface {
	Freeze(context.Context) error
	Kill(context.Context) (bool, error)
	Thaw() error
	Close() error
}

// WrapSessionCommandWithCustody wraps a long-lived agent command with the
// hidden launcher that establishes an OS-owned containment boundary before the
// command can fork. Unsupported platforms return the command unchanged; their
// later zombie cleanup therefore fails closed.
func WrapSessionCommandWithCustody(gtBinary, command string) (wrapped, custody string, err error) {
	if !sessionCustodyLaunchSupported() {
		return command, "", nil
	}
	if !filepath.IsAbs(gtBinary) {
		return "", "", errors.New("session custody launcher requires an absolute gt binary path")
	}
	if strings.TrimSpace(command) == "" {
		return "", "", errors.New("session custody launcher requires a command")
	}
	custody = uuid.NewString()
	wrapped = "exec " + config.ShellQuote(gtBinary) + " session-custody --id " + config.ShellQuote(custody) + " -- " + config.ShellQuote(command)
	return wrapped, custody, nil
}

// CanWrapCurrentExecutable reports whether this process is the installed gt
// binary rather than a Go test binary. Tests may inject a wrapper explicitly.
func CanWrapCurrentExecutable(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return base == "gt" || (runtime.GOOS == "windows" && base == "gt.exe")
}

// RunSessionCustodyCommand is the implementation of the hidden launcher.
func RunSessionCustodyCommand(custody, command string) error {
	if !validSessionGenerationRe.MatchString(custody) {
		return errors.New("invalid session custody token")
	}
	if inherited := os.Getenv(EnvSessionCustody); inherited != custody {
		return fmt.Errorf("session custody token does not match inherited %s", EnvSessionCustody)
	}
	if strings.TrimSpace(command) == "" {
		return errors.New("session custody command is empty")
	}
	return runSessionWithCustody(custody, command)
}

// RunSessionCustodyInit enters the trusted namespace-init path selected by the
// hidden internal command. Ordinary invocations are rejected.
func RunSessionCustodyInit() error {
	requested, err := runSessionCustodyInit()
	if !requested {
		return errors.New("session custody init was not requested by a trusted launcher")
	}
	return err
}
