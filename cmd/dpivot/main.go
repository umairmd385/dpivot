// Package main is the dpivot CLI entrypoint.
//
// dpivot ships as a single binary that works in two ways:
//
//  1. Standalone: `dpivot generate`, `dpivot rollout web`, …
//  2. Docker CLI plugin: `docker dpivot rollout web` (binary named docker-dpivot)
//
// Plugin mode is detected automatically via argv[0] or the
// DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND environment variable.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dpivot/dpivot/internal/api"
	"github.com/dpivot/dpivot/internal/compose"
	"github.com/dpivot/dpivot/internal/metrics"
	"github.com/dpivot/dpivot/internal/plugin"
	"github.com/dpivot/dpivot/internal/proxy"
	"github.com/dpivot/dpivot/internal/rollout"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const version = "0.1.0"

func main() {
	// Docker CLI plugin: handle metadata probe before anything else.
	if plugin.HandleMetadataRequest(version) {
		os.Exit(0)
	}
	// Strip the extra "dpivot" arg injected by Docker in plugin mode.
	plugin.StripPluginArgs()

	log, _ := zap.NewProduction()
	defer log.Sync() //nolint:errcheck

	root := buildRoot(log)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// buildRoot constructs the full Cobra command tree.
func buildRoot(log *zap.Logger) *cobra.Command {
	root := &cobra.Command{
		Use:   "dpivot",
		Short: "Zero-downtime deployments for Docker Compose",
		Long: `dpivot injects a built-in TCP proxy into your Docker Compose stack
so that container replacements happen without dropping a single connection.

No external proxy (Traefik, nginx) required.

Example:
  dpivot generate                       # enhance docker-compose.yml
  docker compose -f dpivot-compose.yml up -d
  dpivot rollout web                    # roll out a new version of 'web'
  dpivot rollback web                   # restore previous version if deploy fails`,
		SilenceUsage: true,
	}

	root.AddCommand(
		generateCmd(log),
		rolloutCmd(log),
		rollbackCmd(log),
		statusCmd(log),
		scaleCmd(log),
		proxyCmd(log),
		versionCmd(),
	)
	return root
}

// ── generate ─────────────────────────────────────────────────────────────────

func generateCmd(log *zap.Logger) *cobra.Command {
	var input, output string

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate a dpivot-enhanced compose file from docker-compose.yml",
		Long: `Reads docker-compose.yml and writes dpivot-compose.yml.

The original file is never modified. The generated file:
  - Moves port bindings from app services to dpivot-proxy-<service>
  - Adds a dpivot_mesh bridge network
  - Injects dpivot labels for service tracking

Auto-detection rules (per service, first match wins):
  1. x-dpivot: skip: true  → pass through unchanged
  2. No ports declared       → pass through unchanged
  3. Known database image    → pass through (with warning)
  4. Everything else         → proxy injected

Example:
  dpivot generate
  dpivot generate --file docker-compose.prod.yml --output dpivot-compose.prod.yml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cf, err := compose.ParseFile(input)
			if err != nil {
				return fmt.Errorf("generate: %w", err)
			}

			out, sum, err := compose.Generate(cf)
			if err != nil {
				return fmt.Errorf("generate: %w", err)
			}

			// Emit summary.
			fmt.Fprintf(os.Stderr, "Parsed %d service(s) — %d eligible for proxy injection\n\n",
				len(cf.Services), len(sum.Proxied))
			fmt.Fprintln(os.Stderr, "dpivot Transform Summary:")
			for _, svc := range sum.Proxied {
				fmt.Fprintf(os.Stderr, "  ✓ Enabling zero-downtime for service '%s'\n", svc)
			}
			for _, svc := range sum.Skipped {
				fmt.Fprintf(os.Stderr, "  ⚠ Skipped '%s' (known database image)\n", svc)
			}

			if err := writeComposeFile(output, out); err != nil {
				return fmt.Errorf("generate: write %s: %w", output, err)
			}
			fmt.Fprintf(os.Stderr, "\nGenerated: %s\n", output)
			return nil
		},
	}

	cmd.Flags().StringVarP(&input, "file", "f", "docker-compose.yml", "Input compose file")
	cmd.Flags().StringVarP(&output, "output", "o", "dpivot-compose.yml", "Output file path")
	return cmd
}

// ── rollout ───────────────────────────────────────────────────────────────────

func rolloutCmd(log *zap.Logger) *cobra.Command {
	var opts rollout.Options

	cmd := &cobra.Command{
		Use:   "rollout <service>",
		Short: "Zero-downtime rolling update for a service",
		Long: `Performs a zero-downtime rolling update for the named service.

Steps:
  1. Acquire rollout lock (prevents concurrent rollouts for the same service)
  2. Optional: pull latest image
  3. Scale service +1 (start new container)
  4. Wait for new container healthcheck to pass
  5. Register new container with the dpivot proxy
  6. Save rollout state to /tmp (enables rollback if deploy fails)
  7. Drain period — in-flight requests complete on old container
  8. Deregister old container from proxy
  9. Scale back to original count

If the new container fails its healthcheck within --timeout, dpivot scales
back to 1 automatically without disrupting traffic.

Example:
  dpivot rollout web
  dpivot rollout web --pull --timeout 120s --drain 10s`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Service = args[0]

			// Verify the service exists in dpivot-compose.yml.
			cf, err := compose.ParseFile(opts.ComposeFile)
			if err != nil {
				return fmt.Errorf("rollout: read compose file: %w\n(did you run dpivot generate first?)", err)
			}
			if _, ok := cf.Services[opts.Service]; !ok {
				return fmt.Errorf("rollout: service %q not found in %s\n(did you run dpivot generate first?)",
					opts.Service, opts.ComposeFile)
			}

			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGTERM, syscall.SIGINT)
			defer cancel()

			return rollout.Run(ctx, opts, log)
		},
	}

	cmd.Flags().StringVarP(&opts.ComposeFile, "file", "f", "dpivot-compose.yml", "dpivot compose file")
	cmd.Flags().BoolVar(&opts.Pull, "pull", false, "Pull latest image before rolling out")
	cmd.Flags().DurationVarP(&opts.Timeout, "timeout", "t", 60*time.Second, "Healthcheck timeout")
	cmd.Flags().DurationVarP(&opts.Drain, "drain", "d", 5*time.Second, "Drain period before removing old container")
	cmd.Flags().StringVar(&opts.ControlAddr, "control-addr", "http://localhost:9900", "Proxy control API address")
	cmd.Flags().StringVar(&opts.APIToken, "api-token", os.Getenv("DPIVOT_API_TOKEN"), "Control API bearer token")
	return cmd
}

// ── rollback ──────────────────────────────────────────────────────────────────

func rollbackCmd(log *zap.Logger) *cobra.Command {
	var controlAddr, apiToken string
	var drain time.Duration

	cmd := &cobra.Command{
		Use:   "rollback <service>",
		Short: "Restore traffic to the previous version after a failed rollout",
		Long: `Reads the last rollout state for the service, re-registers the old
backend with the proxy, drains the new (failing) backend, and removes it.

The rollout command saves a state file to /tmp/dpivot-<service>-state.json
between the point the new backend is registered and the old one is removed.
If the new deployment fails, run this command immediately to restore traffic.

Example:
  dpivot rollback web
  dpivot rollback web --drain 15s
  dpivot rollback web --control-addr http://localhost:9901`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			state, err := rollout.LoadState(service)
			if err != nil {
				return err
			}
			// Allow flag overrides.
			if controlAddr != "" {
				state.ControlAddr = controlAddr
			}
			if apiToken != "" {
				state.APIToken = apiToken
			}
			if drain != 0 {
				state.Drain = drain
			}

			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGTERM, syscall.SIGINT)
			defer cancel()

			return rollout.Rollback(ctx, state, log)
		},
	}

	cmd.Flags().StringVar(&controlAddr, "control-addr", "", "Override proxy control API address from state")
	cmd.Flags().StringVar(&apiToken, "api-token", os.Getenv("DPIVOT_API_TOKEN"), "Control API bearer token")
	cmd.Flags().DurationVarP(&drain, "drain", "d", 0, "Override drain period from state")
	return cmd
}

