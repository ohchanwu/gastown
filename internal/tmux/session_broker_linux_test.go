//go:build linux

package tmux

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSessionBrokerRequestRoundTrip(t *testing.T) {
	want := sessionBrokerRequest{
		Version:    sessionBrokerProtocolVersion,
		DeadlineMS: 30_000,
		Args:       []string{"mail", "inbox", "--json"},
	}
	payload, err := encodeSessionBrokerRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSessionBrokerRequest(payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version || got.DeadlineMS != want.DeadlineMS || strings.Join(got.Args, "\x00") != strings.Join(want.Args, "\x00") {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestSessionBrokerRequestRejectsMalformedBounds(t *testing.T) {
	tests := []struct {
		name    string
		request sessionBrokerRequest
	}{
		{name: "wrong version", request: sessionBrokerRequest{Version: 0, DeadlineMS: 1_000, Args: []string{"prime"}}},
		{name: "zero deadline", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, Args: []string{"prime"}}},
		{name: "excessive deadline", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: sessionBrokerMaxDeadlineMS + 1, Args: []string{"prime"}}},
		{name: "no arguments", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000}},
		{name: "too many arguments", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: make([]string, sessionBrokerMaxArgs+1)}},
		{name: "empty argument", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"prime", ""}}},
		{name: "nul argument", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"mail", "inbox\x00--json"}}},
		{name: "oversized argument", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{strings.Repeat("x", sessionBrokerMaxArgBytes+1)}}},
		{name: "oversized frame", request: sessionBrokerRequest{Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: repeatedBrokerArguments(17, sessionBrokerMaxArgBytes)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := encodeSessionBrokerRequest(test.request); err == nil {
				t.Fatal("malformed request encoded successfully")
			}
		})
	}
}

