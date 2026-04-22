'use strict';

const express = require('express');
const http    = require('http');

const PORT        = parseInt(process.env.PORT        || '3000', 10);
const APP_VERSION = process.env.APP_VERSION           || '1.0.0';
const API_URL     = process.env.API_URL               || 'http://api:3001';

// ── Logging ───────────────────────────────────────────────────────────────────

function log(level, event, fields = {}) {
  console.log(JSON.stringify({ level, event, version: APP_VERSION, ts: new Date().toISOString(), ...fields }));
}

// ── API client ────────────────────────────────────────────────────────────────

function apiRequest(method, path, body) {
  return new Promise((resolve, reject) => {
    const url     = new URL(path, API_URL);
    const payload = body ? JSON.stringify(body) : null;
    const options = {
      method,
      headers: {
        'Content-Type':  'application/json',
        ...(payload ? { 'Content-Length': Buffer.byteLength(payload) } : {}),
      },
    };

    const req = http.request(url.toString(), options, (res) => {
      let data = '';
      res.on('data', (chunk) => { data += chunk; });
      res.on('end', () => {
        try { resolve({ status: res.statusCode, body: JSON.parse(data) }); }
        catch { resolve({ status: res.statusCode, body: data }); }
      });
    });
    req.on('error', reject);
    if (payload) req.write(payload);
    req.end();
  });
}

// ── App ───────────────────────────────────────────────────────────────────────

const app = express();
app.use(express.json());
app.use(express.urlencoded({ extended: true }));

app.use((req, res, next) => {
  const start = Date.now();
  res.on('finish', () =>
    log('info', 'request', { method: req.method, path: req.path, status: res.statusCode, ms: Date.now() - start }));
  next();
});

// ── Health endpoints ──────────────────────────────────────────────────────────

app.get('/health/live', (_req, res) => {
  res.json({ status: 'ok', version: APP_VERSION, pid: process.pid });
});

// Readiness delegates to the backend: frontend is only ready when the API is.
app.get('/health/ready', async (_req, res) => {
  try {
    const result = await apiRequest('GET', '/health/ready');
    if (result.status === 200) {
      res.json({ status: 'ready', version: APP_VERSION, api: result.body });
    } else {
      res.status(503).json({ status: 'not_ready', reason: 'api not ready', api: result.body });
    }
  } catch (err) {
    res.status(503).json({ status: 'not_ready', reason: 'api unreachable', error: err.message });
  }
});

// ── UI ────────────────────────────────────────────────────────────────────────

