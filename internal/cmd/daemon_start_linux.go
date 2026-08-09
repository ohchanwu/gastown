//go:build linux

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func startDaemonBackground(townRoot, _ string) (*os.Process, error) {
	command := exec.Command("systemctl", "--user", "start", "gastown-daemon.service")
	command.Dir = townRoot
	output, err := command.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return nil, fmt.Errorf("starting delegated systemd user unit: %s: %w", detail, err)
		}
		return nil, fmt.Errorf("starting delegated systemd user unit: %w (run 'gt daemon enable-supervisor' first)", err)
	}
	return nil, nil
}
