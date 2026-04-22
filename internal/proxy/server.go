package proxy

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/dpivot/dpivot/internal/metrics"
	"go.uber.org/zap"
)

// dialTimeout is the maximum time allowed to establish an upstream connection.
const dialTimeout = 5 * time.Second

var dialer = &net.Dialer{
	Timeout:   dialTimeout,
	KeepAlive: 30 * time.Second,
}

// PortBinding describes one host port the proxy should own.
type PortBinding struct {
	// ListenPort is the host-side port (left side of the Compose port mapping).
	ListenPort int

	// TargetPort is the port on the backend container to dial when
	// backend.Addr does not include an explicit port override.
	// When 0, the router is expected to provide the full addr including port.
	TargetPort int
}

type portListener struct {
	binding  PortBinding
	listener net.Listener
	done     chan struct{}
}

// Server owns one TCP listener per PortBinding and routes accepted connections
// to backends chosen by the Router.
//
// All public methods are safe for concurrent use.
type Server struct {
	router  *Router
	log     *zap.Logger
	metrics *metrics.Proxy

	mu          sync.RWMutex
	listeners   map[int]*portListener // real listen port → listener
	activeConns sync.WaitGroup        // tracks in-flight connections for graceful drain
}

// NewServer creates a proxy server backed by the given router and metrics.
func NewServer(router *Router, log *zap.Logger, m *metrics.Proxy) *Server {
	return &Server{
		router:    router,
		log:       log,
		metrics:   m,
		listeners: make(map[int]*portListener),
	}
}

// Bind opens a TCP listener for the given PortBinding. Accepts connections
// asynchronously; returns once the listener is open.
//
// If ListenPort is 0, the OS assigns a free port. The real assigned port is
// stored in the map key and in the returned Bindings() snapshot.
func (s *Server) Bind(b PortBinding) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", b.ListenPort))
	if err != nil {
		return fmt.Errorf("proxy: listen :%d: %w", b.ListenPort, err)
	}

	realPort := ln.Addr().(*net.TCPAddr).Port
	b.ListenPort = realPort

	// Guard against duplicate Bind on the same port.
	if _, exists := s.listeners[realPort]; exists {
		ln.Close()
		return fmt.Errorf("proxy: port %d already bound", realPort)
	}

	pl := &portListener{binding: b, listener: ln, done: make(chan struct{})}
	s.listeners[realPort] = pl
	go s.acceptLoop(pl)

	s.log.Info("proxy: port bound", zap.Int("port", realPort))
	return nil
}

// Unbind stops the listener on listenPort. In-flight connections run to
// completion; only new accept() calls are rejected.
func (s *Server) Unbind(listenPort int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pl, exists := s.listeners[listenPort]
	if !exists {
		return fmt.Errorf("proxy: port %d not bound", listenPort)
	}
	close(pl.done)
	pl.listener.Close()
	delete(s.listeners, listenPort)
	s.log.Info("proxy: port unbound", zap.Int("port", listenPort))
	return nil
}

// Close shuts down all active listeners immediately.
// In-flight connections continue until they complete naturally; use
// CloseGraceful to wait for them on SIGTERM.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for port, pl := range s.listeners {
		close(pl.done)
		pl.listener.Close()
		delete(s.listeners, port)
	}
	s.log.Info("proxy: all ports closed")
}

// CloseGraceful stops accepting new connections, then waits up to timeout for
// all in-flight connections to complete. Returns an error on timeout so the
// caller can decide whether to force-close or log the situation.
//
// Typical SIGTERM handler:
//
//	if err := srv.CloseGraceful(30 * time.Second); err != nil {
//	    log.Warn("drain timeout — forcing close", zap.Error(err))
//	    srv.Close()
//	}
func (s *Server) CloseGraceful(timeout time.Duration) error {
	s.mu.Lock()
	for port, pl := range s.listeners {
		close(pl.done)
		pl.listener.Close()
		delete(s.listeners, port)
	}
	s.mu.Unlock()

	s.log.Info("proxy: listeners closed — waiting for in-flight connections",
		zap.Duration("timeout", timeout))

	done := make(chan struct{})
	go func() {
		s.activeConns.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.log.Info("proxy: all connections drained")
		return nil
	case <-time.After(timeout):
		s.log.Warn("proxy: graceful drain timed out — connections may be interrupted",
			zap.Duration("timeout", timeout))
		return fmt.Errorf("proxy: drain timeout (%s)", timeout)
	}
}

// Bindings returns a snapshot of currently active PortBindings.
// Each ListenPort reflects the real OS-assigned port (never 0).
func (s *Server) Bindings() []PortBinding {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]PortBinding, 0, len(s.listeners))
	for _, pl := range s.listeners {
		out = append(out, pl.binding)
	}
	return out
}

// ── Accept loop ───────────────────────────────────────────────────────────────

func (s *Server) acceptLoop(pl *portListener) {
	for {
		conn, err := pl.listener.Accept()
		if err != nil {
			select {
			case <-pl.done:
				return // clean shutdown
			default:
				s.log.Warn("proxy: accept error",
					zap.Int("port", pl.binding.ListenPort),
					zap.Error(err))
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}
		// Add(1) before the goroutine so CloseGraceful's Wait() always sees it.
		s.activeConns.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(client net.Conn) {
	s.metrics.ConnStart()
	defer func() {
		s.metrics.ConnEnd()
		s.activeConns.Done()
		client.Close()
	}()

	backend, err := s.router.Next()
	if err != nil {
		s.metrics.ConnFailed()
		s.log.Warn("proxy: no backend — dropping connection",
			zap.String("client", client.RemoteAddr().String()),
			zap.Error(err))
		return
	}

	upstream, err := dialer.Dial("tcp", backend.Addr)
	if err != nil {
		s.metrics.ConnFailed()
		s.log.Warn("proxy: dial backend failed",
			zap.String("addr", backend.Addr),
			zap.Error(err))
		return
	}
	defer upstream.Close()

	s.log.Debug("proxy: connection established",
		zap.String("client", client.RemoteAddr().String()),
		zap.String("upstream", backend.Addr),
		zap.String("backend_id", backend.ID))

	pipe(client, upstream)
}

// ── Bidirectional pipe ────────────────────────────────────────────────────────

func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, a) //nolint:errcheck
		closeWrite(b)
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, b) //nolint:errcheck
		closeWrite(a)
	}()
	wg.Wait()
}

func closeWrite(conn net.Conn) {
	type halfCloser interface {
		CloseWrite() error
	}
	if hc, ok := conn.(halfCloser); ok {
		hc.CloseWrite() //nolint:errcheck
	} else {
		conn.Close() //nolint:errcheck
	}
}
