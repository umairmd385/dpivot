# How dpivot works

This document walks through the full lifecycle of a rollout — from `dpivot generate` to the moment the old container is gone and your logs show zero dropped connections.

---

## The core idea

Normally when you update a Docker Compose service the sequence is:

```
Old container running → docker compose up → Old stops → New starts → New running
```

The gap between "old stops" and "new running" is where connections die. Even with `--no-deps` and fast image pulls, there's always a moment where the host port belongs to nobody.

dpivot inserts a proxy between the host port and your containers:

```
Host port :3000 → [dpivot-proxy-web] → web:3000 (internal, replaceable)
```

The proxy never stops. The host port is never released. What changes during a rollout is which container is registered as the proxy's backend — and that update is atomic from the client's perspective.

---

## Step 1: `dpivot generate`

You run `dpivot generate` once. It reads your `docker-compose.yml` and outputs `dpivot-compose.yml`.

For each service it applies four rules in order:

1. Has `x-dpivot: skip: true` → leave it alone
2. No `ports` → leave it alone
3. Image is a known database → leave it alone, log a warning
4. Has `ports` and not a database → inject proxy

For an eligible service like this:

```yaml
services:
  web:
    image: myapp:1.0
    ports:
      - "3000:3000"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/health"]
      interval: 10s
      timeout: 3s
      retries: 3
```

The generated output creates two services:

**`web` (backing service):**
- `ports` removed — it only listens on the internal `dpivot_mesh` network
- `expose: ["3000"]` so Docker documents the port without binding it to the host
- `networks` extended with both `dpivot_mesh` and `default` (so it can still reach unproxied services like databases)
- `DPIVOT_BACKEND: "web:3000"` env var for introspection
- `dpivot.managed: "true"` label so tooling can identify it

**`dpivot-proxy-web` (injected proxy):**
- Owns `ports: ["3000:3000"]` — the host-facing traffic binding
- Also maps the control API to the host: `ports: ["9900:9900"]` (formula: `service_host_port + 6900`)
- Image: `technicaltalk/dpivot-proxy:latest`
- Configured via env vars: `DPIVOT_BINDS`, `DPIVOT_TARGETS`, `DPIVOT_CONTROL_PORT`
- Joins `dpivot_mesh` — the internal bridge network shared with `web`

---

## Step 2: The proxy starts

When you run `docker compose -f dpivot-compose.yml up -d`, the proxy container starts and reads:

- `DPIVOT_BINDS=3000:3000` — listen on host port 3000, forward to container port 3000
- `DPIVOT_TARGETS=web-default:web:3000` — initial backend: the `web` container at `web:3000` on the mesh network
- `DPIVOT_CONTROL_PORT=9900` — HTTP control API port (mapped to the host at `service_host_port + 6900`)

It opens a TCP listener on `:3000` and immediately starts accepting connections. It will hold that listener until the proxy container itself stops.

---

## Step 3: Traffic flows

```
Client → :3000 → proxy accepts connection
                       ↓
               router.Next() picks a backend (round-robin across non-draining backends)
                       ↓
               dial backend.Addr (e.g. "172.20.0.3:3000" on dpivot_mesh)
                       ↓
               bidirectional io.Copy — data flows in both directions simultaneously
                       ↓
               when either side closes, CloseWrite() the other (correct TCP half-close)
```

Each connection is handled in its own goroutine. The proxy itself is stateless — it just pipes bytes.

---

## Step 4: `dpivot rollout web`

You push a new image. You run `dpivot rollout web`. Here's exactly what happens:

```
1.  dpivot reads dpivot-compose.yml → confirms service 'web' exists
2.  Optional: docker compose pull web
3.  docker compose up -d --no-deps --scale web=2 web
    → Docker starts a second 'web' container alongside the first
4.  dpivot polls docker ps every 2 seconds waiting for the new container's
    healthcheck to pass (or --timeout to expire)
5.  docker inspect the new container → pick its IP on dpivot_mesh, get its exposed port
    Result: addr = "172.20.0.6:3000"
6.  POST http://localhost:9900/backends {"id":"web-<id>","addr":"172.20.0.6:3000"}
    → proxy immediately starts routing new connections to both old and new
7.  PUT http://localhost:9900/backends/web-<old-id>/drain
    → old backend marked draining — no new connections routed to it
8.  Wait --drain (default 5s) for in-flight requests on the old container to complete
9.  DELETE http://localhost:9900/backends/web-<old-id>
    → old backend removed from registry
10. docker stop <old-container-id> && docker rm <old-container-id>
    → old container removed directly (keeps the new container as the active one)
```

Total downtime for your clients: zero. Between steps 6 and 7 there are briefly two backends active — both the old and new container serve traffic. After step 7, only the new one gets new connections, while existing connections on the old one complete naturally.

> **Why stop the old container directly instead of `--scale=1`?**
> Docker Compose's `--scale service=1` removes the *newest* container and keeps the oldest. That's exactly backwards from what we want. dpivot explicitly stops and removes the old container by ID, leaving the new one running.

---

## The backend registry

The proxy maintains an in-memory backend registry. Every registered backend has:

- `id` — caller-supplied identifier (used for drain and remove)
- `addr` — the dial address (`host:port`) — always an IP, not a DNS name, to avoid stale cache issues
- `draining` — bool; draining backends are skipped by the router
- `requests` — running count of connections routed to this backend

The registry is protected by `sync.RWMutex`. The request counter is a `*atomic.Uint64` on the heap — the pointer is copied when the router takes a snapshot, not the counter itself. This avoids a data race that would otherwise occur when the router increments a counter while another goroutine is copying the backend struct.

---

## The router

The router uses a global `atomic.Uint64` counter and modulo-N selection over a sorted snapshot of active backends:

```go
n := r.counter.Add(1) - 1
b := &active[int(n) % len(active)]
```

Backends are sorted by ID before selection so the distribution is deterministic regardless of Go map iteration order. If all backends are draining, `Next()` returns an error and the connection is dropped with a log warning — never panics.

---

## The control API

All backend management during a rollout goes through the proxy's HTTP control API. The API listens on port 9900 inside the container and is mapped to the Docker host at `service_host_port + 6900`:

- Service at `:3000` → `http://localhost:9900`
- Service at `:3001` → `http://localhost:9901`
- Service at `:8080` → `http://localhost:14980`

If you set `DPIVOT_API_TOKEN`, the proxy requires an `Authorization: Bearer <token>` header on all backend management endpoints. `/health` is always unauthenticated.

---

## What happens if the rollout fails

If the new container fails its healthcheck before `--timeout` expires, dpivot:

1. Logs the failure with the container ID and last health status
2. Scales back down (removes the extra replica)
3. Returns a non-zero exit code

The proxy is left running with the original backend. Traffic was never interrupted.

---

## Network layout

All proxied services and their corresponding proxy containers join `dpivot_mesh`. Proxied backing services also keep the `default` network so they can still reach unproxied services (databases, caches, etc.) by hostname.

```
Docker host
├── :3000 → dpivot-proxy-web (dpivot_mesh)
│           ↓
│      web container (dpivot_mesh + default, expose 3000, no host binding)
│           ↓
│      db container (default only — not proxied, reachable by "db" DNS name)
│
├── :9900 → dpivot-proxy-web control API (mapped to host for dpivot rollout)
│
└── :5432 → db container (direct, postgres auto-excluded)
```

The `dpivot_mesh` bridge network is created automatically by `dpivot generate`. You can define additional networks on your services — they're preserved verbatim in the generated file.