app.get('/', async (_req, res) => {
  let items  = [];
  let apiVer = { version: 'unavailable', uptime: null, instance: '', requests: 0 };
  let error  = '';

  try {
    const [itemsRes, verRes] = await Promise.all([
      apiRequest('GET', '/api/items'),
      apiRequest('GET', '/api/version'),
    ]);
    items  = itemsRes.body.items || [];
    apiVer = verRes.body;
  } catch (err) {
    error = err.message;
  }

  const rows = items.length
    ? items.map((i) => `
        <tr>
          <td>${i.id}</td>
          <td>${esc(i.name)}</td>
          <td>${esc(i.description)}</td>
          <td>${new Date(i.created_at).toLocaleString()}</td>
          <td>
            <form method="POST" action="/items/${i.id}/delete" style="display:inline">
              <button class="btn-danger">Delete</button>
            </form>
          </td>
        </tr>`).join('')
    : '<tr><td colspan="5" style="text-align:center;color:#888">No items yet — add one below.</td></tr>';

  res.send(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>dpivot testapp</title>
  <style>
    *{box-sizing:border-box}
    body{font-family:system-ui,monospace;max-width:900px;margin:40px auto;padding:0 20px;background:#f9f9f9;color:#333}
    h1{margin-bottom:4px}
    .live-bar{display:flex;align-items:center;gap:10px;flex-wrap:wrap;background:#fff;border:1px solid #e0e0e0;border-radius:8px;padding:10px 16px;margin-bottom:20px;box-shadow:0 1px 4px #0001}
    .badge{display:inline-block;padding:3px 11px;border-radius:12px;font-size:12px;font-weight:bold}
    .badge-blue{background:#0070f3;color:#fff}
    .badge-green{background:#0a7c42;color:#fff}
    .badge-grey{background:#555;color:#fff}
    .badge-orange{background:#e07800;color:#fff}
    @keyframes flash{0%,100%{opacity:1}50%{opacity:.35}}
    .changed{animation:flash .6s ease 3}
    .dot{width:8px;height:8px;border-radius:50%;background:#22c55e;display:inline-block;margin-right:4px;box-shadow:0 0 4px #22c55e}
    .dot.stale{background:#f59e0b;box-shadow:0 0 4px #f59e0b}
    table{width:100%;border-collapse:collapse;background:#fff;border-radius:8px;overflow:hidden;box-shadow:0 1px 4px #0001}
    th,td{padding:10px 14px;text-align:left;border-bottom:1px solid #eee}
    th{background:#f0f0f0;font-size:13px}
    form.inline{display:flex;gap:8px;flex-wrap:wrap;margin-top:24px}
    form.inline input{padding:8px 12px;border:1px solid #ccc;border-radius:6px;font-size:14px}
    .btn,.btn-danger{padding:8px 16px;border:none;border-radius:6px;cursor:pointer;font-size:14px}
    .btn{background:#0070f3;color:#fff}
    .btn-danger{background:#e53e3e;color:#fff;padding:4px 10px;font-size:12px}
    .error{background:#fff3f3;border:1px solid #f99;padding:10px 16px;border-radius:6px;margin-bottom:16px}
    .meta{font-size:12px;color:#888;margin-top:6px}
    footer{margin-top:32px;font-size:12px;color:#aaa}
    code{background:#f0f0f0;padding:1px 5px;border-radius:3px}
  </style>
</head>
<body>
  <h1>dpivot testapp</h1>

  <!-- Live rollout monitor bar — updates every 2 s via JS polling -->
  <div class="live-bar" id="live-bar">
    <span><span class="dot" id="dot"></span><strong>Live</strong></span>
    <span class="badge badge-blue">frontend v${APP_VERSION}</span>
    <span class="badge badge-green" id="badge-ver">api v${esc(String(apiVer.version))}</span>
    <span class="badge badge-grey"  id="badge-inst">instance: ${esc(String(apiVer.instance || '?'))}</span>
    <span class="badge badge-grey"  id="badge-req">requests: ${esc(String(apiVer.requests ?? 0))}</span>
    <span class="badge badge-grey"  id="badge-uptime">uptime: ${apiVer.uptime != null ? Number(apiVer.uptime).toFixed(0)+'s' : '?'}</span>
    <span style="margin-left:auto;font-size:11px;color:#aaa" id="poll-ts">polled just now</span>
  </div>

  ${error ? `<div class="error">⚠ API error: ${esc(error)}</div>` : ''}

  <table>
    <thead><tr><th>#</th><th>Name</th><th>Description</th><th>Created</th><th></th></tr></thead>
    <tbody>${rows}</tbody>
  </table>
  <p class="meta">Cache TTL: 30 s — first load shows "source: db", subsequent loads show "source: cache".</p>

  <form class="inline" method="POST" action="/items">
    <input name="name"        placeholder="Item name"        required>
    <input name="description" placeholder="Description (optional)">
    <button class="btn" type="submit">Add Item</button>
  </form>

  <footer>
    <p>
      To test a rollout: bump <code>API_VERSION</code>, rebuild, then run<br>
      <code>dpivot rollout api --file dpivot-compose.yml --control-addr http://localhost:9901</code><br>
      Watch the <strong>api version</strong> and <strong>instance</strong> badges above flip live — no page refresh needed.
    </p>
    <p>Health: <a href="/health/live">/health/live</a> | <a href="/health/ready">/health/ready</a></p>
  </footer>

  <script>
    // Poll /api/version every 2 s and update the live bar without reloading.
    let lastVer = '${esc(String(apiVer.version))}';
    let lastInst = '${esc(String(apiVer.instance || ''))}';

    function flash(el) {
      el.classList.remove('changed');
      void el.offsetWidth; // force reflow
      el.classList.add('changed');
    }

    async function poll() {
      try {
        const r = await fetch('/api/version');
        if (!r.ok) throw new Error(r.status);
        const d = await r.json();
        const dot = document.getElementById('dot');
        dot.classList.remove('stale');

        const bVer  = document.getElementById('badge-ver');
        const bInst = document.getElementById('badge-inst');

        if (d.version !== lastVer || d.instance !== lastInst) {
          bVer.textContent  = 'api v' + d.version;
          bInst.textContent = 'instance: ' + (d.instance || '?');
          flash(bVer);
          flash(bInst);
          lastVer  = d.version;
          lastInst = d.instance;
        }

        document.getElementById('badge-req').textContent    = 'requests: ' + (d.requests ?? 0);
        document.getElementById('badge-uptime').textContent = 'uptime: '   + (d.uptime != null ? Math.round(d.uptime) + 's' : '?');
        document.getElementById('poll-ts').textContent      = 'polled ' + new Date().toLocaleTimeString();
      } catch (_) {
        document.getElementById('dot').classList.add('stale');
        document.getElementById('poll-ts').textContent = 'poll failed ' + new Date().toLocaleTimeString();
      }
    }

    setInterval(poll, 2000);
  </script>
</body>
</html>`);
});

app.post('/items', async (req, res) => {
  const { name = '', description = '' } = req.body;
  try {
    await apiRequest('POST', '/api/items', { name, description });
  } catch (err) {
    log('warn', 'create_item_failed', { error: err.message });
  }
  res.redirect('/');
});

app.post('/items/:id/delete', async (req, res) => {
  try {
    await apiRequest('DELETE', `/api/items/${req.params.id}`);
  } catch (err) {
    log('warn', 'delete_item_failed', { id: req.params.id, error: err.message });
  }
  res.redirect('/');
});

function esc(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// ── Startup ───────────────────────────────────────────────────────────────────

const server = app.listen(PORT, () =>
  log('info', 'server_started', { port: PORT, version: APP_VERSION, api: API_URL }));

const shutdown = (signal) => {
  log('info', 'shutdown_start', { signal });
  server.close(() => { log('info', 'shutdown_complete'); process.exit(0); });
  setTimeout(() => process.exit(1), 30_000).unref();
};
process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT',  () => shutdown('SIGINT'));
process.on('unhandledRejection', (r) => { log('error', 'unhandled_rejection', { reason: String(r) }); process.exit(1); });
