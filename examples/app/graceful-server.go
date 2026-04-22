// graceful-server.go — Production Go HTTP server for use with dpivot.
//
// Demonstrates:
//   1. /health/live  — liveness probe (process alive)
//   2. /health/ready — readiness probe (DB + cache reachable)
//   3. SIGTERM drain — completes in-flight requests before exiting
//   4. Idempotent handler pattern via Idempotency-Key header
//   5. Structured JSON logging
//
// Build: go build -o server ./examples/app/graceful-server.go
// Run:   DATABASE_URL=postgres://... REDIS_URL=redis://... ./server

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// ── Config ────────────────────────────────────────────────────────────────────

var (
	port          = envOr("PORT", "3000")
	drainTimeout  = envDuration("DRAIN_TIMEOUT", 30*time.Second)
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return def
}

// ── Structured logger ─────────────────────────────────────────────────────────

func logJSON(level, event string, fields map[string]interface{}) {
	fields["level"] = level
	fields["event"] = event
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	data, _ := json.Marshal(fields)
	fmt.Fprintf(os.Stderr, "%s\n", data)
}

// ── Idempotency cache (in-memory; replace with Redis in production) ───────────

type idempotencyStore struct {
	// In production use Redis: SET idem:{key} {result} EX 86400 NX
	// This in-memory stub demonstrates the pattern.
	m map[string]idempotentResult
}

type idempotentResult struct {
	Status int
	Body   []byte
}

var idemStore = &idempotencyStore{m: make(map[string]idempotentResult)}

func (s *idempotencyStore) Get(key string) (idempotentResult, bool) {
	v, ok := s.m[key]
	return v, ok
}

func (s *idempotencyStore) Set(key string, r idempotentResult) {
	s.m[key] = r
}

// ── Health checks ─────────────────────────────────────────────────────────────

// checkDeps verifies that the application's dependencies (DB, cache) are
// reachable. Called by /health/ready. Returns nil when all deps are healthy.
func checkDeps(ctx context.Context) error {
	// TCP dial to postgres (replace with actual DB ping in production).
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL != "" {
		dialer := &net.Dialer{}
		conn, err := dialer.DialContext(ctx, "tcp", dbURL)
		if err != nil {
			return fmt.Errorf("db unreachable: %w", err)
		}
		conn.Close()
	}

	// TCP dial to Redis.
	redisURL := os.Getenv("REDIS_URL")
	if redisURL != "" {
		dialer := &net.Dialer{}
		conn, err := dialer.DialContext(ctx, "tcp", redisURL)
		if err != nil {
			return fmt.Errorf("redis unreachable: %w", err)
		}
		conn.Close()
	}

	return nil
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"pid":    os.Getpid(),
	})
}

func handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := checkDeps(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status": "not_ready",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ready"})
}

// handleCreateOrder demonstrates an idempotent POST handler.
// Clients must send Idempotency-Key: <uuid> with every request.
// Duplicate requests return the same response without re-executing the mutation.
func handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Idempotency-Key header is required",
		})
		return
	}

	// Return cached response for duplicate requests.
	if cached, ok := idemStore.Get(key); ok {
		w.Header().Set("X-Idempotent-Replay", "true")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(cached.Status)
		w.Write(cached.Body) //nolint:errcheck
		return
	}

	// Process exactly once — decode and persist the result.
	var order map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&order); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	id := newID()
	result, _ := json.Marshal(map[string]string{"id": id, "status": "created"})
	idemStore.Set(key, idempotentResult{Status: http.StatusCreated, Body: result})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(result) //nolint:errcheck
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", handleLive)
	mux.HandleFunc("/health/ready", handleReady)
	mux.HandleFunc("/orders", handleCreateOrder)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,

		// Timeouts prevent slow-client attacks and resource exhaustion.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Start in background.
	go func() {
		logJSON("info", "server_start", map[string]interface{}{"port": port})
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logJSON("error", "server_fatal", map[string]interface{}{"err": err.Error()})
			os.Exit(1)
		}
	}()

	// ── Graceful shutdown on SIGTERM / SIGINT ─────────────────────────────────
	// Docker sends SIGTERM when stopping a container.
	// dpivot drains the proxy-side connection first (drain period), then
	// Docker stops the container. This server-side drain ensures in-flight
	// HTTP requests complete even if the proxy drain is shorter than expected.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit

	logJSON("info", "shutdown_start", map[string]interface{}{
		"signal":     sig.String(),
		"drain_secs": drainTimeout.Seconds(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	// Shutdown stops the listener and waits for active handlers to complete.
	if err := srv.Shutdown(ctx); err != nil {
		logJSON("warn", "shutdown_timeout", map[string]interface{}{"err": err.Error()})
		os.Exit(1)
	}

	logJSON("info", "shutdown_complete", map[string]interface{}{})
}
