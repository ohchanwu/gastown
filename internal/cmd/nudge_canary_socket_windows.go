//go:build windows

package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
)

func removeWakeCanarySocketPath(socket string) error {
	if filepath.Base(socket) != socket || !strings.HasPrefix(socket, "gt-wake-canary-ndg-") {
		return fmt.Errorf("refusing ambiguous wake canary socket ownership: %q", socket)
	}
	return nil
}
