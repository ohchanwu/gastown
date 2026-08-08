//go:build darwin

package tmux

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func readProcessStartIdentity(pid int) (string, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if process == nil || int(process.Proc.P_pid) != pid {
		return "", errProcessNotFound
	}
	started := process.Proc.P_starttime
	return fmt.Sprintf("%d:%06d", started.Sec, started.Usec), nil
}
