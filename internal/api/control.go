// Package api implements the dpivot HTTP control plane.
//
// The control API runs on the dpivot-proxy container at DPIVOT_CONTROL_PORT
// (default: 9900). It is reachable only from within the dpivot_mesh bridge
// network — not from the Docker host.
//
// Optional bearer-token authentication is enabled when the DPIVOT_API_TOKEN
// environment variable is set on the proxy container. If unset, a warning is
// logged at startup (the API still works).
package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/dpivot/dpivot/internal/proxy"
	"go.uber.org/zap"
)

// ControlServer exposes the dpivot backend management API over HTTP.
type ControlServer struct {
	reg    *proxy.Registry
	srv    *proxy.Server
	log    *zap.Logger
	token  string   // empty → unauthenticated (warning at startup)
	ln     net.Listener
	mux    *http.ServeMux
}

// NewControlServer creates a control server backed by reg and srv.
// It reads DPIVOT_API_TOKEN from the environment at construction time.
func NewControlServer(reg *proxy.Registry, srv *proxy.Server, log *zap.Logger) *ControlServer {
	token := os.Getenv("DPIVOT_API_TOKEN")
	if token == "" {
		log.Warn("control API is unauthenticated — set DPIVOT_API_TOKEN to secure it")
	}

	cs := &ControlServer{
		reg:   reg,
		srv:   srv,
		log:   log,
		token: token,
		mux:   http.NewServeMux(),
	}
	cs.registerRoutes()
	return cs
}

// ListenAndServe binds to addr (e.g. ":9900") and serves requests.
// It blocks until the server is stopped via Close().
func (cs *ControlServer) ListenAndServe(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("control: listen %s: %w", addr, err)
	}
	cs.ln = ln
	cs.log.Info("control API listening", zap.String("addr", addr))
	return http.Serve(ln, cs.mux) //nolint:wrapcheck
}

// Close shuts down the HTTP listener.
func (cs *ControlServer) Close() error {
	if cs.ln != nil {
		return cs.ln.Close()
	}
	return nil
}

// Handler returns the underlying http.Handler for use with httptest.NewServer.
func (cs *ControlServer) Handler() http.Handler { return cs.mux }

// ── Route registration ────────────────────────────────────────────────────────

func (cs *ControlServer) registerRoutes() {
	cs.mux.HandleFunc("/health", cs.handleHealth)
	cs.mux.HandleFunc("/backends", cs.auth(cs.handleBackends))
	cs.mux.HandleFunc("/backends/", cs.auth(cs.handleBackendByID))
}

// ── Handlers ──────────────────────────────────────────────────────────────────

// GET /health
func (cs *ControlServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r.Method, "GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"backends": cs.reg.Len(),
	})
}

// GET /backends        → list all
// POST /backends       → register a new backend
func (cs *ControlServer) handleBackends(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cs.listBackends(w, r)
	case http.MethodPost:
		cs.addBackend(w, r)
	default:
		methodNotAllowed(w, r.Method, "GET, POST")
	}
}

// DELETE /backends/{id}        → drain + deregister
// PUT    /backends/{id}/drain  → mark draining only
func (cs *ControlServer) handleBackendByID(w http.ResponseWriter, r *http.Request) {
	// Path: /backends/<id>  or  /backends/<id>/drain
	path := strings.TrimPrefix(r.URL.Path, "/backends/")
	parts := strings.SplitN(path, "/", 2)
	id := parts[0]

	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing backend ID in URL", "invalid_path")
		return
	}

	if len(parts) == 2 && parts[1] == "drain" {
		// PUT /backends/{id}/drain
		if r.Method != http.MethodPut {
			methodNotAllowed(w, r.Method, "PUT")
			return
		}
		cs.drainBackend(w, id)
		return
	}

	// DELETE /backends/{id}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, r.Method, "DELETE")
		return
	}
	cs.removeBackend(w, id)
}

// ── Individual actions ────────────────────────────────────────────────────────

func (cs *ControlServer) listBackends(w http.ResponseWriter, _ *http.Request) {
	all := cs.reg.Backends()
	type row struct {
		ID       string `json:"id"`
		Addr     string `json:"addr"`
		Draining bool   `json:"draining"`
		Requests uint64 `json:"requests"`
	}
	rows := make([]row, len(all))
	for i, b := range all {
		rows[i] = row{
			ID:       b.ID,
			Addr:     b.Addr,
			Draining: b.Draining,
			Requests: b.MarshalledRequests(),
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends": rows,
		"count":    len(rows),
	})
}

func (cs *ControlServer) addBackend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "invalid_body")
		return
	}
	if req.Addr == "" {
		writeErr(w, http.StatusBadRequest, "addr is required", "missing_field")
		return
	}
	// Auto-generate ID if omitted.
	if req.ID == "" {
		req.ID = req.Addr
	}

	b := proxy.Backend{ID: req.ID, Addr: req.Addr}
	if err := cs.reg.Add(b); err != nil {
		code := http.StatusInternalServerError
		errCode := "internal_error"
		if strings.Contains(err.Error(), "already registered") {
			code = http.StatusConflict
			errCode = "conflict"
		}
		writeErr(w, code, err.Error(), errCode)
		return
	}
	cs.log.Info("backend registered", zap.String("id", req.ID), zap.String("addr", req.Addr))

	registered, _ := cs.reg.Get(req.ID)
	writeJSON(w, http.StatusCreated, registered)
}

func (cs *ControlServer) drainBackend(w http.ResponseWriter, id string) {
	if err := cs.reg.SetDraining(id); err != nil {
		code := http.StatusNotFound
		if !strings.Contains(err.Error(), "not found") {
			code = http.StatusInternalServerError
		}
		writeErr(w, code, err.Error(), "not_found")
		return
	}
	cs.log.Info("backend draining", zap.String("id", id))
	w.WriteHeader(http.StatusNoContent)
}

func (cs *ControlServer) removeBackend(w http.ResponseWriter, id string) {
	// Drain first (best effort — ignore error if already draining).
	_ = cs.reg.SetDraining(id)

	if err := cs.reg.Remove(id); err != nil {
		code := http.StatusNotFound
		if !strings.Contains(err.Error(), "not found") {
			code = http.StatusInternalServerError
		}
		writeErr(w, code, err.Error(), "not_found")
		return
	}
	cs.log.Info("backend removed", zap.String("id", id))
	w.WriteHeader(http.StatusNoContent)
}

// ── Auth middleware ───────────────────────────────────────────────────────────

func (cs *ControlServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cs.token == "" {
			next(w, r)
			return
		}
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeErr(w, http.StatusUnauthorized, "Bearer token required", "unauthorized")
			return
		}
		if strings.TrimPrefix(authHeader, "Bearer ") != cs.token {
			writeErr(w, http.StatusForbidden, "invalid token", "forbidden")
			return
		}
		next(w, r)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func methodNotAllowed(w http.ResponseWriter, got, allowed string) {
	w.Header().Set("Allow", allowed)
	writeErr(w, http.StatusMethodNotAllowed,
		fmt.Sprintf("method %s not allowed; use %s", got, allowed),
		"method_not_allowed")
}
