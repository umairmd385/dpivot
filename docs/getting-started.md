# Getting started with dpivot

This guide walks you from zero to a running zero-downtime rollout. Every command includes a recovery path for when things go wrong, because in real environments they will.

---

## Prerequisites

| Requirement | Check |
|---|---|
| Docker Engine 24+ or Docker Desktop | `docker --version` |
| Docker Compose v2 | `docker compose version` |
| Go 1.26 or newer | `go version` |
| Git | `git --version` |

If any of these fail, install them before continuing. dpivot will not work without Docker Compose v2 — the old `docker-compose` (v1) is not supported.

---

## Step 1 — Get the code

```bash
git clone https://github.com/umairmnd385/dpivot.git
cd dpivot
```

---

## Step 2 — Build the binary

```bash
make build
```

This produces `./bin/dpivot`. If `make` is not available, build directly:

```bash
go build -o ./bin/dpivot ./cmd/dpivot
```

Verify it works:

```bash
./bin/dpivot version
# dpivot 0.1.0
```

**If `go build` fails:** Check your Go version (`go version` must be 1.26+). Run `go mod download` first if you're behind a proxy.

---

## Step 3 — Understand what dpivot does to your compose file

dpivot reads your existing `docker-compose.yml` and produces an enhanced `dpivot-compose.yml`. The original file is never modified. You use the generated file with `docker compose` as you normally would.

For each HTTP/TCP service with port bindings:
- The service's ports are moved to a generated `dpivot-proxy-<service>` container
- The proxy forwards traffic and can hot-swap backends without dropping connections
- A `dpivot_mesh` bridge network connects the proxy to the backing service by IP

Services dpivot automatically leaves alone: database images (postgres, mysql, mongodb, redis, mariadb, elasticsearch), services with no ports, and services marked `x-dpivot: skip: true`.

---

## Step 4 — Generate the enhanced compose file

Navigate to your project directory (or use the included testapp):

```bash
cd examples/testapp
```

Copy the environment file:

```bash
cp .env.example .env
```

Generate:

```bash
../../bin/dpivot generate
```

You'll see a summary of what was transformed:

```
Parsed 4 service(s) — 2 eligible for proxy injection

dpivot Transform Summary:
  ✓ Enabling zero-downtime for service 'api'
  ✓ Enabling zero-downtime for service 'frontend'

Generated: dpivot-compose.yml
```

**If generate fails:**
- `no such file: docker-compose.yml` → you're in the wrong directory, or pass `--file path/to/compose.yml`
- `parse error` on a port like `"3000"` → dpivot expects `"hostPort:containerPort"` format; single-port shorthand is also accepted

Inspect the generated file to confirm port ownership moved to the proxy services:

```bash
grep -A5 "dpivot-proxy-api" dpivot-compose.yml
```

---

## Step 5 — Build the app images

```bash
docker compose build
```

This builds the images your app services need. If your images are already on Docker Hub or a registry, skip this — `docker compose pull` will fetch them when you start the stack.

**If build fails:** Check `docker compose build --no-cache` to rule out a stale layer. Read the error — it's almost always a missing dependency in your Dockerfile.

---

## Step 6 — Start the stack

```bash
docker compose -f dpivot-compose.yml up -d
```

On first run, Docker pulls `technicaltalk/dpivot-proxy:latest` from Docker Hub (once, then cached). Postgres and Redis base images are also pulled the first time.

Wait a few seconds then check all containers are up:

```bash
docker compose -f dpivot-compose.yml ps
```

You want to see all containers with `Up` or `Up (healthy)` status. If anything shows `Exit` or `Restarting`:

```bash
# See why a container failed to start
docker compose -f dpivot-compose.yml logs <service-name>
```

**Common startup failures and fixes:**

| Symptom | Cause | Fix |
|---|---|---|
| `api` exits immediately | Missing env vars | Check `.env` file; ensure all required vars are set |
| `db` never becomes healthy | Port conflict | Something else on port 5432; change in `docker-compose.yml` and regenerate |
| `dpivot-proxy-api` exits | `DPIVOT_BINDS` wrong | Check the generated compose file; re-run `dpivot generate` |
| Everything up but app returns 502 | App not listening on declared port | Verify your app actually binds to the port in `EXPOSE` |

Open the app in your browser once it's up. For the testapp: `http://localhost:3000`.

---

## Step 7 — Verify the proxy is working

Before running a rollout, confirm the proxy control API is reachable:

```bash
# Find the control port — it's host_port + 6900
# Example: api on port 3001 → control on port 9901
docker ps --filter "name=dpivot-proxy" --format "{{.Names}}: {{.Ports}}"
```

Example output:
```
testapp-dpivot-proxy-api-1: 0.0.0.0:3001->3001/tcp, 0.0.0.0:9901->9900/tcp
```

The right-hand port before `->9900/tcp` is your `--control-addr` port.

Query the proxy to see the registered backends:

```bash
../../bin/dpivot status --control-addr http://localhost:9901
```

You should see the seed backend (`api-default`) listed. If this command returns "connection refused":
1. Check the proxy container is running: `docker ps | grep dpivot-proxy`
2. Check the mapped port matches: `docker port testapp-dpivot-proxy-api-1`
3. Check the proxy logs: `docker logs testapp-dpivot-proxy-api-1`

---

## Step 8 — Run your first rollout

Build a new version of the service you want to deploy. For the testapp:

```bash
API_VERSION=2.0.0 docker compose build api
```

Then run the rollout (use the control port you found in Step 7):

```bash
../../bin/dpivot rollout api \
  --file dpivot-compose.yml \
  --control-addr http://localhost:9901
```

Watch the log output — it shows each step as it happens:

