// Package rollout implements zero-downtime rolling updates for dpivot services.
package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Options configures a single rollout operation.
type Options struct {
	// ComposeFile is the path to dpivot-compose.yml (default: dpivot-compose.yml).
	ComposeFile string

	// Service is the name of the service to roll out.
	Service string

	// Pull fetches the latest image before rolling out.
	Pull bool

	// Timeout is how long to wait for the new container's healthcheck to pass.
	// Default: 60 seconds.
	Timeout time.Duration

	// Drain is how long to wait for in-flight connections to complete on the
	// old container after the new one is healthy. Default: 5 seconds.
	Drain time.Duration

	// ControlAddr is the HTTP address of the dpivot proxy control API.
	// Default: "http://localhost:9900"
	ControlAddr string

	// APIToken is the Bearer token for the control API. Empty means unauthenticated.
	APIToken string
}

func (o *Options) defaults() {
	if o.ComposeFile == "" {
		o.ComposeFile = "dpivot-compose.yml"
	}
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	if o.Drain == 0 {
		o.Drain = 5 * time.Second
	}
	if o.ControlAddr == "" {
		o.ControlAddr = "http://localhost:9900"
	}
}

// ── Rollout state (for rollback) ──────────────────────────────────────────────

// RolloutState is written to /tmp between steps 5 and 7 of a rollout (after
// the new backend is registered, before the old one is removed). It enables
// the rollback command to restore traffic to the previous version if the new
// deployment is unhealthy.
type RolloutState struct {
	Service      string        `json:"service"`
	OldBackendID string        `json:"old_backend_id"`
	OldAddr      string        `json:"old_addr"`
	NewBackendID string        `json:"new_backend_id"`
	NewAddr      string        `json:"new_addr"`
	ControlAddr  string        `json:"control_addr"`
	APIToken     string        `json:"api_token,omitempty"`
	Drain        time.Duration `json:"drain_ns"`
	StartedAt    time.Time     `json:"started_at"`
}

func statePath(service string) string {
	return fmt.Sprintf("/tmp/dpivot-%s-state.json", service)
}

func saveState(s RolloutState) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(s.Service), data, 0600)
}

// LoadState reads the last rollout state for the given service so Rollback can
// consume it. Returns an error if no state file exists.
func LoadState(service string) (RolloutState, error) {
	data, err := os.ReadFile(statePath(service))
	if err != nil {
		if os.IsNotExist(err) {
			return RolloutState{}, fmt.Errorf("no rollout state for %q — run a rollout first", service)
		}
		return RolloutState{}, err
	}
	var s RolloutState
	return s, json.Unmarshal(data, &s)
}

func clearState(service string) {
	os.Remove(statePath(service)) //nolint:errcheck
}

// ── Mutual exclusion ──────────────────────────────────────────────────────────

// lockRollout prevents concurrent rollouts for the same service by creating an
// exclusive lock file. Returns an unlock function on success.
func lockRollout(service string) (func(), error) {
	lockPath := fmt.Sprintf("/tmp/dpivot-rollout-%s.lock", service)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("rollout already in progress for %q (lock: %s) — wait or remove if stale", service, lockPath)
		}
		return nil, fmt.Errorf("rollout: acquire lock: %w", err)
	}
	f.Close()
	return func() { os.Remove(lockPath) }, nil //nolint:errcheck
}

// ── Run ───────────────────────────────────────────────────────────────────────

