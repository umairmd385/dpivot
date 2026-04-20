# dpivot

> Zero-downtime deployments for Docker Compose — no external proxy required.

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Tests](https://img.shields.io/badge/tests-71%20passing-brightgreen)]()
[![Race Detector](https://img.shields.io/badge/race--clean-✓-brightgreen)]()

`dpivot` is a single Go binary that you drop next to your existing `docker-compose.yml`. It reads the file, injects its own built-in TCP proxy, and writes a new `dpivot-compose.yml`. No Traefik. No nginx. No Kubernetes. Just run the enhanced stack and your host port never goes dark again — even during deployments.

---

## The problem it solves

When you `docker compose up --force-recreate web`, Docker stops the old container, then starts the new one. That gap — usually under a second — is enough to drop HTTP connections, fail health checks in load balancers, and interrupt WebSocket sessions. The standard fix is to add Traefik or nginx as a reverse proxy, configure labels, and hope you got the sticky sessions right.

dpivot takes a different approach: it owns the host port from the first `docker compose up` and **never releases it**. Container replacement is invisible to the network because the proxy is still there, still accepting connections, routing around whatever is happening underneath.

---

## Quick start

```bash
# Install
go install github.com/dpivot/dpivot/cmd/dpivot@latest

# Point at your existing docker-compose.yml
dpivot generate

# Start the enhanced stack
docker compose -f dpivot-compose.yml up -d

# Deploy a new version of a service
dpivot rollout web
```

That's the whole workflow.

---

## How it works

![dpivot architecture](docs/architecture.svg)

Take any standard compose file:

```yaml
# docker-compose.yml — unchanged, original file
services:
  web:
    image: myapp:1.0
    ports:
      - "3000:3000"
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: secret
```

Run `dpivot generate`. It reads each service and applies four rules in order:

1. `x-dpivot: skip: true` → pass through unchanged
2. No `ports` → pass through (workers, sidecars, etc.)
3. Recognised database image → pass through with a warning
4. Everything else with ports → inject proxy

```
Parsed 2 service(s) — 1 eligible for proxy injection

dpivot Transform Summary:
  ✓ Enabling zero-downtime for service 'web'
  ⚠ Skipped 'db' (known database image)

Generated: dpivot-compose.yml
```

The generated file rewires things so the proxy owns the host port and `web` is only reachable on the internal `dpivot_mesh` bridge network:

```
Client :3000 → dpivot-proxy-web (permanent host port) → web:3000 (replaceable)
```

When you `dpivot rollout web`:

1. A second `web` container starts
2. dpivot waits for its healthcheck to pass
3. The new container is registered with the proxy via `POST /backends`
4. The old container is marked draining — no new connections
5. After a short drain window, the old container is deregistered and stopped

Your clients see nothing. The port never dropped.

---

## Installation

**From source (recommended until a release is tagged):**

```bash
git clone https://github.com/dpivot/dpivot.git
cd dpivot
make build
# binary is at ./bin/dpivot
```

**As a Docker CLI plugin:**

```bash
make install-plugin
# now works as: docker dpivot rollout web
```

**Verify:**

```bash
dpivot version
dpivot --help
```

---

## Commands

### `dpivot generate`

Reads `docker-compose.yml` and writes `dpivot-compose.yml`. The original file is never modified.

```bash
dpivot generate
dpivot generate --file docker-compose.prod.yml --output dpivot-compose.prod.yml
```

**Options:**

| Flag | Default | Description |
|------|---------|-------------|
| `--file`, `-f` | `docker-compose.yml` | Input compose file |
| `--output`, `-o` | `dpivot-compose.yml` | Output file path |

---

### `dpivot rollout <service>`

Zero-downtime rolling update for a named service.

```bash
dpivot rollout web
dpivot rollout web --pull --timeout 120s --drain 10s
```

**Options:**

| Flag | Default | Description |
|------|---------|-------------|
| `--file`, `-f` | `dpivot-compose.yml` | dpivot compose file |
| `--pull` | `false` | Pull latest image before starting |
| `--timeout`, `-t` | `60s` | Healthcheck wait timeout |
| `--drain`, `-d` | `5s` | Drain window before removing old container |
| `--control-addr` | `http://localhost:9900` | Proxy control API address |
| `--api-token` | `$DPIVOT_API_TOKEN` | Bearer token for the control API |

If the service isn't found in the compose file, you get a clear message:

```
Error: service "web" not found in dpivot-compose.yml
(did you run dpivot generate first?)
```

---

### `dpivot status`

Queries the proxy control API and shows the current backend state.

```bash
dpivot status
dpivot status --control-addr http://localhost:9901
```

---

### `dpivot version`

```bash
dpivot version
# dpivot 0.1.0
```

---

## Docker CLI plugin

After `make install-plugin`, dpivot works as a native Docker CLI plugin:

```bash
docker dpivot generate
docker dpivot rollout web
docker dpivot status
docker dpivot --help
```

The plugin binary is the same as the standalone `dpivot` binary. Mode is detected automatically from `argv[0]`.

---

## Auto-detection rules

dpivot applies these rules per service, stopping at the first match:

| Condition | What happens |
|-----------|-------------|
| `x-dpivot: skip: true` | Passed through exactly as-is |
| No `ports` declared | Passed through (sidecar, worker, etc.) |
| Image matches database list | Passed through with a log warning |
| Everything else with ports | Proxy injected |

**Database images that are never proxied by default:**

`postgres`, `postgresql`, `mysql`, `mariadb`, `redis`, `mongo`, `mongodb`,
`elasticsearch`, `opensearch`, `cassandra`, `couchdb`, `influxdb`, `rabbitmq`,
`kafka`, `zookeeper`, `mssql`, `clickhouse`, `minio`, `vault`

Matching strips registry prefixes and tags — `docker.io/library/postgres:16` and `bitnami/postgres:latest` both match.

---

## Opting out of a specific service

```yaml
services:
  admin:
    image: myapp:latest
    ports:
      - "9000:9000"
    x-dpivot:
      skip: true    # keep port on the service, no proxy injected
```

The `x-dpivot` block is stripped from the generated file. Docker never sees it.

---

## Multi-port services

Multiple ports work natively — one proxy listener per port:

```yaml
services:
  frontend:
    image: nginx:alpine
    ports:
      - "80:80"
      - "443:443"
```

After `dpivot generate`:

```
dpivot-proxy-frontend owns :80 and :443
frontend expose-only on dpivot_mesh: 80, 443
DPIVOT_BINDS = "80:80,443:443"
```

---

## HTTP control API

The proxy runs an HTTP control API on port `9900` (internal only — on `dpivot_mesh`, not exposed to the Docker host by default).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Liveness check, returns backend count |
| `GET` | `/backends` | List all registered backends with request counts |
| `POST` | `/backends` | Register a new backend `{"id":"...","addr":"host:port"}` |
| `PUT` | `/backends/{id}/drain` | Mark backend as draining (no new connections) |
| `DELETE` | `/backends/{id}` | Drain then deregister backend |

**Optional authentication:** Set `DPIVOT_API_TOKEN` on the proxy container. Requests must include `Authorization: Bearer <token>`. Without the env var, the API works but logs a warning at startup.

---

## Examples

| Example | What it shows |
|---------|--------------|
| [`examples/basic/`](examples/basic/) | Minimal single-service app |
| [`examples/advanced/`](examples/advanced/) | x-dpivot skip, multi-service stack |
| [`examples/multi-port/`](examples/multi-port/) | Multiple host ports on one service |
| [`examples/generated/`](examples/generated/) | Annotated generated file output |

---

## Documentation

| Doc | Description |
|-----|-------------|
| [docs/how-it-works.md](docs/how-it-works.md) | Step-by-step deployment lifecycle |
| [docs/control-api.md](docs/control-api.md) | Full HTTP control API reference |
| [docs/configuration.md](docs/configuration.md) | All environment variables and flags |

---

## Comparison

|  | docker-rollout | Dokku | **dpivot** |
|--|---------------|-------|-----------|
| Works with existing `docker-compose.yml` | ✅ | ❌ | ✅ |
| No external proxy needed | ❌ Requires Traefik | ❌ Built-in nginx | ✅ Built-in |
| Host port stays live during rollout | ❌ | ✅ | ✅ |
| HTTP backend management API | ❌ | ❌ | ✅ |
| Database auto-exclusion | ❌ | ❌ | ✅ |
| No server/root required | ✅ | ❌ | ✅ |
| Docker CLI plugin | ❌ | ❌ | ✅ |

docker-rollout requires Traefik or nginx-proxy already running — that's their own documented caveat. dpivot includes the proxy, so you don't need to bring your own.

---

## Technical details

- **TCP proxy:** pure `net` package, no CGO, no system dependencies
- **Round-robin:** lock-free `atomic.Uint64` counter, deterministic backend ordering
- **Registry:** `sync.RWMutex` + heap-allocated `*atomic.Uint64` per backend (race-safe struct copies)
- **Port ownership:** listener opened once at `docker compose up`, never closed during rollouts
- **Half-close:** bidirectional `io.Copy` with `CloseWrite()` for correct TCP teardown
- **Tests:** 71 unit tests, race detector clean across 10 consecutive runs

---

## Building from source

```bash
make build              # ./bin/dpivot
make test               # go test -race ./...
make docker-build       # dpivot/proxy:latest
make install-plugin     # ~/.docker/cli-plugins/docker-dpivot
make lint               # golangci-lint run
```

---

## License

MIT. See [LICENSE](LICENSE).
