# Basic example

This is the minimal case: a single Node.js API with a Postgres database.

Run:

```bash
dpivot generate
docker compose -f dpivot-compose.yml up -d
```

dpivot auto-detects `api` as a proxy target (it has `ports`) and auto-excludes `db` (postgres image). You don't configure anything — it just works.

After bring-up:

- `:3000` is owned by `dpivot-proxy-api`, not by `api` directly
- `db:5432` is accessible as-is — no proxy involved
- Control API is at `dpivot-proxy-api:9900` on the internal `dpivot_mesh` network

To deploy a new version:

```bash
# update image in docker-compose.yml to myapp:2.0, then:
dpivot generate
dpivot rollout api
```
