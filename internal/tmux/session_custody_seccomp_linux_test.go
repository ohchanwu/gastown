//go:build linux

package tmux

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

const linuxCustodySeccompThreadHelperEnv = "GT_TEST_SESSION_CUSTODY_SECCOMP_THREAD_HELPER"
const linuxCustodyNonDumpableHelperEnv = "GT_TEST_SESSION_CUSTODY_NON_DUMPABLE_HELPER"

func testLinuxAuditArchitecture(t *testing.T) uint32 {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64
	default:
		t.Fatalf("unsupported test architecture %s", runtime.GOARCH)
		return 0
	}
}

func runLinuxCustodySeccompProgram(
	t *testing.T,
	filter []unix.SockFilter,
	architecture, syscallNumber uint32,
	arguments ...uint64,
) uint32 {
	t.Helper()
	data := make([]byte, 16+8*len(arguments))
	binary.LittleEndian.PutUint32(data[0:4], syscallNumber)
	binary.LittleEndian.PutUint32(data[4:8], architecture)
	for index, argument := range arguments {
		binary.LittleEndian.PutUint64(data[16+8*index:24+8*index], argument)
	}
	var accumulator uint32
	for pc := 0; pc < len(filter); {
		instruction := filter[pc]
		switch instruction.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			offset := int(instruction.K)
			if offset < 0 || offset+4 > len(data) {
				t.Fatalf("load at offset %d exceeds seccomp data", offset)
			}
			accumulator = binary.LittleEndian.Uint32(data[offset : offset+4])
			pc++
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if accumulator == instruction.K {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if accumulator&instruction.K != 0 {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JGT | unix.BPF_K:
			if accumulator > instruction.K {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
			}
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			if accumulator >= instruction.K {
				pc += int(instruction.Jt) + 1
			} else {
				pc += int(instruction.Jf) + 1
			}
		case unix.BPF_RET | unix.BPF_K:
			return instruction.K
		default:
			t.Fatalf("unsupported test instruction %#x at %d", instruction.Code, pc)
		}
	}
	t.Fatal("seccomp program fell off the end")
	return 0
}

func TestLinuxCustodySeccompRejectsX32SyscallBit(t *testing.T) {
	filter, err := buildLinuxCustodySeccompFilter(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	got := runLinuxCustodySeccompProgram(
		t,
		filter,
		unix.AUDIT_ARCH_X86_64,
		uint32(unix.SYS_GETPID)|uint32(0x40000000),
	)
	want := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	if got != want {
		t.Fatalf("x32 syscall action = %#x, want %#x", got, want)
	}
}

func TestLinuxCustodySeccompDeniesProcessInspection(t *testing.T) {
	filter, err := buildLinuxCustodySeccompFilter("amd64")
	if err != nil {
		t.Fatal(err)
	}
	want := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	for name, syscallNumber := range map[string]uint32{
		"ptrace":            uint32(unix.SYS_PTRACE),
		"process_vm_readv":  uint32(unix.SYS_PROCESS_VM_READV),
		"process_vm_writev": uint32(unix.SYS_PROCESS_VM_WRITEV),
		"kcmp":              uint32(unix.SYS_KCMP),
		"pidfd_getfd":       uint32(unix.SYS_PIDFD_GETFD),
	} {
		t.Run(name, func(t *testing.T) {
			got := runLinuxCustodySeccompProgram(t, filter, testLinuxAuditArchitecture(t), syscallNumber)
			if got != want {
				t.Fatalf("syscall action = %#x, want %#x", got, want)
			}
		})
	}
}

func TestLinuxCustodySeccompProtectsBrokerDescriptor(t *testing.T) {
	filter, err := buildLinuxCustodySeccompFilter(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	wantDeny := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	wantAllow := uint32(unix.SECCOMP_RET_ALLOW)
	const brokerFD = uint64(6)
	tests := []struct {
		name    string
		syscall uint32
		args    []uint64
		want    uint32
	}{
		{name: "close broker", syscall: uint32(unix.SYS_CLOSE), args: []uint64{brokerFD}, want: wantDeny},
		{name: "close other", syscall: uint32(unix.SYS_CLOSE), args: []uint64{brokerFD + 1}, want: wantAllow},
		{name: "close range covers broker", syscall: uint32(unix.SYS_CLOSE_RANGE), args: []uint64{3, 99}, want: wantDeny},
		{name: "close range below broker", syscall: uint32(unix.SYS_CLOSE_RANGE), args: []uint64{3, 5}, want: wantAllow},
		{name: "dup3 over broker", syscall: uint32(unix.SYS_DUP3), args: []uint64{7, brokerFD}, want: wantDeny},
		{name: "dup broker source", syscall: uint32(unix.SYS_DUP), args: []uint64{brokerFD}, want: wantAllow},
		{name: "fcntl dup broker source", syscall: uint32(unix.SYS_FCNTL), args: []uint64{brokerFD, unix.F_DUPFD, 0}, want: wantAllow},
		{name: "fcntl dup cloexec broker source", syscall: uint32(unix.SYS_FCNTL), args: []uint64{brokerFD, unix.F_DUPFD_CLOEXEC, 0}, want: wantAllow},
		{name: "fcntl cloexec broker", syscall: uint32(unix.SYS_FCNTL), args: []uint64{brokerFD, unix.F_SETFD, unix.FD_CLOEXEC}, want: wantDeny},
		{name: "fcntl clear cloexec broker", syscall: uint32(unix.SYS_FCNTL), args: []uint64{brokerFD, unix.F_SETFD, 0}, want: wantAllow},
	}
	if runtime.GOARCH == "amd64" {
		tests = append(tests, struct {
			name    string
			syscall uint32
			args    []uint64
			want    uint32
		}{name: "dup2 over broker", syscall: linuxAMD64SysDup2, args: []uint64{7, brokerFD}, want: wantDeny})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runLinuxCustodySeccompProgram(t, filter, testLinuxAuditArchitecture(t), test.syscall, test.args...)
			if got != test.want {
				t.Fatalf("syscall action = %#x, want %#x", got, test.want)
			}
		})
	}
}

func TestLinuxCustodySeccompThreadHelper(t *testing.T) {
	if os.Getenv(linuxCustodySeccompThreadHelperEnv) != "1" {
		return
	}
	runtime.GOMAXPROCS(2)
	ready := make(chan int, 1)
	probe := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		ready <- unix.Gettid()
		<-probe
		fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		result <- err
	}()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	workerTID := <-ready
	if workerTID == unix.Gettid() {
		t.Fatal("probe goroutine did not retain a distinct OS thread")
	}
	if err := installLinuxCustodySeccomp(); err != nil {
		t.Fatal(err)
	}
	close(probe)
	if err := <-result; err != unix.EPERM {
		t.Fatalf("pre-existing worker thread AF_UNIX error = %v, want seccomp EPERM", err)
	}
}

