//go:build linux

package tmux

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestValidatePublicHTTPSDestination(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		port     uint16
		resolved []string
		wantErr  bool
	}{
		{name: "public ipv4", host: "example.com", port: 443, resolved: []string{"8.8.8.8"}},
		{name: "public ipv6", host: "example.com", port: 443, resolved: []string{"2606:4700:4700::1111"}},
		{name: "multiple public", host: "example.com", port: 443, resolved: []string{"8.8.8.8", "1.1.1.1"}},
		{name: "ip literal", host: "8.8.8.8", port: 443, resolved: []string{"8.8.8.8"}, wantErr: true},
		{name: "bracketed ip literal", host: "[2606:4700:4700::1111]", port: 443, resolved: []string{"2606:4700:4700::1111"}, wantErr: true},
		{name: "non 443", host: "example.com", port: 80, resolved: []string{"8.8.8.8"}, wantErr: true},
		{name: "empty answers", host: "example.com", port: 443, wantErr: true},
		{name: "loopback ipv4", host: "example.com", port: 443, resolved: []string{"127.0.0.1"}, wantErr: true},
		{name: "loopback ipv6", host: "example.com", port: 443, resolved: []string{"::1"}, wantErr: true},
		{name: "private ipv4 10", host: "example.com", port: 443, resolved: []string{"10.0.0.1"}, wantErr: true},
		{name: "private ipv4 172", host: "example.com", port: 443, resolved: []string{"172.16.0.1"}, wantErr: true},
		{name: "private ipv4 192", host: "example.com", port: 443, resolved: []string{"192.168.0.1"}, wantErr: true},
		{name: "private ipv6", host: "example.com", port: 443, resolved: []string{"fd00::1"}, wantErr: true},
		{name: "carrier nat", host: "example.com", port: 443, resolved: []string{"100.64.0.1"}, wantErr: true},
		{name: "link local ipv4", host: "example.com", port: 443, resolved: []string{"169.254.1.1"}, wantErr: true},
		{name: "link local ipv6", host: "example.com", port: 443, resolved: []string{"fe80::1"}, wantErr: true},
		{name: "multicast ipv4", host: "example.com", port: 443, resolved: []string{"224.0.0.1"}, wantErr: true},
		{name: "multicast ipv6", host: "example.com", port: 443, resolved: []string{"ff02::1"}, wantErr: true},
		{name: "documentation ipv4 a", host: "example.com", port: 443, resolved: []string{"192.0.2.1"}, wantErr: true},
		{name: "documentation ipv4 b", host: "example.com", port: 443, resolved: []string{"198.51.100.1"}, wantErr: true},
		{name: "documentation ipv4 c", host: "example.com", port: 443, resolved: []string{"203.0.113.1"}, wantErr: true},
		{name: "documentation ipv6", host: "example.com", port: 443, resolved: []string{"2001:db8::1"}, wantErr: true},
		{name: "unspecified ipv4", host: "example.com", port: 443, resolved: []string{"0.0.0.0"}, wantErr: true},
		{name: "unspecified ipv6", host: "example.com", port: 443, resolved: []string{"::"}, wantErr: true},
		{name: "mapped public ipv4", host: "example.com", port: 443, resolved: []string{"::ffff:8.8.8.8"}, wantErr: true},
		{name: "mixed public private", host: "example.com", port: 443, resolved: []string{"8.8.8.8", "10.0.0.1"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved := make([]net.IP, 0, len(test.resolved))
			for _, raw := range test.resolved {
				address := net.ParseIP(raw)
				if address != nil && !strings.Contains(raw, ":") {
					address = address.To4()
				}
				resolved = append(resolved, address)
			}
			err := validatePublicHTTPSDestination(test.host, test.port, resolved)
			if (err != nil) != test.wantErr {
				t.Fatalf("validatePublicHTTPSDestination(%q, %d, %v) error = %v, wantErr %v", test.host, test.port, test.resolved, err, test.wantErr)
			}
		})
	}
}

func TestCaptureConfiguredDoltEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		env      []string
		wantHost string
		wantPort uint16
		wantErr  bool
	}{
		{name: "defaults", wantHost: "127.0.0.1", wantPort: 3307},
		{name: "gt canonical overrides stale beads", env: []string{"GT_DOLT_HOST=127.0.0.2", "GT_DOLT_PORT=5507", "BEADS_DOLT_SERVER_HOST=stale", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9998"}, wantHost: "127.0.0.2", wantPort: 5507},
		{name: "beads server fallback", env: []string{"BEADS_DOLT_SERVER_HOST=dolt.internal", "BEADS_DOLT_SERVER_PORT=4407"}, wantHost: "dolt.internal", wantPort: 4407},
		{name: "beads legacy fallback", env: []string{"BEADS_DOLT_PORT=4307"}, wantHost: "127.0.0.1", wantPort: 4307},
		{name: "invalid zero", env: []string{"GT_DOLT_PORT=0"}, wantErr: true},
		{name: "invalid range", env: []string{"GT_DOLT_PORT=65536"}, wantErr: true},
		{name: "invalid text", env: []string{"GT_DOLT_PORT=not-a-port"}, wantErr: true},
		{name: "invalid host scheme", env: []string{"GT_DOLT_HOST=http://127.0.0.1", "GT_DOLT_PORT=3307"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host, port, err := captureConfiguredDoltEndpoint(test.env)
			if (err != nil) != test.wantErr {
				t.Fatalf("captureConfiguredDoltEndpoint(%v) error = %v, wantErr %v", test.env, err, test.wantErr)
			}
			if err == nil && (host != test.wantHost || port != test.wantPort) {
				t.Fatalf("captureConfiguredDoltEndpoint(%v) = %s:%d, want %s:%d", test.env, host, port, test.wantHost, test.wantPort)
			}
		})
	}
}

type proxyRemoteAddrConn struct {
	net.Conn
	remote net.Addr
}

func (connection *proxyRemoteAddrConn) RemoteAddr() net.Addr { return connection.remote }

func TestServeHTTPSConnectPreservesTLSIdentity(t *testing.T) {
	fixture, roots := newHTTPSProxyFixture(t, "fixture.test", "fixture-identity-ok")
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = proxy.Close() })

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveHTTPSConnectWithConfig(ctx, proxy, httpsConnectConfig{
			resolve: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("8.8.8.8").To4()}, nil
			},
			dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "8.8.8.8:443" {
					return nil, fmt.Errorf("dialed %q; want exact validated address", address)
				}
				connection, err := (&net.Dialer{}).DialContext(ctx, network, fixture)
				if err != nil {
					return nil, err
				}
				return &proxyRemoteAddrConn{Conn: connection, remote: &net.TCPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 443}}, nil
			},
			headerTimeout:  time.Second,
			dialTimeout:    time.Second,
			idleTimeout:    time.Second,
			totalTimeout:   5 * time.Second,
			maxHeaderBytes: 4 * 1024,
			maxConnections: 4,
		})
	}()

	proxyURL, err := url.Parse("http://" + proxy.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
		Timeout: 3 * time.Second,
	}
	response, err := client.Get("https://fixture.test/identity")
	if err != nil {
		t.Fatalf("HTTPS through proxy failed certificate identity verification: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "fixture-identity-ok" {
		t.Fatalf("body = %q, want fixture identity", got)
	}
	wrongIdentity, err := client.Get("https://other.test/identity")
	if err == nil {
		_ = wrongIdentity.Body.Close()
		t.Fatal("HTTPS client accepted a certificate for the wrong destination identity")
	}

	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("proxy shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not stop after cancellation")
	}
}

func TestServeHTTPSConnectRejectsReboundPeer(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = serveHTTPSConnectWithConfig(ctx, proxy, httpsConnectConfig{
			resolve: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("8.8.8.8").To4()}, nil
			},
			dial: func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				go server.Close()
				return &proxyRemoteAddrConn{Conn: client, remote: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}}, nil
			},
			headerTimeout: time.Second, dialTimeout: time.Second, idleTimeout: time.Second,
			totalTimeout: time.Second, maxHeaderBytes: 4 * 1024, maxConnections: 2,
		})
	}()
	connection, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = io.WriteString(connection, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	status, readErr := bufio.NewReader(connection).ReadString('\n')
	if readErr == nil && strings.Contains(status, " 200 ") {
		t.Fatalf("rebound peer received success response %q", status)
	}
}

