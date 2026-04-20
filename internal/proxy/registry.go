// Package proxy implements the dpivot TCP reverse proxy components.
package proxy

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Backend represents a single registered upstream instance.
type Backend struct {
	// ID uniquely identifies this backend. Caller-supplied; must be non-empty.
	ID string `json:"id"`

	// Addr is the dial address of the upstream: "host:port".
	Addr string `json:"addr"`

	// Draining signals that no new connections should be sent to this backend.
	// The backend is still in the registry so it can finish in-flight work.
	Draining bool `json:"draining"`

	// AddedAt is set automatically by Registry.Add.
	AddedAt time.Time `json:"added_at"`

	// requests is a pointer to an atomic counter to avoid copying atomics.
	// The counter lives on the heap so struct copies (snapshots) remain safe.
	requests *atomic.Uint64
}

// Requests returns the total connections routed to this backend.
func (b *Backend) Requests() uint64 {
	if b.requests == nil {
		return 0
	}
	return b.requests.Load()
}

// IncrRequests atomically increments the connection counter.
func (b *Backend) IncrRequests() {
	if b.requests != nil {
		b.requests.Add(1)
	}
}

// MarshalledRequests is an alias for Requests() — used in JSON serialisation.
func (b *Backend) MarshalledRequests() uint64 { return b.Requests() }

// Registry is a thread-safe store of backends keyed by ID.
//
// Invariants:
//   - IDs are globally unique.
//   - Add/Remove/SetDraining are atomic with respect to each other.
//   - Snapshot reads (Active, Backends, Get) are always consistent: they return
//     value copies of the Backend struct, which is safe because the only
//     concurrently-mutated field (requests) lives behind a pointer.
type Registry struct {
	mu    sync.RWMutex
	store map[string]*Backend
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{store: make(map[string]*Backend)}
}

// Add registers a new backend. Returns an error if:
//   - ID or Addr is empty
//   - ID is already registered
func (r *Registry) Add(b Backend) error {
	if b.ID == "" {
		return fmt.Errorf("registry: backend ID must not be empty")
	}
	if b.Addr == "" {
		return fmt.Errorf("registry: backend %q: addr must not be empty", b.ID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.store[b.ID]; exists {
		return fmt.Errorf("registry: backend %q is already registered", b.ID)
	}
	b.AddedAt = time.Now()
	b.requests = &atomic.Uint64{} // each backend owns its counter on the heap
	r.store[b.ID] = &b
	return nil
}

// Remove deregisters a backend by ID.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.store[id]; !exists {
		return fmt.Errorf("registry: backend %q not found", id)
	}
	delete(r.store, id)
	return nil
}

// SetDraining marks a backend as draining (no new connections) without removing
// it from the registry. Returns an error if the ID is not found.
func (r *Registry) SetDraining(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	b, exists := r.store[id]
	if !exists {
		return fmt.Errorf("registry: backend %q not found", id)
	}
	b.Draining = true
	return nil
}

// Get returns a copy of the backend with the given ID.
func (r *Registry) Get(id string) (Backend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	b, ok := r.store[id]
	if !ok {
		return Backend{}, false
	}
	return *b, true
}

// Backends returns a point-in-time snapshot of all registered backends
// (including draining ones). Results are sorted by ID for deterministic
// round-robin selection. Safe to read concurrently with mutations.
func (r *Registry) Backends() []Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Backend, 0, len(r.store))
	for _, b := range r.store {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Active returns a sorted snapshot of all non-draining backends.
func (r *Registry) Active() []Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Backend, 0, len(r.store))
	for _, b := range r.store {
		if !b.Draining {
			out = append(out, *b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Len returns the total number of registered backends (including draining).
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.store)
}

// incrRequests atomically increments the request counter for the backend.
// Called by the router after each connection is assigned.
func (r *Registry) incrRequests(id string) {
	r.mu.RLock()
	b := r.store[id]
	r.mu.RUnlock()
	if b != nil {
		b.IncrRequests()
	}
}
