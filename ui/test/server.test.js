import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { createApp } from '../server.js';

let upstream, upstreamUrl, app, appUrl;
let lastUpstreamReq = null;

before(async () => {
  upstream = http.createServer((req, res) => {
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      lastUpstreamReq = { method: req.method, url: req.url, body, ct: req.headers['content-type'] };
      if (req.url === '/api/nearest') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ found: false, age_s: 1, nearby: 0, radius_km: 10 }));
      } else if (req.url === '/api/settings' && req.method === 'PUT') {
        res.writeHead(body.includes('"radius_km":0') ? 400 : 200, { 'Content-Type': 'application/json' });
        res.end(body.includes('"radius_km":0') ? '{"error":"radius_km must be > 0"}' : body);
      } else if (req.url === '/api/settings') {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end('{"origin":"","dest":"","airport":"AGP","radius_km":10}');
      } else if (req.url === '/api/logo/BA') {
        res.writeHead(200, { 'Content-Type': 'image/png', 'Cache-Control': 'public, max-age=86400' });
        res.end(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x00, 0xff, 0xfe, 0x0d, 0x0a]));
      } else {
        res.writeHead(404); res.end();
      }
    });
  });
  await new Promise((r) => upstream.listen(0, '127.0.0.1', r));
  upstreamUrl = `http://127.0.0.1:${upstream.address().port}`;
  app = createApp({ apiBase: upstreamUrl, logger: () => {} }).listen(0, '127.0.0.1');
  await new Promise((r) => app.once('listening', r));
  appUrl = `http://127.0.0.1:${app.address().port}`;
});

after(() => { app.close(); upstream.close(); });

test('serves the page', async () => {
  const res = await fetch(`${appUrl}/`);
  assert.equal(res.status, 200);
  assert.match(res.headers.get('content-type'), /text\/html/);
  assert.match(await res.text(), /AirDash/);
});

test('healthz', async () => {
  const res = await fetch(`${appUrl}/healthz`);
  assert.equal(res.status, 200);
  assert.equal(await res.text(), 'ok');
});

test('proxies GET /api/nearest with status, body and no-store', async () => {
  const res = await fetch(`${appUrl}/api/nearest`);
  assert.equal(res.status, 200);
  assert.equal(res.headers.get('cache-control'), 'no-store');
  assert.deepEqual(await res.json(), { found: false, age_s: 1, nearby: 0, radius_km: 10 });
  assert.equal(lastUpstreamReq.url, '/api/nearest');
});

test('forwards PUT body and passes 400 through', async () => {
  const ok = await fetch(`${appUrl}/api/settings`, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ airport: 'AGP', radius_km: 15 }),
  });
  assert.equal(ok.status, 200);
  assert.deepEqual(await ok.json(), { airport: 'AGP', radius_km: 15 });
  assert.equal(lastUpstreamReq.method, 'PUT');
  assert.match(lastUpstreamReq.ct, /application\/json/);

  const bad = await fetch(`${appUrl}/api/settings`, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ radius_km: 0 }),
  });
  assert.equal(bad.status, 400);
  assert.equal((await bad.json()).error, 'radius_km must be > 0');
});

test('upstream down gives 502', async () => {
  const dead = createApp({ apiBase: 'http://127.0.0.1:1', logger: () => {} }).listen(0, '127.0.0.1');
  await new Promise((r) => dead.once('listening', r));
  const res = await fetch(`http://127.0.0.1:${dead.address().port}/api/nearest`);
  assert.equal(res.status, 502);
  assert.deepEqual(await res.json(), { error: 'service unreachable' });
  dead.close();
});

test('unknown api path is passed through as 404', async () => {
  const res = await fetch(`${appUrl}/api/nope`);
  assert.equal(res.status, 404);
});

test('logs upstream state changes once', async () => {
  const logs = [];
  const capture = (msg) => logs.push(msg);

  // Start from a known-good state (one successful proxied request through
  // a working upstream) so the module-level upstreamDown flag is false
  // before we drive it down; the flag is shared across app instances, so
  // this isolates the test from whatever state earlier tests left it in.
  const up = createApp({ apiBase: upstreamUrl, logger: capture }).listen(0, '127.0.0.1');
  await new Promise((r) => up.once('listening', r));
  await fetch(`http://127.0.0.1:${up.address().port}/api/nearest`);
  up.close();

  const dead = createApp({ apiBase: 'http://127.0.0.1:1', logger: capture }).listen(0, '127.0.0.1');
  await new Promise((r) => dead.once('listening', r));
  const deadUrl = `http://127.0.0.1:${dead.address().port}`;
  await fetch(`${deadUrl}/api/nearest`);
  await fetch(`${deadUrl}/api/nearest`);
  dead.close();

  const errorLines = logs.filter((l) => l.startsWith('upstream error'));
  assert.equal(errorLines.length, 1, `want exactly one "upstream error" line for two consecutive failures, got: ${JSON.stringify(logs)}`);
});

test('proxies binary bodies unchanged', async () => {
  const res = await fetch(`${appUrl}/api/logo/BA`);
  assert.equal(res.status, 200);
  assert.equal(res.headers.get('content-type'), 'image/png');
  assert.equal(res.headers.get('cache-control'), 'public, max-age=86400');
  assert.deepEqual(new Uint8Array(await res.arrayBuffer()), new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x00, 0xff, 0xfe, 0x0d, 0x0a]));
});

test('keeps no-store for everything except logos', async () => {
  const res = await fetch(`${appUrl}/api/nearest`);
  assert.equal(res.headers.get('cache-control'), 'no-store');
});

test('serves the embeddable widget page', async () => {
  const res = await fetch(`${appUrl}/widget`);
  assert.equal(res.status, 200);
  assert.match(res.headers.get('content-type'), /text\/html/);
  const html = await res.text();
  assert.match(html, /id="screen"/);
  assert.match(html, /mirror\.js/);
  assert.doesNotMatch(html, /id="filters"/);
});

test('settings page carries the Home Assistant snippet and the shared mirror', async () => {
  const html = await (await fetch(`${appUrl}/`)).text();
  assert.match(html, /id="ha-yaml"/);
  assert.match(html, /mirror\.js/);
  assert.match(html, /mirror\.css/);
  const js = await fetch(`${appUrl}/mirror.js`);
  assert.equal(js.status, 200);
  assert.match(await js.text(), /async function poll\(\)/);
});
