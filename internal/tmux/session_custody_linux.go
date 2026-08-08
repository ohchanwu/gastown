//go:build linux

package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	linuxCgroupRoot          = "/sys/fs/cgroup"
	linuxProcRoot            = "/proc"
	sessionCustodyPrefix     = ".gastown-session-"
	sessionCustodyReadyProbe = 300 * time.Millisecond
)

type linuxSessionCustody struct {
	dir       *os.File
	parent    *os.File
	name      string
	frozen    bool
	committed bool
}

func sessionCustodyLaunchSupported() bool { return true }

func linuxUnifiedCgroupPath(procRoot string, pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			path := strings.TrimPrefix(line, "0::")
			if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return "", errors.New("invalid unified cgroup path")
			}
			return path, nil
		}
	}
	return "", errors.New("process is not in a cgroup v2 hierarchy")
}

func linuxCgroupFilesystemPath(relative string) (string, error) {
	clean := filepath.Clean(relative)
	if !filepath.IsAbs(clean) || clean == "/" && relative != "/" {
		return "", errors.New("invalid cgroup path")
	}
	path := filepath.Join(linuxCgroupRoot, strings.TrimPrefix(clean, "/"))
	rootWithSeparator := linuxCgroupRoot + string(os.PathSeparator)
	if path != linuxCgroupRoot && !strings.HasPrefix(path, rootWithSeparator) {
		return "", errors.New("cgroup path escapes unified hierarchy")
	}
	return path, nil
}

func expectedSessionCustodyCgroup(custody string) string {
	return sessionCustodyPrefix + custody
}

func activateLinuxSessionCustody(custody string) error {
	current, err := linuxUnifiedCgroupPath(linuxProcRoot, os.Getpid())
	if err != nil {
		return err
	}
	parentPath, err := linuxCgroupFilesystemPath(current)
	if err != nil {
		return err
	}
	childPath := filepath.Join(parentPath, expectedSessionCustodyCgroup(custody))
	if err := os.Mkdir(childPath, 0o700); err != nil {
		return fmt.Errorf("creating delegated session cgroup: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(childPath)
		}
	}()
	for _, control := range []string{"cgroup.freeze", "cgroup.kill", "cgroup.events", "cgroup.procs"} {
		if _, err := os.Stat(filepath.Join(childPath, control)); err != nil {
			return fmt.Errorf("session cgroup lacks %s: %w", control, err)
		}
	}
	pid := []byte(strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(filepath.Join(childPath, "cgroup.procs"), pid, 0o600); err != nil {
		return fmt.Errorf("entering session cgroup: %w", err)
	}
	confirmed, err := linuxUnifiedCgroupPath(linuxProcRoot, os.Getpid())
	if err != nil || filepath.Base(confirmed) != expectedSessionCustodyCgroup(custody) {
		_ = os.WriteFile(filepath.Join(parentPath, "cgroup.procs"), pid, 0o600)
		if err != nil {
			return fmt.Errorf("confirming session cgroup: %w", err)
		}
		return errors.New("session process did not enter the generation-bound cgroup")
	}
	cleanup = false
	return nil
}

func runSessionWithCustody(custody, command string) error {
	if err := activateLinuxSessionCustody(custody); err != nil {
		// Availability varies by cgroup delegation. Preserve ordinary startup,
		// but leave the pane provably uncontained so later destructive cleanup
		// fails closed.
		fmt.Fprintf(os.Stderr, "gt session-custody: containment unavailable: %v\n", err)
	}
	child := exec.Command("/bin/sh", "-lc", command)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	timer := time.NewTimer(sessionCustodyReadyProbe)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
	}
	// After startup is proven, keep the pane root alive if the agent exits.
	// Witness can then revalidate the exact pane and cgroup generations before
	// replacing a zombie. Normal TERM/KILL signals still terminate this process.
	if err := <-done; err != nil {
		fmt.Fprintf(os.Stderr, "gt session-custody: supervised command exited: %v\n", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func openCgroupControl(dir *os.File, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readCgroupControl(dir *os.File, name string) (string, error) {
	file, err := openCgroupControl(dir, name, unix.O_RDONLY)
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(file)
	return string(data), errors.Join(readErr, file.Close())
}

func writeCgroupControl(dir *os.File, name, value string) error {
	file, err := openCgroupControl(dir, name, unix.O_WRONLY)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(file, value)
	return errors.Join(writeErr, file.Close())
}

func cgroupEventValue(events, key string) (string, bool) {
	for _, line := range strings.Split(events, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			return fields[1], true
		}
	}
	return "", false
}

func cgroupContainsPID(dir *os.File, pid int) (bool, error) {
	data, err := readCgroupControl(dir, "cgroup.procs")
	if err != nil {
		return false, err
	}
	want := strconv.Itoa(pid)
	for _, line := range strings.Fields(data) {
		if line == want {
			return true, nil
		}
	}
	return false, nil
}

func retainSessionCustody(custody string, panePID int) (sessionCustodyHandle, error) {
	if !validSessionGenerationRe.MatchString(custody) {
		return nil, ErrSessionCustodyUnsupported
	}
	relative, err := linuxUnifiedCgroupPath(linuxProcRoot, panePID)
	if err != nil || filepath.Base(relative) != expectedSessionCustodyCgroup(custody) {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err)
	}
	path, err := linuxCgroupFilesystemPath(relative)
	if err != nil {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err)
	}
	parentPath := filepath.Dir(path)
	name := filepath.Base(path)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err)
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err, parent.Close())
	}
	dir := os.NewFile(uintptr(fd), path)
	reject := func(err error) (sessionCustodyHandle, error) {
		return nil, errors.Join(ErrSessionCustodyUnsupported, err, dir.Close(), parent.Close())
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil || stat.Type != unix.CGROUP2_SUPER_MAGIC {
		return reject(errors.Join(errors.New("session custody is not a cgroup v2 directory"), err))
	}
	confirmed, err := linuxUnifiedCgroupPath(linuxProcRoot, panePID)
	if err != nil || confirmed != relative {
		return reject(errors.Join(errors.New("pane cgroup changed during custody acquisition"), err))
	}
	contains, err := cgroupContainsPID(dir, panePID)
	if err != nil || !contains {
		return reject(errors.Join(errors.New("session cgroup does not contain the captured pane process"), err))
	}
	for control, flags := range map[string]int{
		"cgroup.freeze": unix.O_WRONLY,
		"cgroup.kill":   unix.O_WRONLY,
		"cgroup.events": unix.O_RDONLY,
	} {
		file, err := openCgroupControl(dir, control, flags)
		if err != nil {
			return reject(fmt.Errorf("opening %s: %w", control, err))
		}
		if err := file.Close(); err != nil {
			return reject(fmt.Errorf("closing %s: %w", control, err))
		}
	}
	return &linuxSessionCustody{dir: dir, parent: parent, name: name}, nil
}

