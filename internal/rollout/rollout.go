// Package rollout implements zero-downtime rolling updates for dpivot services.
package rollout

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// Run executes a zero-downtime rolling update for the given service.
//
// Steps:
//  1. Optionally pull the new image.
//  2. Scale the service to +1 instance (docker compose up --scale).
//  3. Wait for the new container's healthcheck to pass (or timeout).
//  4. Determine the new container's IP on dpivot_mesh.
//  5. Register the new container with the proxy via POST /backends.
//  6. Wait for the drain period so in-flight requests complete.
//  7. Deregister the old container via DELETE /backends/{id}.
//  8. Scale back to the original count (remove old container).
func Run(ctx context.Context, opts Options, log *zap.Logger) error {
	opts.defaults()

	proxyName := "dpivot-proxy-" + opts.Service

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
		// Cleanup: scale back down.
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

	// ── Step 6: Drain period ──────────────────────────────────────────────
	log.Info("rollout: draining old connections",
		zap.Duration("drain", opts.Drain))

	if oldID != "" {
		_ = drainBackend(ctx, opts, opts.Service+"-"+oldID[:12], log)
	}

	select {
	case <-time.After(opts.Drain):
	case <-ctx.Done():
		return ctx.Err()
	}

	// ── Step 7: Deregister old backend ────────────────────────────────────
	if oldID != "" {
		oldBackendID := opts.Service + "-" + oldID[:12]
		if err := deregisterBackend(ctx, opts, oldBackendID, log); err != nil {
			log.Warn("rollout: could not deregister old backend",
				zap.String("id", oldBackendID),
				zap.Error(err))
		}
	}

	// ── Step 8: Scale back to 1 ───────────────────────────────────────────
	log.Info("rollout: scaling back to 1", zap.String("service", opts.Service))
	if err := composeRun(ctx, opts.ComposeFile, "up", "-d", "--no-deps",
		"--scale", opts.Service+"=1", opts.Service); err != nil {
		return fmt.Errorf("rollout: scale down: %w", err)
	}

	log.Info("rollout: complete",
		zap.String("service", opts.Service),
		zap.String("proxy", proxyName))
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

	// Query the newest ID (first in `docker ps` output, sorted newest-first).
	id = ids[0]
	inspectOut, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format",
		`{{.State.Health.Status}} {{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}`,
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
	// "healthy" | "starting" (has healthcheck) or first field is IP (no healthcheck)
	if healthStatus == "starting" {
		return "", "", fmt.Errorf("container %s healthcheck is still starting", id)
	}

	// IP is the last field.
	ip := fields[len(fields)-1]
	if ip == "" {
		return "", "", fmt.Errorf("container %s has no IP on dpivot_mesh", id)
	}
	return id, ip, nil
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
