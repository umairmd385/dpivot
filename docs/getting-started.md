# Getting started with dpivot

This guide takes you from zero to your first zero-downtime deployment in about 15 minutes. By the end you'll have a real multi-service app running behind the dpivot proxy, and you'll watch a version update happen live in the browser with zero dropped connections.

---

## What you need before starting

- **Docker and Docker Compose** — Docker Desktop works fine, or Docker Engine on Linux
- **Go 1.26 or newer** — needed to build dpivot from source. Check with `go version`.
- **Git** — to clone the repo

That's it. No cloud account, no Kubernetes, no extra software.

---

## Step 1 — Get the code

```bash
git clone https://github.com/dpivot/dpivot.git
cd dpivot
```

---

## Step 2 — Build dpivot

```bash
make build
```

This compiles the dpivot binary and puts it at `./bin/dpivot`. The binary is self-contained — it's the CLI tool, the Docker plugin, and the TCP proxy all in one file.

Verify it works:

```bash
./bin/dpivot version
# dpivot 0.1.0

./bin/dpivot --help
```

If you want to use `dpivot` from anywhere without typing `./bin/dpivot`, either add the `bin/` directory to your `PATH`, or install it as a Docker CLI plugin (covered in [Step 7](#step-7--optional-install-as-a-docker-cli-plugin)).

---

## Step 3 — Meet the test app

The repo includes a fully working microservice app at `examples/testapp/` specifically designed to demonstrate dpivot rollouts. It has:

- A **Node.js API** (port 3001) with Postgres + Redis
- A **Node.js frontend** (port 3000) with a live version monitor that updates every 2 seconds
- Both services produce **deployment markers** — when a new version starts, it writes a visible record to the database so you can see the exact moment traffic switched

Set it up:

```bash
cd examples/testapp
cp .env.example .env
docker compose build
```

This builds the `dpivot-testapp-api:1.0.0` and `dpivot-testapp-frontend:1.0.0` images. It takes a minute on first run while it downloads Node.js dependencies.

---

## Step 4 — Generate the dpivot-enhanced compose file

The test app ships with a standard `docker-compose.yml`. This file works with plain `docker compose` but doesn't have any rollout capability. dpivot transforms it:

```bash
../../bin/dpivot generate
```

> If you added `bin/` to your PATH: `dpivot generate`

You'll see output like this:

```
Parsed 4 service(s) — 2 eligible for proxy injection

dpivot Transform Summary:
  ✓ Enabling zero-downtime for service 'api'
  ✓ Enabling zero-downtime for service 'frontend'

Generated: dpivot-compose.yml
```

The `db` (postgres) and `redis` services were automatically excluded — dpivot recognises database images and never proxies them. The `api` and `frontend` services got proxy containers injected.

Take a look at what was generated:

```bash
cat dpivot-compose.yml
```

Notice that:
- `api` and `frontend` no longer have `ports:` entries — the ports are now owned by `dpivot-proxy-api` and `dpivot-proxy-frontend`
- Two new proxy services appear: `dpivot-proxy-api` and `dpivot-proxy-frontend`
- All services that talk to each other join a `dpivot_mesh` bridge network
- `api` and `frontend` also keep the `default` network so they can still reach `db` and `redis`

---

## Step 5 — Start the enhanced stack

```bash
docker compose -f dpivot-compose.yml up -d
```

Docker will pull `postgres:16-alpine` and `redis:7-alpine` on first run (only once). The proxy image `technicaltalk/dpivot-proxy:latest` is pulled from Docker Hub automatically.

Wait a few seconds for the health checks to pass, then check everything is running:

```bash
docker compose -f dpivot-compose.yml ps
```

You should see all 6 containers with `(healthy)` or `Up` status:

```
NAME                              STATUS
testapp-api-1                     Up (healthy)
testapp-db-1                      Up (healthy)
testapp-dpivot-proxy-api-1        Up
testapp-dpivot-proxy-frontend-1   Up
testapp-frontend-1                Up (healthy)
testapp-redis-1                   Up (healthy)
```

Now open `http://localhost:3000` in your browser. You'll see the dpivot test app with a live monitor bar at the top showing:

- **frontend version** — static, set when the image was built
- **api version** — live, polled every 2 seconds from the running container
- **instance** — the container ID currently serving traffic
- **uptime** — how long the current instance has been running

---

## Step 6 — Do your first zero-downtime rollout

This is the moment everything is built for. You're going to deploy a new version of the API while keeping the app available.

**Build a new version:**

```bash
API_VERSION=2.0.0 docker compose build api
```

This builds `dpivot-testapp-api:2.0.0`. The original `1.0.0` image is untouched.

**Run the rollout:**

```bash
API_VERSION=2.0.0 ../../bin/dpivot rollout api \
  --file dpivot-compose.yml \
  --control-addr http://localhost:9901
```

Keep the browser open at `http://localhost:3000` while this runs. You'll see the log output:

```json
{"msg":"rollout: starting","service":"api"}
{"msg":"rollout: scaling +1","service":"api"}
{"msg":"rollout: new container healthy","addr":"172.20.0.6:3001"}
{"msg":"rollout: new backend registered","backend_id":"api-3a0b052..."}
{"msg":"rollout: draining old connections","drain":5}
{"msg":"rollout: removing old container"}
{"msg":"rollout: complete","service":"api"}
```

Simultaneously in the browser, the **api version badge will flip from `1.0.0` to `2.0.0`** and the **instance badge will change** to the new container ID. The badge flashes briefly to highlight the change.

Also notice the items list now shows a new `🚀 Deployed v2.0.0` entry — the new container wrote this to the database on startup, so you have a permanent record of when each version went live.

> **Why port 9901?** The control API for each proxy is mapped to the host at `service_host_port + 6900`. The API service uses port `3001`, so `3001 + 6900 = 9901`. The frontend uses port `3000`, so its control API is at port `9900`.

---

## Step 7 — Optional: Install as a Docker CLI plugin

If you prefer to type `docker dpivot` instead of `./bin/dpivot`, install the plugin:

```bash
sudo make install-plugin
```

This installs the binary to `/usr/local/lib/docker/cli-plugins/docker-dpivot` where Docker looks for plugins system-wide. After installing:

```bash
docker dpivot version
docker dpivot rollout api --file dpivot-compose.yml --control-addr http://localhost:9901
```

The plugin and the standalone binary are the same file — it auto-detects which mode to use.

---

## Step 8 — Add dpivot to your own app

Now that you've seen it work, here's how to apply dpivot to an existing project.

**1. Make sure your services have healthchecks**

dpivot waits for the new container's healthcheck to pass before switching traffic. Without a healthcheck, dpivot will assume the container is ready as soon as it starts, which may be too early.

```yaml
services:
  web:
    image: myapp:latest
    ports:
      - "3000:3000"
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost:3000/health || exit 1"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 15s
```

**2. Mark stateful services with `x-dpivot: skip: true`**

dpivot already auto-detects common databases, but for anything unusual (custom database, legacy stateful service):

```yaml
services:
  legacy-cache:
    image: custom-cache:latest
    x-dpivot:
      skip: true
```

**3. Run generate**

```bash
dpivot generate --file docker-compose.yml --output dpivot-compose.yml
```

**4. Start the enhanced stack**

```bash
docker compose -f dpivot-compose.yml up -d
```

**5. Deploy updates with rollout**

After building a new image:

```bash
dpivot rollout web \
  --file dpivot-compose.yml \
  --control-addr http://localhost:$(( 3000 + 6900 ))
```

Replace `3000` with your service's actual host port.

---

## Troubleshooting

**"connection refused" on `--control-addr`**

Check which port the proxy's control API is mapped to:

```bash
docker ps --filter "name=dpivot-proxy" --format "{{.Names}}: {{.Ports}}"
```

Look for a line like `0.0.0.0:9901->9900/tcp`. The left side (9901) is your `--control-addr` port.

**Rollout says the container is unhealthy**

The new container failed its healthcheck. Check the logs:

```bash
docker logs <container-id>
```

The rollout exits without touching traffic — the original container is still running and serving.

**Service can't reach the database after generate**

Make sure your database service either uses `x-dpivot: skip: true` or has no `ports` declared (most databases don't expose ports). The proxied services join both `default` and `dpivot_mesh` networks, so they can still reach unproxied services by hostname.

**`version` in the compose file is "obsolete"**

A warning about the `version` field being obsolete is from Docker Compose v2 — it's harmless. Remove the `version:` line from your `docker-compose.yml` and regenerate to silence it.

---

## What's next

- Read [docs/how-it-works.md](how-it-works.md) to understand the full deployment lifecycle
- Check [docs/configuration.md](configuration.md) for all flags and environment variables
- Look at [examples/production/](../examples/production/) for a setup with Nginx, TLS, and Prometheus
- Look at [examples/scripts/safe-rollout.sh](../examples/scripts/safe-rollout.sh) for a battle-tested rollout script with automatic rollback on error
