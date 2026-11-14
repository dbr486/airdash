import test from 'node:test';
import assert from 'node:assert/strict';

await import('../public/rawdata.js');
const RD = globalThis.RawData;

const adsbdb = (name = 'British Airways') => JSON.stringify({
  response: { flightroute: {
    callsign: 'BAW466',
    airline: { name, iata: 'BA', icao: 'BAW' },
    origin: { iata_code: 'LHR', icao_code: 'EGLL', municipality: 'London', latitude: 51.4706, longitude: -0.461941 },
    destination: { iata_code: 'BCN', icao_code: 'LEBL', municipality: 'Barcelona', latitude: 41.2971, longitude: 2.07846 },
  } },
});

const flight = (dest) => ({
  ident: 'TRA9033', registration: 'PH-HZA', status: 'En Route',
  origin: { code_icao: 'LEMG', code_iata: 'AGP' },
  destination: dest,
  scheduled_off: '2026-09-28T13:15:00Z', actual_off: '2026-09-28T13:41:17Z',
  scheduled_on: '2026-09-28T16:00:00Z', actual_on: null,
});

const board = JSON.stringify({ response: [{
  flight_iata: 'D85054', flight_icao: 'NSZ5054',
  dep_iata: 'AGP', dep_icao: 'LEMG', arr_iata: 'ARN', arr_icao: 'ESSA',
  dep_time_utc: '2026-09-29 13:10', dep_estimated_utc: '2026-09-29 13:11', dep_actual_utc: '2026-09-29 13:12',
  arr_time_utc: '2026-09-29 17:25', arr_estimated_utc: null, arr_actual_utc: null,
  dep_delayed: 1, arr_delayed: 7, status: 'active',
}] });

test('normalisers', () => {
  assert.equal(RD.normReg(' ec-mxx '), 'ECMXX');
  assert.equal(RD.normCallsign(' ibe1 '), 'IBE1');
  assert.deepEqual(RD.faSubjects({ callsign: 'ibe1 ', reg: 'EC-MXX' }), ['IBE1', 'ECMXX']);
  assert.deepEqual(RD.faSubjects({ callsign: '', reg: 'EC-MXX' }), ['ECMXX']);
  assert.deepEqual(RD.faSubjects({ callsign: 'abc', reg: 'ABC' }), ['ABC']);
});

test('responseLink', () => {
  const pass = { callsign: 'RYR5EL', reg: 'EI-DWB', last_seen: 100, adsbdb: { status: 'hit', route: 'AGP-STN' }, hexdb: { status: 'not_asked' }, board: { status: 'hit' }, aeroapi: { status: 'hit', route: 'A-B' } };
  assert.equal(RD.responseLink(pass, 'adsbdb'), 'response.html?source=adsbdb&subject=RYR5EL&before=160&pass=AGP-STN');
  assert.equal(RD.responseLink(pass, 'board'), 'board.html?before=160&flight=RYR5EL');
  assert.equal(RD.responseLink(pass, 'hexdb'), null);
  assert.match(RD.responseLink(pass, 'aeroapi'), /&alt=EIDWB$/);
});

test('routeOf prefers IATA', () => {
  assert.equal(RD.routeOf({ code_iata: 'AGP', code_icao: 'LEMG' }), 'AGP');
  assert.equal(RD.routeOf({ code_icao: 'LEMG' }), 'LEMG');
  assert.equal(RD.routeOf(null), '');
});

test('adsbdb summary escapes values and handles no-route', () => {
  const html = RD.summarize('adsbdb', 200, adsbdb('<b>X</b>'), {});
  assert.ok(!html.includes('<b>X</b>'));
  assert.ok(html.includes('&lt;b&gt;X&lt;/b&gt;'));
  assert.ok(html.includes('Barcelona'));
  assert.match(RD.summarize('adsbdb', 404, '', {}), /no route/);
  assert.match(RD.summarize('adsbdb', 200, '{"response":"unknown callsign"}', {}), /no route/);
});

test('aeroapi marks the used row once', () => {
  const body = JSON.stringify({ flights: [flight({ code_icao: 'EHAM', code_iata: 'AMS' }), flight({ code_icao: 'EGKK', code_iata: 'LGW' })] });
  const html = RD.summarize('aeroapi', 200, body, { usedRoute: 'AGP-LGW' });
  assert.equal(html.split('class="used"').length - 1, 1);
});