func TestServeHTTPSConnectClosesIdleTunnel(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstreamPeers := make(chan net.Conn, 1)
	go func() {
		_ = serveHTTPSConnectWithConfig(ctx, proxy, httpsConnectConfig{
			resolve: func(context.Context, string) ([]net.IP, error) {
				return []net.IP{net.ParseIP("8.8.8.8").To4()}, nil
			},
			dial: func(context.Context, string, string) (net.Conn, error) {
				client, server := net.Pipe()
				upstreamPeers <- server
				return &proxyRemoteAddrConn{Conn: client, remote: &net.TCPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 443}}, nil
			},
			headerTimeout: time.Second, dialTimeout: time.Second, idleTimeout: 40 * time.Millisecond,
			totalTimeout: time.Second, maxHeaderBytes: 4 * 1024, maxConnections: 2,
		})
	}()
	connection, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _ = io.WriteString(connection, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, " 200 ") {
		t.Fatalf("CONNECT status = %q, error %v", status, err)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		if line == "\r\n" {
			break
		}
	}
	peer := <-upstreamPeers
	defer peer.Close()
	_ = connection.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buffer := make([]byte, 1)
	if _, err := reader.Read(buffer); err == nil {
		t.Fatal("idle tunnel remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatalf("idle tunnel hit client deadline instead of proxy idle timeout: %v", err)
	}
}

func TestServeHTTPSConnectBoundsHeadersAndConnections(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	go func() {
		_ = serveHTTPSConnectWithConfig(ctx, proxy, httpsConnectConfig{
			resolve: func(ctx context.Context, _ string) ([]net.IP, error) {
				if calls.Add(1) == 1 {
					close(entered)
				}
				select {
				case <-release:
					return nil, context.Canceled
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
			dial:          func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unexpected dial") },
			headerTimeout: time.Second, dialTimeout: time.Second, idleTimeout: time.Second,
			totalTimeout: 2 * time.Second, maxHeaderBytes: 256, maxConnections: 1,
		})
	}()
	first, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_, _ = io.WriteString(first, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first connection did not occupy worker")
	}

	second, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(second, "CONNECT example.com:443 HTTP/1.1\r\n\r\n")
	_ = second.SetReadDeadline(time.Now().Add(time.Second))
	if data, readErr := io.ReadAll(second); len(data) != 0 || readErr != nil && !errors.Is(readErr, syscall.ECONNRESET) {
		t.Fatalf("connection flood was not closed cleanly: data %q, error %v", data, readErr)
	}
	_ = second.Close()
	close(release)
}

func TestServeHTTPSConnectRejectsOversizedHeaderBeforeResolution(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var resolutions atomic.Int32
	go func() {
		_ = serveHTTPSConnectWithConfig(ctx, proxy, httpsConnectConfig{
			resolve: func(context.Context, string) ([]net.IP, error) {
				resolutions.Add(1)
				return []net.IP{net.ParseIP("8.8.8.8").To4()}, nil
			},
			dial: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("unexpected dial")
			},
			headerTimeout: time.Second, dialTimeout: time.Second, idleTimeout: time.Second,
			totalTimeout: 2 * time.Second, maxHeaderBytes: 256, maxConnections: 1,
		})
	}()
	oversized, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer oversized.Close()
	_, _ = io.WriteString(oversized, "CONNECT example.com:443 HTTP/1.1\r\nX-Fill: "+strings.Repeat("x", 512)+"\r\n\r\n")
	_ = oversized.SetReadDeadline(time.Now().Add(time.Second))
	status, readErr := bufio.NewReader(oversized).ReadString('\n')
	if readErr != nil || !strings.Contains(status, " 400 ") {
		t.Fatalf("oversized header status = %q, error %v", status, readErr)
	}
	if got := resolutions.Load(); got != 0 {
		t.Fatalf("oversized header triggered %d DNS resolutions", got)
	}
}

func TestServeDoltProxyUsesOnlyCapturedEndpoint(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		connection, acceptErr := upstream.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = serveDoltProxy(ctx, proxy, upstream.Addr().String()) }()
	connection, err := net.DialTimeout("tcp", proxy.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(time.Second))
	if _, err := connection.Write([]byte("dolt-exact-endpoint")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("dolt-exact-endpoint"))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatal(err)
	}
	if got := string(response); got != "dolt-exact-endpoint" {
		t.Fatalf("echo = %q", got)
	}
}