// ── status ────────────────────────────────────────────────────────────────────

func statusCmd(log *zap.Logger) *cobra.Command {
	var controlAddr string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show current proxy backends and request counts",
		Long: `Queries the dpivot proxy control API and prints the current state.

Example:
  dpivot status
  dpivot status --control-addr http://localhost:9901`,
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := doGet(controlAddr + "/backends")
			if err != nil {
				return fmt.Errorf("status: %w\n\nHint: is the proxy running? Check: docker ps --filter name=dpivot-proxy", err)
			}

			var result struct {
				Backends []struct {
					ID       string `json:"id"`
					Addr     string `json:"addr"`
					Draining bool   `json:"draining"`
					Requests uint64 `json:"requests"`
				} `json:"backends"`
				Count int `json:"count"`
			}
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				fmt.Println(raw)
				return nil
			}

			fmt.Fprintf(os.Stderr, "Control API: %s\n\n", controlAddr)
			if result.Count == 0 {
				fmt.Println("No backends registered.")
				return nil
			}

			fmt.Printf("%-36s  %-24s  %10s  %s\n", "BACKEND ID", "ADDRESS", "REQUESTS", "STATUS")
			fmt.Println(strings.Repeat("─", 82))
			for _, b := range result.Backends {
				status := "active"
				if b.Draining {
					status = "draining"
				}
				fmt.Printf("%-36s  %-24s  %10d  %s\n", b.ID, b.Addr, b.Requests, status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&controlAddr, "control-addr", "http://localhost:9900", "Proxy control API address")
	return cmd
}

