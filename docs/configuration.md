# Configuration reference

All dpivot configuration is done via CLI flags and environment variables. There's no config file — the `dpivot-compose.yml` output and the proxy container's environment are the only persistent state.

---

## CLI flags

### `dpivot generate`

| Flag | Default | Description |
|------|---------|-------------|
| `--file`, `-f` | `docker-compose.yml` | Input compose file path |
| `--output`, `-o` | `dpivot-compose.yml` | Output compose file path |

### `dpivot rollout`

| Flag | Default | Env override | Description |
|------|---------|--------------|-------------|
| `--file`, `-f` | `dpivot-compose.yml` | — | dpivot compose file |
| `--pull` | `false` | — | Pull image before rollout |
| `--timeout`, `-t` | `60s` | — | Max wait for healthcheck |
| `--drain`, `-d` | `5s` | — | Drain window before deregistering old backend |
| `--control-addr` | `http://localhost:9900` | — | Proxy control API address |
| `--api-token` | `""` | `DPIVOT_API_TOKEN` | Bearer token for control API |

### `dpivot status`

| Flag | Default | Description |
|------|---------|-------------|
| `--control-addr` | `http://localhost:9900` | Proxy control API address |

---

## Proxy container environment variables

These are set automatically by `dpivot generate` based on the service's port mappings. You can override them in the generated file if needed, but regenerating will reset them.

| Variable | Example | Description |
|----------|---------|-------------|
| `DPIVOT_BINDS` | `3000:3000,8080:8080` | Comma-separated `listenPort:targetPort` pairs |
| `DPIVOT_TARGETS` | `web-default:web:3000` | Initial backend: `id:host:port` |
| `DPIVOT_CONTROL_PORT` | `9900` | HTTP control API port (default: `9900`) |
| `DPIVOT_API_TOKEN` | _(empty)_ | Bearer token for API auth. Unset → unauthenticated + warning |

The proxy reads these at startup. They're static after that — the control API handles all runtime changes.

---

## `x-dpivot` service extension

Add an `x-dpivot` block to any service to override auto-detection. The block is stripped from the generated file.

```yaml
services:
  myservice:
    image: myapp:latest
    ports:
      - "8080:8080"
    x-dpivot:
      skip: true    # pass through, do not inject proxy
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `skip` | bool | `false` | If `true`, service is passed through unchanged regardless of image or ports |

---

## Generated compose file structure

`dpivot generate` outputs a standalone file. It doesn't use `include:` or `extends:` — all services are written inline. This makes it self-contained and safe to commit.

Fields dpivot adds or modifies per service:

**On backing services (proxy-injected):**
- `ports` removed → moved to the proxy service
- `expose` list extended with the container port numbers
- `networks` extended with `dpivot_mesh`
- `environment` extended with `DPIVOT_BACKEND`
- `labels` extended with `dpivot.managed`, `dpivot.service`

**Proxy services (new, named `dpivot-proxy-<service>`):**
- `image: dpivot/proxy:latest`
- `ports` from the backing service
- `expose: ["9900"]` for the control API
- `networks: [dpivot_mesh]`
- `depends_on: [<service>]`
- `environment`: `DPIVOT_BINDS`, `DPIVOT_TARGETS`, `DPIVOT_CONTROL_PORT`
- `labels`: `dpivot.proxy: "true"`, `dpivot.service`, `dpivot.managed`
- `restart: unless-stopped`

**Fields that are always preserved verbatim:**
`healthcheck`, `volumes`, `restart`, `deploy`, `ulimits`, `user`, `working_dir`, `entrypoint`, `command`, and any other field not listed above.

---

## Database images excluded from proxy injection

Match is against the rightmost path segment of the image name, after stripping the tag. Case-insensitive.

```
postgres    postgresql  mysql      mariadb    redis
mongo       mongodb     elasticsearch  opensearch cassandra
couchdb     influxdb    rabbitmq   kafka      zookeeper
mssql       clickhouse  minio      vault
```

Examples that match: `postgres:16`, `docker.io/library/mysql:8.0`, `bitnami/redis:7`.

To proxy a database image explicitly (not recommended), set `x-dpivot: skip: false` — note this does nothing because skip only suppresses, not forces. The detector takes priority. For now, databases must be opted in manually by removing the `x-dpivot` block, and this is intentional.

---

## Network: `dpivot_mesh`

A bridge network created automatically by `dpivot generate`. All proxy containers and their corresponding backing services join this network. It provides:

- DNS-based service discovery (containers reach each other by service name)
- Isolation from other stacks running on the same host
- A private channel for the control API (port 9900 is not exposed externally)

If your services already define networks, they're preserved. dpivot adds `dpivot_mesh` to the list for any proxied service.

---

## Building the proxy image

The proxy image runs the `dpivot proxy` command internally — it's the same binary as the CLI:

```bash
make docker-build
# builds dpivot/proxy:latest from docker/proxy/Dockerfile
```

The Dockerfile uses a multi-stage build with a `scratch` final image. The resulting image is just the static binary and nothing else.

If you're running in a private registry:

```bash
# Override the image in the generated file
sed -i 's|dpivot/proxy:latest|registry.example.com/dpivot/proxy:0.1.0|g' dpivot-compose.yml
```

Or set the image name before generating (a `--proxy-image` flag is planned).
