//go:build !windows && !darwin && !linux

package tmux

import (
	"os/exec"
	"strconv"
	"strings"
)

func readProcessStartIdentity(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
