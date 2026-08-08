//go:build windows

package tmux

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsRetainedProcess struct {
	handle    windows.Handle
	pid       int
	parentPID int
	suspended bool
}

var (
	ntdll            = windows.NewLazySystemDLL("ntdll.dll")
	ntSuspendProcess = ntdll.NewProc("NtSuspendProcess")
	ntResumeProcess  = ntdll.NewProc("NtResumeProcess")
)

func (p *windowsRetainedProcess) PID() int       { return p.pid }
func (p *windowsRetainedProcess) ParentPID() int { return p.parentPID }
func (p *windowsRetainedProcess) Close() error {
	return errors.Join(p.Thaw(), windows.CloseHandle(p.handle))
}

func (p *windowsRetainedProcess) Freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.suspended {
		return nil
	}
	status, _, _ := ntSuspendProcess.Call(uintptr(p.handle))
	if int32(status) < 0 {
		alive, aliveErr := p.Alive()
		if aliveErr == nil && !alive {
			return errProcessNotFound
		}
		return fmt.Errorf("NtSuspendProcess failed with NTSTATUS %#x", uint32(status))
	}
	p.suspended = true
	return nil
}

func (p *windowsRetainedProcess) Thaw() error {
	if !p.suspended {
		return nil
	}
	status, _, _ := ntResumeProcess.Call(uintptr(p.handle))
	if int32(status) < 0 {
		alive, aliveErr := p.Alive()
		if aliveErr == nil && !alive {
			p.suspended = false
			return nil
		}
		return fmt.Errorf("NtResumeProcess failed with NTSTATUS %#x", uint32(status))
	}
	p.suspended = false
	return nil
}

func (p *windowsRetainedProcess) Alive() (bool, error) {
	result, err := windows.WaitForSingleObject(p.handle, 0)
	if err != nil {
		return false, err
	}
	switch result {
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	case uint32(windows.WAIT_OBJECT_0):
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait result %d", result)
	}
}

func (p *windowsRetainedProcess) Signal(processSignal) error {
	if err := windows.TerminateProcess(p.handle, 1); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			alive, aliveErr := p.Alive()
			if aliveErr == nil && !alive {
				return errProcessNotFound
			}
		}
		return err
	}
	return nil
}

func snapshotWindowsProcessRelations() ([]processRelation, error) {
	handle, _, callErr := createToolhelp32Snap.Call(uintptr(thSnapProcess), 0)
	if handle == invalidHandle {
		return nil, callErr
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(handle)) }()

	var entry processEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	ret, _, callErr := process32FirstW.Call(handle, uintptr(unsafe.Pointer(&entry)))
	if ret == 0 {
		return nil, callErr
	}
	var relations []processRelation
	for {
		relations = append(relations, processRelation{PID: int(entry.ProcessID), ParentPID: int(entry.ParentProcessID)})
		entry.Size = uint32(unsafe.Sizeof(entry))
		ret, _, _ = process32NextW.Call(handle, uintptr(unsafe.Pointer(&entry)))
		if ret == 0 {
			break
		}
	}
	return relations, nil
}

func currentWindowsParentPID(pid int) (int, error) {
	relations, err := snapshotWindowsProcessRelations()
	if err != nil {
		return 0, err
	}
	for _, relation := range relations {
		if relation.PID == pid {
			return relation.ParentPID, nil
		}
	}
	return 0, errProcessNotFound
}

func acquireWindowsRetainedProcess(pid int) (retainedProcess, error) {
	access := uint32(windows.PROCESS_QUERY_LIMITED_INFORMATION | windows.PROCESS_TERMINATE | windows.PROCESS_SUSPEND_RESUME | windows.SYNCHRONIZE)
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	process := &windowsRetainedProcess{handle: handle, pid: pid}
	parentPID, err := currentWindowsParentPID(pid)
	if err != nil {
		return nil, errors.Join(err, process.Close())
	}
	process.parentPID = parentPID
	alive, err := process.Alive()
	if err != nil || !alive {
		if err != nil {
			return nil, errors.Join(err, process.Close())
		}
		return nil, errors.Join(errProcessNotFound, process.Close())
	}
	return process, nil
}

func windowsProcessRelations(rootPID int) ([]processRelation, error) {
	all, err := snapshotWindowsProcessRelations()
	if err != nil {
		return nil, err
	}
	owned := map[int]bool{rootPID: true}
	pending := all
	var descendants []processRelation
	for len(pending) > 0 {
		progress := false
		next := pending[:0]
		for _, relation := range pending {
			if relation.PID == rootPID {
				continue
			}
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
	relations, err := windowsProcessRelations(rootPID)
	if err != nil {
		return nil, err
	}
	return captureRetainedProcessTree(rootPID, relations, acquireWindowsRetainedProcess)
}

func stabilizeProcessTree(ctx context.Context, rootPID int, processes []retainedProcess) ([]retainedProcess, error) {
	return stabilizeRetainedProcessTree(ctx, rootPID, processes, windowsProcessRelations, acquireWindowsRetainedProcess)
}
