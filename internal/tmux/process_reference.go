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
	Freeze(context.Context) error
	Thaw() error
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
				var captureErr error
				if aliveErr != nil {
					captureErr = fmt.Errorf("validating retained parent process %d: %w", relation.ParentPID, aliveErr)
				} else {
					captureErr = fmt.Errorf("retained parent process %d exited during ancestry capture", relation.ParentPID)
				}
				return nil, errors.Join(captureErr, closeRetainedProcesses(processes))
			}
			child, acquireErr := acquire(relation.PID)
			if errors.Is(acquireErr, errProcessNotFound) {
				progress = true
				continue
			}
			if acquireErr != nil {
				return nil, errors.Join(
					fmt.Errorf("retaining descendant process %d: %w", relation.PID, acquireErr),
					closeRetainedProcesses(processes),
				)
			}
			if child.ParentPID() != relation.ParentPID {
				if closeErr := child.Close(); closeErr != nil {
					return nil, errors.Join(
						fmt.Errorf("closing reused descendant process %d: %w", relation.PID, closeErr),
						closeRetainedProcesses(processes),
					)
				}
				progress = true
				continue
			}
			parentAlive, aliveErr = parent.Alive()
			if aliveErr != nil || !parentAlive {
				var captureErr error
				if aliveErr != nil {
					captureErr = fmt.Errorf("revalidating retained parent process %d after child acquisition: %w", relation.ParentPID, aliveErr)
				} else {
					captureErr = fmt.Errorf("retained parent process %d exited during ancestry capture", relation.ParentPID)
				}
				return nil, errors.Join(captureErr, child.Close(), closeRetainedProcesses(processes))
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

type processRelationSource func(int) ([]processRelation, error)

const stableProcessTreeScans = 2

// stabilizeRetainedProcessTree freezes every retained process and repeatedly
// re-enumerates the owned ancestry. A process that forks while the boundary is
// being established is retained and frozen on the next scan; success requires
// two consecutive scans with no newly owned descendants.
func stabilizeRetainedProcessTree(
	ctx context.Context,
	rootPID int,
	processes []retainedProcess,
	relations processRelationSource,
	acquire retainedProcessFactory,
) ([]retainedProcess, error) {
	if len(processes) == 0 || processes[0].PID() != rootPID {
		return processes, errors.New("stable process custody has no retained root")
	}
	owned := make(map[int]retainedProcess, len(processes))
	for _, process := range processes {
		if _, exists := owned[process.PID()]; exists {
			return processes, fmt.Errorf("stable process custody contains duplicate PID %d", process.PID())
		}
		owned[process.PID()] = process
	}
	var frozen []retainedProcess
	fail := func(primary error) ([]retainedProcess, error) {
		return processes, errors.Join(primary, thawRetainedProcesses(frozen))
	}
	freeze := func(process retainedProcess) error {
		if err := process.Freeze(ctx); err != nil {
			return fmt.Errorf("freezing retained process %d: %w", process.PID(), err)
		}
		frozen = append(frozen, process)
		return nil
	}
	for _, process := range processes {
		if err := freeze(process); err != nil {
			return fail(err)
		}
	}

	stableScans := 0
	for stableScans < stableProcessTreeScans {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		snapshot, err := relations(rootPID)
		if err != nil {
			return fail(fmt.Errorf("re-enumerating frozen process ancestry: %w", err))
		}
		pending := append([]processRelation(nil), snapshot...)
		added := 0
		for len(pending) > 0 {
			progress := false
			next := pending[:0]
			for _, relation := range pending {
				if _, exists := owned[relation.PID]; exists {
					progress = true
					continue
				}
				parent, parentOwned := owned[relation.ParentPID]
				if !parentOwned {
					next = append(next, relation)
					continue
				}
				parentAlive, aliveErr := parent.Alive()
				if aliveErr != nil || !parentAlive {
					if aliveErr != nil {
						return fail(fmt.Errorf("validating frozen parent process %d: %w", relation.ParentPID, aliveErr))
					}
					return fail(fmt.Errorf("frozen parent process %d exited during stable capture", relation.ParentPID))
				}
				child, acquireErr := acquire(relation.PID)
				if errors.Is(acquireErr, errProcessNotFound) {
					progress = true
					continue
				}
				if acquireErr != nil {
					return fail(fmt.Errorf("retaining late descendant process %d: %w", relation.PID, acquireErr))
				}
				if child.ParentPID() != relation.ParentPID {
					if closeErr := child.Close(); closeErr != nil {
						return fail(fmt.Errorf("closing reused late descendant process %d: %w", relation.PID, closeErr))
					}
					progress = true
					continue
				}
				parentAlive, aliveErr = parent.Alive()
				if aliveErr != nil || !parentAlive {
					var ancestryErr error
					if aliveErr != nil {
						ancestryErr = fmt.Errorf("revalidating frozen parent process %d: %w", relation.ParentPID, aliveErr)
					} else {
						ancestryErr = fmt.Errorf("frozen parent process %d exited during stable capture", relation.ParentPID)
					}
					return fail(errors.Join(ancestryErr, child.Close()))
				}
				if err := freeze(child); err != nil {
					return fail(errors.Join(err, child.Close()))
				}
				owned[relation.PID] = child
				processes = append(processes, child)
				added++
				progress = true
			}
			pending = next
			if !progress {
				break
			}
		}
		if added > 0 {
			stableScans = 0
			continue
		}
		stableScans++
		if stableScans < stableProcessTreeScans {
			if err := waitForContext(ctx, processExitPollInterval); err != nil {
				return fail(err)
			}
		}
	}
	return processes, nil
}

func thawRetainedProcesses(processes []retainedProcess) error {
	var errs []error
	for i := len(processes) - 1; i >= 0; i-- {
		if err := processes[i].Thaw(); err != nil {
			errs = append(errs, fmt.Errorf("thawing retained process %d: %w", processes[i].PID(), err))
		}
	}
	return errors.Join(errs...)
}

func cleanupFrozenRetainedProcesses(
	ctx context.Context,
	processes []retainedProcess,
	wait func(context.Context, time.Duration) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var cleanupErrs []error
	for i := len(processes) - 1; i >= 0; i-- {
		if err := processes[i].Signal(processSignalKill); err != nil && !errors.Is(err, errProcessNotFound) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("signaling frozen retained process %d with KILL: %w", processes[i].PID(), err))
		}
	}
	for waited := time.Duration(0); waited <= processKillGracePeriod; waited += processExitPollInterval {
		remaining := 0
		for _, process := range processes {
			alive, err := process.Alive()
			if err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("verifying frozen retained process %d exit: %w", process.PID(), err))
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
					cleanupErrs = append(cleanupErrs, fmt.Errorf("frozen retained process %d remained after KILL", process.PID()))
				}
			}
			break
		}
		if err := wait(ctx, processExitPollInterval); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("waiting for frozen retained process exit: %w", err))
			break
		}
	}
	return errors.Join(cleanupErrs...)
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
