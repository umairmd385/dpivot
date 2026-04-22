# HTTP Control API reference

The dpivot proxy exposes an HTTP API for backend management. This is what `dpivot rollout` calls internally to register new containers, drain old ones, and clean up. You can also call it directly from deployment scripts, monitoring tools, or any container on the `dpivot_mesh` network.

---

## Where to reach the API

The control API listens on port `9900` inside the proxy container (`DPIVOT_CONTROL_PORT`). dpivot maps this port to the Docker host so the `dpivot rollout` command can reach it without going inside a container.

The host port follows this formula: **`service_host_port + 6900`**

| Service host port | Control API on Docker host |
|------------------|---------------------------|
| `:3000` | `http://localhost:9900` |
| `:3001` | `http://localhost:9901` |
| `:8080` | `http://localhost:14980` |

You can always check the exact port mapping:

```bash
docker ps --filter "name=dpivot-proxy" --format "{{.Names}}: {{.Ports}}"
# testapp-dpivot-proxy-api-1: 0.0.0.0:3001->3001/tcp, 0.0.0.0:9901->9900/tcp
```

From another container on `dpivot_mesh`, use the proxy's service name directly:

```bash
curl http://dpivot-proxy-web:9900/backends
```

---

## Authentication

By default the API requires no authentication, but logs a warning at startup:

```
control API is unauthenticated — set DPIVOT_API_TOKEN to secure it
```

To enable bearer token auth, set `DPIVOT_API_TOKEN` on the proxy container:

```yaml
environment:
  DPIVOT_API_TOKEN: "my-secret-token"
```

Then include the token on every request:

```
Authorization: Bearer my-secret-token
```

`GET /health` is always unauthenticated even when a token is set.

---

## Endpoints

### `GET /health`

Liveness check. Returns the current backend count.

```bash
curl http://localhost:9901/health
```

```json
{
  "status": "ok",
  "backends": 2
}
```

---

### `GET /health/ready`

Readiness check. Returns `200` if at least one non-draining backend is registered, `503` otherwise. Use this in load balancer health checks.

```bash
curl http://localhost:9901/health/ready
```

```json
{ "status": "ready" }
```

---

### `GET /backends`

Lists all registered backends including draining ones. Useful for checking rollout state.

```bash
curl http://localhost:9901/backends
```

```json
{
  "count": 2,
  "backends": [
    {
      "id": "api-3a0b05299305",
      "addr": "172.20.0.6:3001",
      "draining": false,
      "requests": 1247
    },
    {
      "id": "api-default",
      "addr": "api:3001",
      "draining": true,
      "requests": 88
    }
  ]
}
```

`requests` is the total number of TCP connections routed to that backend since it was registered.

---

### `POST /backends`

Registers a new backend. The proxy starts routing new connections to it immediately.

```bash
curl -X POST http://localhost:9901/backends \
  -H 'Content-Type: application/json' \
  -d '{"id":"api-3a0b05299305","addr":"172.20.0.6:3001"}'
```

**Body:**

| Field | Required | Description |
|-------|----------|-------------|
| `id` | No (defaults to `addr`) | Unique identifier for this backend |
| `addr` | Yes | Dial address in `host:port` format |

**Responses:**

- `201 Created` — backend registered
- `400 Bad Request` — missing `addr` or invalid JSON
- `409 Conflict` — a backend with this `id` is already registered

---

### `PUT /backends/{id}/drain`

Marks a backend as draining. The router stops routing new connections to it, but existing connections continue until they finish naturally.

```bash
curl -X PUT http://localhost:9901/backends/api-3a0b05299305/drain
```

**Responses:**

- `204 No Content` — backend marked draining
- `404 Not Found` — no backend with this ID

After draining, wait for in-flight requests to complete, then call `DELETE /backends/{id}` to remove it.

---

### `DELETE /backends/{id}`

Removes a backend from the registry.

