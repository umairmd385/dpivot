# HTTP Control API reference

The dpivot proxy exposes an HTTP API for backend management. It listens on the port set by `DPIVOT_CONTROL_PORT` (default: `9900`) and is reachable only on the `dpivot_mesh` bridge network — not from the Docker host.

The rollout command uses this API automatically. You can also call it directly from deployment scripts or from inside another container on the same network.

---

## Authentication

Set `DPIVOT_API_TOKEN` on the proxy container to enable bearer token authentication:

```yaml
environment:
  DPIVOT_API_TOKEN: "my-secret-token"
```

Authenticated requests must include:

```
Authorization: Bearer my-secret-token
```

Without the env var, the API works without auth but logs a warning at startup:

```
control API is unauthenticated — set DPIVOT_API_TOKEN to secure it
```

`GET /health` is always unauthenticated regardless of `DPIVOT_API_TOKEN`.

---

## Endpoints

### `GET /health`

Liveness check. Returns the current backend count.

```bash
curl http://localhost:9900/health
```

```json
{
  "status": "ok",
  "backends": 2
}
```

---

### `GET /backends`

Lists all registered backends including draining ones.

```bash
curl http://localhost:9900/backends
```

```json
{
  "count": 2,
  "backends": [
    {
      "id": "web-a1b2c3d4e5f6",
      "addr": "172.20.0.3:3000",
      "draining": false,
      "requests": 1247
    },
    {
      "id": "web-deadbeef1234",
      "addr": "172.20.0.5:3000",
      "draining": true,
      "requests": 88
    }
  ]
}
```

`requests` is the total number of TCP connections that have been routed to that backend since it was registered.

---

### `POST /backends`

Registers a new backend. The proxy starts routing new connections to it immediately.

```bash
curl -X POST http://localhost:9900/backends \
  -H 'Content-Type: application/json' \
  -d '{"id":"web-a1b2c3d4e5f6","addr":"172.20.0.5:3000"}'
```

**Body:**

| Field | Required | Description |
|-------|----------|-------------|
| `id` | No — defaults to `addr` | Unique identifier for this backend |
| `addr` | Yes | Dial address: `host:port` |

**Responses:**

- `201 Created` — backend registered, body contains the registered backend object
- `400 Bad Request` — missing `addr` or invalid JSON
- `409 Conflict` — a backend with this `id` is already registered

---

### `PUT /backends/{id}/drain`

Marks a backend as draining. The router stops sending new connections to it, but existing connections continue until they complete naturally.

```bash
curl -X PUT http://localhost:9900/backends/web-a1b2c3d4e5f6/drain
```

**Responses:**

- `204 No Content` — backend marked draining
- `404 Not Found` — no backend with this ID

After draining, wait for in-flight connections to finish, then call `DELETE /backends/{id}` to remove it.

---

### `DELETE /backends/{id}`

Removes a backend. The proxy first marks it draining (stopping new connections), then removes it from the registry.

```bash
curl -X DELETE http://localhost:9900/backends/web-a1b2c3d4e5f6
```

**Responses:**

- `204 No Content` — backend removed
- `404 Not Found` — no backend with this ID

---

## Error responses

All errors return a structured JSON body:

```json
{
  "error": "backend \"web-abc\" is already registered",
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

## Calling the API from deployment scripts

From the Docker host (the API is on the internal mesh network, not exposed externally):

```bash
# Use docker exec to reach the API from inside the proxy container
docker exec dpivot-proxy-web curl -s http://localhost:9900/backends

# Or use docker network connect and call from another container
# Or expose 9900 temporarily in dpivot-compose.yml for debugging
```

From another container on `dpivot_mesh`:

```bash
# The proxy container's DNS name on the mesh network is "dpivot-proxy-web"
curl http://dpivot-proxy-web:9900/backends
```

---

## Scripted canary rollout example

```bash
#!/bin/bash
# Simple canary: send 10% of traffic, monitor, then promote or rollback

NEW_CONTAINER_IP="$1"
SERVICE="web"
CONTROL="http://localhost:9900"

# Register new container (now both old and new are active — round-robin splits traffic)
curl -sf -X POST "$CONTROL/backends" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"${SERVICE}-canary\",\"addr\":\"${NEW_CONTAINER_IP}:3000\"}"

echo "Canary is live. Monitoring for 60 seconds..."
sleep 60

# Check error rate on canary (your monitoring here)
ERROR_RATE=$(check_error_rate "${SERVICE}-canary")

if [ "$ERROR_RATE" -gt 5 ]; then
  echo "Error rate too high ($ERROR_RATE%). Rolling back."
  curl -sf -X DELETE "$CONTROL/backends/${SERVICE}-canary"
  exit 1
fi

echo "Canary healthy. Draining old backend."
curl -sf -X PUT "$CONTROL/backends/${SERVICE}-default/drain"
sleep 10
curl -sf -X DELETE "$CONTROL/backends/${SERVICE}-default"
echo "Rollout complete."
```
