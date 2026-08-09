//go:build linux

package tmux

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultProxyHeaderTimeout  = 5 * time.Second
	defaultProxyDialTimeout    = 10 * time.Second
	defaultProxyIdleTimeout    = 2 * time.Minute
	defaultProxyTotalTimeout   = 15 * time.Minute
	defaultProxyMaxHeaderBytes = 16 * 1024
	defaultProxyMaxConnections = 32
	defaultProxyMaxTunnelBytes = 64 * 1024 * 1024
)

type linuxCustodyProxySet struct {
	HTTPS net.Listener
}

func (proxies *linuxCustodyProxySet) Close() error {
	if proxies == nil {
		return nil
	}
	var errs []error
	if proxies.HTTPS != nil {
		if err := proxies.HTTPS.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
		proxies.HTTPS = nil
	}
	return errors.Join(errs...)
}

type linuxCustodyProxyPorts struct {
	HTTPS uint16
}

type httpsConnectConfig struct {
	resolve        func(context.Context, string) ([]net.IP, error)
	dial           func(context.Context, string, string) (net.Conn, error)
	headerTimeout  time.Duration
	dialTimeout    time.Duration
	idleTimeout    time.Duration
	totalTimeout   time.Duration
	maxHeaderBytes int
	maxConnections int
	maxTunnelBytes int64
}

var nonPublicHTTPSPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func validatePublicHTTPSDestination(host string, port uint16, resolved []net.IP) error {
	if port != 443 {
		return fmt.Errorf("HTTPS proxy destination port is %d; only 443 is allowed", port)
	}
	if err := validateProxyHostname(host); err != nil {
		return err
	}
	if len(resolved) == 0 {
		return errors.New("HTTPS proxy destination has no resolved addresses")
	}
	for _, raw := range resolved {
		address, ok := netip.AddrFromSlice(raw)
		if !ok {
			return fmt.Errorf("HTTPS proxy destination has invalid address %q", raw)
		}
		if address.Is4In6() {
			return fmt.Errorf("HTTPS proxy destination uses mapped IPv4 address %s", address)
		}
		address = address.Unmap()
		if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
			return fmt.Errorf("HTTPS proxy destination address %s is not public", address)
		}
		for _, prefix := range nonPublicHTTPSPrefixes {
			if prefix.Contains(address) {
				return fmt.Errorf("HTTPS proxy destination address %s is in non-public range %s", address, prefix)
			}
		}
	}
	return nil
}

func validateProxyHostname(host string) error {
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return errors.New("HTTPS proxy destination hostname is empty or too long")
	}
	if strings.HasPrefix(host, "[") || net.ParseIP(host) != nil || strings.ContainsAny(host, ":/%@ 	\r\n") {
		return fmt.Errorf("HTTPS proxy destination %q is not a DNS hostname", host)
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("HTTPS proxy destination hostname %q has an invalid label", host)
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' {
				return fmt.Errorf("HTTPS proxy destination hostname %q contains an invalid character", host)
			}
		}
	}
	return nil
}

func bringUpLinuxCustodyLoopback() error {
	socket, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("opening isolated loopback control socket: %w", err)
	}
	defer unix.Close(socket)
	request, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("creating isolated loopback request: %w", err)
	}
	if err := unix.IoctlIfreq(socket, unix.SIOCGIFFLAGS, request); err != nil {
		return fmt.Errorf("reading isolated loopback flags: %w", err)
	}
	request.SetUint16(request.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(socket, unix.SIOCSIFFLAGS, request); err != nil {
		return fmt.Errorf("enabling isolated loopback: %w", err)
	}
	return nil
}

func createAndSendLinuxCustodyProxyListeners(brokerFD int) (linuxCustodyProxyPorts, error) {
	httpsFD, httpsPort, err := createLinuxCustodyLoopbackListener()
	if err != nil {
		return linuxCustodyProxyPorts{}, err
	}
	defer unix.Close(httpsFD)
	frame := []byte{'P', 'X', 1}
	written, err := unix.SendmsgN(brokerFD, frame, unix.UnixRights(httpsFD), nil, unix.MSG_NOSIGNAL)
	if err != nil {
		return linuxCustodyProxyPorts{}, fmt.Errorf("sending session proxy listeners: %w", err)
	}
	if written != len(frame) {
		return linuxCustodyProxyPorts{}, io.ErrShortWrite
	}
	return linuxCustodyProxyPorts{HTTPS: httpsPort}, nil
}