// ── scale ─────────────────────────────────────────────────────────────────────

func scaleCmd(log *zap.Logger) *cobra.Command {
	var opts rollout.Options

	cmd := &cobra.Command{
		Use:    "scale <service> <n>",
		Short:  "Register n replicas of a service with the proxy",
		Hidden: true, // not yet implemented; hidden so it doesn't appear in --help
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("scale is not yet implemented — use 'dpivot rollout' to deploy new versions")
		},
	}
	cmd.Flags().StringVarP(&opts.ComposeFile, "file", "f", "dpivot-compose.yml", "dpivot compose file")
	return cmd
}

// ── proxy (internal — runs inside the dpivot-proxy container) ─────────────────

func proxyCmd(log *zap.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "proxy",
		Short:  "Start the built-in TCP proxy (runs inside the proxy container)",
		Hidden: true, // internal; not part of the user-facing CLI
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProxy(log)
		},
	}
	return cmd
}

func runProxy(log *zap.Logger) error {
	m := metrics.New()

	reg := proxy.NewRegistry()
	router := proxy.NewRouter(reg)
	srv := proxy.NewServer(router, log, m)

	// Parse DPIVOT_BINDS: "listenPort:targetPort,..."
	bindsEnv := os.Getenv("DPIVOT_BINDS")
	if bindsEnv == "" {
		return fmt.Errorf("proxy: DPIVOT_BINDS is required")
	}
	for _, spec := range strings.Split(bindsEnv, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("proxy: invalid bind spec %q (expected listenPort:targetPort)", spec)
		}
		lp, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("proxy: invalid listen port in %q: %w", spec, err)
		}
		tp, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("proxy: invalid target port in %q: %w", spec, err)
		}
		if err := srv.Bind(proxy.PortBinding{ListenPort: lp, TargetPort: tp}); err != nil {
			return fmt.Errorf("proxy: bind %d: %w", lp, err)
		}
	}

	// Parse DPIVOT_TARGETS: "id:addr,..." (initial backends)
	targetsEnv := os.Getenv("DPIVOT_TARGETS")
	for _, spec := range strings.Split(targetsEnv, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		parts := strings.SplitN(spec, ":", 2)
		if len(parts) != 2 {
			log.Warn("proxy: invalid target spec — skipping", zap.String("spec", spec))
			continue
		}
		if err := reg.Add(proxy.Backend{ID: parts[0], Addr: parts[1]}); err != nil {
			log.Warn("proxy: could not register initial backend", zap.Error(err))
		}
	}

	// Start the control API.
	controlPort := os.Getenv("DPIVOT_CONTROL_PORT")
	if controlPort == "" {
		controlPort = "9900"
	}
	controlSrv := api.NewControlServer(reg, srv, log, m)
	go func() {
		if err := controlSrv.ListenAndServe(":" + controlPort); err != nil {
			log.Error("control API stopped", zap.Error(err))
		}
	}()

	log.Info("dpivot proxy started",
		zap.String("binds", bindsEnv),
		zap.String("control_port", controlPort))

	// Wait for SIGTERM / SIGINT, then drain in-flight connections gracefully.
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	<-ctx.Done()

	log.Info("proxy: SIGTERM received — draining in-flight connections")
	const drainTimeout = 30 * time.Second
	if err := srv.CloseGraceful(drainTimeout); err != nil {
		log.Warn("proxy: drain timeout — forcing close", zap.Error(err))
		srv.Close()
	}
	controlSrv.Close() //nolint:errcheck
	return nil
}

// ── version ───────────────────────────────────────────────────────────────────

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print dpivot version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("dpivot %s\n", version)
		},
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeComposeFile(path string, cf *compose.ComposeFile) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := yaml.NewEncoder(f)
	enc.SetIndent(4)
	return enc.Encode(cf)
}

func doGet(url string) (string, error) {
	resp, err := httpClient.Get(url) //nolint:noctx
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s: unexpected status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

var httpClient = &http.Client{Timeout: 5 * time.Second}