func TestLinuxCustodyProxyDescriptorHandoff(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[0])
	defer unix.Close(pair[1])
	ports, err := createAndSendLinuxCustodyProxyListeners(pair[0])
	if err != nil {
		t.Fatal(err)
	}
	listeners, err := receiveLinuxCustodyProxySet(pair[1])
	if err != nil {
		t.Fatal(err)
	}
	defer listeners.Close()
	for _, endpoint := range []struct {
		name     string
		listener net.Listener
		port     uint16
	}{{"HTTPS", listeners.HTTPS, ports.HTTPS}, {"Dolt", listeners.Dolt, ports.Dolt}} {
		t.Run(endpoint.name, func(t *testing.T) {
			address, ok := endpoint.listener.Addr().(*net.TCPAddr)
			if !ok || !address.IP.IsLoopback() || address.Port != int(endpoint.port) {
				t.Fatalf("listener address = %v, want isolated loopback port %d", endpoint.listener.Addr(), endpoint.port)
			}
			accepted := make(chan error, 1)
			go func() {
				connection, acceptErr := endpoint.listener.Accept()
				if acceptErr == nil {
					_ = connection.Close()
				}
				accepted <- acceptErr
			}()
			connection, dialErr := net.DialTimeout("tcp", endpoint.listener.Addr().String(), time.Second)
			if dialErr != nil {
				t.Fatal(dialErr)
			}
			_ = connection.Close()
			if acceptErr := <-accepted; acceptErr != nil {
				t.Fatal(acceptErr)
			}
		})
	}
}

func TestLinuxCustodyProxyTruncatedRightsDoNotLeakDescriptors(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[0])
	defer unix.Close(pair[1])
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.SendmsgN(pair[0], []byte{'P', 'X', 1}, unix.UnixRights(0, 0, 0), nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := receiveLinuxCustodyProxySet(pair[1]); err == nil {
		t.Fatal("receiveLinuxCustodyProxySet() accepted truncated descriptor rights")
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("open descriptor count after truncated proxy rights = %d, want %d", len(after), len(before))
	}
}

func TestRewriteLinuxCustodyNetworkEnvironment(t *testing.T) {
	environment := rewriteLinuxCustodyNetworkEnvironment([]string{
		"KEEP=value",
		"HTTPS_PROXY=http://stale",
		"https_proxy=http://stale",
		"HTTP_PROXY=http://stale",
		"ALL_PROXY=socks5://stale",
		"NO_PROXY=*",
		"GT_DOLT_HOST=host-side-secret",
		"GT_DOLT_PORT=33327",
		"BEADS_DOLT_SERVER_HOST=stale",
		"BEADS_DOLT_SERVER_PORT=9999",
		"BEADS_DOLT_PORT=9998",
	}, linuxCustodyProxyPorts{HTTPS: 41001, Dolt: 41002})
	values := make(map[string]string)
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if found {
			values[key] = value
		}
	}
	if values["KEEP"] != "value" {
		t.Fatalf("unrelated environment was lost: %v", values)
	}
	proxyURL := "http://127.0.0.1:41001"
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if values[key] != proxyURL {
			t.Fatalf("%s = %q, want %q", key, values[key], proxyURL)
		}
	}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		if values[key] != "" {
			t.Fatalf("%s = %q, want empty", key, values[key])
		}
	}
	for _, key := range []string{"GT_DOLT_HOST", "BEADS_DOLT_SERVER_HOST"} {
		if values[key] != "127.0.0.1" {
			t.Fatalf("%s = %q, want isolated loopback", key, values[key])
		}
	}
	for _, key := range []string{"GT_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"} {
		if values[key] != strconv.Itoa(41002) {
			t.Fatalf("%s = %q, want isolated Dolt port", key, values[key])
		}
	}
	if strings.Contains(strings.Join(environment, "\n"), "host-side-secret") || strings.Contains(strings.Join(environment, "\n"), "33327") {
		t.Fatalf("captured upstream leaked into workload environment: %v", environment)
	}
}

func newHTTPSProxyFixture(t *testing.T, hostname, body string) (string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	server := &http.Server{
		ErrorLog: log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			_, _ = io.WriteString(response, body)
		}),
	}
	go func() { _ = server.Serve(tlsListener) }()
	t.Cleanup(func() { _ = server.Close() })
	roots := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots.AddCert(parsed)
	return listener.Addr().String(), roots
}