func createLinuxCustodyLoopbackListener() (int, uint16, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, 0, fmt.Errorf("creating isolated proxy listener: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return -1, 0, fmt.Errorf("configuring isolated proxy listener: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		return -1, 0, fmt.Errorf("binding isolated proxy listener: %w", err)
	}
	if err := unix.Listen(fd, 32); err != nil {
		return -1, 0, fmt.Errorf("listening on isolated proxy socket: %w", err)
	}
	address, err := unix.Getsockname(fd)
	if err != nil {
		return -1, 0, fmt.Errorf("reading isolated proxy address: %w", err)
	}
	ipv4, ok := address.(*unix.SockaddrInet4)
	if !ok || ipv4.Addr != [4]byte{127, 0, 0, 1} || ipv4.Port < 1 || ipv4.Port > 65535 {
		return -1, 0, fmt.Errorf("isolated proxy bound unexpected address %v", address)
	}
	closeOnError = false
	return fd, uint16(ipv4.Port), nil
}

func receiveLinuxCustodyProxySet(brokerFD int) (_ linuxCustodyProxySet, retErr error) {
	frame := make([]byte, 3)
	rightsBuffer := make([]byte, unix.CmsgSpace(4))
	frameBytes, rightsBytes, flags, _, err := unix.Recvmsg(brokerFD, frame, rightsBuffer, unix.MSG_CMSG_CLOEXEC)
	if err != nil {
		return linuxCustodyProxySet{}, fmt.Errorf("receiving session proxy listeners: %w", err)
	}
	descriptors, err := parseSessionBrokerDescriptors(rightsBuffer[:rightsBytes])
	if err != nil {
		return linuxCustodyProxySet{}, err
	}
	defer func() {
		if retErr != nil {
			closeSessionBrokerDescriptors(descriptors)
		}
	}()
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || frameBytes != len(frame) || !bytes.Equal(frame, []byte{'P', 'X', 1}) {
		return linuxCustodyProxySet{}, errors.New("session proxy descriptor frame was invalid or truncated")
	}
	if len(descriptors) != 1 {
		return linuxCustodyProxySet{}, fmt.Errorf("session proxy descriptor frame contained %d descriptors; want 1", len(descriptors))
	}
	listeners := make([]net.Listener, 0, 1)
	for index, fd := range descriptors {
		if err := validateLinuxCustodyProxyListenerFD(fd); err != nil {
			for _, listener := range listeners {
				_ = listener.Close()
			}
			return linuxCustodyProxySet{}, fmt.Errorf("validating session proxy listener %d: %w", index, err)
		}
		file := os.NewFile(uintptr(fd), fmt.Sprintf("session-proxy-listener-%d", index))
		listener, err := net.FileListener(file)
		_ = file.Close()
		descriptors[index] = -1
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return linuxCustodyProxySet{}, fmt.Errorf("retaining session proxy listener %d: %w", index, err)
		}
		listeners = append(listeners, listener)
	}
	return linuxCustodyProxySet{HTTPS: listeners[0]}, nil
}

func waitLinuxCustodyProxySet(launch *linuxCustodyLaunch, timeout time.Duration) error {
	if launch == nil || launch.broker == nil {
		return errors.New("session custody proxy broker is unavailable")
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("timed out waiting for session proxy descriptors")
		}
		milliseconds := int(remaining / time.Millisecond)
		if milliseconds < 1 {
			milliseconds = 1
		}
		poll := []unix.PollFd{{Fd: int32(launch.broker.Fd()), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
		count, err := unix.Poll(poll, milliseconds)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("waiting for session proxy descriptors: %w", err)
		}
		if count == 0 {
			continue
		}
		proxies, err := receiveLinuxCustodyProxySet(int(launch.broker.Fd()))
		if err != nil {
			return err
		}
		launch.proxies = proxies
		return nil
	}
}

func validateLinuxCustodyProxyListenerFD(fd int) error {
	socketType, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil {
		return fmt.Errorf("reading proxy descriptor socket type: %w", err)
	}
	if socketType != unix.SOCK_STREAM {
		return fmt.Errorf("proxy descriptor socket type is %d; want SOCK_STREAM", socketType)
	}
	accepting, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
	if err != nil {
		return fmt.Errorf("reading proxy descriptor listening state: %w", err)
	}
	if accepting != 1 {
		return fmt.Errorf("proxy descriptor listening state is %d; want 1", accepting)
	}
	address, err := unix.Getsockname(fd)
	if err != nil {
		return err
	}
	ipv4, ok := address.(*unix.SockaddrInet4)
	if !ok || ipv4.Addr != [4]byte{127, 0, 0, 1} || ipv4.Port < 1 {
		return fmt.Errorf("proxy descriptor has unexpected address %v", address)
	}
	return nil
}