func TestLinuxCustodySeccompSynchronizesExistingThreads(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxCustodySeccompThreadHelper$")
	cmd.Env = append(os.Environ(), linuxCustodySeccompThreadHelperEnv+"=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("seccomp thread helper: %v\n%s", err, output)
	}
}

func TestLinuxCustodyNonDumpableHelper(t *testing.T) {
	if os.Getenv(linuxCustodyNonDumpableHelperEnv) != "1" {
		return
	}
	if err := setLinuxCustodyNonDumpable(); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestLinuxCustodyTrustedInitIsNonDumpable(t *testing.T) {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		t.Fatal(err)
	}
	capabilityWord := uint(unix.CAP_SYS_PTRACE) / 32
	capabilityBit := uint(unix.CAP_SYS_PTRACE) % 32
	if data[capabilityWord].Effective&(uint32(1)<<capabilityBit) != 0 {
		t.Skip("CAP_SYS_PTRACE intentionally overrides non-dumpable protection")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxCustodyNonDumpableHelper$")
	cmd.Env = append(os.Environ(), linuxCustodyNonDumpableHelperEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("hardening helper readiness = %q, %v", line, err)
	}
	if err := unix.PtraceAttach(cmd.Process.Pid); err == nil {
		var status unix.WaitStatus
		_, _ = unix.Wait4(cmd.Process.Pid, &status, 0, nil)
		_ = unix.PtraceDetach(cmd.Process.Pid)
		t.Fatal("same-UID parent attached to trusted init after hardening")
	} else if err != unix.EPERM {
		t.Fatalf("ptrace attach error = %v, want EPERM", err)
	}
}
