//go:build linux

package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxProcRoot                         = "/proc"
	sessionCustodyReadyProbe              = 300 * time.Millisecond
	linuxCustodyHandshakeTimeout          = 3 * time.Second
	linuxCustodyReapTimeout               = 3 * time.Second
	linuxCustodyReadyFD                   = 3
	linuxCustodyPermitFD                  = 4
	linuxCustodySupervisorLifeFD          = 5
	envLinuxSessionCustodyInit            = "GT_INTERNAL_SESSION_CUSTODY_INIT"
	envLinuxSessionCustodyCommand         = "GT_INTERNAL_SESSION_CUSTODY_COMMAND"
	envLinuxSessionCustodyNamespaced      = "GT_INTERNAL_SESSION_CUSTODY_NAMESPACED"
	linuxSessionCustodyReadyByte     byte = 'R'
	linuxSessionCustodyHardenedByte  byte = 'H'
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

func sessionCustodyLaunchSupported() bool {
	return runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"
}

func withoutEnvironmentKeys(env []string, keys ...string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		keep := true
		for _, key := range keys {
			if strings.HasPrefix(entry, key+"=") {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

type linuxCustodyLaunch struct {
	child  *exec.Cmd
	wait   <-chan error
	pidfd  int
	ready  *os.File
	permit *os.File
	life   *os.File
	broker *os.File
}

func startLinuxCustodyWait(child *exec.Cmd) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- child.Wait()
		close(result)
	}()
	return result
}

func waitLinuxCustodyProcess(launch *linuxCustodyLaunch, timeout time.Duration) error {
	if launch == nil || launch.wait == nil {
		return errors.New("session custody process has no wait handle")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-launch.wait:
		launch.wait = nil
		return err
	case <-timer.C:
		return fmt.Errorf("session custody process was not reaped: %w", context.DeadlineExceeded)
	}
}

func newLinuxUncontainedCustodyCommand(command string) (*exec.Cmd, *int) {
	child := exec.Command("/bin/sh", "-lc", command)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = withoutEnvironmentKeys(os.Environ(), EnvSessionCustody)
	pidfd := -1
	child.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: unix.SIGKILL,
		PidFD:     &pidfd,
	}
	return child, &pidfd
}

func startLinuxCustodyInitCommand(command string, namespaced bool) (_ *linuxCustodyLaunch, retErr error) {
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = readyReader.Close()
			_ = readyWriter.Close()
		}
	}()
	permitReader, permitWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = permitReader.Close()
			_ = permitWriter.Close()
		}
	}()
	lifeReader, lifeWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = lifeReader.Close()
			_ = lifeWriter.Close()
		}
	}()
	brokerPair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("creating session custody broker socket: %w", err)
	}
	brokerServer := os.NewFile(uintptr(brokerPair[0]), "session-custody-broker-server")
	brokerClient := os.NewFile(uintptr(brokerPair[1]), "session-custody-broker-client")
	defer func() {
		if retErr != nil {
			if brokerServer != nil {
				_ = brokerServer.Close()
			}
			if brokerClient != nil {
				_ = brokerClient.Close()
			}
		}
	}()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	child := exec.Command(executable, "session-custody-init")
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.ExtraFiles = []*os.File{readyWriter, permitReader, lifeReader, brokerClient}
	child.Env = append(
		withoutEnvironmentKeys(os.Environ(), envLinuxSessionCustodyInit, envLinuxSessionCustodyCommand, envLinuxSessionCustodyNamespaced),
		envLinuxSessionCustodyInit+"=1",
		envLinuxSessionCustodyCommand+"="+command,
	)
	if namespaced {
		child.Env = append(child.Env, envLinuxSessionCustodyNamespaced+"=1")
	}
	pidfd := -1
	child.SysProcAttr = &syscall.SysProcAttr{
		PidFD: &pidfd,
	}
	if namespaced {
		child.SysProcAttr.Cloneflags = unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNS | unix.CLONE_NEWNET
		child.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
		child.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
		child.SysProcAttr.GidMappingsEnableSetgroups = false
	}
	if err := child.Start(); err != nil {
		return nil, err
	}
	launch := &linuxCustodyLaunch{
		child: child, wait: startLinuxCustodyWait(child), pidfd: pidfd,
		ready: readyReader, permit: permitWriter, life: lifeWriter, broker: brokerServer,
	}
	brokerServer = nil
	_ = readyWriter.Close()
	_ = permitReader.Close()
	_ = lifeReader.Close()
	_ = brokerClient.Close()
	brokerClient = nil
	if pidfd < 0 {
		return nil, errors.Join(errors.New("kernel did not return a pidfd for session custody"), closeLinuxCustodyLaunch(launch, true))
	}
	return launch, nil
}

