//go:build linux

package tmux

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

type linuxRetainedProcess struct {
	fd        int
	pid       int
	parentPID int
}

func (p *linuxRetainedProcess) PID() int       { return p.pid }
func (p *linuxRetainedProcess) ParentPID() int { return p.parentPID }
func (p *linuxRetainedProcess) Close() error   { return unix.Close(p.fd) }

func (p *linuxRetainedProcess) Alive() (bool, error) {
	err := unix.PidfdSendSignal(p.fd, 0, nil, 0)
	switch {
	case err == nil, errors.Is(err, unix.EPERM):
		return true, nil
	case errors.Is(err, unix.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func (p *linuxRetainedProcess) Signal(requested processSignal) error {
	var signal unix.Signal
	switch requested {
	case processSignalTerminate:
		signal = unix.SIGTERM
	case processSignalKill:
		signal = unix.SIGKILL
	default:
		return errors.New("unknown retained process signal")
	}
	if err := unix.PidfdSendSignal(p.fd, signal, nil, 0); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errProcessNotFound
		}
		return err
	}
	return nil
}

func acquireLinuxRetainedProcess(pid int) (retainedProcess, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	process := &linuxRetainedProcess{fd: fd, pid: pid}
	stat, err := readLinuxProcessStat(pid)
	if err != nil {
		_ = process.Close()
		return nil, err
	}
	process.parentPID = stat.parentPID
	alive, err := process.Alive()
	if err != nil || !alive {
		_ = process.Close()
		if err != nil {
			return nil, err
		}
		return nil, errProcessNotFound
	}
	return process, nil
}

func linuxProcessRelations(rootPID int) ([]processRelation, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	all := make([]processRelation, 0, len(entries))
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid == rootPID {
			continue
		}
		stat, statErr := readLinuxProcessStat(pid)
		if statErr != nil {
			continue
		}
		all = append(all, processRelation{PID: pid, ParentPID: stat.parentPID})
	}
	owned := map[int]bool{rootPID: true}
	pending := all
	var descendants []processRelation
	for len(pending) > 0 {
		progress := false
		next := pending[:0]
		for _, relation := range pending {
			if owned[relation.ParentPID] {
				owned[relation.PID] = true
				descendants = append(descendants, relation)
				progress = true
				continue
			}
			next = append(next, relation)
		}
		pending = next
		if !progress {
			break
		}
	}
	return descendants, nil
}

func retainProcessTree(rootPID int) ([]retainedProcess, error) {
	relations, err := linuxProcessRelations(rootPID)
	if err != nil {
		return nil, err
	}
	return captureRetainedProcessTree(rootPID, relations, acquireLinuxRetainedProcess)
}