```
{"msg":"rollout: starting","service":"api","compose":"dpivot-compose.yml"}
{"msg":"rollout: scaling +1","service":"api"}
{"msg":"rollout: new container healthy","id":"3a0b052...","addr":"172.20.0.6:3001"}
{"msg":"rollout: new backend registered","backend_id":"api-3a0b052..."}
{"msg":"rollout: draining old connections","drain":"5s"}
{"msg":"rollout: removing old container","id":"1c9f2d1..."}
{"msg":"rollout: seed backend deregistered","id":"api-default"}
{"msg":"rollout: complete","service":"api"}
```

**If a rollout step fails, dpivot stops safely.** Traffic is never cut until the new container is confirmed healthy.

**Failure scenarios and what dpivot does:**

| When it fails | What dpivot does | What you do |
|---|---|---|
| Can't scale up (Docker error) | Stops immediately, original container still serving | Fix Docker issue, retry rollout |
| New container unhealthy (timeout) | Scales back to original count, exits | Check `docker logs` on the failed container, fix the bug, build again |
| Can't register new backend | Stops, original backend still active | Check control API is reachable (`dpivot status`) |
| Drain fails | Stops with error, old backend still registered | Run `dpivot rollback api` to restore clean state |
| Process killed mid-rollout | State saved in `/tmp/dpivot-api-state.json` | Run `dpivot rollback api` to restore previous version |

---

## Step 9 — Verify the rollout worked

Check the proxy now routes to the new backend:

```bash
../../bin/dpivot status --control-addr http://localhost:9901
```

The `api-default` seed backend should be gone; only the new IP-based backend should appear.

Verify traffic is going to the new container:

```bash
curl -s http://localhost:3001/api/version | python3 -m json.tool
```

---

## Step 10 — Roll back if something goes wrong

dpivot saves rollout state between steps. If the new deployment looks bad after the rollout completes (e.g., error rates spike, logs show crashes), restore the previous version:

```bash
../../bin/dpivot rollback api \
  --control-addr http://localhost:9901
```

This re-registers the old backend, drains the new one, and removes it. The state file is cleared on success.

**Rollback will fail if:**
- No rollout state exists (`/tmp/dpivot-api-state.json` was deleted or never written) — in that case you must do a manual rollout to the previous image
- The old container was already removed and its image isn't available — rebuild the old image and run rollout again

---

## Step 11 — Optional: Install as a Docker CLI plugin

```bash
sudo make install-plugin
```

After this, `docker dpivot` works everywhere Docker does:

```bash
docker dpivot rollout api \
  --file dpivot-compose.yml \
  --control-addr http://localhost:9901
```

Verify it's registered:

```bash
docker dpivot version
docker help | grep dpivot
```

---

## Applying dpivot to your own project

**1. Add healthchecks to your services**

dpivot waits for the new container's healthcheck to pass before switching traffic. Without a healthcheck, it waits for the container to be in `running` state — which may be before your app is actually ready to serve requests.

```yaml
healthcheck:
  test: ["CMD-SHELL", "wget -qO- http://localhost:3000/health || exit 1"]
  interval: 10s
  timeout: 5s
  retries: 3
  start_period: 20s
```

`start_period` is important — it's the grace period during which failed checks don't count. Set it longer than your app's cold-start time.

**2. Use `x-dpivot: skip: true` for stateful services**

dpivot auto-detects common databases. For anything else that should not be proxied:

```yaml
services:
  legacy-cache:
    image: custom-cache:latest
    x-dpivot:
      skip: true
```

**3. Know the control port formula**

The proxy control API is always mapped to the host at `service_host_port + 6900`. If your service's host port changes, regenerate the compose file and restart the proxy.

| Service host port | Control API port |
|---|---|
| 3000 | 9900 |
| 3001 | 9901 |
| 8080 | 14980 |
| 80 | 6980 |

**4. Rebuild and rollout for every deploy**

The pattern for every deployment is:

```bash
# 1. Build the new image with a new version tag
docker build -t myapp:2.0.0 .

# 2. Update your compose file to reference the new tag, then rollout
dpivot rollout myservice \
  --file dpivot-compose.yml \
  --control-addr http://localhost:$(( SERVICE_PORT + 6900 ))
```

Or use `--pull` to let dpivot pull the latest tag automatically (for `latest`-tagged images):

```bash
dpivot rollout myservice --pull --file dpivot-compose.yml --control-addr http://localhost:9901
```

---

## Diagnosing issues without a testapp

When something is broken, work through this checklist in order:

```bash
# 1. Are all containers up?
docker compose -f dpivot-compose.yml ps

# 2. What are the proxy containers exposing?
docker ps --filter "name=dpivot-proxy" --format "{{.Names}}: {{.Ports}}"

# 3. What backends does the proxy know about?
dpivot status --control-addr http://localhost:<control-port>

# 4. Can the proxy reach the backing service?
docker exec testapp-dpivot-proxy-api-1 wget -qO- http://api:3001/health

# 5. What does the proxy log say?
docker logs testapp-dpivot-proxy-api-1 --tail 50

# 6. Is there a stale rollout lock?
ls /tmp/dpivot-*.lock
# Remove if stale (process is dead):
rm /tmp/dpivot-api.lock

# 7. Is there rollout state from a previous incomplete rollout?
cat /tmp/dpivot-api-state.json
# If it exists and the rollout is not in progress, run rollback:
dpivot rollback api --control-addr http://localhost:9901
```

---

## Next steps

- [docs/how-it-works.md](how-it-works.md) — full lifecycle of a rollout and how the proxy works internally
- [docs/configuration.md](configuration.md) — all flags, environment variables, and network layout
- [docs/control-api.md](control-api.md) — REST API reference for the proxy control plane