// Run executes a zero-downtime rolling update for the given service.
//
// Steps:
//  1. Acquire exclusive lock (prevents concurrent rollouts for this service).
//  2. Optionally pull the new image.
//  3. Scale the service to +1 instance (docker compose up --scale).
//  4. Wait for the new container's healthcheck to pass (or timeout).
//  5. Register the new container with the proxy via POST /backends.
//  6. Persist rollout state to /tmp (enables rollback).
//  7. Drain old container; wait drain period so in-flight requests complete.
//  8. Deregister the old container via DELETE /backends/{id}.
//  9. Scale back to the original count (remove old container).
//  10. Clear rollout state.
func Run(ctx context.Context, opts Options, log *zap.Logger) error {
	opts.defaults()

	unlock, err := lockRollout(opts.Service)
	if err != nil {
		return err
	}
	defer unlock()

	log.Info("rollout: starting",
		zap.String("service", opts.Service),
		zap.String("compose", opts.ComposeFile))

	// ── Step 1: Pull new image ────────────────────────────────────────────
	if opts.Pull {
		log.Info("rollout: pulling image", zap.String("service", opts.Service))
		if err := composeRun(ctx, opts.ComposeFile, "pull", opts.Service); err != nil {
			return fmt.Errorf("rollout: pull: %w", err)
		}
	}

	// ── Step 2: Scale to +1 ───────────────────────────────────────────────
	log.Info("rollout: scaling +1", zap.String("service", opts.Service))
	if err := composeRun(ctx, opts.ComposeFile, "up", "-d", "--no-deps",
		"--scale", opts.Service+"=2", opts.Service); err != nil {
		return fmt.Errorf("rollout: scale up: %w", err)
	}

	// ── Step 3: Wait for healthcheck ──────────────────────────────────────
	newID, newAddr, err := waitForNewContainer(ctx, opts, log)
	if err != nil {
		// Cleanup: scale back down on healthcheck timeout.
		_ = composeRun(ctx, opts.ComposeFile, "up", "-d", "--no-deps",
			"--scale", opts.Service+"=1", opts.Service)
		return fmt.Errorf("rollout: wait for healthy container: %w", err)
	}

	log.Info("rollout: new container healthy",
		zap.String("id", newID),
		zap.String("addr", newAddr))

	// ── Step 4: Find old container ID ────────────────────────────────────
	oldID, err := findOldContainer(ctx, opts.Service, newID)
	if err != nil {
		log.Warn("rollout: could not identify old container — skipping deregister",
			zap.Error(err))
	}

	// ── Step 5: Register new backend with proxy ───────────────────────────
	newBackendID := opts.Service + "-" + newID[:12]
	if err := registerBackend(ctx, opts, newBackendID, newAddr, log); err != nil {
		return fmt.Errorf("rollout: register new backend: %w", err)
	}
	log.Info("rollout: new backend registered",
		zap.String("backend_id", newBackendID),
		zap.String("addr", newAddr))

	// ── Step 6: Persist rollout state (enables rollback) ─────────────────
	oldBackendID := ""
	oldAddr := ""
	if oldID != "" {
		oldBackendID = opts.Service + "-" + oldID[:12]
		if addr, err := containerAddr(ctx, oldID); err == nil {
			oldAddr = addr
		}
	}
	_ = saveState(RolloutState{
		Service:      opts.Service,
		OldBackendID: oldBackendID,
		OldAddr:      oldAddr,
		NewBackendID: newBackendID,
		NewAddr:      newAddr,
		ControlAddr:  opts.ControlAddr,
		APIToken:     opts.APIToken,
		Drain:        opts.Drain,
		StartedAt:    time.Now(),
	})

	// ── Step 7: Drain old connections ─────────────────────────────────────
	log.Info("rollout: draining old connections", zap.Duration("drain", opts.Drain))
	if oldID != "" {
		_ = drainBackend(ctx, opts, oldBackendID, log)
	}
	select {
	case <-time.After(opts.Drain):
	case <-ctx.Done():
		return ctx.Err()
	}

	// ── Step 8: Deregister old backend ────────────────────────────────────
	if oldID != "" {
		if err := deregisterBackend(ctx, opts, oldBackendID, log); err != nil {
			log.Warn("rollout: could not deregister old backend",
				zap.String("id", oldBackendID),
				zap.Error(err))
		}
	}

	// ── Step 9: Remove old container (keep new one) ───────────────────────
	// We stop and remove the OLD container explicitly instead of using
	// --scale=1, because compose scale-down removes the newest container
	// (api-2) and keeps the old one (api-1), which is the opposite of what
	// we want.
	if oldID != "" {
		log.Info("rollout: removing old container", zap.String("id", oldID))
		_ = exec.CommandContext(ctx, "docker", "stop", oldID).Run()
		_ = exec.CommandContext(ctx, "docker", "rm", oldID).Run()
	}

	// ── Step 10: Clear state ──────────────────────────────────────────────
	clearState(opts.Service)

	log.Info("rollout: complete", zap.String("service", opts.Service))
	return nil
}

// ── Rollback ──────────────────────────────────────────────────────────────────

