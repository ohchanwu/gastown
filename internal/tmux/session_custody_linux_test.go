//go:build linux

package tmux

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/steveyegge/gastown/internal/config"
	"golang.org/x/sys/unix"
)

type linuxCustodyWorkloadEvidence struct {
	Escape      string `json:"escape"`
	WorkloadPID string `json:"workload_pid"`
	Hardening   string `json:"hardening"`
	Socket      string `json:"socket"`
	Network     string `json:"network"`
	Proxy       string `json:"proxy"`
	Cgroup      string `json:"cgroup"`
	Storage     string `json:"storage"`
	IPC         string `json:"ipc"`
}

func TestPinLinuxSessionTmuxExecutableRetainsExactInodeAcrossPATHReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	pinned, err := pinLinuxSessionTmuxExecutable()
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	pinnedInfo, err := pinned.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	pinnedStat := pinnedInfo.Sys().(*syscall.Stat_t)
	replacementStat := replacementInfo.Sys().(*syscall.Stat_t)
	if pinnedStat.Dev == replacementStat.Dev && pinnedStat.Ino == replacementStat.Ino {
		t.Fatal("pinned tmux descriptor followed the hostile PATH replacement")
	}
}

func TestLinuxSessionCustodyWorkloadHelper(t *testing.T) {
	if os.Getenv("GT_TEST_SESSION_CUSTODY_WORKLOAD") == "" {
		return
	}
	supervisorPID, err := strconv.Atoi(os.Getenv("GT_TEST_SESSION_CUSTODY_SUPERVISOR"))
	if err != nil || supervisorPID <= 0 {
		t.Fatalf("invalid supervisor PID: %v", err)
	}
	escape := "open-denied"
	if parentNS, openErr := os.Open(fmt.Sprintf("/proc/%d/ns/pid", supervisorPID)); openErr == nil {
		_, _, setnsErr := syscall.RawSyscall(uintptr(unix.SYS_SETNS), parentNS.Fd(), uintptr(unix.CLONE_NEWPID), 0)
		_ = parentNS.Close()
		escape = "setns-denied:" + setnsErr.Error()
		if setnsErr == 0 {
			escape = "escaped"
		}
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	workloadPID := ""
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			workloadPID = line
			break
		}
	}
	hardening := make([]string, 0, 7)
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "CapEff:") || strings.HasPrefix(line, "CapBnd:") || strings.HasPrefix(line, "NoNewPrivs:") {
			hardening = append(hardening, line)
		}
	}
	for name, resource := range map[string]int{
		"RlimitNOFILE": unix.RLIMIT_NOFILE,
		"RlimitFSIZE":  unix.RLIMIT_FSIZE,
		"RlimitAS":     unix.RLIMIT_AS,
		"RlimitCORE":   unix.RLIMIT_CORE,
	} {
		var limit unix.Rlimit
		if err := unix.Getrlimit(resource, &limit); err != nil {
			t.Fatal(err)
		}
		hardening = append(hardening, fmt.Sprintf("%s:%d:%d", name, limit.Cur, limit.Max))
	}
	if unmountErr := unix.Unmount(linuxProcRoot, unix.MNT_DETACH); unmountErr == nil {
		escape += ";mount-escaped"
	} else {
		escape += ";unmount-denied:" + unmountErr.Error()
	}
	socketResult := "unix=allowed"
	fd, socketErr := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if socketErr != nil {
		socketResult = "unix=" + socketErr.Error()
	} else {
		_ = unix.Close(fd)
	}
	if pair, pairErr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0); pairErr != nil {
		socketResult += ";socketpair=" + pairErr.Error()
	} else {
		socketResult += ";socketpair=allowed"
		_ = unix.Close(pair[0])
		_ = unix.Close(pair[1])
	}
	_, _, ioUringErr := unix.RawSyscall(unix.SYS_IO_URING_SETUP, 1, 0, 0)
	socketResult += ";io_uring=" + ioUringErr.Error()
	for name, syscallNumber := range map[string]uintptr{
		"ptrace":            unix.SYS_PTRACE,
		"process_vm_readv":  unix.SYS_PROCESS_VM_READV,
		"process_vm_writev": unix.SYS_PROCESS_VM_WRITEV,
		"kcmp":              unix.SYS_KCMP,
		"pidfd_getfd":       unix.SYS_PIDFD_GETFD,
	} {
		_, _, syscallErr := unix.RawSyscall6(syscallNumber, ^uintptr(0), 0, 0, 0, 0, 0)
		socketResult += ";" + name + "=" + syscallErr.Error()
	}
	if closeErr := unix.Close(int(linuxCustodyBrokerFD)); closeErr != nil {
		socketResult += ";broker_close=" + closeErr.Error()
	} else {
		socketResult += ";broker_close=allowed"
	}
	_, _, closeRangeErr := unix.RawSyscall(unix.SYS_CLOSE_RANGE, uintptr(linuxCustodyBrokerFD), uintptr(linuxCustodyBrokerFD), 0)
	socketResult += ";broker_close_range=" + closeRangeErr.Error()
	if dupErr := unix.Dup3(0, int(linuxCustodyBrokerFD), 0); dupErr != nil {
		socketResult += ";broker_dup_over=" + dupErr.Error()
	} else {
		socketResult += ";broker_dup_over=allowed"
	}
	if _, fcntlErr := unix.FcntlInt(uintptr(linuxCustodyBrokerFD), unix.F_SETFD, unix.FD_CLOEXEC); fcntlErr != nil {
		socketResult += ";broker_cloexec=" + fcntlErr.Error()
	} else {
		socketResult += ";broker_cloexec=allowed"
	}
	if socketType, endpointErr := unix.GetsockoptInt(int(linuxCustodyBrokerFD), unix.SOL_SOCKET, unix.SO_TYPE); endpointErr != nil {
		socketResult += ";broker_endpoint=" + endpointErr.Error()
	} else {
		socketResult += ";broker_endpoint=" + strconv.Itoa(socketType)
	}
	if inetFD, inetErr := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0); inetErr != nil {
		socketResult += ";inet=" + inetErr.Error()
	} else {
		socketResult += ";inet=allowed"
		_ = unix.Close(inetFD)
	}
	networkResult := "connected"
	connection, networkErr := net.DialTimeout("tcp", os.Getenv("GT_TEST_SESSION_CUSTODY_HOST_LOOPBACK"), 250*time.Millisecond)
	if networkErr != nil {
		networkResult = networkErr.Error()
	} else {
		_ = connection.Close()
	}
	proxyEvidence := make([]string, 0, 16)
	doltEndpoint := net.JoinHostPort(os.Getenv("GT_DOLT_HOST"), os.Getenv("GT_DOLT_PORT"))
	doltConnection, doltErr := net.DialTimeout("tcp", doltEndpoint, time.Second)
	if doltErr != nil {
		proxyEvidence = append(proxyEvidence, "dolt=denied")
	} else {
		_ = doltConnection.Close()
		proxyEvidence = append(proxyEvidence, "dolt=connected")
	}
	httpsProxy, parseErr := url.Parse(os.Getenv("HTTPS_PROXY"))
	if parseErr != nil || httpsProxy.Host == "" {
		proxyEvidence = append(proxyEvidence, fmt.Sprintf("https_proxy=invalid:%v", parseErr))
	} else {
		proxyConnection, proxyErr := net.DialTimeout("tcp", httpsProxy.Host, time.Second)
		if proxyErr != nil {
			proxyEvidence = append(proxyEvidence, "https_proxy="+proxyErr.Error())
		} else {
			_ = proxyConnection.SetDeadline(time.Now().Add(time.Second))
			_, _ = io.WriteString(proxyConnection, "CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n")
			status, _ := bufio.NewReader(proxyConnection).ReadString('\n')
			_ = proxyConnection.Close()
			proxyEvidence = append(proxyEvidence, "https_proxy="+strings.TrimSpace(status))
		}
	}
	for _, target := range []string{"10.0.0.1:443", "8.8.8.8:80"} {
		connection, directErr := net.DialTimeout("tcp", target, 250*time.Millisecond)
		if directErr == nil {
			_ = connection.Close()
			proxyEvidence = append(proxyEvidence, "direct="+target+":connected")
		} else {
			proxyEvidence = append(proxyEvidence, "direct="+target+":denied")
		}
	}
	for _, key := range []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "GT_DOLT_HOST", "GT_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT", "BEADS_DOLT_AUTO_START"} {
		proxyEvidence = append(proxyEvidence, key+"="+os.Getenv(key))
	}
	cgroupEvidence := verifyContainedCgroupIsReadOnly()
	storageEvidence := verifyContainedScratchIsBounded(t)
	ipcEvidence := verifyContainedIPCIsolation()
	if os.Getenv("GT_TEST_SESSION_CUSTODY_BROKER_PROOF") != "" {
		socketType, err := unix.GetsockoptInt(linuxSessionBrokerFD, unix.SOL_SOCKET, unix.SO_TYPE)
		if err != nil {
			t.Fatalf("checking inherited broker endpoint: %v", err)
		}
		if socketType != unix.SOCK_SEQPACKET {
			t.Fatalf("inherited broker socket type = %d, want SOCK_SEQPACKET", socketType)
		}
		exitCode, err := runSessionBrokerClientAtFD(
			linuxSessionBrokerFD,
			[]string{"-test.run=^TestLinuxSessionBrokerOutsideWorkerHelper$"},
			os.Stdin,
			os.Stdout,
			os.Stderr,
			10*time.Second,
		)
		if err != nil || exitCode != 0 {
			t.Fatalf("brokered worker exit code = %d, error = %v", exitCode, err)
		}
	}
	evidence := linuxCustodyWorkloadEvidence{
		Escape: escape, WorkloadPID: workloadPID, Hardening: strings.Join(hardening, "\n"),
		Socket: socketResult, Network: networkResult, Proxy: strings.Join(proxyEvidence, "\n"),
		Cgroup: cgroupEvidence, Storage: storageEvidence, IPC: ipcEvidence,
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("GT_CUSTODY_EVIDENCE=%s\n", base64.StdEncoding.EncodeToString(payload))
	script := "(setsid sh -c 'printf GT_CUSTODY_GRANDCHILD=; grep ^NSpid: /proc/$$/status; while :; do sleep 60; done' </dev/null >&1 2>&1 &) &"
	grandchild := exec.Command("sh", "-c", script)
	grandchild.Stdout = os.Stdout
	grandchild.Stderr = os.Stderr
	if err := grandchild.Run(); err != nil {
		t.Fatalf("starting detached grandchild: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestLinuxCustodyWorkloadEnvironmentUsesExplicitAllowlist(t *testing.T) {
	source := []string{
		"PATH=/usr/bin:/bin",
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"GT_ROLE=testrig/witness",
		"GT_INTERNAL_SESSION_CUSTODY_COMMAND=escape",
		"BEADS_DOLT_SERVER_PORT=3306",
		"BD_ACTOR=testrig/witness",
		"OPENAI_API_KEY=reviewed-secret",
		"NODE_OPTIONS=--require=/outer/escape.js",
		"HOME=/outer/home",
		"XDG_RUNTIME_DIR=/outer/runtime",
		"LD_PRELOAD=/outer/escape.so",
		"BASH_ENV=/outer/bash-env",
		"PYTHONPATH=/outer/python",
		"GT_TEST_FLOW_DIR=/fixture",
		"GT_TEST_FLOW_PRESET_ENV=must-not-pass",
		"GT_TEST_OUTER_SENTINEL=must-not-pass",
		"GT_FUTURE_CREDENTIAL=must-not-pass",
		"BD_FUTURE_CREDENTIAL=must-not-pass",
		"BEADS_FUTURE_CREDENTIAL=must-not-pass",
		"LC_FUTURE_CREDENTIAL=must-not-pass",
	}
	got := linuxCustodyWorkloadEnvironment(source)
	joined := "\x00" + strings.Join(got, "\x00") + "\x00"
	for _, want := range []string{
		"PATH=/usr/bin:/bin",
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"GT_ROLE=testrig/witness",
		"BEADS_DOLT_SERVER_PORT=3306",
		"BD_ACTOR=testrig/witness",
		"OPENAI_API_KEY=reviewed-secret",
		"NODE_OPTIONS=",
	} {
		if !strings.Contains(joined, "\x00"+want+"\x00") {
			t.Fatalf("allowlisted environment missing %q: %q", want, got)
		}
	}
	for _, denied := range []string{
		"GT_INTERNAL_SESSION_CUSTODY_COMMAND=escape",
		"HOME=/outer/home",
		"XDG_RUNTIME_DIR=/outer/runtime",
		"LD_PRELOAD=/outer/escape.so",
		"BASH_ENV=/outer/bash-env",
		"PYTHONPATH=/outer/python",
		"GT_TEST_FLOW_DIR=/fixture",
		"GT_TEST_FLOW_PRESET_ENV=must-not-pass",
		"GT_TEST_OUTER_SENTINEL=must-not-pass",
		"GT_FUTURE_CREDENTIAL=must-not-pass",
		"BD_FUTURE_CREDENTIAL=must-not-pass",
		"BEADS_FUTURE_CREDENTIAL=must-not-pass",
		"LC_FUTURE_CREDENTIAL=must-not-pass",
	} {
		if strings.Contains(joined, "\x00"+denied+"\x00") {
			t.Fatalf("unreviewed environment escaped containment: %q in %q", denied, got)
		}
	}
}

func TestDecodeLinuxCustodyAllowedPathsRejectsScratchOverlap(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "witness")
	if err := os.Mkdir(allowed, 0o700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, linuxSessionScratchPrefix+"nonce")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeSessionCustodyPaths([]string{allowed})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeLinuxCustodyAllowedPaths(encoded, scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLinuxCustodyMountSources(got)
	if len(got) != 1 || got[0].path != allowed || got[0].fd < 0 {
		t.Fatalf("decoded allowlist = %v", got)
	}
	overlap, err := EncodeSessionCustodyPaths([]string{filepath.Join(scratch, "escape")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeLinuxCustodyAllowedPaths(overlap, scratch); err == nil || !strings.Contains(err.Error(), "overlaps scratch") {
		t.Fatalf("scratch-overlap error = %v", err)
	}
	ancestor, err := EncodeSessionCustodyPaths([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeLinuxCustodyAllowedPaths(ancestor, scratch); err == nil || !strings.Contains(err.Error(), "overlaps scratch") {
		t.Fatalf("scratch-ancestor-overlap error = %v", err)
	}
}

func TestDecodeLinuxCustodyAllowedPathsRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(root, linuxSessionScratchPrefix+"nonce")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	terminalLink := filepath.Join(root, "terminal-link")
	if err := os.Symlink(realDir, terminalLink); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(root, "ancestor-link")
	if err := os.Symlink(root, ancestorLink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{terminalLink, filepath.Join(ancestorLink, "real")} {
		encoded, err := EncodeSessionCustodyPaths([]string{path})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeLinuxCustodyAllowedPaths(encoded, scratch); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink path %q error = %v", path, err)
		}
	}
}

func TestBindLinuxCustodyPathUsesPinnedInodeAfterPathReplacement(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("mount namespace unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	sourcePath := filepath.Join(parent, "allowed")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "marker"), []byte("pinned"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := pinLinuxCustodyMountSource(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLinuxCustodyMountSources([]linuxCustodyMountSource{source})

	movedPath := filepath.Join(parent, "original")
	if err := os.Rename(sourcePath, movedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "marker"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := bindLinuxCustodyPathReadOnly(root, source); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, strings.TrimPrefix(sourcePath, string(filepath.Separator)))
	defer unix.Unmount(target, unix.MNT_DETACH)
	data, err := os.ReadFile(filepath.Join(target, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "pinned" {
		t.Fatalf("mounted source after path replacement = %q, want pinned inode", data)
	}
}

func TestBindLinuxCustodyPathUsesPinnedInodeAfterAncestorReplacement(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skipf("mount namespace unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}

	parent := t.TempDir()
	ancestor := filepath.Join(parent, "approved")
	sourcePath := filepath.Join(ancestor, "allowed")
	if err := os.MkdirAll(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "marker"), []byte("pinned"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := pinLinuxCustodyMountSource(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLinuxCustodyMountSources([]linuxCustodyMountSource{source})

	if err := os.Rename(ancestor, filepath.Join(parent, "original-ancestor")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcePath, "marker"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := bindLinuxCustodyPathReadOnly(root, source); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, strings.TrimPrefix(sourcePath, string(filepath.Separator)))
	defer unix.Unmount(target, unix.MNT_DETACH)
	data, err := os.ReadFile(filepath.Join(target, "marker"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "pinned" {
		t.Fatalf("mounted source after ancestor replacement = %q, want pinned inode", data)
	}
}

func TestLinuxCustodyDefaultReadOnlyPathsExcludeBroadOpt(t *testing.T) {
	for _, path := range linuxCustodyDefaultReadOnlyPaths() {
		if path == "/opt" || linuxCustodyPathContains(path, "/opt/unrelated-secret") {
			t.Fatalf("default containment exposes broad /opt path %q", path)
		}
	}
}

func verifyContainedCgroupIsReadOnly() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "membership-error=" + err.Error()
	}
	relative, err := parseLinuxUnifiedCgroupPath(data)
	if err != nil {
		return "membership-error=" + err.Error()
	}
	path := filepath.Join(linuxCgroupMount, relative)
	controlErr := os.WriteFile(filepath.Join(path, "pids.max"), []byte("max"), 0o600)
	siblingErr := os.Mkdir(filepath.Join(filepath.Dir(path), "gastown-session-escape"), 0o700)
	if siblingErr == nil {
		_ = os.Remove(filepath.Join(filepath.Dir(path), "gastown-session-escape"))
	}
	return fmt.Sprintf("control=%v;sibling=%v", controlErr, siblingErr)
}

func verifyContainedScratchIsBounded(t *testing.T) string {
	t.Helper()
	scratch := os.Getenv("TMPDIR")
	var stat unix.Statfs_t
	if err := unix.Statfs(scratch, &stat); err != nil {
		return "statfs-error=" + err.Error()
	}
	capacity := uint64(stat.Blocks) * uint64(stat.Bsize)
	inodes := uint64(stat.Files)
	first, err := os.CreateTemp(scratch, "quota-a-")
	if err != nil {
		return "create-error=" + err.Error()
	}
	defer os.Remove(first.Name())
	defer first.Close()
	second, err := os.CreateTemp(scratch, "quota-b-")
	if err != nil {
		return "create-error=" + err.Error()
	}
	defer os.Remove(second.Name())
	defer second.Close()
	chunk := int64(linuxSessionScratchBytes * 3 / 5)
	firstErr := unix.Fallocate(int(first.Fd()), 0, 0, chunk)
	secondErr := unix.Fallocate(int(second.Fd()), 0, 0, chunk)
	hostWrite := os.WriteFile(filepath.Join(filepath.Dir(os.Args[0]), "custody-host-write"), []byte("escape"), 0o600)
	return fmt.Sprintf("capacity=%d;inodes=%d;first=%v;second=%v;host=%v", capacity, inodes, firstErr, secondErr, hostWrite)
}

func verifyContainedIPCIsolation() string {
	key, keyErr := strconv.Atoi(os.Getenv("GT_TEST_SESSION_CUSTODY_SYSV_KEY"))
	_, sysvErr := unix.SysvShmGet(key, 0, 0)
	_, posixErr := os.Open(os.Getenv("GT_TEST_SESSION_CUSTODY_POSIX_SHM"))
	return fmt.Sprintf("key=%v;sysv=%v;posix=%v", keyErr, sysvErr, posixErr)
}

func TestLinuxSessionBrokerOutsideWorkerHelper(t *testing.T) {
	proofPath := os.Getenv("GT_TEST_SESSION_CUSTODY_BROKER_PROOF")
	if proofPath == "" {
		return
	}
	pidNamespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	proof := fmt.Sprintf(
		"pidns=%s;custody=%q;init=%q;command=%q;namespaced=%q",
		pidNamespace,
		os.Getenv(EnvSessionCustody),
		os.Getenv(envLinuxSessionCustodyInit),
		os.Getenv(envLinuxSessionCustodyCommand),
		os.Getenv(envLinuxSessionCustodyNamespaced),
	)
	if err := os.WriteFile(proofPath, []byte(proof), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxSessionCustodySupervisorHelper(t *testing.T) {
	custody := os.Getenv("GT_TEST_SESSION_CUSTODY_HELPER")
	if custody == "" {
		return
	}
	t.Setenv(EnvSessionCustody, custody)
	if os.Getenv(EnvSessionCustodyPaths) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		executable, err := filepath.Abs(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		paths := []string{cwd, filepath.Dir(executable)}
		encoded, err := EncodeSessionCustodyPaths(paths)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(EnvSessionCustodyPaths, encoded)
	}
	t.Setenv("GT_TEST_SESSION_CUSTODY_SUPERVISOR", strconv.Itoa(os.Getpid()))
	command := os.Getenv("GT_TEST_SESSION_CUSTODY_COMMAND")
	if command == "" {
		var assignments []string
		for _, key := range []string{
			"GT_TEST_SESSION_CUSTODY_WORKLOAD",
			"GT_TEST_SESSION_CUSTODY_SUPERVISOR",
			"GT_TEST_SESSION_CUSTODY_SYSV_KEY",
			"GT_TEST_SESSION_CUSTODY_POSIX_SHM",
			"GT_TEST_SESSION_CUSTODY_HOST_LOOPBACK",
			"GT_TEST_SESSION_CUSTODY_BROKER_PROOF",
		} {
			if value := os.Getenv(key); value != "" {
				assignments = append(assignments, key+"="+config.ShellQuote(value))
			}
		}
		command = strings.Join(assignments, " ") + " exec " + config.ShellQuote(os.Args[0]) + " -test.run=^TestLinuxSessionCustodyWorkloadHelper$"
	}
	validator := SessionBrokerValidator(func([]string) error {
		return errors.New("test session broker command denied")
	})
	if os.Getenv("GT_TEST_SESSION_CUSTODY_BROKER_PROOF") != "" {
		validator = func(args []string) error {
			if len(args) == 1 && args[0] == "-test.run=^TestLinuxSessionBrokerOutsideWorkerHelper$" {
				return nil
			}
			return fmt.Errorf("unexpected test broker command %q", args)
		}
	}
	if err := RunSessionCustodyCommandWithBroker(custody, command, validator); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxDirectChildrenIncludesNonLeaderThreadChildren(t *testing.T) {
	type childResult struct {
		cmd *exec.Cmd
		tid int
		err error
	}
	started := make(chan childResult, 1)
	release := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cmd := exec.Command("sleep", "60")
		err := cmd.Start()
		started <- childResult{cmd: cmd, tid: unix.Gettid(), err: err}
		<-release
	}()
	result := <-started
	if result.err != nil {
		close(release)
		t.Fatal(result.err)
	}
	t.Cleanup(func() {
		_ = result.cmd.Process.Kill()
		_ = result.cmd.Wait()
		close(release)
	})
	if result.tid == os.Getpid() {
		t.Skip("runtime scheduled child launch on the process leader")
	}
	children, err := linuxDirectChildren(linuxProcRoot, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if child == result.cmd.Process.Pid {
			return
		}
	}
	t.Fatalf("non-leader thread %d child %d missing from %v", result.tid, result.cmd.Process.Pid, children)
}

func TestLaunchLinuxCustodyFailsClosedWhenNamespacesAreUnavailable(t *testing.T) {
	var attempts []bool
	launch, contained, err := launchLinuxCustodyCommand("sleep 60", func(command string, namespaced bool) (*linuxCustodyLaunch, error) {
		attempts = append(attempts, namespaced)
		return nil, unix.EPERM
	})
	if launch != nil || contained {
		t.Fatalf("launch = %v, contained %v, want no uncontained child", launch, contained)
	}
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("launch error = %v, want EPERM", err)
	}
	if len(attempts) != 1 || !attempts[0] {
		t.Fatalf("launch attempts = %v, want one contained attempt", attempts)
	}
}

func TestLaunchLinuxCustodyDoesNotFallbackAfterNonNamespaceStartFailure(t *testing.T) {
	startErr := errors.New("broken trusted init executable")
	var attempts []bool
	launch, contained, err := launchLinuxCustodyCommand("printf unsafe", func(_ string, namespaced bool) (*linuxCustodyLaunch, error) {
		attempts = append(attempts, namespaced)
		return nil, startErr
	})
	if launch != nil || contained {
		t.Fatalf("launch = %v, contained %v", launch, contained)
	}
	if !errors.Is(err, startErr) {
		t.Fatalf("launch error = %v, want %v", err, startErr)
	}
	if len(attempts) != 1 || !attempts[0] {
		t.Fatalf("launch attempts = %v, want one contained attempt", attempts)
	}
}

func TestLaunchLinuxCustodyValidationFailureNeverExecutesWorkload(t *testing.T) {
	sideEffect := filepath.Join(t.TempDir(), "ran")
	command := "printf 'ran\\n' >> " + config.ShellQuote(sideEffect)
	validationErr := errors.New("forced post-start validation failure")
	var attempts []bool
	launch, contained, err := launchLinuxCustodyCommandValidated(
		command,
		func(command string, namespaced bool) (*linuxCustodyLaunch, error) {
			attempts = append(attempts, namespaced)
			// Exercise the trusted readiness protocol without requiring nested
			// namespaces; the injected validator supplies the failure boundary.
			return startLinuxCustodyInitCommand(command, false)
		},
		func(string, int, int) (string, uint64, error) {
			return "", 0, validationErr
		},
	)
	if launch != nil || contained {
		t.Fatalf("launch = %v, contained %v", launch, contained)
	}
	if !errors.Is(err, validationErr) {
		t.Fatalf("launch error = %v, want %v", err, validationErr)
	}
	if len(attempts) != 1 || !attempts[0] {
		t.Fatalf("launch attempts = %v, want one contained attempt", attempts)
	}
	if data, readErr := os.ReadFile(sideEffect); !os.IsNotExist(readErr) {
		t.Fatalf("workload ran before validation: data %q, error %v", data, readErr)
	}
}

func TestLinuxSessionCustodyRetainsEmptyNamespaceInit(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	allowedPaths, err := EncodeSessionCustodyPaths([]string{"/usr", workingDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvSessionCustodyPaths, allowedPaths)
	launch, contained, err := launchLinuxCustodyCommand("sleep 0.1; exit 7", startLinuxCustodyCommand)
	if errors.Is(err, ErrSessionCustodyUnsupported) {
		t.Skipf("nested PID namespace setup unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = closeLinuxCustodyLaunch(launch, true)
	}()
	if !contained {
		t.Skip("nested PID namespaces unavailable")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		children, childrenErr := linuxDirectChildren(linuxProcRoot, launch.child.Process.Pid)
		if childrenErr == nil && len(children) == 0 {
			break
		}
		if childrenErr != nil {
			t.Fatalf("reading namespace init children: %v", childrenErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("namespace init did not reap exited workload; children %v", children)
		}
		time.Sleep(10 * time.Millisecond)
	}
	handle, err := retainSessionCustody(uuid.NewString(), os.Getpid())
	if err != nil {
		t.Fatalf("retaining empty namespace init: %v", err)
	}
	defer handle.Close()
	if err := handle.Freeze(context.Background()); err != nil {
		t.Fatalf("preparing empty namespace init: %v", err)
	}
}

func TestLaunchLinuxCustodyRequiresHardeningAckBeforeLiveness(t *testing.T) {
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyWriter.Close()
	permitReader, permitWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer permitReader.Close()
	lifeReader, lifeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer lifeReader.Close()
	scratch := filepath.Join(t.TempDir(), "scratch-receipt")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	launch := &linuxCustodyLaunch{
		child:   &exec.Cmd{Process: &os.Process{Pid: 424242}},
		pidfd:   -1,
		ready:   readyReader,
		permit:  permitWriter,
		life:    lifeWriter,
		scratch: scratch,
	}
	permitObserved := make(chan struct{})
	sendHardeningAck := make(chan struct{})
	go func() {
		_ = writeLinuxCustodyProtocolByte(readyWriter, linuxSessionCustodyReadyByte)
		var permit [1]byte
		_, _ = io.ReadFull(permitReader, permit[:])
		close(permitObserved)
		<-sendHardeningAck
		_ = writeLinuxCustodyProtocolByte(readyWriter, 'H')
	}()
	result := make(chan error, 1)
	go func() {
		_, _, launchErr := launchLinuxCustodyCommandValidated(
			"ignored",
			func(string, bool) (*linuxCustodyLaunch, error) { return launch, nil },
			func(string, int, int) (string, uint64, error) { return "identity", 99, nil },
		)
		result <- launchErr
	}()
	select {
	case <-permitObserved:
	case <-time.After(time.Second):
		t.Fatal("launch did not send validation permit")
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("scratch mountpoint still had a host path before workload permit: %v", err)
	}
	lifeResult := make(chan error, 1)
	go func() {
		var value [1]byte
		_, readErr := io.ReadFull(lifeReader, value[:])
		lifeResult <- readErr
	}()
	select {
	case readErr := <-lifeResult:
		t.Fatalf("supervisor liveness was exposed before hardening acknowledgment: %v", readErr)
	case <-time.After(100 * time.Millisecond):
	}
	close(sendHardeningAck)
	select {
	case readErr := <-lifeResult:
		if readErr != nil {
			t.Fatal(readErr)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor liveness was not exposed after hardening acknowledgment")
	}
	select {
	case launchErr := <-result:
		if launchErr != nil {
			t.Fatal(launchErr)
		}
	case <-time.After(time.Second):
		t.Fatal("launch did not finish after hardening acknowledgment")
	}
}

func TestWaitLinuxCustodyProcessBoundsUnreapedChild(t *testing.T) {
	launch := &linuxCustodyLaunch{wait: make(chan error)}
	started := time.Now()
	err := waitLinuxCustodyProcess(launch, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded wait took %v", elapsed)
	}
}

func TestWaitLinuxProcessGoneRejectsZombieUntilParentReaps(t *testing.T) {
	child := exec.Command("/bin/sh", "-c", "sleep 1h")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	releaseWait := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWait) }) }
	reaped := make(chan error, 1)
	go func() {
		<-releaseWait
		reaped <- child.Wait()
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		release()
		<-reaped
	})

	initialStat, err := readLinuxProcessStat(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity := initialStat.startTime
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		stat, statErr := readLinuxProcessStat(child.Process.Pid)
		if statErr == nil && stat.state == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d did not become an unreaped zombie: stat=%#v err=%v", child.Process.Pid, stat, statErr)
		}
		time.Sleep(time.Millisecond)
	}

	waitContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := waitLinuxProcessGone(waitContext, child.Process.Pid, identity); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for unreaped zombie = %v, want deadline exceeded", err)
	}
	release()
	reapContext, cancelReap := context.WithTimeout(context.Background(), time.Second)
	defer cancelReap()
	if err := waitLinuxProcessGone(reapContext, child.Process.Pid, identity); err != nil {
		t.Fatalf("wait after parent reap: %v", err)
	}
}

func TestCloseLinuxCustodyLaunchClosesPidfdAfterReapTimeout(t *testing.T) {
	fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	launch := &linuxCustodyLaunch{
		child: &exec.Cmd{},
		wait:  make(chan error),
		pidfd: fd,
	}
	err = closeLinuxCustodyLaunchWithTimeout(launch, true, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error = %v, want bounded reap evidence", err)
	}
	if launch.pidfd != -1 {
		t.Fatalf("launch pidfd = %d, want released ownership", launch.pidfd)
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("released pidfd F_GETFD error = %v, want EBADF", err)
	}
}

func TestLinuxSessionCustodyKillRefreshesEveryPostCommitBudget(t *testing.T) {
	custody := &linuxSessionCustody{
		supervisorPID: 101, supervisorIdentity: "supervisor", supervisorFD: 11,
		initPID: 202, initIdentity: "init", initFD: 22, prepared: true,
	}
	var signals []string
	var waits []string
	budget := 20 * time.Millisecond
	wait := func(ctx context.Context, pid int, identity string) error {
		if ctx.Err() != nil {
			t.Fatalf("phase %s started with exhausted context: %v", identity, ctx.Err())
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < budget/2 {
			t.Fatalf("phase %s did not receive a fresh budget: deadline %v", identity, deadline)
		}
		waits = append(waits, identity)
		if len(waits) < 3 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	ops := linuxCustodyKillOps{
		revalidate: func() (linuxProcessStat, error) {
			return linuxProcessStat{startTime: "init", state: 'S'}, nil
		},
		signal: func(fd int, signal unix.Signal) error {
			signals = append(signals, fmt.Sprintf("%d:%d", fd, signal))
			return nil
		},
		waitTerminal: wait,
		waitGone:     wait,
	}
	committed, err := custody.killWithBudgets(
		context.Background(),
		linuxCustodyKillBudgets{Init: budget, Broker: budget, Supervisor: budget},
		ops,
	)
	if !committed || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("killWithBudgets() = committed %v, error %v; want committed timeout evidence", committed, err)
	}
	wantSignals := []string{
		fmt.Sprintf("22:%d", unix.SIGKILL),
		fmt.Sprintf("11:%d", unix.SIGTERM),
		fmt.Sprintf("11:%d", unix.SIGKILL),
	}
	if strings.Join(signals, ",") != strings.Join(wantSignals, ",") {
		t.Fatalf("signals = %v, want %v", signals, wantSignals)
	}
	if strings.Join(waits, ",") != "init,supervisor,supervisor" {
		t.Fatalf("wait phases = %v", waits)
	}
}

func TestLinuxSessionCustodyFinalReapRetainsOwnershipUntilConfirmed(t *testing.T) {
	custody := &linuxSessionCustody{
		supervisorPID:      101,
		supervisorIdentity: "supervisor",
		supervisorFD:       -1,
		initFD:             -1,
		cgroup:             "/fixture/session",
		prepared:           true,
		committed:          true,
	}
	reapErr := errors.New("supervisor still awaiting parent reap")
	removeCalls := 0
	err := custody.finalizeAfterParentReleaseWithOps(
		context.Background(),
		func(context.Context, int, string) error { return reapErr },
		func(context.Context, string, time.Duration) error {
			removeCalls++
			return nil
		},
	)
	if !errors.Is(err, reapErr) {
		t.Fatalf("finalize error = %v, want reap evidence", err)
	}
	if custody.finalized || removeCalls != 0 {
		t.Fatalf("failed finalization = finalized %v, removals %d; want retained receipt", custody.finalized, removeCalls)
	}
	if err := custody.Close(); err == nil || !strings.Contains(err.Error(), "retaining ownership handles") {
		t.Fatalf("Close() error = %v, want retained ownership", err)
	}

	if err := custody.finalizeAfterParentReleaseWithOps(
		context.Background(),
		func(context.Context, int, string) error { return nil },
		func(_ context.Context, path string, _ time.Duration) error {
			removeCalls++
			if path != "/fixture/session" {
				t.Fatalf("remove cgroup path = %q", path)
			}
			return nil
		},
	); err != nil {
		t.Fatalf("retry finalization: %v", err)
	}
	if !custody.finalized || custody.cgroup != "" || removeCalls != 1 {
		t.Fatalf("confirmed finalization = finalized %v, cgroup %q, removals %d", custody.finalized, custody.cgroup, removeCalls)
	}
	if err := custody.Close(); err != nil {
		t.Fatalf("Close() after confirmed finalization: %v", err)
	}
}

func TestLinuxSessionCustodyFinalReapCgroupRemovalUsesRemainingCallerDeadline(t *testing.T) {
	custody := &linuxSessionCustody{
		supervisorPID:      101,
		supervisorIdentity: "supervisor",
		supervisorFD:       -1,
		initFD:             -1,
		cgroup:             "/fixture/session",
		prepared:           true,
		committed:          true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := custody.finalizeAfterParentReleaseWithOps(
		ctx,
		func(ctx context.Context, _ int, _ string) error {
			return waitForContext(ctx, 80*time.Millisecond)
		},
		func(ctx context.Context, path string, _ time.Duration) error {
			if path != "/fixture/session" {
				t.Fatalf("remove cgroup path = %q", path)
			}
			<-ctx.Done()
			return ctx.Err()
		},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("finalize error = %v, want caller deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cgroup removal escaped caller deadline: %v", elapsed)
	}
	if custody.finalized || custody.cgroup != "/fixture/session" {
		t.Fatalf("expired finalization = finalized %v, cgroup %q; want retained ownership", custody.finalized, custody.cgroup)
	}
}

func TestLinuxCustodyServiceShutdownBoundsIgnoredBrokerWorker(t *testing.T) {
	serviceContext, cancelServices := context.WithCancel(context.Background())
	results := make(chan linuxCustodyServiceResult, 3)
	results <- linuxCustodyServiceResult{name: "HTTPS proxy"}
	results <- linuxCustodyServiceResult{name: "Dolt proxy"}
	started := time.Now()
	err := shutdownLinuxCustodyServices(cancelServices, results, 3, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want broker timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("ignored broker worker blocked shutdown for %v", elapsed)
	}
	if serviceContext.Err() == nil {
		t.Fatal("service cancellation was not issued")
	}
}

func TestLinuxCustodySupervisorShutdownReapsAlreadyDeadInit(t *testing.T) {
	child := exec.Command("/bin/sh", "-c", "sleep 1h")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	releaseWait := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWait) }) }
	wait := make(chan error, 1)
	go func() {
		<-releaseWait
		wait <- child.Wait()
		close(wait)
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		release()
	})

	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		stat, err := readLinuxProcessStat(child.Process.Pid)
		if err == nil && stat.state == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d did not become an unreaped zombie: stat=%#v err=%v", child.Process.Pid, stat, err)
		}
		time.Sleep(time.Millisecond)
	}

	scratch := filepath.Join(t.TempDir(), linuxSessionScratchPrefix+"shutdown")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	launch := &linuxCustodyLaunch{child: child, wait: wait, pidfd: -1, scratch: scratch}
	serviceContext, cancelServices := context.WithCancel(context.Background())
	results := make(chan linuxCustodyServiceResult)
	go func() {
		time.Sleep(5 * time.Millisecond)
		release()
	}()
	err := shutdownLinuxCustodySupervisor(launch, cancelServices, results, 1, 20*time.Millisecond, 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown supervisor error = %v, want blocked service timeout after init reap", err)
	}
	if serviceContext.Err() == nil {
		t.Fatal("service cancellation was not issued")
	}
	if launch.child != nil || launch.wait != nil {
		t.Fatalf("init reap handles survived supervisor shutdown: %#v", launch)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", child.Process.Pid)); !os.IsNotExist(err) {
		t.Fatalf("reaped init %d still has a proc entry: %v", child.Process.Pid, err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("normal supervisor shutdown retained scratch receipt %q: %v", scratch, err)
	}
}

func TestFinalizeLinuxCustodyInitExitReleasesRetainedState(t *testing.T) {
	ready, readyPeer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyPeer.Close()
	permit, permitPeer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer permitPeer.Close()
	life, lifePeer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer lifePeer.Close()
	broker, brokerPeer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer brokerPeer.Close()

	wait := make(chan error, 1)
	launch := &linuxCustodyLaunch{
		child:  &exec.Cmd{},
		wait:   wait,
		pidfd:  -1,
		ready:  ready,
		permit: permit,
		life:   life,
		broker: broker,
	}
	processErr := errors.New("test init exit")
	err = finalizeLinuxCustodyInitExit(launch, processErr, "during test")
	if !errors.Is(err, processErr) || !strings.Contains(err.Error(), "session custody init stopped during test") {
		t.Fatalf("finalize error = %v, want wrapped process exit", err)
	}
	if launch.child != nil || launch.wait != nil || launch.ready != nil || launch.permit != nil || launch.life != nil || launch.broker != nil || launch.pidfd != -1 {
		t.Fatalf("retained launch state survived init exit: %#v", launch)
	}
}

func TestLinuxSessionCustodyContainsDoubleForkAndDeniesParentNamespace(t *testing.T) {
	custody := uuid.NewString()
	testDir := t.TempDir()
	hostListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hostListener.Close()
	brokerProofPath := testDir + "/broker-proof"
	ipcKey := int(time.Now().UnixNano()&0x3fffffff) | 0x10000
	sharedMemoryID, err := unix.SysvShmGet(ipcKey, 4096, unix.IPC_CREAT|unix.IPC_EXCL|0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.SysvShmCtl(sharedMemoryID, unix.IPC_RMID, nil)
	posixSharedMemoryPath := filepath.Join("/dev/shm", "gt-custody-"+uuid.NewString())
	if err := os.WriteFile(posixSharedMemoryPath, []byte("host namespace"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(posixSharedMemoryPath)
	wantBrokerPIDNamespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCustodySupervisorHelper$")
	cmd.Env = append(os.Environ(),
		"GT_TEST_SESSION_CUSTODY_HELPER="+custody,
		"GT_TEST_SESSION_CUSTODY_WORKLOAD=1",
		"GT_TEST_SESSION_CUSTODY_SYSV_KEY="+strconv.Itoa(ipcKey),
		"GT_TEST_SESSION_CUSTODY_POSIX_SHM="+posixSharedMemoryPath,
		"GT_TEST_SESSION_CUSTODY_HOST_LOOPBACK="+hostListener.Addr().String(),
		"GT_TEST_SESSION_CUSTODY_BROKER_PROOF="+brokerProofPath,
		"GT_DOLT_HOST=127.0.0.1",
		"GT_DOLT_PORT=33327",
	)
	var output synchronizedStringBuilder
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	supervisorReaped := false
	t.Cleanup(func() {
		if !supervisorReaped {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Errorf("timed out reaping custody supervisor %d", cmd.Process.Pid)
			}
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		snapshot := output.String()
		evidence, evidenceErr := parseLinuxCustodyWorkloadEvidence(snapshot)
		grandchild, grandchildErr := linuxCustodyOutputMarker(snapshot, "GT_CUSTODY_GRANDCHILD=")
		brokerProof, brokerProofErr := os.ReadFile(brokerProofPath)
		if evidenceErr == nil && grandchildErr == nil && brokerProofErr == nil {
			escape := evidence.Escape
			workloadPID := evidence.WorkloadPID
			socketResult := evidence.Socket
			hardeningResult := evidence.Hardening
			networkResult := evidence.Network
			proxyResult := evidence.Proxy
			if strings.Contains(strings.TrimSpace(escape), "escaped") {
				t.Fatal("contained workload joined the supervisor PID namespace")
			}
			fields := strings.Fields(grandchild)
			if len(fields) < 2 {
				t.Fatalf("malformed grandchild NSpid status: %q", grandchild)
			}
			workloadFields := strings.Fields(workloadPID)
			if len(workloadFields) < 2 || workloadFields[len(workloadFields)-1] == "1" {
				t.Fatalf("workload must run below trusted namespace init, got %q", workloadPID)
			}
			socketEvidence := strings.ToLower(socketResult)
			for _, field := range []string{
				"unix=operation not permitted",
				"socketpair=operation not permitted",
				"io_uring=operation not permitted",
				"ptrace=operation not permitted",
				"process_vm_readv=operation not permitted",
				"process_vm_writev=operation not permitted",
				"kcmp=operation not permitted",
				"pidfd_getfd=operation not permitted",
				"broker_close=operation not permitted",
				"broker_close_range=operation not permitted",
				"broker_dup_over=operation not permitted",
				"broker_cloexec=operation not permitted",
				"broker_endpoint=" + strconv.Itoa(unix.SOCK_SEQPACKET),
			} {
				if !strings.Contains(socketEvidence, field) {
					t.Fatalf("contained workload bypassed socket broker containment: %q", socketResult)
				}
			}
			hardeningEvidence := strings.ToLower(hardeningResult)
			for _, field := range []string{
				"capeff:\t0000000000000000",
				"capbnd:\t0000000000000000",
				"nonewprivs:\t1",
				fmt.Sprintf("rlimitnofile:%d:%d", linuxSessionNoFileLimit, linuxSessionNoFileLimit),
				fmt.Sprintf("rlimitfsize:%d:%d", linuxSessionFileLimit, linuxSessionFileLimit),
				fmt.Sprintf("rlimitas:%d:%d", linuxSessionASLimit, linuxSessionASLimit),
				"rlimitcore:0:0",
			} {
				if !strings.Contains(hardeningEvidence, field) {
					t.Fatalf("contained workload lacks hardening %q: %q", field, hardeningResult)
				}
			}
			if !strings.Contains(socketEvidence, "inet=allowed") {
				t.Fatalf("session custody blocked ordinary TCP sockets: %q", socketResult)
			}
			if strings.TrimSpace(networkResult) == "connected" {
				t.Fatal("contained workload reached the supervisor host loopback")
			}
			proxyEvidence := proxyResult
			for _, field := range []string{
				"dolt=denied",
				"https_proxy=HTTP/1.1 403 Forbidden",
				"direct=10.0.0.1:443:denied",
				"direct=8.8.8.8:80:denied",
				"GT_DOLT_HOST=127.0.0.1",
				"GT_DOLT_PORT=1",
				"BEADS_DOLT_SERVER_HOST=127.0.0.1",
				"BEADS_DOLT_SERVER_PORT=1",
				"BEADS_DOLT_PORT=1",
				"BEADS_DOLT_AUTO_START=0",
			} {
				if !strings.Contains(proxyEvidence, field) {
					t.Fatalf("contained workload proxy evidence missing %q: %q", field, proxyResult)
				}
			}
			if strings.Contains(proxyEvidence, "33327") {
				t.Fatalf("outer Dolt endpoint leaked into contained environment: %q", proxyResult)
			}
			cgroupEvidence := strings.ToLower(evidence.Cgroup)
			for _, field := range []string{"control=open", "sibling=mkdir"} {
				if !strings.Contains(cgroupEvidence, field) {
					t.Fatalf("contained workload can alter cgroup custody, missing %q: %q", field, evidence.Cgroup)
				}
			}
			if strings.Count(cgroupEvidence, "read-only file system") < 2 && strings.Count(cgroupEvidence, "no such file or directory") < 2 {
				t.Fatalf("contained workload can reach a writable cgroup hierarchy: %q", evidence.Cgroup)
			}
			storageEvidence := strings.ToLower(evidence.Storage)
			for _, field := range []string{fmt.Sprintf("capacity=%d", linuxSessionScratchBytes), fmt.Sprintf("inodes=%d", linuxSessionScratchInodes), "first=<nil>", "second=no space left on device", "host=open", "read-only file system"} {
				if !strings.Contains(storageEvidence, field) {
					t.Fatalf("contained storage lacks aggregate bound %q: %q", field, evidence.Storage)
				}
			}
			ipcEvidence := strings.ToLower(evidence.IPC)
			for _, field := range []string{"key=<nil>", "sysv=no such file or directory", "posix=open", "no such file or directory"} {
				if !strings.Contains(ipcEvidence, field) {
					t.Fatalf("contained IPC namespace leaked host IPC %q: %q", field, evidence.IPC)
				}
			}
			brokerEvidence := string(brokerProof)
			for _, field := range []string{
				"pidns=" + wantBrokerPIDNamespace,
				`custody=""`,
				`init=""`,
				`command=""`,
				`namespaced=""`,
			} {
				if !strings.Contains(brokerEvidence, field) {
					t.Fatalf("brokered worker did not run outside with sanitized custody environment %q: %q", field, brokerProof)
				}
			}
			grandchildNamespacePID, parseErr := strconv.Atoi(fields[len(fields)-1])
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			handle, err := retainSessionCustody(custody, cmd.Process.Pid)
			if err != nil {
				t.Fatalf("retaining PID-namespace custody: %v\n%s", err, output.String())
			}
			defer handle.Close()
			linuxHandle, ok := handle.(*linuxSessionCustody)
			if !ok {
				t.Fatalf("retained custody type = %T", handle)
			}
			supervisorCgroup, err := linuxCgroupDirectoryForPID(cmd.Process.Pid)
			if err != nil || supervisorCgroup != linuxHandle.cgroup {
				t.Fatalf("broker/proxy supervisor cgroup = %q, err %v; want aggregate session cgroup %q", supervisorCgroup, err, linuxHandle.cgroup)
			}
			grandchildPID, err := findLinuxHostPIDForNamespacePID(linuxHandle.initNamespace, grandchildNamespacePID)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := handle.Freeze(ctx); err != nil {
				t.Fatal(err)
			}
			committed, err := handle.Kill(ctx)
			if !committed || err != nil {
				t.Fatalf("Kill() = committed %v, err %v", committed, err)
			}
			select {
			case <-done:
				supervisorReaped = true
			case <-time.After(2 * time.Second):
				t.Fatal("first custody cleanup left the pane supervisor alive")
			}
			for i := 0; i < 200; i++ {
				stat, statErr := readLinuxProcessStat(grandchildPID)
				if errorsIsProcessGoneOrTerminal(stat, statErr) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("detached grandchild survived namespace-init kill")
		}
		select {
		case err := <-done:
			supervisorReaped = true
			if strings.Contains(output.String(), ErrSessionCustodyUnsupported.Error()) {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("custody supervisor exited before ready: %v\n%s", err, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("custody workload did not become ready\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type synchronizedStringBuilder struct {
	mu      sync.Mutex
	builder strings.Builder
}

func linuxCustodyOutputMarker(output, prefix string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			value := strings.TrimPrefix(line, prefix)
			if value != "" {
				return value, nil
			}
		}
	}
	return "", errors.New("custody output marker is unavailable")
}

func parseLinuxCustodyWorkloadEvidence(output string) (linuxCustodyWorkloadEvidence, error) {
	encoded, err := linuxCustodyOutputMarker(output, "GT_CUSTODY_EVIDENCE=")
	if err != nil {
		return linuxCustodyWorkloadEvidence{}, err
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return linuxCustodyWorkloadEvidence{}, err
	}
	var evidence linuxCustodyWorkloadEvidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		return linuxCustodyWorkloadEvidence{}, err
	}
	return evidence, nil
}

func (builder *synchronizedStringBuilder) Write(payload []byte) (int, error) {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	return builder.builder.Write(payload)
}

func (builder *synchronizedStringBuilder) String() string {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	return builder.builder.String()
}

func TestLinuxSessionGenerationCleanupStopsSupervisorOnFirstRun(t *testing.T) {
	tm := newTestTmux(t)
	session := fmt.Sprintf("gt-custody-stop-%d", time.Now().UnixNano())
	custody := uuid.NewString()
	command := "exec " + config.ShellQuote(os.Args[0]) + " -test.run=^TestLinuxSessionCustodySupervisorHelper$"
	env := map[string]string{
		EnvSessionCustody:                 custody,
		"GT_TEST_SESSION_CUSTODY_HELPER":  custody,
		"GT_TEST_SESSION_CUSTODY_COMMAND": "while :; do sleep 60; done",
	}
	if err := tm.NewSessionWithCommandAndEnv(session, "", command, env); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tm.KillSession(session) })
	generation, err := tm.CaptureSessionGeneration(session)
	if err != nil {
		t.Fatal(err)
	}
	pane, err := tm.CapturePaneProcessGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	var cleanup *SessionGenerationCleanup
	deadline := time.Now().Add(3 * time.Second)
	for {
		cleanup, err = tm.PrepareSessionGenerationProcessCleanup(generation, pane)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			if errors.Is(err, ErrSessionCustodyUnsupported) {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("preparing real session cleanup: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer cleanup.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cleanup.Run(ctx); err != nil {
		t.Fatalf("first exact session cleanup: %v", err)
	}
	has, err := tm.HasSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("first exact session cleanup left the tmux session alive")
	}
}

func TestLinuxSessionCustodySupervisorDeathKillsTrustedInit(t *testing.T) {
	custody := uuid.NewString()
	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "supervisor-output")
	outputFile, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outputFile.Close() })
	readOutput := func() string {
		output, _ := os.ReadFile(outputPath)
		return string(output)
	}
	command := "if [ -e /proc/$$/fd/5 ]; then printf 'GT_LIFE_FD=%s\\n' \"$(readlink /proc/$$/fd/5)\"; else printf 'GT_LIFE_FD=closed\\n'; fi; printf 'GT_READY=1\\n'; while :; do sleep 60; done"
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSessionCustodySupervisorHelper$")
	cmd.Env = append(os.Environ(),
		"GT_TEST_SESSION_CUSTODY_HELPER="+custody,
		"GT_TEST_SESSION_CUSTODY_COMMAND="+command,
	)
	// Use a regular file instead of an exec.Cmd-managed output pipe. Wait waits
	// for pipe-copy goroutines after the supervisor exits, and those goroutines
	// stay blocked while the very descendants this test is checking retain the
	// inherited output descriptors.
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Errorf("timed out reaping custody supervisor %d", cmd.Process.Pid)
			}
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	var handle sessionCustodyHandle
	for {
		if _, markerErr := linuxCustodyOutputMarker(readOutput(), "GT_READY="); markerErr == nil {
			handle, err = retainSessionCustody(custody, cmd.Process.Pid)
			if err == nil {
				break
			}
		}
		select {
		case err := <-done:
			reaped = true
			if strings.Contains(readOutput(), ErrSessionCustodyUnsupported.Error()) {
				t.Skipf("nested PID namespace setup unavailable: %v", err)
			}
			t.Fatalf("custody supervisor exited before ready: %v\n%s", err, readOutput())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("custody supervisor did not become ready\n%s", readOutput())
		}
		time.Sleep(10 * time.Millisecond)
	}
	lifeFD, err := linuxCustodyOutputMarker(readOutput(), "GT_LIFE_FD=")
	if err != nil {
		t.Fatal(err)
	}
	if lifeFD != "closed" {
		t.Fatalf("workload inherited trusted-init liveness descriptor: %s", lifeFD)
	}
	defer handle.Close()
	linuxHandle := handle.(*linuxSessionCustody)
	children, err := linuxDirectChildren(linuxProcRoot, linuxHandle.initPID)
	if err != nil || len(children) == 0 {
		t.Fatalf("trusted init children = %v, error %v", children, err)
	}
	workloadPID := children[0]
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		reaped = true
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reaping killed custody supervisor")
	}
	for _, target := range []struct {
		name string
		pid  int
	}{{"trusted init", linuxHandle.initPID}, {"workload", workloadPID}} {
		for i := 0; i < 200; i++ {
			stat, statErr := readLinuxProcessStat(target.pid)
			if errorsIsProcessGoneOrTerminal(stat, statErr) {
				break
			}
			if i == 199 {
				t.Fatalf("%s PID %d survived supervisor death", target.name, target.pid)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func errorsIsProcessGoneOrTerminal(stat linuxProcessStat, err error) bool {
	return errors.Is(err, errProcessNotFound) || err == nil && linuxProcessStateTerminal(stat.state)
}

func findLinuxHostPIDForNamespacePID(namespace uint64, namespacePID int) (int, error) {
	entries, err := os.ReadDir(linuxProcRoot)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		candidateNamespace, err := linuxPIDNamespaceInode(linuxProcRoot, pid)
		if err != nil || candidateNamespace != namespace {
			continue
		}
		pids, err := linuxNamespacePIDs(linuxProcRoot, pid)
		if err == nil && len(pids) > 0 && pids[len(pids)-1] == namespacePID {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("namespace %d has no process %d", namespace, namespacePID)
}