func startLinuxCustodyCommand(command string, namespaced bool) (*linuxCustodyLaunch, error) {
	if namespaced {
		return startLinuxCustodyInitCommand(command, true)
	}
	child, pidfd := newLinuxUncontainedCustodyCommand(command)
	if err := child.Start(); err != nil {
		return nil, err
	}
	launch := &linuxCustodyLaunch{child: child, wait: startLinuxCustodyWait(child), pidfd: *pidfd}
	if *pidfd < 0 {
		return nil, errors.Join(errors.New("kernel did not return a pidfd for session custody"), closeLinuxCustodyLaunch(launch, true))
	}
	return launch, nil
}

type linuxCustodyStarter func(string, bool) (*linuxCustodyLaunch, error)
type linuxNamespaceValidator func(string, int, int) (string, uint64, error)

func closeLinuxCustodyLaunch(launch *linuxCustodyLaunch, terminate bool) error {
	if launch == nil {
		return nil
	}
	var errs []error
	reaped := false
	if terminate && launch.child != nil {
		if launch.pidfd >= 0 {
			errs = append(errs, unix.PidfdSendSignal(launch.pidfd, unix.SIGKILL, nil, 0))
		} else {
			errs = append(errs, launch.child.Process.Kill())
		}
		waitErr := waitLinuxCustodyProcess(launch, linuxCustodyReapTimeout)
		errs = append(errs, waitErr)
		reaped = !errors.Is(waitErr, context.DeadlineExceeded)
	}
	if launch.ready != nil {
		errs = append(errs, launch.ready.Close())
		launch.ready = nil
	}
	if launch.permit != nil {
		errs = append(errs, launch.permit.Close())
		launch.permit = nil
	}
	if terminate && launch.life != nil {
		errs = append(errs, launch.life.Close())
		launch.life = nil
	}
	if terminate && launch.broker != nil {
		errs = append(errs, launch.broker.Close())
		launch.broker = nil
	}
	if terminate && reaped && launch.pidfd >= 0 {
		errs = append(errs, unix.Close(launch.pidfd))
		launch.pidfd = -1
	}
	return errors.Join(errs...)
}

func waitLinuxCustodyReady(launch *linuxCustodyLaunch) error {
	if launch == nil || launch.ready == nil {
		return errors.New("session custody init has no readiness pipe")
	}
	return waitLinuxCustodyProtocolByte(launch.ready, linuxSessionCustodyReadyByte, "readiness")
}

func waitLinuxCustodyHardened(launch *linuxCustodyLaunch) error {
	if launch == nil || launch.ready == nil {
		return errors.New("session custody init has no hardening pipe")
	}
	return waitLinuxCustodyProtocolByte(launch.ready, linuxSessionCustodyHardenedByte, "hardening")
}