test('hexdb flags stale routes', () => {
  const old = Math.floor(Date.now() / 1000) - 40 * 86400;
  assert.match(RD.summarize('hexdb', 200, `{"route":"LEMG-EGKK","updatetime":${old}}`, {}), /older than 30 days/);
  const fresh = Math.floor(Date.now() / 1000) - 86400;
  assert.ok(!RD.summarize('hexdb', 200, `{"route":"LEMG-EGKK","updatetime":${fresh}}`, {}).includes('older than'));
});

test('hexdb uses airport names from ctx', () => {
  const html = RD.summarize('hexdb', 200, '{"route":"LEMG-EBAW","updatetime":1}', { airports: { EBAW: { airport: 'Antwerp International Airport', iata: 'ANR' } } });
  assert.ok(html.includes('Antwerp International Airport'));
});

test('http errors never throw', () => {
  assert.match(RD.summarize('adsbdb', 429, 'Too Many Requests', {}), /rate-limited/);
  assert.match(RD.summarize('adsbdb', 500, '{}', {}), /HTTP 500/);
  assert.match(RD.summarize('aeroapi', 200, 'not json', {}), /no readable summary/);
  assert.doesNotThrow(() => RD.summarize('nope', 200, null, null));
});

test('airlabs summary counts rows and shows direction', () => {
  const html = RD.summarize('airlabs', 200, board, { subject: 'dep:AGP' });
  assert.match(html, /1 flight/);
  assert.match(html, /[Dd]epartures/);
});

test('groupRefreshes pairs dep and arr within 120s', () => {
  const m = (subject, t) => ({ source: 'airlabs', subject, fetched_at: t });
  const g = RD.groupRefreshes([m('arr:AGP', 1000), m('dep:AGP', 990), m('dep:AGP', 500)]);
  assert.equal(g.length, 2);
  assert.equal(g[0].at, 1000);
  assert.equal(g[0].dep.fetched_at, 990);
  assert.equal(g[0].arr.fetched_at, 1000);
  assert.equal(g[1].arr, null);
  assert.equal(g[1].dep.fetched_at, 500);
});

test('boardRows uses the direction own times', () => {
  const dep = RD.boardRows(board, 'dep')[0];
  assert.equal(dep.sched, '2026-09-29 13:10');
  assert.equal(dep.delay, 1);
  assert.equal(dep.flightIata, 'D85054');
  const arr = RD.boardRows(board, 'arr')[0];
  assert.equal(arr.sched, '2026-09-29 17:25');
  assert.equal(arr.delay, 7);
  assert.deepEqual(RD.boardRows('', 'dep'), []);
  assert.deepEqual(RD.boardRows('{bad', 'dep'), []);
});

test('hexdb age never goes negative for a future updatetime', () => {
  const future = Math.floor(Date.now() / 1000) + 5 * 86400;
  const html = RD.summarize('hexdb', 200, `{"route":"LEMG-EGKK","updatetime":${future}}`, {});
  assert.match(html, /0 days ago/);
  assert.ok(!html.includes('-'.concat('1 days')));
});

const inst = (day) => ({ ...flight({ code_icao: 'EGKK', code_iata: 'LGW' }),
  scheduled_off: `2026-09-${day}T13:15:00Z`, actual_off: `2026-09-${day}T13:20:00Z` });
const usedCount = (h) => h.split('class="used"').length - 1;

test('aeroapi marks only the instance flown at the pass time', () => {
  const body = JSON.stringify({ flights: [inst(28), inst(27), inst(26)] });
  const at = Date.parse('2026-09-27T14:00:00Z') / 1000;
  const html = RD.summarize('aeroapi', 200, body, { usedRoute: 'AGP-LGW', passAt: at });
  assert.equal(usedCount(html), 1);
  const used = html.split('<tr').find((r) => r.includes('class="used"'));
  assert.ok(used.includes('2026-09-27T13:20:00.000Z'));
});

