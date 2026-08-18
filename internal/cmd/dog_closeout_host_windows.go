//go:build windows

package cmd

import (
	"errors"

	"github.com/steveyegge/gastown/internal/tmux"
)

// Native Windows supports only the minimal CLI. Full tmux workflows run under
// WSL (and therefore compile through the Linux path); fail closed here instead
// of sending POSIX quoting and cd syntax to PowerShell.
func scheduleDogCloseoutHostFallback(
	controller *tmux.Tmux,
	sessionName string,
	executable string,
	townRoot string,
	args []string,
	environment map[string]string,
) (tmux.SessionGeneration, bool, error) {
	return tmux.SessionGeneration{}, false, errors.New("dog tmux closeout requires WSL on Windows")
}