func waitLinuxCustodyProtocolByte(file *os.File, expected byte, phase string) error {
	type readResult struct {
		value byte
		err   error
	}
	result := make(chan readResult, 1)
	go func() {
		var value [1]byte
		_, err := io.ReadFull(file, value[:])
		result <- readResult{value: value[0], err: err}
	}()
	timer := time.NewTimer(linuxCustodyHandshakeTimeout)
	defer timer.Stop()
	select {
	case received := <-result:
		if received.err != nil {
			return fmt.Errorf("session custody init %s: %w", phase, received.err)
		}
		if received.value != expected {
			return fmt.Errorf("session custody init sent invalid %s byte %d", phase, received.value)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out waiting for session custody init %s", phase)
	}
}

func writeLinuxCustodyProtocolByte(file *os.File, value byte) error {
	if file == nil {
		return errors.New("session custody protocol descriptor is unavailable")
	}
	written, err := file.Write([]byte{value})
	if err != nil {
		return err
	}
	if written != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func linuxNamespaceUnavailable(err error) bool {
	return errors.Is(err, unix.EPERM) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS)
}

func launchLinuxCustodyCommand(command string, start linuxCustodyStarter) (*linuxCustodyLaunch, bool, error) {
	return launchLinuxCustodyCommandValidated(command, start, validateLinuxNamespaceInit)
}

func launchLinuxCustodyCommandValidated(command string, start linuxCustodyStarter, validate linuxNamespaceValidator) (*linuxCustodyLaunch, bool, error) {
	launch, err := start(command, true)
	if err != nil {
		if !linuxNamespaceUnavailable(err) {
			return nil, false, err
		}
		// User namespaces may be disabled by the host. Preserve ordinary
		// startup, but leave a directly observable non-namespace child so later
		// destructive cleanup fails closed.
		fmt.Fprintf(os.Stderr, "gt session-custody: PID namespace unavailable: %v\n", err)
		launch, err = start(command, false)
		if err != nil {
			return nil, false, err
		}
		return launch, false, nil
	}
	if err := waitLinuxCustodyReady(launch); err != nil {
		return nil, false, errors.Join(ErrSessionCustodyUnsupported, err, closeLinuxCustodyLaunch(launch, true))
	}
	if _, _, err := validate(linuxProcRoot, os.Getpid(), launch.child.Process.Pid); err != nil {
		return nil, false, errors.Join(err, closeLinuxCustodyLaunch(launch, true))
	}
	if err := writeLinuxCustodyProtocolByte(launch.permit, 1); err != nil {
		return nil, false, errors.Join(err, closeLinuxCustodyLaunch(launch, true))
	}
	if err := waitLinuxCustodyHardened(launch); err != nil {
		return nil, false, errors.Join(ErrSessionCustodyUnsupported, err, closeLinuxCustodyLaunch(launch, true))
	}
	if err := writeLinuxCustodyProtocolByte(launch.life, 1); err != nil {
		return nil, false, errors.Join(errors.New("session custody supervisor liveness handshake failed"), err, closeLinuxCustodyLaunch(launch, true))
	}
	if err := closeLinuxCustodyLaunch(launch, false); err != nil {
		return nil, false, errors.Join(err, closeLinuxCustodyLaunch(launch, true))
	}
	return launch, true, nil
}

func runSessionCustodyInit() (bool, error) {
	if os.Getenv(envLinuxSessionCustodyInit) != "1" {
		return false, nil
	}
	command := os.Getenv(envLinuxSessionCustodyCommand)
	if strings.TrimSpace(command) == "" {
		return true, errors.New("session custody init command is empty")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Getenv(envLinuxSessionCustodyNamespaced) == "1" {
		if err := prepareLinuxCustodyNamespace(); err != nil {
			return true, err
		}
	}
	if err := installLinuxCustodySeccomp(); err != nil {
		return true, err
	}
	ready := os.NewFile(linuxCustodyReadyFD, "session-custody-ready")
	permit := os.NewFile(linuxCustodyPermitFD, "session-custody-permit")
	life := os.NewFile(linuxCustodySupervisorLifeFD, "session-custody-supervisor-life")
	if ready == nil || permit == nil || life == nil {
		return true, errors.New("session custody init protocol descriptors are unavailable")
	}
	if err := validateLinuxSessionBrokerEndpoint(linuxSessionBrokerFD); err != nil {
		return true, err
	}
	unix.CloseOnExec(int(life.Fd()))
	go func() {
		var value [1]byte
		for {
			if _, err := life.Read(value[:]); err != nil {
				// A PID namespace protects its PID 1 from signals sent inside
				// that namespace, including a self-directed SIGKILL. Exiting the
				// trusted init itself makes the kernel tear down every remaining
				// process in the namespace when the supervisor disappears.
				os.Exit(125)
			}
		}
	}()
	if err := writeLinuxCustodyProtocolByte(ready, linuxSessionCustodyReadyByte); err != nil {
		return true, err
	}
	var permission [1]byte
	if _, err := io.ReadFull(permit, permission[:]); err != nil {
		return true, fmt.Errorf("waiting for session custody launch permit: %w", err)
	}
	if err := permit.Close(); err != nil {
		return true, err
	}
	if err := setLinuxCustodyNonDumpable(); err != nil {
		return true, err
	}
	if err := writeLinuxCustodyProtocolByte(ready, linuxSessionCustodyHardenedByte); err != nil {
		return true, err
	}
	if err := ready.Close(); err != nil {
		return true, err
	}
	return true, runLinuxCustodyWorkload(command)
}

func prepareLinuxCustodyNamespace() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making session custody mounts private: %w", err)
	}
	if err := unix.Mount("proc", linuxProcRoot, "proc", unix.MS_NOSUID|unix.MS_NOEXEC|unix.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mounting private session custody procfs: %w", err)
	}
	for capability := 0; capability < 64; capability++ {
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(capability), 0, 0, 0); err != nil && !errors.Is(err, unix.EINVAL) {
			return fmt.Errorf("dropping session custody capability %d: %w", capability, err)
		}
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&header, &data[0]); err != nil {
		return fmt.Errorf("clearing session custody capabilities: %w", err)
	}
	return nil
}

