//go:build linux

package tmux

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	linuxX32SyscallBit   = uint32(0x40000000)
	linuxCustodyBrokerFD = uint32(6)
	linuxAMD64SysDup2    = uint32(33)
)

func buildLinuxCustodySeccompFilter(goarch string) ([]unix.SockFilter, error) {
	var auditArchitecture uint32
	switch goarch {
	case "amd64":
		auditArchitecture = unix.AUDIT_ARCH_X86_64
	case "arm64":
		auditArchitecture = unix.AUDIT_ARCH_AARCH64
	default:
		return nil, ErrSessionCustodyUnsupported
	}
	deny := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, Jf: 0, K: auditArchitecture},
		{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	if goarch == "amd64" {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jt: 0, Jf: 1, K: linuxX32SyscallBit},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		)
	}
	filter = append(filter,
		// close(fd): deny only the inherited broker endpoint.
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 3, K: uint32(unix.SYS_CLOSE)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: linuxCustodyBrokerFD},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		// close_range(first, last): deny any range that covers the broker endpoint.
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 5, K: uint32(unix.SYS_CLOSE_RANGE)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGT | unix.BPF_K, Jt: 3, Jf: 0, K: linuxCustodyBrokerFD},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 24},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, Jt: 0, Jf: 1, K: linuxCustodyBrokerFD},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
	)
	dupTargetSyscalls := []uint32{uint32(unix.SYS_DUP3)}
	if goarch == "amd64" {
		dupTargetSyscalls = append(dupTargetSyscalls, linuxAMD64SysDup2)
	}
	for _, syscallNumber := range dupTargetSyscalls {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 3, K: syscallNumber},
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 24},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: linuxCustodyBrokerFD},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		)
	}
	filter = append(filter,
		// fcntl(fd, F_SETFD): the endpoint must remain inherited across exec.
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 5, K: uint32(unix.SYS_FCNTL)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 3, K: linuxCustodyBrokerFD},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 24},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: uint32(unix.F_SETFD)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
	)
	for _, syscallNumber := range []uint32{
		uint32(unix.SYS_PTRACE),
		uint32(unix.SYS_PROCESS_VM_READV),
		uint32(unix.SYS_PROCESS_VM_WRITEV),
		uint32(unix.SYS_KCMP),
		uint32(unix.SYS_PIDFD_GETFD),
	} {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: syscallNumber},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		)
	}
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 2, K: uint32(unix.SYS_SOCKET)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 4, Jf: 5, K: uint32(unix.AF_UNIX)},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 2, K: uint32(unix.SYS_SOCKETPAIR)},
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, Jf: 2, K: uint32(unix.AF_UNIX)},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: uint32(unix.SYS_IO_URING_SETUP)},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	)
	return filter, nil
}

func installLinuxCustodySeccomp() error {
	filter, err := buildLinuxCustodySeccompFilter(runtime.GOARCH)
	if err != nil {
		return err
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("setting session custody no-new-privileges: %w", err)
	}
	_, _, errno := unix.RawSyscall(
		unix.SYS_SECCOMP,
		unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&program)),
	)
	if errno != 0 {
		return fmt.Errorf("installing session custody seccomp filter: %w", errno)
	}
	return nil
}

func setLinuxCustodyNonDumpable() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("making trusted session init non-dumpable: %w", err)
	}
	return nil
}
