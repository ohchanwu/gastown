//go:build !windows

package cmd

import (
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
)

// scheduleDogCloseoutHostFallback creates a second exact tmux generation so the
// finalizer survives destruction of the owned dog pane, including when the dog
// was the server's last session. Keeping the fallback in tmux also gives every
// supported platform the same authenticated host-generation receipt.
func scheduleDogCloseoutHostFallback(
	controller *tmux.Tmux,
	sessionName string,
	executable string,
	townRoot string,
	args []string,
	environment map[string]string,
) (tmux.SessionGeneration, bool, error) {
	commandParts := []string{config.ShellQuote(executable)}
	for _, arg := range args {
		commandParts = append(commandParts, config.ShellQuote(arg))
	}
	generation, err := controller.StartTransientSessionWithCommandAndEnv(
		sessionName,
		townRoot,
		strings.Join(commandParts, " "),
		environment,
	)
	return generation, true, err
}
