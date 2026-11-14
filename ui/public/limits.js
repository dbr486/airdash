// Pure helpers for the Sources page's Limits card: turn one /api/limits
// service into the strings a row shows. A classic script (no modules), like
// rawdata.js, so the page loads it with a plain <script> tag and tests import
// it for its side effect; only renderLimits touches the DOM.
(function () {
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const hm = (iso) => new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  // Dates read "14 Nov" whatever the browser locale, like the page's other
  // en-GB stamps; times stay local.
  const dm = (d) => new Date(d).toLocaleDateString('en-GB', { day: 'numeric', month: 'short' });
  // The cap of a month resets on the 1st (UTC) of the next one.
  const nextMonth = (iso) => { const d = new Date(iso); return Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1); };

  function fmtEvery(sec) {
    const m = Math.round(sec / 60);
    const h = Math.floor(m / 60);
    if (!h) return `${m} min`;
    return m % 60 ? `${h} h ${m % 60} min` : `${h} h`;
  }
  const fmt = (n) => Number(n || 0).toLocaleString();

  function usageText(s) {
    if (!s.enabled) return 'not configured';
    const period = s.period === 'month' ? 'this month' : 'today';
    let out = s.capped ? `${fmt(s.used)} / ${fmt(s.cap)} ${period}` : `${fmt(s.calls)} calls today · no cap`;
    if (s.poll_s) out += ` · polled every ${s.poll_s} s`;
    if (s.refresh_s) out += ` · refreshes every ${fmtEvery(s.refresh_s)}`;
    return out;
  }

  // usageNote is the short line beside a cap field on the settings page.
  function usageNote(s) {
    if (!s.enabled) return 'not configured';
    const period = s.period === 'month' ? 'this month' : 'today';
    let out = `${fmt(s.capped ? s.used : s.calls)} used ${period}`;
    if (!s.capped) return `${out} · no cap`;
    if (s.state === 'cap_reached') {
      return s.period === 'month' && s.state_since ? `${out} · cap reached, resumes ${dm(nextMonth(s.state_since))}` : `${out} · cap reached`;
    }
    if (s.refresh_s) out += ` · refreshes every ${fmtEvery(s.refresh_s)}`;
    return out;
  }

  function stateText(s) {
    switch (s.state) {
      case 'off': return 'Off';
      case 'blocked': return `Blocked: ${s.state_reason || 'calls refused'}`;
      case 'paused': return s.state_until ? `Paused until ${hm(s.state_until)} (429 ban)` : 'Paused (429 ban)';
      case 'cap_reached':
        if (!s.state_since) return 'Cap reached';
        // A monthly cap stays reached for weeks, so the day matters there.
        return s.period === 'month'
          ? `Cap reached on ${dm(s.state_since)} ${hm(s.state_since)} · resumes ${dm(nextMonth(s.state_since))}`
          : `Cap reached at ${hm(s.state_since)}`;
      default: return 'OK';
    }
  }

  // limitRow returns escaped, display-ready strings for one service.
  function limitRow(s) {
    const recent = (s.recent_errors || []).map((e) =>
      `${esc(hm(e.at))} · ${esc(e.message)}${e.subject ? ` · ${esc(e.subject)}` : ''}`);
    const latest = (s.recent_errors || [])[0];
    return {
      label: esc(s.label),
      usage: esc(usageText(s)),
      state: esc(stateText(s)),
      refused: s.refused > 0 ? `${fmt(s.refused)} aircraft` : '',
      errors: s.errors > 0 ? `${fmt(s.errors)}${latest ? ` · ${esc(hm(latest.at))} · ${esc(latest.message)}` : ''}` : '',
      recent,
      meter: s.enabled && s.capped ? (s.cap > 0 ? Math.min(1, s.used / s.cap) : 1) : null,
      flagged: s.state === 'paused' || s.state === 'cap_reached' || s.state === 'blocked' || s.errors > 0,
    };
  }

  function renderLimits(el, services) {
    const rows = (services || []).map((s) => {
      const r = limitRow(s);
      const meter = r.meter == null ? '' : `<span class="lim-meter"><span style="width:${(r.meter * 100).toFixed(1)}%"></span></span>`;
      const more = r.recent.length ? `<ul class="lim-recent" hidden>${r.recent.map((x) => `<li>${x}</li>`).join('')}</ul>` : '';
      return `<tr class="lim${r.flagged ? ' flagged' : ''}${r.recent.length ? ' has-recent' : ''}"${r.recent.length ? ' tabindex="0" aria-expanded="false"' : ''}>
        <th scope="row">${r.label}</th>
        <td>${r.usage}${meter}</td>
        <td class="lim-state">${r.state}</td>
        <td>${r.refused}</td>
        <td>${r.errors}${more}</td>
      </tr>`;
    }).join('');
    el.innerHTML = `<thead><tr><th>Service</th><th>Usage</th><th>State</th><th>Refused today</th><th>Errors today</th></tr></thead><tbody>${rows}</tbody>`;
  }

  globalThis.Limits = { limitRow, renderLimits, usageNote };
})();
