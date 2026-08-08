//go:build linux

package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxProcRoot            = "/proc"
	sessionCustodyReadyProbe = 300 * time.Millisecond
)

type linuxSessionCustody struct {
	supervisorPID      int
	supervisorIdentity string
	supervisorFD       int
	initPID            int
	initIdentity       string
	initNamespace      uint64
	initFD             int
	prepared           bool
	committed          bool
}

func sessionCustodyLaunchSupported() bool { return true }

func withoutEnvironmentKey(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func newLinuxCustodyCommand(command string, namespaced bool) (*exec.Cmd, *int) {
	child := exec.Command("/bin/sh", "-lc", command)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = withoutEnvironmentKey(os.Environ(), EnvSessionCustody)
	pidfd := -1
	attrs := &syscall.SysProcAttr{
		Pdeathsig: unix.SIGKILL,
		PidFD:     &pidfd,
	}
	if namespaced {
		attrs.Cloneflags = unix.CLONE_NEWUSER | unix.CLONE_NEWPID
		attrs.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
		attrs.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
		attrs.GidMappingsEnableSetgroups = false
	}
	child.SysProcAttr = attrs
	return child, &pidfd
}

func startLinuxCustodyCommand(command string, namespaced bool) (*exec.Cmd, int, error) {
	child, pidfd := newLinuxCustodyCommand(command, namespaced)
	if err := child.Start(); err != nil {
		return nil, -1, err
	}
	if *pidfd < 0 {
		_ = child.Process.Kill()
		_ = child.Wait()
		return nil, -1, errors.New("kernel did not return a pidfd for session custody")
	}
	return child, *pidfd, nil
}

type linuxCustodyStarter func(string, bool) (*exec.Cmd, int, error)

func launchLinuxCustodyCommand(command string, start linuxCustodyStarter) (*exec.Cmd, int, bool, error) {
	child, pidfd, err := start(command, true)
	if err == nil {
		_, _, err = validateLinuxNamespaceInit(linuxProcRoot, os.Getpid(), child.Process.Pid)
	}
	if err != nil {
		if child != nil {
			if pidfd >= 0 {
				_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
			} else {
				_ = child.Process.Kill()
			}
			_ = child.Wait()
			if pidfd >= 0 {
				_ = unix.Close(pidfd)
			}
		}
		// User namespaces may be disabled by the host. Preserve ordinary
		// startup, but leave a directly observable non-namespace child so later
		// destructive cleanup fails closed.
		fmt.Fprintf(os.Stderr, "gt session-custody: PID namespace unavailable: %v\n", err)
		child, pidfd, err = start(command, false)
		if err != nil {
			return nil, -1, false, err
		}
		return child, pidfd, false, nil
	}
	return child, pidfd, true, nil
}

func runSessionWithCustody(_ string, command string) error {
	child, pidfd, _, err := launchLinuxCustodyCommand(command, startLinuxCustodyCommand)
	if err != nil {
		return err
	}
	// Retain both the direct-child relationship and the pidfd until exact
	// teardown. Never reap the namespace init here: if the workload exits, its
	// zombie remains a generation-stable proof that the namespace and all of its
	// descendants have terminated.
	_ = child
	_ = pidfd
	timer := time.NewTimer(sessionCustodyReadyProbe)
	defer timer.Stop()
	<-timer.C
	for {
		time.Sleep(time.Hour)
	}
}

func linuxDirectChildren(procRoot string, pid int) ([]int, error) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/task/%d/children", procRoot, pid, pid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	fields := strings.Fields(string(data))
	children := make([]int, 0, len(fields))
	for _, field := range fields {
		child, err := strconv.Atoi(field)
		if err != nil || child <= 0 {
			return nil, fmt.Errorf("invalid direct child PID %q", field)
		}
		children = append(children, child)
	}
	return children, nil
}

func linuxNamespacePIDs(procRoot string, pid int) ([]int, error) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/status", procRoot, pid))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		pids := make([]int, 0, len(fields))
		for _, field := range fields {
			value, err := strconv.Atoi(field)
			if err != nil || value <= 0 {
				return nil, fmt.Errorf("invalid namespace PID %q", field)
			}
			pids = append(pids, value)
		}
		if len(pids) == 0 {
			return nil, errors.New("process status has an empty NSpid field")
		}
		return pids, nil
	}
	return nil, errors.New("process status has no NSpid field")
}

func linuxPIDNamespaceInode(procRoot string, pid int) (uint64, error) {
	info, err := os.Stat(fmt.Sprintf("%s/%d/ns/pid", procRoot, pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, errProcessNotFound
		}
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return 0, errors.New("PID namespace has no inode identity")
	}
	return stat.Ino, nil
}

func validateLinuxNamespaceInit(procRoot string, supervisorPID, initPID int) (string, uint64, error) {
	stat, err := readLinuxProcessStatAt(procRoot, initPID)
	if err != nil {
		return "", 0, err
	}
	if stat.parentPID != supervisorPID {
		return "", 0, errors.New("session namespace init is not the pane supervisor's direct child")
	}
	nspids, err := linuxNamespacePIDs(procRoot, initPID)
	if err != nil {
		return "", 0, err
	}
	if len(nspids) < 2 || nspids[len(nspids)-1] != 1 {
		return "", 0, ErrSessionCustodyUnsupported
	}
	namespace, err := linuxPIDNamespaceInode(procRoot, initPID)
	if err != nil {
		return "", 0, err
	}
	return stat.startTime, namespace, nil
}

func readLinuxProcessStatAt(procRoot string, pid int) (linuxProcessStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", procRoot, pid))
	if err != nil {
		if os.IsNotExist(err) {
			return linuxProcessStat{}, errProcessNotFound
		}
		return linuxProcessStat{}, err
	}
	return parseLinuxProcessStat(data)
}

