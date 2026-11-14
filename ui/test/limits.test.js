import test from 'node:test';
import assert from 'node:assert/strict';

await import('../public/limits.js');
const L = globalThis.Limits;

const hm = (iso) => new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
const base = { id: 'x', label: 'X', enabled: true, period: 'day', used: 0, cap: 0, state: 'ok',
  state_since: null, state_until: null, refused: 0, calls: 0, errors: 0, recent_errors: [] };

test('capped day at the cap is flagged with the time it was hit', () => {
  const r = L.limitRow({ ...base, capped: true, used: 65, cap: 65, calls: 65, state: 'cap_reached', state_since: '2026-11-02T09:28:00Z' });
  assert.equal(r.usage, '65 / 65 today');
  assert.equal(r.state, `Cap reached at ${hm('2026-11-02T09:28:00Z')}`);
  assert.equal(r.flagged, true);
  assert.equal(r.meter, 1);
});

test('uncapped service shows its calls and no cap', () => {
  const r = L.limitRow({ ...base, used: 412, calls: 412 });
  assert.equal(r.usage, '412 calls today · no cap');
  assert.equal(r.state, 'OK');
  assert.equal(r.flagged, false);
  assert.equal(r.meter, null);
});

test('positions add the poll interval', () => {
  const r = L.limitRow({ ...base, used: 9000, calls: 9000, poll_s: 5 });
  assert.match(r.usage, / · polled every 5 s$/);
});

test('paused adsbdb shows until when', () => {
  const r = L.limitRow({ ...base, state: 'paused', state_until: '2026-11-02T16:40:00Z' });
  assert.equal(r.state, `Paused until ${hm('2026-11-02T16:40:00Z')} (429 ban)`);
  assert.equal(r.flagged, true);
});

test('a monthly cap reached names the day', () => {
  const at = '2026-11-14T09:28:00Z';
  const r = L.limitRow({ ...base, capped: true, period: 'month', used: 900, cap: 900, state: 'cap_reached', state_since: at });
  assert.equal(r.usage, '900 / 900 this month');
  assert.equal(r.state, `Cap reached on 14 Nov ${hm(at)} · resumes 1 Dec`);
});

test('an off service is not flagged', () => {
  const r = L.limitRow({ ...base, enabled: false, state: 'off' });
  assert.equal(r.state, 'Off');
  assert.equal(r.usage, 'not configured');
  assert.equal(r.flagged, false);
});

test('errors show count and the latest message; zero refused is blank', () => {
  const recent = [{ at: '2026-11-02T14:02:00Z', message: 'status 503', subject: 'RYR1' },
    { at: '2026-11-02T13:00:00Z', message: 'timeout', subject: 'RYR2' }];
  const r = L.limitRow({ ...base, errors: 2, recent_errors: recent });
  assert.equal(r.errors, `2 · ${hm(recent[0].at)} · status 503`);
  assert.equal(r.recent.length, 2);
  assert.equal(r.refused, '');
  assert.equal(r.flagged, true);
  assert.equal(L.limitRow({ ...base, refused: 4 }).refused, '4 aircraft');
});

test('strings from the service are escaped', () => {
  const r = L.limitRow({ ...base, label: '<b>', errors: 1, recent_errors: [{ at: '2026-11-02T14:02:00Z', message: '<i>', subject: '<s>' }] });
  assert.equal(r.label, '&lt;b&gt;');
  assert.ok(!r.errors.includes('<i>'));
  assert.ok(r.recent[0].includes('&lt;s&gt;'));
});

test('a blocked service shows why and is flagged', () => {
  const r = L.limitRow({ ...base, capped: true, used: 3, cap: 65, state: 'blocked', state_reason: 'call count could not be saved; paid calls refused' });
  assert.equal(r.state, 'Blocked: call count could not be saved; paid calls refused');
  assert.equal(r.flagged, true);
});

test('a zero allowance on a capped service is not "no cap"', () => {
  const r = L.limitRow({ ...base, capped: true, used: 0, cap: 0, state: 'cap_reached' });
  assert.equal(r.usage, '0 / 0 today');
  assert.equal(r.state, 'Cap reached');
});

test('AirLabs usage shows the automatic refresh interval', () => {
  const r = L.limitRow({ ...base, capped: true, period: 'month', used: 54, cap: 900, refresh_s: 5760 });
  assert.equal(r.usage, '54 / 900 this month · refreshes every 1 h 36 min');
  assert.equal(L.limitRow({ ...base, capped: true, period: 'month', used: 1, cap: 100000, refresh_s: 900 }).usage,
    '1 / 100,000 this month · refreshes every 15 min');
});

test('a monthly cap reached says when it resumes', () => {
  const at = '2026-11-14T09:28:00Z';
  const r = L.limitRow({ ...base, capped: true, period: 'month', used: 900, cap: 900, state: 'cap_reached', state_since: at });
  assert.match(r.state, / · resumes 1 Dec$/);
});

test('usageNote for the settings page', () => {
  assert.equal(L.usageNote({ ...base, calls: 412, used: 412 }), '412 used today · no cap');
  assert.equal(L.usageNote({ ...base, capped: true, used: 12, cap: 65 }), '12 used today');
  assert.equal(L.usageNote({ ...base, capped: true, period: 'month', used: 54, cap: 900, refresh_s: 5760 }),
    '54 used this month · refreshes every 1 h 36 min');
  assert.equal(L.usageNote({ ...base, capped: true, period: 'month', used: 900, cap: 900, state: 'cap_reached', state_since: '2026-11-14T09:28:00Z' }),
    '900 used this month · cap reached, resumes 1 Dec');
  assert.equal(L.usageNote({ ...base, enabled: false, state: 'off' }), 'not configured');
});
