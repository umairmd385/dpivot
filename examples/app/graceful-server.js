/**
 * Node.js production server with graceful SIGTERM handling for dpivot.
 *
 * Key behaviours implemented here:
 *   1. /health/live  — liveness probe (process running)
 *   2. /health/ready — readiness probe (DB + Redis reachable)
 *   3. SIGTERM drain — stop accepting, finish in-flight requests, then exit
 *   4. Idempotent POST pattern via Idempotency-Key header
 *   5. Client-side retry with exponential backoff (see retryFetch below)
 *
 * How it integrates with dpivot:
 *   - Docker HEALTHCHECK uses /health/ready
 *   - dpivot rollout waits for /health/ready to return 200
 *   - On SIGTERM (container stop), the server drains for up to 30s
 *   - In-flight requests complete; no 502 errors at the nginx/dpivot layer
 */

'use strict';

const http = require('http');
const { Pool } = require('pg');       // npm i pg
const { createClient } = require('redis'); // npm i redis

// ── Dependencies ──────────────────────────────────────────────────────────────

const db = new Pool({ connectionString: process.env.DATABASE_URL });
const redisClient = createClient({ url: process.env.REDIS_URL });

async function connectDeps() {
  await redisClient.connect();
  await db.query('SELECT 1'); // verify DB is reachable on startup
}

// ── Idempotency store ─────────────────────────────────────────────────────────
// Stores processed Idempotency-Key → response so duplicate requests return
// the same result without re-executing the mutation.

async function getIdempotentResult(key) {
  const cached = await redisClient.get(`idem:${key}`);
  return cached ? JSON.parse(cached) : null;
}

async function saveIdempotentResult(key, result, ttlSeconds = 86400) {
  await redisClient.setEx(`idem:${key}`, ttlSeconds, JSON.stringify(result));
}

// ── Request handler ───────────────────────────────────────────────────────────

async function handleRequest(req, res) {
  const url = new URL(req.url, `http://${req.headers.host}`);

  // Liveness: always 200 while the process is alive.
  if (url.pathname === '/health/live') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ status: 'ok', pid: process.pid }));
    return;
  }

  // Readiness: only 200 when dependencies are healthy.
  if (url.pathname === '/health/ready') {
    try {
      await db.query('SELECT 1');
      await redisClient.ping();
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ status: 'ready' }));
    } catch (err) {
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ status: 'not_ready', error: err.message }));
    }
    return;
  }

  // Example idempotent POST: POST /orders
  if (url.pathname === '/orders' && req.method === 'POST') {
    const idempotencyKey = req.headers['idempotency-key'];
    if (!idempotencyKey) {
      res.writeHead(400, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'Idempotency-Key header required for POST /orders' }));
      return;
    }

    // Return cached result for duplicate requests — safe during retries.
    const cached = await getIdempotentResult(idempotencyKey);
    if (cached) {
      res.writeHead(cached.status, { 'Content-Type': 'application/json', 'X-Idempotent-Replay': 'true' });
      res.end(JSON.stringify(cached.body));
      return;
    }

    // Process the mutation exactly once.
    const body = await readBody(req);
    const order = JSON.parse(body);
    const result = await db.query(
      'INSERT INTO orders (data, created_at) VALUES ($1, NOW()) RETURNING id',
      [JSON.stringify(order)]
    );
    const response = { id: result.rows[0].id, status: 'created' };

    // Persist result so retries get the same response.
    await saveIdempotentResult(idempotencyKey, { status: 201, body: response });

    res.writeHead(201, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(response));
    return;
  }

  res.writeHead(404);
  res.end();
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', c => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString()));
    req.on('error', reject);
  });
}

// ── Server lifecycle ──────────────────────────────────────────────────────────

const server = http.createServer((req, res) => {
  handleRequest(req, res).catch(err => {
    console.error({ event: 'request_error', path: req.url, err: err.message });
    if (!res.headersSent) {
      res.writeHead(500, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'internal server error' }));
    }
  });
});

const PORT = parseInt(process.env.PORT || '3000', 10);
const DRAIN_TIMEOUT_MS = parseInt(process.env.DRAIN_TIMEOUT_MS || '30000', 10);

async function start() {
  await connectDeps();
  server.listen(PORT, () => {
    console.log(JSON.stringify({ event: 'server_start', port: PORT }));
  });
}

// ── Graceful shutdown (SIGTERM) ───────────────────────────────────────────────
// Docker / dpivot sends SIGTERM before killing the container.
// Steps:
//   1. Stop accepting new TCP connections (server.close).
//   2. Wait for all in-flight requests to complete (or force-exit on timeout).
//   3. Close DB pool and Redis connection cleanly.

function shutdown(signal) {
  console.log(JSON.stringify({ event: 'shutdown_start', signal, drain_ms: DRAIN_TIMEOUT_MS }));

  // 1. Stop accepting new connections. In-flight requests still run.
  server.close(async () => {
    console.log(JSON.stringify({ event: 'shutdown_complete' }));
    await db.end().catch(() => {});
    await redisClient.quit().catch(() => {});
    process.exit(0);
  });

  // 2. Force exit if drain takes too long (prevents hung containers).
  setTimeout(() => {
    console.error(JSON.stringify({ event: 'shutdown_timeout', drain_ms: DRAIN_TIMEOUT_MS }));
    process.exit(1);
  }, DRAIN_TIMEOUT_MS).unref();
}

process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT',  () => shutdown('SIGINT'));

// Crash loudly on unhandled rejections so the container restarts.
process.on('unhandledRejection', (reason) => {
  console.error(JSON.stringify({ event: 'unhandled_rejection', reason: String(reason) }));
  process.exit(1);
});

start().catch(err => {
  console.error(JSON.stringify({ event: 'startup_failed', err: err.message }));
  process.exit(1);
});

// ── Client-side retry with exponential backoff ────────────────────────────────
// Use this pattern in any service that calls this API.
// Retries are safe for GET and for POST requests that include Idempotency-Key.

async function retryFetch(url, options = {}, maxAttempts = 4) {
  const { idempotencyKey, ...fetchOptions } = options;
  if (idempotencyKey) {
    fetchOptions.headers = {
      ...fetchOptions.headers,
      'Idempotency-Key': idempotencyKey,
    };
  }

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      const res = await fetch(url, fetchOptions);
      // Retry on 502/503/504 (proxy/upstream errors) and 429 (rate limit).
      if ([429, 502, 503, 504].includes(res.status) && attempt < maxAttempts) {
        const backoffMs = Math.min(100 * 2 ** (attempt - 1), 5000) + Math.random() * 100;
        console.warn(`retryFetch: attempt ${attempt} got ${res.status}, retrying in ${backoffMs.toFixed(0)}ms`);
        await new Promise(r => setTimeout(r, backoffMs));
        continue;
      }
      return res;
    } catch (err) {
      if (attempt === maxAttempts) throw err;
      const backoffMs = Math.min(100 * 2 ** (attempt - 1), 5000);
      await new Promise(r => setTimeout(r, backoffMs));
    }
  }
}

module.exports = { retryFetch };
