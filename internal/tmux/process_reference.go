package tmux

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrProcessReferenceUnsupported means the platform cannot retain a
// generation-bound kernel process reference. Callers must fail before any
// destructive mutation rather than fall back to a raw PID signal.
var ErrProcessReferenceUnsupported = errors.New("generation-bound process signaling is unsupported")

var errProcessNotFound = errors.New("process generation no longer exists")

const processExitPollInterval = 50 * time.Millisecond

type processSignal string

const (
	processSignalTerminate processSignal = "TERM"
	processSignalKill      processSignal = "KILL"
)

type processRelation struct {
	PID       int
	ParentPID int
}

type retainedProcess interface {
	PID() int
	ParentPID() int
	Alive() (bool, error)
	Signal(processSignal) error
	Close() error
}

type retainedProcessFactory func(int) (retainedProcess, error)

func closeRetainedProcesses(processes []retainedProcess) error {
	var errs []error
	for i := len(processes) - 1; i >= 0; i-- {
		if err := processes[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// captureRetainedProcessTree accepts a snapshot relation only when the
// retained child reference still reports the captured, already-owned parent.
// A PID reused outside the captured ancestry is closed and ignored.
func captureRetainedProcessTree(
	rootPID int,
	relations []processRelation,
	acquire retainedProcessFactory,
) ([]retainedProcess, error) {
	root, err := acquire(rootPID)
	if err != nil {
		return nil, err
	}
	processes := []retainedProcess{root}
	owned := map[int]retainedProcess{rootPID: root}
	pending := append([]processRelation(nil), relations...)

	for len(pending) > 0 {
		progress := false
		next := pending[:0]
		for _, relation := range pending {
			parent, parentOwned := owned[relation.ParentPID]
			if !parentOwned {
				next = append(next, relation)
				continue
			}
			parentAlive, aliveErr := parent.Alive()
			if aliveErr != nil || !parentAlive {
				closeRetainedProcesses(processes)
				if aliveErr != nil {
					return nil, fmt.Errorf("validating retained parent process %d: %w", relation.ParentPID, aliveErr)
				}
				return nil, fmt.Errorf("retained parent process %d exited during ancestry capture", relation.ParentPID)
			}
			child, acquireErr := acquire(relation.PID)
			if errors.Is(acquireErr, errProcessNotFound) {
				progress = true
				continue
			}
			if acquireErr != nil {
				closeRetainedProcesses(processes)
				return nil, fmt.Errorf("retaining descendant process %d: %w", relation.PID, acquireErr)
			}
			if child.ParentPID() != relation.ParentPID {
				_ = child.Close()
				progress = true
				continue
			}
			parentAlive, aliveErr = parent.Alive()
			if aliveErr != nil || !parentAlive {
				_ = child.Close()
				closeRetainedProcesses(processes)
				if aliveErr != nil {
					return nil, fmt.Errorf("revalidating retained parent process %d after child acquisition: %w", relation.ParentPID, aliveErr)
				}
				return nil, fmt.Errorf("retained parent process %d exited during ancestry capture", relation.ParentPID)
			}
			owned[relation.PID] = child
			processes = append(processes, child)
			progress = true
		}
		pending = next
		if !progress {
			break
		}
	}
	return processes, nil
}

func cleanupRetainedProcesses(
	ctx context.Context,
	processes []retainedProcess,
	wait func(context.Context, time.Duration) error,
) error {
	var cleanupErrs []error
	for i := len(processes) - 1; i >= 0; i-- {
		if err := processes[i].Signal(processSignalTerminate); err != nil && !errors.Is(err, errProcessNotFound) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("signaling retained process %d with TERM: %w", processes[i].PID(), err))
		}
	}
	if len(processes) > 0 {
		if err := wait(ctx, processKillGracePeriod); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("waiting after retained TERM: %w", err))
		}
	}
	for i := len(processes) - 1; i >= 0; i-- {
		alive, err := processes[i].Alive()
		if err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("checking retained process %d before KILL: %w", processes[i].PID(), err))
			continue
		}
		if !alive {
			continue
		}
		if err := processes[i].Signal(processSignalKill); err != nil && !errors.Is(err, errProcessNotFound) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("signaling retained process %d with KILL: %w", processes[i].PID(), err))
		}
	}

	for waited := time.Duration(0); waited <= processKillGracePeriod; waited += processExitPollInterval {
		remaining := 0
		for _, process := range processes {
			alive, err := process.Alive()
			if err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("verifying retained process %d exit: %w", process.PID(), err))
				continue
			}
			if alive {
				remaining++
			}
		}
		if remaining == 0 {
			return errors.Join(cleanupErrs...)
		}
		if waited == processKillGracePeriod {
			for _, process := range processes {
				alive, _ := process.Alive()
				if alive {
					cleanupErrs = append(cleanupErrs, fmt.Errorf("retained process %d remained after KILL", process.PID()))
				}
			}
			break
		}
		if err := wait(ctx, processExitPollInterval); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("waiting for retained process exit: %w", err))
			break
		}
	}
	return errors.Join(cleanupErrs...)
}