func runLinuxCustodyWorkload(command string) error {
	env := withoutEnvironmentKeys(
		os.Environ(),
		EnvSessionCustody,
		envLinuxSessionCustodyInit,
		envLinuxSessionCustodyCommand,
		envLinuxSessionCustodyNamespaced,
		"TMUX",
		"TMUX_PANE",
	)
	_, err := syscall.ForkExec(
		"/bin/sh",
		[]string{"/bin/sh", "-lc", command},
		&syscall.ProcAttr{Env: env, Files: []uintptr{0, 1, 2, ^uintptr(0), ^uintptr(0), ^uintptr(0), uintptr(linuxSessionBrokerFD)}},
	)
	if err != nil {
		return err
	}
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			for {
				time.Sleep(time.Hour)
			}
		}
		if err != nil {
			return err
		}
		_ = pid
	}
}

func runSessionWithCustody(_ string, command string, validate SessionBrokerValidator) error {
	launch, contained, err := launchLinuxCustodyCommand(command, startLinuxCustodyCommand)
	if err != nil {
		return err
	}
	var brokerDone <-chan error
	if contained {
		if launch.broker == nil {
			return errors.Join(errors.New("contained session has no broker endpoint"), closeLinuxCustodyLaunch(launch, true))
		}
		brokerFD, dupErr := unix.Dup(int(launch.broker.Fd()))
		if dupErr != nil {
			return errors.Join(fmt.Errorf("retaining session broker endpoint: %w", dupErr), closeLinuxCustodyLaunch(launch, true))
		}
		if closeErr := launch.broker.Close(); closeErr != nil {
			_ = unix.Close(brokerFD)
			launch.broker = nil
			return errors.Join(fmt.Errorf("closing original session broker endpoint: %w", closeErr), closeLinuxCustodyLaunch(launch, true))
		}
		launch.broker = nil
		executable, executableErr := os.Executable()
		if executableErr != nil {
			_ = unix.Close(brokerFD)
			return errors.Join(executableErr, closeLinuxCustodyLaunch(launch, true))
		}
		brokerContext, cancelBroker := context.WithCancel(context.Background())
		defer cancelBroker()
		result := make(chan error, 1)
		go func() {
			result <- ServeSessionBroker(brokerContext, executable, brokerFD, validate)
		}()
		brokerDone = result
	}
	// Retain the namespace init pidfd and the supervisor-liveness writer until
	// exact teardown. The trusted init remains alive after reaping the workload,
	// so it is a stable direct-child custody boundary rather than an untrusted
	// PID-1 process or a signal-disposition-dependent zombie.
	timer := time.NewTimer(sessionCustodyReadyProbe)
	defer timer.Stop()
	if brokerDone != nil {
		select {
		case brokerErr := <-brokerDone:
			if brokerErr == nil {
				brokerErr = errors.New("session broker stopped without an error")
			}
			return errors.Join(
				fmt.Errorf("session broker stopped before readiness: %w", brokerErr),
				closeLinuxCustodyLaunch(launch, true),
			)
		case <-timer.C:
		}
	} else {
		<-timer.C
	}
	for {
		if brokerDone != nil {
			select {
			case brokerErr := <-brokerDone:
				if brokerErr == nil {
					brokerErr = errors.New("session broker stopped without an error")
				}
				return errors.Join(
					fmt.Errorf("session broker stopped: %w", brokerErr),
					closeLinuxCustodyLaunch(launch, true),
				)
			case <-time.After(time.Hour):
			}
		} else {
			time.Sleep(time.Hour)
		}
		if contained && launch.life == nil {
			return errors.New("session custody lost its supervisor liveness handle")
		}
		runtime.KeepAlive(launch)
	}
}

