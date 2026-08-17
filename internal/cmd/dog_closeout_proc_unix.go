//go:build !windows

package cmd

import (
	"os/exec"
	"syscall"
)

func configureDogCloseoutProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