func rewriteLinuxCustodyNetworkEnvironment(env []string, ports linuxCustodyProxyPorts) []string {
	filtered := withoutEnvironmentKeys(
		env,
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
		"GT_DOLT_HOST", "GT_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT", "BEADS_DOLT_AUTO_START",
	)
	proxyURL := "http://127.0.0.1:" + strconv.Itoa(int(ports.HTTPS))
	return append(filtered,
		"HTTPS_PROXY="+proxyURL,
		"https_proxy="+proxyURL,
		"HTTP_PROXY="+proxyURL,
		"http_proxy="+proxyURL,
		"ALL_PROXY="+proxyURL,
		"all_proxy="+proxyURL,
		"NO_PROXY=",
		"no_proxy=",
		"GT_DOLT_HOST=127.0.0.1",
		"GT_DOLT_PORT=1",
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BEADS_DOLT_SERVER_PORT=1",
		"BEADS_DOLT_PORT=1",
		"BEADS_DOLT_AUTO_START=0",
	)
}

func serveHTTPSConnect(ctx context.Context, listener net.Listener) error {
	resolver := net.DefaultResolver
	dialer := &net.Dialer{}
	return serveHTTPSConnectWithConfig(ctx, listener, httpsConnectConfig{
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			resolved, err := resolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			return normalizeHTTPSResolverAddresses(resolved), nil
		},
		dial:           dialer.DialContext,
		headerTimeout:  defaultProxyHeaderTimeout,
		dialTimeout:    defaultProxyDialTimeout,
		idleTimeout:    defaultProxyIdleTimeout,
		totalTimeout:   defaultProxyTotalTimeout,
		maxHeaderBytes: defaultProxyMaxHeaderBytes,
		maxConnections: defaultProxyMaxConnections,
		maxTunnelBytes: defaultProxyMaxTunnelBytes,
	})
}

func normalizeHTTPSResolverAddresses(resolved []netip.Addr) []net.IP {
	answers := make([]net.IP, 0, len(resolved))
	for _, address := range resolved {
		// Linux's resolver may represent an ordinary A record as ::ffff:a.b.c.d.
		// Normalize that resolver artifact before strict destination validation;
		// validatePublicHTTPSDestination still rejects mapped inputs supplied by
		// any other boundary.
		answers = append(answers, net.IP(address.Unmap().AsSlice()))
	}
	return answers
}

func serveHTTPSConnectWithConfig(ctx context.Context, listener net.Listener, config httpsConnectConfig) error {
	if config.resolve == nil || config.dial == nil {
		return errors.New("HTTPS proxy resolver and dialer are required")
	}
	if config.headerTimeout <= 0 || config.dialTimeout <= 0 || config.idleTimeout <= 0 || config.totalTimeout <= 0 {
		return errors.New("HTTPS proxy timeouts must be positive")
	}
	if config.maxHeaderBytes < 1 || config.maxConnections < 1 || config.maxTunnelBytes < 1 {
		return errors.New("HTTPS proxy limits must be positive")
	}
	return serveBoundedProxy(ctx, listener, config.maxConnections, func(serverContext context.Context, client net.Conn) {
		handleHTTPSConnect(serverContext, client, config)
	})
}