func TestSessionBrokerRejectsCommandBeforeWorkerExec(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func([]string) error {
			return errors.New("not reviewed")
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	proofPath := filepath.Join(t.TempDir(), "escaped")
	code, err := runSessionBrokerClientAtFD(
		clientFD,
		[]string{"-c", "touch " + proofPath},
		os.Stdin,
		os.Stdout,
		os.Stderr,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if code != sessionBrokerDeniedExitCode {
		t.Fatalf("denied exit code = %d, want %d", code, sessionBrokerDeniedExitCode)
	}
	if _, err := os.Stat(proofPath); !os.IsNotExist(err) {
		t.Fatalf("denied broker request executed worker; stat error = %v", err)
	}
}

func TestSessionBrokerRejectsNonPipeCompletionBeforeValidation(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	var validations atomic.Int32
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/false", serverFD, func([]string) error {
			validations.Add(1)
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	frame, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"unsafe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	completion, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer completion.Close()
	rights := unix.UnixRights(0, 1, 2, int(completion.Fd()))
	if _, err := unix.SendmsgN(clientFD, frame, rights, nil, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := validations.Load(); got != 0 {
		t.Fatalf("validator calls = %d, want 0", got)
	}
}

func TestSessionBrokerCancellationKillsWorkerProcessGroup(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func([]string) error { return nil }, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
	})

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutReader.Close()
	clientDone := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		defer stdoutWriter.Close()
		code, err := runSessionBrokerClientAtFD(
			clientFD,
			[]string{"-c", "sleep 30 & echo $!; wait"},
			os.Stdin,
			stdoutWriter,
			os.Stderr,
			30*time.Second,
		)
		clientDone <- struct {
			code int
			err  error
		}{code: code, err: err}
	}()

	pidLine := make(chan struct {
		line string
		err  error
	}, 1)
	go func() {
		line, err := bufio.NewReader(stdoutReader).ReadString('\n')
		pidLine <- struct {
			line string
			err  error
		}{line: line, err: err}
	}()
	var childPID int
	select {
	case result := <-pidLine:
		if result.err != nil {
			t.Fatalf("reading broker worker barrier: %v", result.err)
		}
		childPID, err = strconv.Atoi(strings.TrimSpace(result.line))
		if err != nil || childPID <= 0 {
			t.Fatalf("broker worker child PID = %q: %v", result.line, err)
		}
	case result := <-clientDone:
		t.Fatalf("broker client exited before child barrier: code %d, error %v", result.code, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("broker worker did not publish child PID barrier")
	}
	cancel()
	select {
	case result := <-clientDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.code != sessionBrokerCanceledExitCode {
			t.Fatalf("canceled exit code = %d, want %d", result.code, sessionBrokerCanceledExitCode)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("broker client did not receive cancellation completion")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("serveSessionBroker() cancellation error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serveSessionBroker() did not stop after cancellation")
	}
	for attempt := 0; attempt < 200; attempt++ {
		stat, err := readLinuxProcessStat(childPID)
		if errorsIsProcessGoneOrTerminal(stat, err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("broker worker child PID %d survived cancellation", childPID)
}

func TestSessionBrokerRequestRejectsMalformedFrames(t *testing.T) {
	valid, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version:    sessionBrokerProtocolVersion,
		DeadlineMS: 1_000,
		Args:       []string{"prime"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "empty", payload: nil},
		{name: "truncated", payload: valid[:len(valid)-1]},
		{name: "trailing data", payload: append(append([]byte(nil), valid...), 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeSessionBrokerRequest(test.payload); err == nil {
				t.Fatal("malformed frame decoded successfully")
			}
		})
	}
}

func TestSessionBrokerRunsWorkerWithPassedStdio(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func(args []string) error {
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdoutReader.Close()
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()

	exitCode, err := runSessionBrokerClientAtFD(clientFD, []string{"-c", "printf broker-ok; exit 7"}, stdin, stdoutWriter, stderr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 7 {
		t.Fatalf("exit code = %d, want 7", exitCode)
	}
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "broker-ok" {
		t.Fatalf("stdout = %q, want broker-ok", output)
	}
}

func TestSessionBrokerRejectsWrongDescriptorCountBeforeValidation(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var validations atomic.Int32
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/false", serverFD, func(args []string) error {
			validations.Add(1)
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	frame, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"unsafe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rights := unix.UnixRights(0, 1, 2)
	if _, err := unix.SendmsgN(clientFD, frame, rights, nil, 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := validations.Load(); got != 0 {
		t.Fatalf("validator calls = %d, want 0", got)
	}
}

func TestSessionBrokerTruncatedRightsDoNotLeakDescriptors(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	defer unix.Close(serverFD)
	defer unix.Close(clientFD)
	frame, err := encodeSessionBrokerRequest(sessionBrokerRequest{
		Version: sessionBrokerProtocolVersion, DeadlineMS: 1_000, Args: []string{"unsafe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.SendmsgN(clientFD, frame, unix.UnixRights(0, 0, 0, 0, 0), nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiveSessionBrokerRequest(serverFD); err == nil {
		t.Fatal("receiveSessionBrokerRequest() accepted truncated rights")
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("open descriptor count after truncated rights = %d, want %d", len(after), len(before))
	}
}

func TestSessionBrokerConcurrencyLimitRejectsBusyRequest(t *testing.T) {
	serverFD, clientFD := newSessionBrokerSocketPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	firstValidated := make(chan struct{}, 1)
	go func() {
		serverDone <- serveSessionBroker(ctx, "/bin/sh", serverFD, func(args []string) error {
			select {
			case firstValidated <- struct{}{}:
			default:
			}
			return nil
		}, 1)
	}()
	t.Cleanup(func() {
		cancel()
		_ = unix.Close(clientFD)
		select {
		case err := <-serverDone:
			if err != nil {
				t.Errorf("serveSessionBroker() cleanup error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serveSessionBroker() did not stop")
		}
	})

	firstResult := make(chan int, 1)
	go func() {
		code, _ := runSessionBrokerClientAtFD(clientFD, []string{"-c", "sleep 0.3"}, os.Stdin, os.Stdout, os.Stderr, time.Second)
		firstResult <- code
	}()
	select {
	case <-firstValidated:
	case <-time.After(time.Second):
		t.Fatal("first request was not validated")
	}
	secondCode, err := runSessionBrokerClientAtFD(clientFD, []string{"-c", "exit 0"}, os.Stdin, os.Stdout, os.Stderr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if secondCode != sessionBrokerBusyExitCode {
		t.Fatalf("busy exit code = %d, want %d", secondCode, sessionBrokerBusyExitCode)
	}
	if code := <-firstResult; code != 0 {
		t.Fatalf("first exit code = %d, want 0", code)
	}
}

func newSessionBrokerSocketPair(t *testing.T) (serverFD, clientFD int) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	return pair[0], pair[1]
}

func repeatedBrokerArguments(count, size int) []string {
	args := make([]string, count)
	for index := range args {
		args[index] = strings.Repeat("x", size)
	}
	return args
}
