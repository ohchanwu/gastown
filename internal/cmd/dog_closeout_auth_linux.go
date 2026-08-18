//go:build linux

package cmd

import (
	"os"

	"github.com/steveyegge/gastown/internal/tmux"
)

func dogCloseoutFinalizerAuthorized(encoded string) bool {
	return os.Getenv(tmux.EnvSessionBrokerWorker) == "1" || dogCloseoutHostSessionAuthorized(encoded)
}
