//go:build linux

package tmux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type linuxProcessStat struct {
	parentPID int
	startTime string
}

func readLinuxProcessStat(pid int) (linuxProcessStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return linuxProcessStat{}, errProcessNotFound
		}
		return linuxProcessStat{}, err
	}
	stat := string(data)
	commandEnd := strings.LastIndex(stat, ")")
	if commandEnd < 0 || commandEnd+2 > len(stat) {
		return linuxProcessStat{}, errors.New("malformed process stat record")
	}
	fields := strings.Fields(stat[commandEnd+2:])
	if len(fields) <= 19 {
		return linuxProcessStat{}, errors.New("process stat record has no start time")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil || parentPID < 0 {
		return linuxProcessStat{}, fmt.Errorf("invalid process parent PID %q", fields[1])
	}
	return linuxProcessStat{parentPID: parentPID, startTime: fields[19]}, nil
}

func readProcessStartIdentity(pid int) (string, error) {
	stat, err := readLinuxProcessStat(pid)
	if err != nil {
		return "", err
	}
	return stat.startTime, nil
}