func handleHTTPSConnect(serverContext context.Context, client net.Conn, config httpsConnectConfig) {
	defer client.Close()
	connectionContext, cancel := context.WithTimeout(serverContext, config.totalTimeout)
	defer cancel()
	_ = client.SetReadDeadline(time.Now().Add(config.headerTimeout))
	request, clientReader, err := readBoundedConnectRequest(client, config.maxHeaderBytes)
	if err != nil {
		writeHTTPProxyFailure(client, http.StatusBadRequest)
		return
	}
	if request.Method != http.MethodConnect {
		writeHTTPProxyFailure(client, http.StatusMethodNotAllowed)
		return
	}
	host, portText, err := net.SplitHostPort(request.RequestURI)
	if err != nil || request.URL.Scheme != "" || request.URL.User != nil {
		writeHTTPProxyFailure(client, http.StatusForbidden)
		return
	}
	portValue, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || portValue == 0 {
		writeHTTPProxyFailure(client, http.StatusForbidden)
		return
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if uint16(portValue) != 443 || validateProxyHostname(host) != nil {
		writeHTTPProxyFailure(client, http.StatusForbidden)
		return
	}
	dialContext, cancelDial := context.WithTimeout(connectionContext, config.dialTimeout)
	resolved, err := config.resolve(dialContext, host)
	if err != nil || validatePublicHTTPSDestination(host, uint16(portValue), resolved) != nil {
		cancelDial()
		writeHTTPProxyFailure(client, http.StatusForbidden)
		return
	}
	upstream, err := dialValidatedHTTPSPeer(dialContext, config.dial, resolved)
	cancelDial()
	if err != nil {
		writeHTTPProxyFailure(client, http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = client.SetDeadline(time.Time{})
	relayProxyConnections(connectionContext, client, upstream, clientReader, config.idleTimeout, config.maxTunnelBytes)
}

func readBoundedConnectRequest(connection net.Conn, maxBytes int) (*http.Request, *bufio.Reader, error) {
	reader := bufio.NewReaderSize(connection, min(maxBytes, 4*1024))
	var header bytes.Buffer
	for {
		line, err := reader.ReadString('\n')
		if header.Len()+len(line) > maxBytes {
			return nil, reader, errors.New("proxy request header exceeds limit")
		}
		header.WriteString(line)
		if err != nil {
			return nil, reader, err
		}
		if !strings.HasSuffix(line, "\r\n") {
			return nil, reader, errors.New("proxy request header requires CRLF framing")
		}
		if bytes.HasSuffix(header.Bytes(), []byte("\r\n\r\n")) {
			break
		}
	}
	request, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(header.Bytes())))
	if err != nil {
		return nil, reader, err
	}
	defer request.Body.Close()
	if request.ContentLength > 0 || len(request.TransferEncoding) != 0 {
		return nil, reader, errors.New("proxy CONNECT request has a body")
	}
	return request, reader, nil
}

func dialValidatedHTTPSPeer(
	ctx context.Context,
	dial func(context.Context, string, string) (net.Conn, error),
	resolved []net.IP,
) (net.Conn, error) {
	var errs []error
	for _, address := range resolved {
		connection, err := dial(ctx, "tcp", net.JoinHostPort(address.String(), "443"))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !validatedHTTPSRemote(connection.RemoteAddr(), resolved) {
			_ = connection.Close()
			errs = append(errs, errors.New("HTTPS proxy peer differed from validated DNS answers"))
			continue
		}
		return connection, nil
	}
	return nil, errors.Join(errs...)
}

func validatedHTTPSRemote(remote net.Addr, resolved []net.IP) bool {
	tcpAddress, ok := remote.(*net.TCPAddr)
	if !ok || tcpAddress.Port != 443 || tcpAddress.IP == nil {
		return false
	}
	for _, expected := range resolved {
		if tcpAddress.IP.Equal(expected) {
			return true
		}
	}
	return false
}

func writeHTTPProxyFailure(connection net.Conn, status int) {
	_ = connection.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = fmt.Fprintf(connection, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
}

func serveBoundedProxy(
	ctx context.Context,
	listener net.Listener,
	maxConnections int,
	handle func(context.Context, net.Conn),
) error {
	if listener == nil {
		return errors.New("proxy listener is unavailable")
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	defer close(stop)
	workers := make(chan struct{}, maxConnections)
	var workerGroup sync.WaitGroup
	defer workerGroup.Wait()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var temporary interface{ Temporary() bool }
			if errors.As(err, &temporary) && temporary.Temporary() {
				continue
			}
			return err
		}
		select {
		case workers <- struct{}{}:
			workerGroup.Add(1)
			go func() {
				defer workerGroup.Done()
				defer func() { <-workers }()
				handle(ctx, connection)
			}()
		default:
			_ = connection.Close()
		}
	}
}

type proxyActivityReader struct {
	io.Reader
	touch func()
}

func (reader proxyActivityReader) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	if count > 0 {
		reader.touch()
	}
	return count, err
}

type proxyActivityWriter struct {
	io.Writer
	touch func()
}

func (writer proxyActivityWriter) Write(buffer []byte) (int, error) {
	count, err := writer.Writer.Write(buffer)
	if count > 0 {
		writer.touch()
	}
	return count, err
}

func relayProxyConnections(
	ctx context.Context,
	client net.Conn,
	upstream net.Conn,
	clientReader io.Reader,
	idleTimeout time.Duration,
	maxBytes int64,
) {
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	touch := func() { lastActivity.Store(time.Now().UnixNano()) }
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(proxyActivityWriter{Writer: upstream, touch: touch}, proxyActivityReader{Reader: io.LimitReader(clientReader, maxBytes), touch: touch})
		closeProxyWrite(upstream)
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(proxyActivityWriter{Writer: client, touch: touch}, proxyActivityReader{Reader: io.LimitReader(upstream, maxBytes), touch: touch})
		closeProxyWrite(client)
		copyDone <- struct{}{}
	}()
	tickerInterval := idleTimeout / 2
	if tickerInterval < time.Millisecond {
		tickerInterval = time.Millisecond
	}
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()
	completed := 0
	for completed < 2 {
		select {
		case <-copyDone:
			completed++
		case <-ctx.Done():
			_ = client.Close()
			_ = upstream.Close()
			return
		case <-ticker.C:
			last := time.Unix(0, lastActivity.Load())
			if time.Since(last) >= idleTimeout {
				_ = client.Close()
				_ = upstream.Close()
				return
			}
		}
	}
}

func closeProxyWrite(connection net.Conn) {
	if halfCloser, ok := connection.(interface{ CloseWrite() error }); ok {
		_ = halfCloser.CloseWrite()
		return
	}
	_ = connection.Close()
}
