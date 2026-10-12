// AirDash web UI: serves the mirror page and proxies /api/* to the Go
// service so the browser talks to one origin (no CORS) and the service's
// address stays a server-side setting.
import express from 'express';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));

// Module-level (not per-app) so state survives across createApp() calls in
// the same process: every poll hits this proxy, so logging every failure
// while the service is down would flood the log; only the transition in
// and out of the down state is worth a line.
let upstreamDown = false;

export function createApp({ apiBase, logger = console.log } = {}) {
  const base = (apiBase ?? process.env.AIRDASH_API ?? 'http://localhost:8080').replace(/\/$/, '');
  const app = express();
  app.disable('x-powered-by');

  app.get('/healthz', (_req, res) => res.type('text/plain').send('ok'));

  app.use('/api', express.raw({ type: '*/*', limit: '16kb' }), async (req, res) => {
    const url = base + '/api' + req.url;
    const init = { method: req.method, headers: {}, signal: AbortSignal.timeout(5000) };
    if (req.method !== 'GET' && req.method !== 'HEAD' && req.body?.length) {
      init.body = req.body;
      init.headers['content-type'] = req.get('content-type') ?? 'application/json';
    }
    try {
      const up = await fetch(url, init);
      if (upstreamDown) {
        upstreamDown = false;
        logger('upstream recovered');
      }
      const body = Buffer.from(await up.arrayBuffer());
      if (req.method === 'PUT' && req.url === '/settings') logger(`settings -> ${up.status} ${body.toString().trim()}`);
      // Logos are immutable per airline, so let the browser cache them as
      // the service says; everything else is live data and stays no-store.
      const cache = req.url.startsWith('/logo/') && up.headers.get('cache-control') ? up.headers.get('cache-control') : 'no-store';
      res.status(up.status)
        .set('Cache-Control', cache)
        .type(up.headers.get('content-type') ?? 'application/json')
        .send(body);
    } catch (err) {
      // Every poll hits this proxy, so log only the transition into the
      // down state, not every failed request while it stays down.
      if (!upstreamDown) {
        upstreamDown = true;
        logger(`upstream error: ${err.message}`);
      }
      res.status(502).set('Cache-Control', 'no-store').json({ error: 'service unreachable' });
    }
  });

  // The embeddable mirror for Home Assistant's iframe card lives at /widget.
  app.get('/widget', (_req, res) => res.sendFile(path.join(here, 'public', 'widget.html')));
  app.use(express.static(path.join(here, 'public'), { cacheControl: false, etag: false }));
  return app;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const port = Number(process.env.PORT ?? 3000);
  const apiBase = process.env.AIRDASH_API ?? 'http://localhost:8080';
  createApp({ apiBase }).listen(port, () => console.log(`airdash-ui listening on :${port}, service at ${apiBase}`));
}
