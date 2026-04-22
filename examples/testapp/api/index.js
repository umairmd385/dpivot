'use strict';

const express = require('express');
const { Pool }  = require('pg');
const Redis     = require('ioredis');

const PORT        = parseInt(process.env.PORT        || '3001', 10);
const APP_VERSION = process.env.APP_VERSION           || '1.0.0';
const DB_URL      = process.env.DATABASE_URL          || 'postgres://app:secret@db:5432/appdb';
const REDIS_HOST  = process.env.REDIS_HOST            || 'redis';
const REDIS_PORT  = parseInt(process.env.REDIS_PORT   || '6379', 10);
const REDIS_PASS  = process.env.REDIS_PASSWORD        || undefined;
const HOSTNAME    = require('os').hostname();

const STARTED_AT  = new Date().toISOString();
let   REQUEST_COUNT = 0;
const CACHE_KEY   = 'items:all';
const CACHE_TTL   = 30;

// ── Dependencies ──────────────────────────────────────────────────────────────

const db = new Pool({
  connectionString: DB_URL,
  max: 10,
  idleTimeoutMillis: 30000,
  connectionTimeoutMillis: 3000,
});

const redis = new Redis({
  host: REDIS_HOST,
  port: REDIS_PORT,
  password: REDIS_PASS,
  lazyConnect: true,
  retryStrategy: (times) => Math.min(times * 200, 5000),
  enableOfflineQueue: false,
});

redis.on('error', (err) =>
  log('warn', 'redis_error', { error: err.message }));

// ── Logging ───────────────────────────────────────────────────────────────────

function log(level, event, fields = {}) {
  console.log(JSON.stringify({ level, event, version: APP_VERSION, ts: new Date().toISOString(), ...fields }));
}

// ── App ───────────────────────────────────────────────────────────────────────

const app = express();
app.use(express.json());

// Attach served-by header and increment counter on every response.
app.use((req, res, next) => {
  REQUEST_COUNT++;
  res.setHeader('X-Served-By', `${HOSTNAME}@${APP_VERSION}`);
  const start = Date.now();
  res.on('finish', () =>
    log('info', 'request', { method: req.method, path: req.path, status: res.statusCode, ms: Date.now() - start }));
  next();
});

// ── Health endpoints ──────────────────────────────────────────────────────────

// Liveness: process is alive. Docker uses this to decide whether to restart.
app.get('/health/live', (_req, res) => {
  res.json({ status: 'ok', version: APP_VERSION, pid: process.pid, uptime: process.uptime() });
});

// Readiness: all deps reachable. dpivot rollout waits for 200 before switching traffic.
app.get('/health/ready', async (_req, res) => {
  try {
    await db.query('SELECT 1');
    await redis.ping();
    res.json({ status: 'ready', version: APP_VERSION });
  } catch (err) {
    res.status(503).json({ status: 'not_ready', error: err.message });
  }
});

// ── Version endpoint — polled by the frontend to detect live rollouts ─────────

app.get('/api/version', (_req, res) => {
  res.json({
    version:   APP_VERSION,
    instance:  HOSTNAME,
    started:   STARTED_AT,
    uptime:    process.uptime(),
    pid:       process.pid,
    requests:  REQUEST_COUNT,
  });
});

// ── Items CRUD ────────────────────────────────────────────────────────────────

app.get('/api/items', async (_req, res) => {
  try {
    const cached = await redis.get(CACHE_KEY).catch(() => null);
    if (cached) {
      return res.json({ source: 'cache', version: APP_VERSION, items: JSON.parse(cached) });
    }
    const { rows } = await db.query('SELECT id, name, description, created_at FROM items ORDER BY created_at DESC LIMIT 50');
    await redis.setex(CACHE_KEY, CACHE_TTL, JSON.stringify(rows)).catch(() => {});
    res.json({ source: 'db', version: APP_VERSION, items: rows });
  } catch (err) {
    log('error', 'get_items_failed', { error: err.message });
    res.status(500).json({ error: 'internal server error' });
  }
});

app.get('/api/items/:id', async (req, res) => {
  try {
    const { rows } = await db.query('SELECT * FROM items WHERE id = $1', [req.params.id]);
    if (!rows.length) return res.status(404).json({ error: 'not found' });
    res.json(rows[0]);
  } catch (err) {
    res.status(500).json({ error: 'internal server error' });
  }
});

app.post('/api/items', async (req, res) => {
  const { name, description = '' } = req.body || {};
  if (!name || typeof name !== 'string' || !name.trim()) {
    return res.status(400).json({ error: 'name is required' });
  }
  try {
    const { rows } = await db.query(
      'INSERT INTO items (name, description) VALUES ($1, $2) RETURNING *',
      [name.trim(), description]
    );
    await redis.del(CACHE_KEY).catch(() => {}); // invalidate list cache
    log('info', 'item_created', { id: rows[0].id, name: rows[0].name });
    res.status(201).json(rows[0]);
  } catch (err) {
    log('error', 'create_item_failed', { error: err.message });
    res.status(500).json({ error: 'internal server error' });
  }
});

app.delete('/api/items/:id', async (req, res) => {
  try {
    const { rowCount } = await db.query('DELETE FROM items WHERE id = $1', [req.params.id]);
    if (!rowCount) return res.status(404).json({ error: 'not found' });
    await redis.del(CACHE_KEY).catch(() => {});
    res.status(204).end();
  } catch (err) {
    res.status(500).json({ error: 'internal server error' });
  }
});

// ── Startup ───────────────────────────────────────────────────────────────────

async function start() {
  log('info', 'connecting_deps');

  await redis.connect();
  log('info', 'redis_connected', { host: REDIS_HOST, port: REDIS_PORT });

  // Verify DB connection and run idempotent schema migration.
  await db.connect();
  await db.query(`
    CREATE TABLE IF NOT EXISTS items (
      id          SERIAL PRIMARY KEY,
      name        TEXT        NOT NULL,
      description TEXT        NOT NULL DEFAULT '',
      created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
    )
  `);
  log('info', 'db_ready', { url: DB_URL.replace(/:\/\/.*@/, '://***@') });

  // Write a deployment marker so every rollout is visible in the items list.
  await db.query(
    `INSERT INTO items (name, description) VALUES ($1, $2)`,
    [`🚀 Deployed v${APP_VERSION}`, `instance: ${HOSTNAME} | started: ${STARTED_AT}`]
  );
  await redis.del(CACHE_KEY).catch(() => {});

  const server = app.listen(PORT, () =>
    log('info', 'server_started', { port: PORT, version: APP_VERSION }));

  // ── Graceful shutdown on SIGTERM ─────────────────────────────────────────
  // dpivot proxy drains connections first (drain period), then Docker sends
  // SIGTERM. This handler ensures in-flight HTTP requests finish cleanly.
  const shutdown = (signal) => {
    log('info', 'shutdown_start', { signal });
    server.close(async () => {
      await Promise.allSettled([db.end(), redis.quit()]);
      log('info', 'shutdown_complete');
      process.exit(0);
    });
    // Force-exit if drain takes too long.
    setTimeout(() => { log('warn', 'shutdown_timeout'); process.exit(1); }, 30_000).unref();
  };
  process.on('SIGTERM', () => shutdown('SIGTERM'));
  process.on('SIGINT',  () => shutdown('SIGINT'));
}

process.on('unhandledRejection', (reason) => {
  log('error', 'unhandled_rejection', { reason: String(reason) });
  process.exit(1);
});

start().catch((err) => {
  log('error', 'startup_failed', { error: err.message });
  process.exit(1);
});
