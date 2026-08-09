//go:build !linux

package cmd

import (
	"os"
	"os/exec"

	"github.com/steveyegge/gastown/internal/util"
)

func startDaemonBackground(townRoot, gtPath string) (*os.Process, error) {
	command := exec.Command(gtPath, "daemon", "run")
	command.Dir = townRoot
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	util.SetDetachedProcessGroup(command)
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command.Process, nil
}
