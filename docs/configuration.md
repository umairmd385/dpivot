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
| `--timeout`, `-t` | `60s` | — | Max wait for new container's healthcheck |
| `--drain`, `-d` | `5s` | — | Drain window before removing old backend |
| `--control-addr` | `http://localhost:9900` | — | Proxy control API address on the Docker host |
| `--api-token` | `""` | `DPIVOT_API_TOKEN` | Bearer token for control API |

> **Finding `--control-addr` for your service:** The proxy maps its control API to the host at `service_host_port + 6900`. If your service's host port is `3001`, the control API is at `http://localhost:9901`. If it's `3000`, it's at `http://localhost:9900`.

### `dpivot rollback`

| Flag | Default | Description |
|------|---------|-------------|
| `--control-addr` | `http://localhost:9900` | Proxy control API address |
| `--api-token` | `""` | Bearer token if auth is enabled |
| `--drain`, `-d` | `5s` | Drain window before removing new backend |

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
| `DPIVOT_CONTROL_PORT` | `9900` | HTTP control API port inside the container (default: `9900`) |
| `DPIVOT_API_TOKEN` | _(empty)_ | Bearer token for API auth. Unset → unauthenticated + startup warning |

The proxy reads these at startup. They're static after that — the control API handles all runtime changes.

---

## Port mapping convention

dpivot maps the proxy's control API port to the Docker host so that `dpivot rollout` can reach it without going inside a container. The host port is calculated as:

```
control_host_port = first_service_host_port + 6900
```

Examples:
- Service at `3000:3000` → control at `http://localhost:9900` (3000 + 6900)
- Service at `3001:3001` → control at `http://localhost:9901` (3001 + 6900)
- Service at `8080:8080` → control at `http://localhost:14980` (8080 + 6900)

The generated compose file expresses this as an additional `ports` entry on each proxy service, for example:

```yaml
dpivot-proxy-api:
  ports:
    - "3001:3001"   # traffic
    - "9901:9900"   # control API mapped to host
```

---

## `x-dpivot` service extension

Add an `x-dpivot` block to any service to override auto-detection. The block is stripped from the generated file so Docker never sees it.

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

`dpivot generate` outputs a standalone file. It doesn't use `include:` or `extends:` — all services are written inline. This makes it self-contained and safe to commit (though the generated files in `examples/` are excluded from the repo by `.gitignore` since they can always be regenerated).

Fields dpivot adds or modifies per service:

**On backing services (proxy-injected):**
- `ports` removed → moved to the proxy service
- `expose` list extended with the container port numbers
- `networks` extended with `dpivot_mesh` and `default` (default preserves connectivity to unproxied services)
- `environment` extended with `DPIVOT_BACKEND`
- `labels` extended with `dpivot.managed`, `dpivot.service`

**Proxy services (new, named `dpivot-proxy-<service>`):**
- `image: technicaltalk/dpivot-proxy:latest`
- `ports`: original host port bindings + control API host mapping (`service_port + 6900`)
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

If you need to force a database to be proxied anyway, that is not currently supported — the detector takes priority over all other rules. Use `x-dpivot: skip: true` to keep the service untouched and manage its port binding yourself.

---

## Network: `dpivot_mesh`

A bridge network created automatically by `dpivot generate`. All proxy containers and their corresponding backing services join this network. It provides:

- DNS-based service discovery between proxy and backing containers
- Isolation from other stacks running on the same host
- A private channel for the control API (port 9900 inside the mesh)

Backing services also retain the `default` network (the implicit network Docker Compose creates for every project) so they can reach databases and other unproxied services by hostname, the same way they did before dpivot was introduced.

---

## The proxy image

The proxy runs `technicaltalk/dpivot-proxy:latest` — this is the same dpivot binary compiled as a Docker image. It's available on Docker Hub and pulled automatically when you start the generated stack.

To build it locally instead (for private registries or customisation):

```bash
make docker-build
# builds technicaltalk/dpivot-proxy:latest from the root Dockerfile
```

If you're running in a private registry, update the image name in the generated file:

```bash
sed -i 's|technicaltalk/dpivot-proxy:latest|registry.example.com/dpivot/proxy:0.1.0|g' dpivot-compose.yml
```

A `--proxy-image` flag to set this at generate time is planned.
