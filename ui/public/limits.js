// Pure helpers for the Sources page's Limits card: turn one /api/limits
// service into the strings a row shows. A classic script (no modules), like
// rawdata.js, so the page loads it with a plain <script> tag and tests import
// it for its side effect; only renderLimits touches the DOM.
(function () {
  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const hm = (iso) => new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const dm = (iso) => new Date(iso).toLocaleDateString([], { day: 'numeric', month: 'short' });
  const fmt = (n) => Number(n || 0).toLocaleString();

  function usageText(s) {
    if (!s.enabled) return 'not configured';
    const period = s.period === 'month' ? 'this month' : 'today';
    let out = s.capped ? `${fmt(s.used)} / ${fmt(s.cap)} ${period}` : `${fmt(s.calls)} calls today · no cap`;
    if (s.poll_s) out += ` · polled every ${s.poll_s} s`;
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
        return s.period === 'month' ? `Cap reached on ${dm(s.state_since)} ${hm(s.state_since)}` : `Cap reached at ${hm(s.state_since)}`;
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

  globalThis.Limits = { limitRow, renderLimits };
})();