func linuxDirectChildren(procRoot string, pid int) ([]int, error) {
	taskRoot := fmt.Sprintf("%s/%d/task", procRoot, pid)
	tasks, err := os.ReadDir(taskRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	childrenByPID := make(map[int]struct{})
	for _, task := range tasks {
		tid, err := strconv.Atoi(task.Name())
		if err != nil || tid <= 0 {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("%s/%d/children", taskRoot, tid))
		if err != nil {
			// A Go runtime thread may exit between the task-directory scan and
			// this read. Its surviving children are reparented within the thread
			// group and will appear under another task; other failures are not
			// safe to ignore.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, field := range strings.Fields(string(data)) {
			child, err := strconv.Atoi(field)
			if err != nil || child <= 0 {
				return nil, fmt.Errorf("invalid direct child PID %q", field)
			}
			childrenByPID[child] = struct{}{}
		}
	}
	children := make([]int, 0, len(childrenByPID))
	for child := range childrenByPID {
		children = append(children, child)
	}
	sort.Ints(children)
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
	var errs []error
	if !linuxProcessStateTerminal(initStat.state) {
		if err := unix.PidfdSendSignal(custody.initFD, unix.SIGKILL, nil, 0); err != nil {
			if !errors.Is(err, unix.ESRCH) {
				return false, err
			}
		} else {
			custody.committed = true
			errs = append(errs, waitLinuxProcessTerminal(ctx, custody.initPID, custody.initIdentity))
		}
	}
	if err := unix.PidfdSendSignal(custody.supervisorFD, unix.SIGKILL, nil, 0); err != nil {
		if !errors.Is(err, unix.ESRCH) {
			errs = append(errs, err)
		}
	} else {
		custody.committed = true
		errs = append(errs, waitLinuxProcessTerminal(ctx, custody.supervisorPID, custody.supervisorIdentity))
	}
	if !custody.committed {
		errs = append(errs, ErrSessionGenerationChanged)
	}
	return custody.committed, errors.Join(errs...)
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
