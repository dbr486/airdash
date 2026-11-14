// Pure helpers that tie a logged aircraft pass to the upstream responses the
// service saved, and summarize those provider bodies as small HTML snippets.
// A classic script (no modules) so pages load it with a plain <script> tag
// and tests import it for its side effect; nothing here touches the DOM.
(function () {
  const STALE_DAYS = 30; // hexdb routes older than this are ignored by the service
  const PAIR_WINDOW_S = 120; // dep/arr board fetches of one refresh land within this

  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const enc = encodeURIComponent;

  const normCallsign = (s) => String(s == null ? '' : s).trim().toUpperCase();
  const normReg = (s) => normCallsign(s).replace(/-/g, '');

  function faSubjects(pass) {
    const p = pass || {};
    return [normCallsign(p.callsign), normReg(p.reg)].filter((v, i, a) => v && a.indexOf(v) === i);
  }

  function responseLink(pass, source) {
    const p = pass || {};
    const st = p[source] && p[source].status;
    if (st === 'not_asked' || st === 'off' || st === 'none') return null;
    const before = (Number(p.last_seen) || 0) + 60; // pass ends at last_seen; fetch may land slightly after
    const cs = normCallsign(p.callsign);
    if (source === 'board') return `board.html?before=${enc(before)}&flight=${enc(cs)}`;
    let url = `response.html?source=${enc(source)}&subject=${enc(cs)}&before=${enc(before)}&pass=${enc((p[source] && p[source].route) || '')}`;
    if (source === 'aeroapi') url += `&alt=${enc(normReg(p.reg))}`;
    return url;
  }

  const routeOf = (place) => (place && (place.code_iata || place.code_icao)) || '';

  function parse(text) {
    try { return JSON.parse(text); } catch { return undefined; }
  }

  // Provider stamps are UTC; show local HH:MM and keep the exact stamp in the tooltip.
  function toDate(v) {
    if (v == null || v === '') return null;
    let s = String(v);
    if (/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}/.test(s)) s = s.replace(' ', 'T') + 'Z'; // AirLabs *_utc has no zone marker
    const d = new Date(s);
    return Number.isNaN(d.getTime()) ? null : d;
  }
  const SHORT_MONTH = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  // refDay (a Date) makes stamps from another local day carry a short date.
  function timeHtml(v, refDay) {
    const d = toDate(v);
    if (!d) return '';
    const other = refDay && d.toDateString() !== refDay.toDateString();
    const day = other ? `${d.getDate()} ${SHORT_MONTH[d.getMonth()]} ` : '';
    const hh = String(d.getHours()).padStart(2, '0');
    const mm = String(d.getMinutes()).padStart(2, '0');
    return `<time title="${esc(d.toISOString())}">${esc(day)}${hh}:${mm}</time>`;
  }

  const dl = (pairs) => `<dl class="rd-dl">${pairs.map(([k, v]) => `<dt>${esc(k)}</dt><dd>${v}</dd>`).join('')}</dl>`;
  const badge = (t) => `<span class="rd-badge">${esc(t)}</span>`;
  const note = (t) => `<p class="rd-note">${esc(t)}</p>`;

  function place(p) {
    if (!p || typeof p !== 'object') return '';
    const code = esc(p.iata_code || p.icao_code || '');
    const rest = [p.municipality, p.name].filter(Boolean).map(esc).join(', ');
    const ll = typeof p.latitude === 'number' && typeof p.longitude === 'number' ? ` (${esc(p.latitude)}, ${esc(p.longitude)})` : '';
    return `${code}${rest ? ' ' + rest : ''}${ll}`;
  }

  function sumAdsbdb(status, body) {
    const j = parse(body);
    const fr = j && j.response && typeof j.response === 'object' ? j.response.flightroute : null;
    // Only a definite "unknown" answer means no route; a 429 or 5xx never answered,
    // even when its body carries a string `response`.
    const ok = status >= 200 && status <= 299;
    const unknown = ok && j && typeof j.response === 'string' && /unknown callsign/i.test(j.response);
    if (status === 404 || unknown) return note('adsbdb has no route for this callsign');
    if (!fr) return null;
    const air = fr.airline || {};
    return dl([
      ['Airline', `${esc(air.name || '')}${air.iata ? ' (' + esc(air.iata) + ')' : ''}`],
      ['From', place(fr.origin)],
      ['To', place(fr.destination)],
    ]);
  }

  function sumHexdb(body, ctx) {
    const j = parse(body);
    if (!j || typeof j !== 'object') return null;
    const airports = (ctx && ctx.airports) || {};
    const ends = String(j.route || '').split('-').map((c) => {
      const a = airports[c];
      return a ? `${esc(c)} ${esc(a.airport || '')}${a.iata ? ' (' + esc(a.iata) + ')' : ''}` : esc(c);
    });
    const rows = [['Route', ends.join(' &rarr; ')]];
    if (typeof j.updatetime === 'number') {
      // The service judges age at fetch time (now - updatetime > 30 days), so a
      // historical view must too; only the displayed day count is rounded.
      const at = Number(ctx && ctx.fetchedAt);
      const atFetch = Number.isFinite(at) && at > 0;
      const elapsed = (atFetch ? at : Date.now() / 1000) - j.updatetime;
      const days = Math.max(0, Math.round(elapsed / 86400)); // clock skew must not show negative days
      const stale = elapsed > STALE_DAYS * 86400 ? ' ' + badge(`older than ${STALE_DAYS} days, ignored`) : '';
      rows.push(['Updated', `${days} days ${atFetch ? 'old when fetched' : 'ago'}${stale}`]);
    }
    return dl(rows);
  }

  const secs = (v) => { const d = toDate(v); return d ? d.getTime() / 1000 : null; };
  const pair = (a, b, ref) => `${timeHtml(a, ref) || '&ndash;'} / ${timeHtml(b, ref) || '&ndash;'}`;

  // A daily flight repeats the same route on every instance, so route equality
  // alone would mark them all. Pick the instance flown at the pass time: the
  // latest departure at or before it, else the next scheduled one.
  function pickUsed(flights, used, passAt) {
    const cand = [];
    flights.forEach((f, i) => { if (used && `${routeOf((f || {}).origin)}-${routeOf((f || {}).destination)}` === used) cand.push(i); });
    if (!cand.length) return -1;
    if (!Number.isFinite(passAt)) return cand[0];
    const off = (i) => secs(flights[i].actual_off || flights[i].scheduled_off);
    const before = cand.filter((i) => off(i) != null && off(i) <= passAt).sort((x, y) => off(y) - off(x));
    if (before.length) return before[0];
    const after = cand.filter((i) => secs(flights[i].scheduled_off) != null && secs(flights[i].scheduled_off) > passAt)
      .sort((x, y) => secs(flights[x].scheduled_off) - secs(flights[y].scheduled_off));
    return after.length ? after[0] : -1;
  }

  function sumAeroapi(body, ctx) {
    const j = parse(body);
    if (!j || !Array.isArray(j.flights)) return null;
    const flights = j.flights.map((f) => f || {});
    const passAt = Number(ctx && ctx.passAt);
    const usedIdx = pickUsed(flights, (ctx && ctx.usedRoute) || '', passAt);
    // AeroAPI lists several days of a daily flight; HH:MM alone would make them look identical.
    const refDay = Number.isFinite(passAt) && ctx.passAt !== '' && ctx.passAt != null ? new Date(passAt * 1000) : new Date();
    const rows = flights.map((f, i) => {
      const isUsed = i === usedIdx;
      return `<tr${isUsed ? ' class="used"' : ''}><td>${esc(f.ident || '')}</td><td>${esc(f.registration || '')}</td>` +
        `<td>${esc(routeOf(f.origin))} &rarr; ${esc(routeOf(f.destination))}</td>` +
        `<td>${pair(f.scheduled_off, f.actual_off, refDay)}</td><td>${pair(f.scheduled_on, f.actual_on, refDay)}</td>` +
        `<td>${esc(f.status || '')}${isUsed ? ' ' + badge('used') : ''}</td></tr>`;
    });
    return `<div class="scroll"><table class="rd-table"><thead><tr><th>Flight</th><th>Reg</th><th>Route</th><th>Off (sched / actual)</th><th>On (sched / actual)</th><th>Status</th></tr></thead><tbody>${rows.join('')}</tbody></table></div>`;
  }

  // AirLabs reports key and quota problems as HTTP 200 with an `error` object.
  function airlabsError(j) {
    if (!j || typeof j !== 'object' || !j.error) return null;
    const e = j.error;
    const msg = typeof e === 'object' ? String(e.message || 'unknown error') : String(e);
    const code = typeof e === 'object' && e.code ? ` (${e.code})` : '';
    return `AirLabs error: ${msg}${code}`;
  }

  const UNREADABLE = 'unreadable capture';
  // A 200 can still carry HTML or a truncated body; only a `response` array is a board.
  const isBoard = (j) => !!j && typeof j === 'object' && Array.isArray(j.response);
  const isBlank = (body) => body == null || !String(body).trim();
  const is2xx = (st) => st >= 200 && st <= 299;

  function sumAirlabs(status, body, ctx) {
    const j = parse(body);
    const err = airlabsError(j);
    if (err) return note(err);
    // A failed fetch is stored with an empty body; counting it would read as an empty board.
    if (!is2xx(status)) return null;
    if (isBlank(body)) return note('AirLabs sent an empty body');
    if (!isBoard(j)) return note(UNREADABLE);
    const list = j.response;
    const m = /^(dep|arr):(.*)$/.exec((ctx && ctx.subject) || '');
    const what = m ? (m[1] === 'dep' ? `Departures from ${esc(m[2])}` : `Arrivals at ${esc(m[2])}`) : 'Board';
    return `<p class="rd-note">${what}: ${list.length} flight${list.length === 1 ? '' : 's'}</p>`;
  }

  function summarize(source, status, bodyText, ctx) {
    try {
      const parts = [];
      const st = Number(status);
      if (st === 429) parts.push(badge('rate-limited'));
      else if (st && (st < 200 || st > 299)) parts.push(badge(`HTTP ${st}`));
      let body = null;
      if (source === 'adsbdb') body = sumAdsbdb(st, bodyText);
      else if (source === 'hexdb') body = sumHexdb(bodyText, ctx);
      else if (source === 'aeroapi') body = sumAeroapi(bodyText, ctx);
      else if (source === 'airlabs') body = sumAirlabs(st, bodyText, ctx);
      // A non-2xx status already explains itself; only claim "unreadable" for a 2xx.
      if (body == null && !parts.length) body = note('no readable summary; see the JSON below');
      return `<div class="rd-summary">${parts.join(' ')}${body || ''}</div>`;
    } catch {
      return `<div class="rd-summary">${note('no readable summary; see the JSON below')}</div>`;
    }
  }

  function boardRows(bodyText, dir) {
    const j = parse(bodyText);
    if (!j || !Array.isArray(j.response)) return [];
    const k = dir === 'arr' ? 'arr' : 'dep';
    return j.response.filter((r) => r && typeof r === 'object').map((r) => ({
      flightIata: r.flight_iata || '',
      flightIcao: r.flight_icao || '',
      dep: r.dep_iata || r.dep_icao || '',
      arr: r.arr_iata || r.arr_icao || '',
      sched: r[`${k}_time_utc`] || '',
      est: r[`${k}_estimated_utc`] || '',
      actual: r[`${k}_actual_utc`] || '',
      delay: r[`${k}_delayed`] == null ? null : r[`${k}_delayed`],
      status: r.status || '',
    }));
  }

  // One side of a board refresh, ready to render: either rows or why there are none.
  function boardState(status, bodyText, dir) {
    const st = Number(status);
    if (!is2xx(st)) return { ok: false, error: `AirLabs answered HTTP ${st}`, rows: [] };
    if (isBlank(bodyText)) return { ok: false, error: `AirLabs answered HTTP ${st} with an empty body`, rows: [] };
    const j = parse(bodyText);
    const err = airlabsError(j);
    if (err) return { ok: false, error: err, rows: [] };
    if (!isBoard(j)) return { ok: false, error: UNREADABLE, rows: [] };
    return { ok: true, error: null, rows: boardRows(bodyText, dir) };
  }

  // What the list meta alone shows; an `error` body needs the body itself.
  const captureFailed = (m) => !!m && (!is2xx(Number(m.status)) || m.size === 0);

  // Index of the refresh to open: the linked response's own refresh wins, since
  // a group's time is its later fetch and can fall after `before`; otherwise
  // the newest at or before `before`. -1 when nothing qualifies.
  function pickGroup(groups, before, id) {
    const gs = groups || [];
    if (id != null && id !== '') {
      const has = (m) => m && String(m.id) === String(id);
      const i = gs.findIndex((g) => has(g.dep) || has(g.arr));
      if (i >= 0) return i;
    }
    if (before == null) return gs.length ? 0 : -1;
    return gs.findIndex((g) => g.at <= before);
  }

  // `before` for the next older page, or null when paging should stop. The
  // oldest stamp is repeated (the API is inclusive) so a dep/arr pair saved in
  // the same second cannot be split across pages; callers dedupe by id.
  function nextPageBefore(page, limit, floor, prevBefore) {
    if (!page || page.length < limit) return null;
    const oldest = Math.min(...page.map((m) => m.fetched_at));
    if (!Number.isFinite(oldest) || oldest < floor) return null;
    if (prevBefore != null && oldest >= prevBefore) return null;
    return oldest;
  }

  function mergeResponses(...lists) {
    const byId = new Map();
    for (const l of lists) for (const m of l || []) byId.set(String(m.id), m);
    return Array.from(byId.values()).sort((a, b) => b.fetched_at - a.fetched_at);
  }

  // Each refresh fetches departures and arrivals back to back, so captures
  // close in time belong together. Input and output are newest first.
  function groupRefreshes(metas) {
    const groups = [];
    for (const m of metas || []) {
      const side = String(m.subject || '').startsWith('arr:') ? 'arr' : 'dep';
      const g = groups.find((x) => !x[side] && Math.abs(x.at - m.fetched_at) <= PAIR_WINDOW_S);
      if (g) { g[side] = m; g.at = Math.max(g.at, m.fetched_at); }
      else groups.push({ at: m.fetched_at, dep: null, arr: null, [side]: m });
    }
    return groups.sort((a, b) => b.at - a.at);
  }

  globalThis.RawData = {
    normCallsign, normReg, faSubjects, responseLink, routeOf, summarize, boardRows, groupRefreshes,
    boardState, captureFailed, pickGroup, nextPageBefore, mergeResponses, PAIR_WINDOW_S,
  };
})();