// Rollback restores traffic to the previous backend recorded in the rollout
// state file, and drains/removes the new (failing) backend.
//
// Call this when a just-deployed service is unhealthy and you need to restore
// the previous version without a full re-deploy. The rollout state is cleared
// after a successful rollback.
func Rollback(ctx context.Context, state RolloutState, log *zap.Logger) error {
	if state.OldBackendID == "" || state.OldAddr == "" {
		return fmt.Errorf("rollback: no old backend recorded in state — cannot roll back")
	}

	log.Info("rollback: starting",
		zap.String("service", state.Service),
		zap.String("restoring", state.OldBackendID),
		zap.String("draining", state.NewBackendID))

	opts := Options{
		ControlAddr: state.ControlAddr,
		APIToken:    state.APIToken,
		Drain:       state.Drain,
	}
	if opts.Drain == 0 {
		opts.Drain = 5 * time.Second
	}

	// Re-register old backend (it may have been removed; 409 if still present is ok).
	if err := registerBackend(ctx, opts, state.OldBackendID, state.OldAddr, log); err != nil {
		if !strings.Contains(err.Error(), "409") {
			return fmt.Errorf("rollback: restore old backend: %w", err)
		}
		log.Info("rollback: old backend already registered", zap.String("id", state.OldBackendID))
	} else {
		log.Info("rollback: old backend restored", zap.String("id", state.OldBackendID))
	}

	// Drain the new (failing) backend.
	if state.NewBackendID != "" {
		_ = drainBackend(ctx, opts, state.NewBackendID, log)
		log.Info("rollback: draining new backend",
			zap.String("id", state.NewBackendID),
			zap.Duration("drain", opts.Drain))

		select {
		case <-time.After(opts.Drain):
		case <-ctx.Done():
			return ctx.Err()
		}

		if err := deregisterBackend(ctx, opts, state.NewBackendID, log); err != nil {
			log.Warn("rollback: could not remove new backend (may not exist)",
				zap.String("id", state.NewBackendID),
				zap.Error(err))
		}
	}

	clearState(state.Service)
	log.Info("rollback: complete", zap.String("service", state.Service))
	return nil
}

// ── Docker / Compose helpers ──────────────────────────────────────────────────

func composeRun(ctx context.Context, file string, args ...string) error {
	cmdArgs := append([]string{"compose", "-f", file}, args...)
	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %w\n%s",
			strings.Join(cmdArgs, " "), err, string(out))
	}
	return nil
}

// waitForNewContainer polls for a second instance of the service to appear
// and pass its healthcheck. Returns the container ID and its dpivot_mesh IP.
func waitForNewContainer(ctx context.Context, opts Options, log *zap.Logger) (id, addr string, err error) {
	deadline := time.Now().Add(opts.Timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", "", fmt.Errorf("timeout (%s) waiting for healthy container", opts.Timeout)
			}

			id, addr, err = inspectNewestHealthy(ctx, opts.Service)
			if err == nil {
				return id, addr, nil
			}
			log.Debug("rollout: waiting for healthy container", zap.Error(err))
		}
	}
}

// inspectNewestHealthy finds the most recently started container for the
// service that is either healthy (has healthcheck) or running (no healthcheck).
// Returns id and addr in "ip:port" form ready for the proxy control API.
func inspectNewestHealthy(ctx context.Context, service string) (id, addr string, err error) {
	out, err := exec.CommandContext(ctx, "docker", "ps",
		"--filter", "label=com.docker.compose.service="+service,
		"--format", "{{.ID}}",
	).Output()
	if err != nil {
		return "", "", fmt.Errorf("docker ps: %w", err)
	}

	ids := strings.Fields(string(out))
	if len(ids) < 2 {
		return "", "", fmt.Errorf("service %q: waiting for second container (found %d)", service, len(ids))
	}

	id = ids[0]
	// Emit health status, "name=ip" network pairs, and "port/proto" exposed port pairs.
	// ExposedPorts is map[Port]struct{} so we range with $k,$v to get the key.
	inspectOut, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format",
		`{{.State.Health.Status}}{{range $n, $v := .NetworkSettings.Networks}} net={{$n}}={{$v.IPAddress}}{{end}}{{range $k, $v := .Config.ExposedPorts}} port={{$k}}{{end}}`,
		id,
	).Output()
	if err != nil {
		return "", "", fmt.Errorf("docker inspect %s: %w", id, err)
	}

	fields := strings.Fields(string(inspectOut))
	if len(fields) < 1 {
		return "", "", fmt.Errorf("docker inspect: empty output for %s", id)
	}

	healthStatus := fields[0]
	if healthStatus == "unhealthy" {
		return "", "", fmt.Errorf("container %s is unhealthy", id)
	}
	if healthStatus == "starting" {
		return "", "", fmt.Errorf("container %s healthcheck is still starting", id)
	}

	// Parse network and port tokens.
	var netTokens, portTokens []string
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "net=") {
			netTokens = append(netTokens, strings.TrimPrefix(f, "net="))
		} else if strings.HasPrefix(f, "port=") {
			portTokens = append(portTokens, strings.TrimPrefix(f, "port="))
		}
	}

	ip := pickMeshIP(netTokens)
	if ip == "" {
		return "", "", fmt.Errorf("container %s has no IP address", id)
	}

	// Extract the first exposed port number (format: "3001/tcp").
	port := "80"
	if len(portTokens) > 0 {
		if p, _, found := strings.Cut(portTokens[0], "/"); found {
			port = p
		} else {
			port = portTokens[0]
		}
	}

	return id, ip + ":" + port, nil
}

