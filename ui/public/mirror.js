// Same rules as the firmware: poll every 3 s, "stale" when age_s > 30 or
// the body could not be parsed, "x" when the request failed; keep the last
// good content behind the marks.
const $ = (id) => document.getElementById(id);
const POLL_MS = 3000, STALE_AFTER_S = 30;
let dotOn = false, screen = 'boot', lastNearby = 0;
// Wall-clock time of the last successful render, so the mirror can keep
// aging (Staleness rule: age > 30 s -> stale) while /api/nearest itself
// keeps failing, the same way the firmware ages its last screen rather
// than freezing the "stale" mark at whatever it was when polling broke.
let lastOkAt = null;
// Aircraft being followed (its hex) after "Next aircraft"; '' = the nearest.
let selectedHex = '', nextRequested = false;
// Poll sequence number: a slow interval poll must not overwrite the answer
// to a later click (it would undo the step).
let pollSeq = 0;

function show(which) {
  if (screen === which) return;
  screen = which;
  for (const id of ['aircraft', 'notraffic', 'boot']) $(id).classList.toggle('hidden', id !== which);
}
function marks(stale, err) {
  $('stale').classList.toggle('hidden', !stale);
  $('err').classList.toggle('hidden', !err);
}
function routeText(n) {
  // The service resolves labels (city or code) from show_names; older
  // services without labels fall back to the codes.
  const o = n.origin_label ?? n.origin, d = n.dest_label ?? n.dest;
  if (o || d) return `${o || '?'} -> ${d || '?'}`;
  return n.airline || n.reg || '';
}
function radiusText(r) { return `within ${Number.isInteger(r) ? r : r.toFixed(1)} km`; }
function delayText(d) {
  if (!d) return '';
  switch (d.kind) {
    case 'on_time': return 'On time';
    case 'late': return `Late ${d.min}m`;
    case 'early': return `Early ${Math.abs(d.min)}m`;
    case 'cancelled': return 'Cancelled';
    case 'diverted': return 'Diverted';
    default: return '';
  }
}

function render(n) {
  if (n.found) {
    show('aircraft');
    $('cs').textContent = n.callsign;
    $('route').textContent = routeText(n);
    const dl = $('delay');
    dl.textContent = delayText(n.delay);
    dl.className = 'dl' + (n.delay ? ' ' + n.delay.kind : '');
    $('type').textContent = [n.type, n.airline].filter(Boolean).join('  ');
    $('alt').textContent = `${n.alt_ft} ft  ${n.gs_kt} kt`;
    $('dist').textContent = n.nearby > 1 ? `${n.dist_km.toFixed(1)} km  ${n.index + 1}/${n.nearby}` : `${n.dist_km.toFixed(1)} km`;
    selectedHex = (n.index > 0 || n.hex === selectedHex) ? n.hex : '';
    $('tri').style.transform = `rotate(${n.bearing}deg)`;
    // Only touch src when the airline changes: re-setting it every 3 s
    // poll would flicker, and hidden-until-load avoids a broken-image
    // icon for airlines the CDN lacks.
    const lg = $('logo');
    const code = n.logo || '';
    if (!code) { lg.classList.add('hidden'); lg.removeAttribute('src'); delete lg.dataset.code; }
    else if (lg.dataset.code !== code) { lg.dataset.code = code; lg.classList.add('hidden'); lg.src = `/api/logo/${code}/104x32.png`; }
  } else {
    show('notraffic');
    selectedHex = '';
    $('within').textContent = radiusText(n.radius_km);
    dotOn = !dotOn;
    $('dot').style.visibility = dotOn ? 'visible' : 'hidden';
  }
  lastNearby = n.nearby;
  lastOkAt = Date.now();
  marks(n.age_s > STALE_AFTER_S, false);
}

async function poll() {
  let res;
  try {
    const q = nextRequested ? `?after=${encodeURIComponent(selectedHex)}` : (selectedHex ? `?hex=${encodeURIComponent(selectedHex)}` : '');
    nextRequested = false;
    const mine = ++pollSeq;
    res = await fetch('/api/nearest' + q, { cache: 'no-store' });
    if (mine !== pollSeq) return;
    if (!res.ok) throw new Error(`http ${res.status}`);
  } catch (e) {
    // Network/HTTP failure: the service itself is unreachable or erroring.
    // Staleness must keep advancing on wall-clock time even though polling
    // is failing, so the mirror ages like the firmware instead of freezing
    // the stale mark at whatever it was when the outage started.
    selectedHex = '';
    if (screen === 'boot') $('bootline').textContent = 'service unreachable';
    const currentStaleFromLastRender = !$('stale').classList.contains('hidden');
    const aged = lastOkAt !== null && (Date.now() - lastOkAt) / 1000 > STALE_AFTER_S;
    marks(aged || currentStaleFromLastRender, true);
    if (typeof onMirrorPoll === 'function') onMirrorPoll(false);
    return;
  }
  try {
    const n = await res.json();
    if (typeof n.found !== 'boolean') throw new Error('bad body');
    render(n);
  } catch (e) {
    // The request succeeded but the body was unusable: treat it like aged
    // data (stale), not a service outage (err) — the service answered.
    marks(true, false);
  }
  // The hosting page (settings form) may want to react; the widget has no hook.
  if (typeof onMirrorPoll === 'function') onMirrorPoll(true);
}

// Step to the next aircraft in range (the same as the display's button).
function nextAircraft() { nextRequested = true; poll(); }

// The logo is hidden until it loads (no broken-image icon for airlines the
// CDN lacks). On error, forget the code so the next poll retries; the
// browser's cached 404 (an hour) keeps that retry from reaching the service.
$('logo').addEventListener('load', () => $('logo').classList.remove('hidden'));
$('logo').addEventListener('error', () => { $('logo').classList.add('hidden'); delete $('logo').dataset.code; });

poll();
setInterval(poll, POLL_MS);

