// Package metrics provides atomic proxy counters exported as Prometheus text.
// No external dependencies — uses stdlib only.
package metrics

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Proxy holds per-proxy counters. All fields are safe for concurrent access.
type Proxy struct {
	TotalConns  atomic.Uint64 // lifetime connections accepted
	ActiveConns atomic.Int64  // currently open connections
	FailedConns atomic.Uint64 // connections that could not reach a backend
	startTime   time.Time
}

// New returns a Proxy with the clock started at construction time.
func New() *Proxy {
	return &Proxy{startTime: time.Now()}
}

// ConnStart records one new active connection.
func (p *Proxy) ConnStart() {
	p.TotalConns.Add(1)
	p.ActiveConns.Add(1)
}

// ConnEnd records one completed connection (call via defer alongside activeConns.Done).
func (p *Proxy) ConnEnd() {
	p.ActiveConns.Add(-1)
}

// ConnFailed increments the failed-connection counter.
// ConnEnd must still be called (via defer) to decrement ActiveConns.
func (p *Proxy) ConnFailed() {
	p.FailedConns.Add(1)
}

// WritePrometheus writes Prometheus text-format metrics to w.
// backends and activeBackends are the current registry totals (caller-supplied).
func (p *Proxy) WritePrometheus(w io.Writer, backends, activeBackends int) {
	metric := func(help, typ, name string, value interface{}) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %v\n\n",
			name, help, name, typ, name, value)
	}

	metric("Total TCP connections accepted by the proxy", "counter",
		"dpivot_connections_total", p.TotalConns.Load())

	metric("Currently active TCP connections being proxied", "gauge",
		"dpivot_connections_active", p.ActiveConns.Load())

	metric("TCP connections that could not reach a backend", "counter",
		"dpivot_connections_failed_total", p.FailedConns.Load())

	metric("Total registered backends including draining ones", "gauge",
		"dpivot_backends_total", backends)

	metric("Active non-draining backends", "gauge",
		"dpivot_backends_active", activeBackends)

	metric("Proxy process uptime in seconds", "gauge",
		"dpivot_uptime_seconds", fmt.Sprintf("%.1f", time.Since(p.startTime).Seconds()))
}
