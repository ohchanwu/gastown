//go:build linux

package tmux

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func readProcessStartIdentity(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errProcessNotFound
		}
		return "", err
	}
	stat := string(data)
	commandEnd := strings.LastIndex(stat, ")")
	if commandEnd < 0 || commandEnd+2 > len(stat) {
		return "", errors.New("malformed process stat record")
	}
	fields := strings.Fields(stat[commandEnd+2:])
	if len(fields) <= 19 {
		return "", errors.New("process stat record has no start time")
	}
	return fields[19], nil
}