test('aeroapi falls forward to the next scheduled instance, else none', () => {
  const body = JSON.stringify({ flights: [{ ...inst(28), actual_off: null }, { ...inst(29), actual_off: null }] });
  const html = RD.summarize('aeroapi', 200, body, { usedRoute: 'AGP-LGW', passAt: Date.parse('2026-09-27T00:00:00Z') / 1000 });
  assert.equal(usedCount(html), 1);
  assert.ok(html.split('<tr').find((r) => r.includes('class="used"')).includes('2026-09-28T13:15:00.000Z'));
  assert.equal(usedCount(RD.summarize('aeroapi', 200, body, { usedRoute: 'XXX-YYY', passAt: 1 })), 0);
});

test('aeroapi shows an en dash for a missing time and scrolls inside the card', () => {
  const body = JSON.stringify({ flights: [flight({ code_icao: 'EGKK', code_iata: 'LGW' })] });
  const html = RD.summarize('aeroapi', 200, body, {});
  assert.ok(html.includes('</time> / &ndash;'));
  assert.ok(html.includes('class="scroll"'));
});

test('an adsbdb 429 is not reported as "no route"', () => {
  const html = RD.summarize('adsbdb', 429, '', {});
  assert.ok(!html.includes('no route'));
  assert.match(html, /rate-limited/);
  assert.ok(!RD.summarize('adsbdb', 500, '', {}).includes('no route'));
});

// Built from local wall-clock times so the day boundary is the same in every
// timezone (UTC stamps near 14:00Z cross midnight in UTC+14).
const localInst = (day, h, m) => {
  const off = new Date(2026, 8, day, h, m).toISOString();
  return { ...flight({ code_icao: 'EGKK', code_iata: 'LGW' }), scheduled_off: off, actual_off: off };
};

test('aeroapi dates only the instances from another day than the pass', () => {
  const same = localInst(27, 12, 0);
  const prev = localInst(26, 12, 0);
  const body = JSON.stringify({ flights: [same, prev] });
  const at = new Date(2026, 8, 27, 13, 0).getTime() / 1000;
  const rows = RD.summarize('aeroapi', 200, body, { usedRoute: 'AGP-LGW', passAt: at }).split('<tr').slice(2);
  const sameDay = rows.find((r) => r.includes(same.actual_off));
  const otherDay = rows.find((r) => r.includes(prev.actual_off));
  const firstTime = (r) => /<time[^>]*>([^<]*)<\/time>/.exec(r)[1];
  assert.match(firstTime(sameDay), /^\d{2}:\d{2}$/);
  assert.match(firstTime(otherDay), /^26 Sep \d{2}:\d{2}$/);
});

test('adsbdb says "no route" only for a 2xx unknown-callsign string or a 404', () => {
  const html = RD.summarize('adsbdb', 429, '{"response":"Too many requests"}', {});
  assert.ok(!html.includes('no route'));
  assert.match(html, /rate-limited/);
  assert.ok(!RD.summarize('adsbdb', 500, '{"response":"internal error"}', {}).includes('no route'));
  assert.ok(!RD.summarize('adsbdb', 200, '{"response":"invalid request"}', {}).includes('no route'));
  assert.match(RD.summarize('adsbdb', 200, '{"response":"Unknown Callsign"}', {}), /no route/);
});

test('airlabs summary shows an error answer, escaped, without a flight count', () => {
  const html = RD.summarize('airlabs', 200, '{"error":{"message":"Invalid <api_key>","code":"wrong_key"}}', { subject: 'dep:AGP' });
  assert.match(html, /AirLabs error: Invalid &lt;api_key&gt; \(wrong_key\)/);
  assert.ok(!html.includes('<api_key>'));
  assert.ok(!/\d+ flights?/.test(html));
});

test('airlabs summary skips the flight count for a non-2xx or an empty body', () => {
  const failed = RD.summarize('airlabs', 500, '', { subject: 'dep:AGP' });
  assert.match(failed, /HTTP 500/);
  assert.ok(!/flights?/.test(failed));
  const empty = RD.summarize('airlabs', 200, '', { subject: 'arr:AGP' });
  assert.ok(!/flights?/.test(empty));
  assert.match(empty, /empty body/);
});

test('boardState tells a failed capture from a readable board', () => {
  assert.deepEqual(RD.boardState(500, '', 'dep'), { ok: false, error: 'AirLabs answered HTTP 500', rows: [] });
  assert.equal(RD.boardState(200, ' ', 'dep').ok, false);
  assert.match(RD.boardState(200, '', 'dep').error, /empty body/);
  const e = RD.boardState(200, '{"error":{"message":"Usage limit reached","code":"month_limit_exceeded"}}', 'arr');
  assert.deepEqual(e, { ok: false, error: 'AirLabs error: Usage limit reached (month_limit_exceeded)', rows: [] });
  const ok = RD.boardState(200, board, 'arr');
  assert.equal(ok.ok, true);
  assert.equal(ok.error, null);
  assert.equal(ok.rows[0].sched, '2026-09-29 17:25');
});