// findOldContainer returns the ID of the container that is NOT the newID.
func findOldContainer(ctx context.Context, service, newID string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "ps",
		"--filter", "label=com.docker.compose.service="+service,
		"--format", "{{.ID}}",
	).Output()
	if err != nil {
		return "", fmt.Errorf("docker ps: %w", err)
	}
	for _, id := range strings.Fields(string(out)) {
		if !strings.HasPrefix(newID, id) && !strings.HasPrefix(id, newID[:12]) {
			return id, nil
		}
	}
	return "", fmt.Errorf("could not find old container for service %q", service)
}

// containerAddr returns the dpivot_mesh "ip:port" of the given container.
func containerAddr(ctx context.Context, id string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format",
		`{{range $n, $v := .NetworkSettings.Networks}}net={{$n}}={{$v.IPAddress}} {{end}}{{range $k, $v := .Config.ExposedPorts}}port={{$k}} {{end}}`,
		id,
	).Output()
	if err != nil {
		return "", err
	}
	var netTokens, portTokens []string
	for _, f := range strings.Fields(string(out)) {
		if strings.HasPrefix(f, "net=") {
			netTokens = append(netTokens, strings.TrimPrefix(f, "net="))
		} else if strings.HasPrefix(f, "port=") {
			portTokens = append(portTokens, strings.TrimPrefix(f, "port="))
		}
	}
	ip := pickMeshIP(netTokens)
	if ip == "" {
		return "", fmt.Errorf("container %s has no IP", id)
	}
	port := "80"
	if len(portTokens) > 0 {
		if p, _, found := strings.Cut(portTokens[0], "/"); found {
			port = p
		} else {
			port = portTokens[0]
		}
	}
	return ip + ":" + port, nil
}

// pickMeshIP selects the IP from the dpivot_mesh network out of a slice of
// "networkname=ip" tokens. Falls back to the first parseable IP if no mesh
// network is found.
func pickMeshIP(tokens []string) string {
	fallback := ""
	for _, token := range tokens {
		eq := strings.IndexByte(token, '=')
		if eq < 0 {
			continue
		}
		name, ip := token[:eq], token[eq+1:]
		if ip == "" {
			continue
		}
		if fallback == "" {
			fallback = ip
		}
		if strings.HasSuffix(name, "dpivot_mesh") {
			return ip
		}
	}
	return fallback
}

// ── Control API helpers ───────────────────────────────────────────────────────

func registerBackend(ctx context.Context, opts Options, id, addr string, log *zap.Logger) error {
	body, _ := json.Marshal(map[string]string{"id": id, "addr": addr})
	req, err := http.NewRequestWithContext(ctx,
		http.MethodPost, opts.ControlAddr+"/backends", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST /backends: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("POST /backends: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func drainBackend(ctx context.Context, opts Options, id string, log *zap.Logger) error {
	req, err := http.NewRequestWithContext(ctx,
		http.MethodPut, opts.ControlAddr+"/backends/"+id+"/drain", nil)
	if err != nil {
		return err
	}
	if opts.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func deregisterBackend(ctx context.Context, opts Options, id string, log *zap.Logger) error {
	req, err := http.NewRequestWithContext(ctx,
		http.MethodDelete, opts.ControlAddr+"/backends/"+id, nil)
	if err != nil {
		return err
	}
	if opts.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.APIToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE /backends/%s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("DELETE /backends/%s: unexpected status %d", id, resp.StatusCode)
	}
	return nil
}