func retainSessionCustody(custody string, panePID int) (sessionCustodyHandle, error) {
	if !validSessionGenerationRe.MatchString(custody) {
		return nil, ErrSessionCustodyUnsupported
	}
	supervisorStat, err := readLinuxProcessStat(panePID)
	if err != nil || linuxProcessStateTerminal(supervisorStat.state) {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err)
	}
	supervisorFD, err := unix.PidfdOpen(panePID, 0)
	if err != nil {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err)
	}
	reject := func(primary error, initFD int) (sessionCustodyHandle, error) {
		var initCloseErr error
		if initFD >= 0 {
			initCloseErr = unix.Close(initFD)
		}
		return nil, errors.Join(ErrSessionCustodyUnsupported, primary, initCloseErr, unix.Close(supervisorFD))
	}
	children, err := linuxDirectChildren(linuxProcRoot, panePID)
	if err != nil || len(children) != 1 {
		if err == nil {
			err = fmt.Errorf("pane supervisor has %d direct children, want one namespace init", len(children))
		}
		return reject(err, -1)
	}
	initPID := children[0]
	initIdentity, namespace, err := validateLinuxNamespaceInit(linuxProcRoot, panePID, initPID)
	if err != nil {
		return reject(err, -1)
	}
	initFD, err := unix.PidfdOpen(initPID, 0)
	if err != nil {
		return reject(err, -1)
	}
	confirmedSupervisor, err := readLinuxProcessStat(panePID)
	if err != nil || confirmedSupervisor.startTime != supervisorStat.startTime || linuxProcessStateTerminal(confirmedSupervisor.state) {
		return reject(errors.Join(ErrSessionGenerationChanged, err), initFD)
	}
	confirmedIdentity, confirmedNamespace, err := validateLinuxNamespaceInit(linuxProcRoot, panePID, initPID)
	if err != nil || confirmedIdentity != initIdentity || confirmedNamespace != namespace {
		return reject(errors.Join(ErrSessionGenerationChanged, err), initFD)
	}
	return &linuxSessionCustody{
		supervisorPID:      panePID,
		supervisorIdentity: supervisorStat.startTime,
		supervisorFD:       supervisorFD,
		initPID:            initPID,
		initIdentity:       initIdentity,
		initNamespace:      namespace,
		initFD:             initFD,
	}, nil
}

func (custody *linuxSessionCustody) revalidate() (linuxProcessStat, error) {
	if custody == nil || custody.supervisorFD < 0 || custody.initFD < 0 {
		return linuxProcessStat{}, ErrSessionCustodyUnsupported
	}
	supervisor, err := readLinuxProcessStat(custody.supervisorPID)
	if err != nil || supervisor.startTime != custody.supervisorIdentity || linuxProcessStateTerminal(supervisor.state) {
		return linuxProcessStat{}, errors.Join(ErrSessionGenerationChanged, err)
	}
	identity, namespace, err := validateLinuxNamespaceInit(linuxProcRoot, custody.supervisorPID, custody.initPID)
	if err != nil || identity != custody.initIdentity || namespace != custody.initNamespace {
		return linuxProcessStat{}, errors.Join(ErrSessionGenerationChanged, err)
	}
	return readLinuxProcessStat(custody.initPID)
}

func (custody *linuxSessionCustody) Freeze(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := custody.revalidate(); err != nil {
		return err
	}
	// PID namespace membership is inherited at clone time and cannot migrate
	// outward. No stop-the-world mutation is needed before the final checks.
	custody.prepared = true
	return nil
}

func (custody *linuxSessionCustody) Thaw() error {
	if custody != nil && !custody.committed {
		custody.prepared = false
	}
	return nil
}

func waitLinuxProcessTerminal(ctx context.Context, pid int, identity string) error {
	for {
		stat, err := readLinuxProcessStat(pid)
		if errors.Is(err, errProcessNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if stat.startTime != identity {
			return ErrSessionGenerationChanged
		}
		if linuxProcessStateTerminal(stat.state) {
			return nil
		}
		if err := waitForContext(ctx, processExitPollInterval); err != nil {
			return err
		}
	}
}

func (custody *linuxSessionCustody) Kill(ctx context.Context) (bool, error) {
	if custody == nil || !custody.prepared {
		return false, errors.New("session PID namespace custody is not prepared")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	initStat, err := custody.revalidate()
	if err != nil {
		return false, err
	}
	targetFD := custody.initFD
	targetPID := custody.initPID
	targetIdentity := custody.initIdentity
	if linuxProcessStateTerminal(initStat.state) {
		// The namespace init already exited, which proves all processes in that
		// namespace are terminal. Killing the exact pane supervisor is now the
		// first destructive operation and preserves truthful commit accounting.
		targetFD = custody.supervisorFD
		targetPID = custody.supervisorPID
		targetIdentity = custody.supervisorIdentity
	}
	if err := unix.PidfdSendSignal(targetFD, unix.SIGKILL, nil, 0); err != nil {
		return false, err
	}
	custody.committed = true
	if err := waitLinuxProcessTerminal(ctx, targetPID, targetIdentity); err != nil {
		return true, err
	}
	return true, nil
}

func (custody *linuxSessionCustody) Close() error {
	if custody == nil {
		return nil
	}
	var errs []error
	if custody.initFD >= 0 {
		errs = append(errs, unix.Close(custody.initFD))
		custody.initFD = -1
	}
	if custody.supervisorFD >= 0 {
		errs = append(errs, unix.Close(custody.supervisorFD))
		custody.supervisorFD = -1
	}
	return errors.Join(errs...)
}