test('captureFailed flags non-2xx and empty captures from their meta', () => {
  assert.equal(RD.captureFailed({ status: 429, size: 0 }), true);
  assert.equal(RD.captureFailed({ status: 200, size: 0 }), true);
  assert.equal(RD.captureFailed({ status: 200, size: 5000 }), false);
  assert.equal(RD.captureFailed(null), false);
});

test('pickGroup prefers the linked id, then the newest at or before, else none', () => {
  const g = [
    { at: 3000, dep: { id: 6 }, arr: { id: 5 } },
    { at: 2000, dep: { id: 4 }, arr: null },
    { at: 1000, dep: null, arr: { id: 1 } },
  ];
  assert.equal(RD.pickGroup(g, null, null), 0);
  assert.equal(RD.pickGroup(g, 2500, null), 1);
  assert.equal(RD.pickGroup(g, 2000, null), 1);
  assert.equal(RD.pickGroup(g, 500, null), -1);
  assert.equal(RD.pickGroup(g, 2990, '5'), 0);
  assert.equal(RD.pickGroup(g, 1500, '99'), 2);
  assert.equal(RD.pickGroup([], null, null), -1);
});

test('nextPageBefore pages back until a short page, the floor or no progress', () => {
  const page = (n, oldest) => Array.from({ length: n }, (_, i) => ({ id: i, fetched_at: oldest + n - 1 - i }));
  assert.equal(RD.nextPageBefore(page(100, 5000), 100, 1000, null), 5000);
  assert.equal(RD.nextPageBefore(page(99, 5000), 100, 1000, null), null);
  assert.equal(RD.nextPageBefore(page(100, 900), 100, 1000, null), null);
  assert.equal(RD.nextPageBefore(page(100, 5000), 100, 1000, 5000), null);
  assert.equal(RD.nextPageBefore([], 100, 1000, null), null);
});

test('mergeResponses dedupes by id and sorts newest first', () => {
  const a = [{ id: 3, fetched_at: 30 }, { id: 2, fetched_at: 20 }];
  const b = [{ id: 2, fetched_at: 20 }, { id: 1, fetched_at: 10 }, { id: 4, fetched_at: 40 }];
  assert.deepEqual(RD.mergeResponses(a, b).map((m) => m.id), [4, 3, 2, 1]);
});

test('boardState and the airlabs summary reject a 200 body without a response array', () => {
  for (const body of ['{"response":[', '<html><body>Bad gateway</body></html>', '{}', '{"response":"x"}']) {
    assert.deepEqual(RD.boardState(200, body, 'dep'), { ok: false, error: 'unreadable capture', rows: [] }, body);
    const html = RD.summarize('airlabs', 200, body, { subject: 'dep:AGP' });
    assert.match(html, /unreadable capture/, body);
    assert.ok(!/\d+ flights?/.test(html), body);
  }
  assert.deepEqual(RD.boardState(200, '{"response":[]}', 'dep'), { ok: true, error: null, rows: [] });
  assert.match(RD.summarize('airlabs', 200, '{"response":[]}', { subject: 'dep:AGP' }), /0 flights/);
});

test('hexdb age is measured at fetch time, like the service', () => {
  const fetchedAt = 1_800_000_000;
  const old = RD.summarize('hexdb', 200, `{"route":"LEMG-EGKK","updatetime":${fetchedAt - 30.5 * 86400}}`, { fetchedAt });
  assert.match(old, /older than 30 days, ignored/);
  // 29 days at fetch, but that fetch was long ago: still accepted then.
  const fetchedLongAgo = Math.floor(Date.now() / 1000) - 11 * 86400;
  const kept = RD.summarize('hexdb', 200, `{"route":"LEMG-EGKK","updatetime":${fetchedLongAgo - 29 * 86400}}`, { fetchedAt: fetchedLongAgo });
  assert.ok(!kept.includes('older than'));
  assert.match(kept, /29 days/);
});
