//go:build linux

package tmux

import (
	"context"
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

type linuxRetainedProcess struct {
	fd           int
	pid          int
	parentPID    int
	resumeOnThaw bool
}

func linuxProcessStateTerminal(state byte) bool {
	return state == 'Z' || state == 'X' || state == 'x'
}

func (p *linuxRetainedProcess) PID() int       { return p.pid }
func (p *linuxRetainedProcess) ParentPID() int { return p.parentPID }
func (p *linuxRetainedProcess) Close() error   { return errors.Join(p.Thaw(), unix.Close(p.fd)) }

func (p *linuxRetainedProcess) Freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := readLinuxProcessStat(p.pid)
	if err != nil {
		return err
	}
	if linuxProcessStateTerminal(before.state) {
		return errProcessNotFound
	}
	if before.state == 'T' || before.state == 't' {
		alive, aliveErr := p.Alive()
		if aliveErr != nil {
			return aliveErr
		}
		if !alive {
			return errProcessNotFound
		}
		return nil
	}
	if err := unix.PidfdSendSignal(p.fd, unix.SIGSTOP, nil, 0); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return errProcessNotFound
		}
		return err
	}
	p.resumeOnThaw = true
	for {
		stat, err := readLinuxProcessStat(p.pid)
		if err != nil {
			return errors.Join(err, p.Thaw())
		}
		if stat.state == 'T' || stat.state == 't' {
			alive, aliveErr := p.Alive()
			if aliveErr != nil {
				return errors.Join(aliveErr, p.Thaw())
			}
			if !alive {
				return errors.Join(errProcessNotFound, p.Thaw())
			}
			return nil
		}
		if linuxProcessStateTerminal(stat.state) {
			return errors.Join(errProcessNotFound, p.Thaw())
		}
		if err := waitForContext(ctx, processExitPollInterval); err != nil {
			return errors.Join(err, p.Thaw())
		}
	}
}

func (p *linuxRetainedProcess) Thaw() error {
	if !p.resumeOnThaw {
		return nil
	}
	if err := unix.PidfdSendSignal(p.fd, unix.SIGCONT, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	p.resumeOnThaw = false
	return nil
}

func (p *linuxRetainedProcess) Alive() (bool, error) {
	stat, statErr := readLinuxProcessStat(p.pid)
	if errors.Is(statErr, errProcessNotFound) {
		return false, nil
	}
	if statErr != nil {
		return false, statErr
	}
	if linuxProcessStateTerminal(stat.state) {
		return false, nil
	}
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
		return nil, errors.Join(err, process.Close())
	}
	process.parentPID = stat.parentPID
	alive, err := process.Alive()
	if err != nil || !alive {
		if err != nil {
			return nil, errors.Join(err, process.Close())
		}
		return nil, errors.Join(errProcessNotFound, process.Close())
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

func stabilizeProcessTree(ctx context.Context, rootPID int, processes []retainedProcess) ([]retainedProcess, error) {
	return stabilizeRetainedProcessTree(ctx, rootPID, processes, linuxProcessRelations, acquireLinuxRetainedProcess)
}