```bash
curl -X DELETE http://localhost:9901/backends/api-3a0b05299305
```

**Responses:**

- `204 No Content` — backend removed
- `404 Not Found` — no backend with this ID

---

### `GET /metrics`

Returns Prometheus-format metrics for the proxy.

```bash
curl http://localhost:9901/metrics
```

```
# HELP dpivot_connections_total Total TCP connections accepted
# TYPE dpivot_connections_total counter
dpivot_connections_total 1247

# HELP dpivot_connections_active Currently open connections
# TYPE dpivot_connections_active gauge
dpivot_connections_active 3

# HELP dpivot_connections_failed_total Connections that could not be proxied
# TYPE dpivot_connections_failed_total counter
dpivot_connections_failed_total 0

# HELP dpivot_backends_total Total registered backends
dpivot_backends_total 1

# HELP dpivot_backends_active Non-draining backends available for traffic
dpivot_backends_active 1
```

---

## Error responses

All errors return a structured JSON body:

```json
{
  "error": "backend \"api-abc\" is already registered",
  "code": "conflict"
}
```

**Error codes:**

| Code | HTTP status | When |
|------|------------|------|
| `invalid_body` | 400 | Bad JSON or missing required field |
| `missing_field` | 400 | `addr` not provided |
| `conflict` | 409 | Duplicate backend ID |
| `not_found` | 404 | Unknown backend ID |
| `method_not_allowed` | 405 | Wrong HTTP method for endpoint |
| `unauthorized` | 401 | No `Authorization` header when token is set |
| `forbidden` | 403 | Wrong token |
| `internal_error` | 500 | Unexpected error |

---

## Scripted rollout example

Here's the exact sequence `dpivot rollout` follows, expressed as shell commands, in case you want to wire it into your own CI/CD pipeline:

```bash
#!/bin/bash
SERVICE="api"
CONTROL="http://localhost:9901"   # 3001 + 6900
NEW_ADDR="172.20.0.6:3001"        # IP from docker inspect on dpivot_mesh
NEW_ID="${SERVICE}-$(date +%s)"

# 1. Register the new container — traffic splits between old and new immediately
curl -sf -X POST "$CONTROL/backends" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"${NEW_ID}\",\"addr\":\"${NEW_ADDR}\"}"

# 2. Drain the old backend — stop new connections, let existing ones finish
OLD_ID="${SERVICE}-default"
curl -sf -X PUT "$CONTROL/backends/${OLD_ID}/drain"
sleep 5   # drain window

# 3. Remove the old backend
curl -sf -X DELETE "$CONTROL/backends/${OLD_ID}"

echo "Rollout complete — $NEW_ADDR is the active backend."
```

---

## Canary rollout example

Because you can register multiple backends, you can do a canary deployment manually — send a fraction of traffic to the new version, watch for errors, then promote or roll back:

```bash
#!/bin/bash
NEW_ADDR="$1"
SERVICE="api"
CONTROL="http://localhost:9901"

# Register new container alongside the existing one (50/50 round-robin)
curl -sf -X POST "$CONTROL/backends" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"${SERVICE}-canary\",\"addr\":\"${NEW_ADDR}\"}"

echo "Canary live. Monitoring for 60 seconds..."
sleep 60

# Check error rate — plug in your monitoring here
ERROR_RATE=$(curl -sf "$CONTROL/metrics" | grep failed_total | awk '{print $2}')

if [ "${ERROR_RATE:-0}" -gt 10 ]; then
  echo "Error rate too high. Rolling back canary."
  curl -sf -X DELETE "$CONTROL/backends/${SERVICE}-canary"
  exit 1
fi

echo "Canary healthy. Draining old backend."
curl -sf -X PUT "$CONTROL/backends/${SERVICE}-default/drain"
sleep 10
curl -sf -X DELETE "$CONTROL/backends/${SERVICE}-default"
echo "Rollout complete."
```
