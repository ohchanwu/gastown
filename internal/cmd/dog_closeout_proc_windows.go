//go:build windows

package cmd

import (
	"os/exec"
	"syscall"
)

func configureDogCloseoutProcess(command *exec.Cmd) {
	const createNoWindow = 0x08000000
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow,
	}
}
