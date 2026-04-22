package proxy_test

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/dpivot/dpivot/internal/metrics"
	"github.com/dpivot/dpivot/internal/proxy"
	"go.uber.org/zap"
)

func nopLogger() *zap.Logger { return zap.NewNop() }

// echoServer starts a TCP echo server at a random port and returns its address.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c) //nolint:errcheck
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func dialTimeout(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func newTestServer(t *testing.T) (*proxy.Registry, *proxy.Server) {
	t.Helper()
	reg := proxy.NewRegistry()
	router := proxy.NewRouter(reg)
	srv := proxy.NewServer(router, nopLogger(), metrics.New())
	t.Cleanup(srv.Close)
	return reg, srv
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestServer_Bind_Unbind(t *testing.T) {
	_, srv := newTestServer(t)
	if err := srv.Bind(proxy.PortBinding{ListenPort: 0}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	bindings := srv.Bindings()
	if len(bindings) != 1 {
		t.Fatalf("want 1 binding, got %d", len(bindings))
	}
	port := bindings[0].ListenPort
	if port == 0 {
		t.Error("real port should not be 0")
	}
	if err := srv.Unbind(port); err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	if len(srv.Bindings()) != 0 {
		t.Error("bindings should be empty after unbind")
	}
}

func TestServer_DuplicateBind_ReturnsError(t *testing.T) {
	_, srv := newTestServer(t)
	srv.Bind(proxy.PortBinding{ListenPort: 0}) //nolint:errcheck
	port := srv.Bindings()[0].ListenPort
	err := srv.Bind(proxy.PortBinding{ListenPort: port})
	if err == nil {
		t.Fatal("want error for duplicate bind, got nil")
	}
}

func TestServer_EndToEnd_TCPProxy(t *testing.T) {
	echoAddr := echoServer(t)
	reg, srv := newTestServer(t)

	// Register the echo server as a backend.
	reg.Add(proxy.Backend{ID: "echo", Addr: echoAddr}) //nolint:errcheck

	// Bind a proxy port.
	srv.Bind(proxy.PortBinding{ListenPort: 0}) //nolint:errcheck
	proxyPort := srv.Bindings()[0].ListenPort

	// Connect through the proxy.
	conn := dialTimeout(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
	msg := []byte("hello dpivot\n")
	conn.Write(msg) //nolint:errcheck

	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read from echo: %v", err)
	}
	if string(buf) != string(msg) {
		t.Errorf("echo: got %q, want %q", buf, msg)
	}
}

func TestServer_NoBackend_ConnectionDropped(t *testing.T) {
	_, srv := newTestServer(t) // empty registry
	srv.Bind(proxy.PortBinding{ListenPort: 0}) //nolint:errcheck
	proxyPort := srv.Bindings()[0].ListenPort

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort), time.Second)
	if err != nil {
		return // could not connect — also acceptable
	}
	defer conn.Close()

	// The proxy should drop the connection immediately (no backend).
	conn.SetDeadline(time.Now().Add(time.Second)) //nolint:errcheck
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if n > 0 {
		t.Errorf("expected no data from proxy with no backend, got %d bytes", n)
	}
	if err == nil {
		t.Error("expected read error (connection dropped), got nil")
	}
}

func TestServer_RoundRobin_MultipleBackends(t *testing.T) {
	addr1 := echoServer(t)
	addr2 := echoServer(t)

	reg, srv := newTestServer(t)
	reg.Add(proxy.Backend{ID: "e1", Addr: addr1}) //nolint:errcheck
	reg.Add(proxy.Backend{ID: "e2", Addr: addr2}) //nolint:errcheck
	srv.Bind(proxy.PortBinding{ListenPort: 0})     //nolint:errcheck
	proxyPort := srv.Bindings()[0].ListenPort

	// Make 4 connections; round-robin should alternate.
	for i := 0; i < 4; i++ {
		conn := dialTimeout(t, fmt.Sprintf("127.0.0.1:%d", proxyPort))
		conn.Write([]byte("ping\n")) //nolint:errcheck
		buf := make([]byte, 5)
		conn.SetReadDeadline(time.Now().Add(time.Second)) //nolint:errcheck
		io.ReadFull(conn, buf)                            //nolint:errcheck
	}
	// Verify both backends received requests (request counters > 0).
	all := reg.Backends()
	for _, b := range all {
		if b.Requests() == 0 {
			t.Errorf("backend %s received 0 requests — round-robin may be broken", b.ID)
		}
	}
}