func (custody *linuxSessionCustody) waitEvent(ctx context.Context, key, want string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, err := readCgroupControl(custody.dir, "cgroup.events")
		if err != nil {
			return err
		}
		if got, ok := cgroupEventValue(events, key); ok && got == want {
			return nil
		}
		if err := waitForContext(ctx, processExitPollInterval); err != nil {
			return err
		}
	}
}

func (custody *linuxSessionCustody) Freeze(ctx context.Context) error {
	if custody == nil || custody.dir == nil {
		return ErrSessionCustodyUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeCgroupControl(custody.dir, "cgroup.freeze", "1"); err != nil {
		return err
	}
	custody.frozen = true
	if err := custody.waitEvent(ctx, "frozen", "1"); err != nil {
		return errors.Join(err, custody.Thaw())
	}
	return nil
}

func (custody *linuxSessionCustody) Thaw() error {
	if custody == nil || custody.dir == nil || !custody.frozen || custody.committed {
		return nil
	}
	if err := writeCgroupControl(custody.dir, "cgroup.freeze", "0"); err != nil {
		return err
	}
	custody.frozen = false
	return nil
}

func (custody *linuxSessionCustody) Kill(ctx context.Context) (bool, error) {
	if custody == nil || custody.dir == nil || !custody.frozen {
		return false, errors.New("session custody is not frozen")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := writeCgroupControl(custody.dir, "cgroup.kill", "1"); err != nil {
		return false, err
	}
	custody.committed = true
	if err := custody.waitEvent(ctx, "populated", "0"); err != nil {
		return true, err
	}
	custody.frozen = false
	return true, nil
}

func (custody *linuxSessionCustody) Close() error {
	if custody == nil || custody.dir == nil {
		return nil
	}
	thawErr := custody.Thaw()
	closeErr := custody.dir.Close()
	custody.dir = nil
	var removeErr error
	if custody.committed && custody.parent != nil {
		removeErr = unix.Unlinkat(int(custody.parent.Fd()), custody.name, unix.AT_REMOVEDIR)
		if errors.Is(removeErr, unix.ENOENT) {
			removeErr = nil
		}
	}
	var parentCloseErr error
	if custody.parent != nil {
		parentCloseErr = custody.parent.Close()
		custody.parent = nil
	}
	return errors.Join(thawErr, closeErr, removeErr, parentCloseErr)
}
