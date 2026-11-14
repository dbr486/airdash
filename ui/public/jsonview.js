// Dependency-free JSON tree viewer. A classic script (no modules) so the
// pages can load it with a plain <script> tag and the tests can import it for
// its side effect. Everything that touches the DOM lives in mount(); the rest
// is pure string work so it runs under node.
(function () {
  const LAZY_OVER = 50; // arrays longer than this render their children on first open
  const OPEN_DEPTH = 1; // containers nested deeper than this start collapsed
  const MIN_TERM = 2; // one letter matches most of a 500-row board
  const FIND_CAP = 200; // reveal at most this many hits; opening thousands of rows freezes the tab

  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  // RFC 6901 escaping so keys containing "/" cannot be mistaken for nesting.
  const seg = (k) => String(k).replace(/~/g, '~0').replace(/\//g, '~1');
  const unseg = (s) => s.replace(/~1/g, '/').replace(/~0/g, '~');
  const isContainer = (v) => v !== null && typeof v === 'object';
  const countOf = (v) => (Array.isArray(v) ? v.length : Object.keys(v).length);
  const entries = (v) => (Array.isArray(v) ? v.map((x, i) => [i, x]) : Object.entries(v));

  function leaf(v) {
    if (v === null) return '<span class="jv-null">null</span>';
    if (typeof v === 'string') return `<span class="jv-str">"${esc(v)}"</span>`;
    if (typeof v === 'boolean') return `<span class="jv-bool">${v}</span>`;
    return `<span class="jv-num">${esc(v)}</span>`;
  }

  const keyHtml = (key) => (key === undefined ? '' : `<span class="jv-key">${esc(key)}</span><span class="jv-sep">: </span>`);

  function node(value, key, path, depth) {
    const attr = `data-path="${esc(path)}"`;
    if (!isContainer(value)) return `<div class="jv-row" ${attr}>${keyHtml(key)}${leaf(value)}</div>`;
    const n = countOf(value);
    const arr = Array.isArray(value);
    const mark = arr ? '[…]' : '{…}';
    if (n === 0) return `<div class="jv-row" ${attr}>${keyHtml(key)}<span class="jv-mark">${arr ? '[]' : '{}'}</span></div>`;
    const summary = `<summary>${keyHtml(key)}<span class="jv-mark">${mark}</span> <span class="jv-count">${n}</span></summary>`;
    if (arr && n > LAZY_OVER) return `<details class="jv-node jv-lazy" ${attr}>${summary}</details>`;
    const open = depth <= OPEN_DEPTH ? ' open' : '';
    return `<details class="jv-node" ${attr}${open}>${summary}${kids(value, path, depth)}</details>`;
  }

  function kids(value, path, depth) {
    return `<div class="jv-kids">${entries(value).map(([k, v]) => node(v, k, `${path}/${seg(k)}`, depth + 1)).join('')}</div>`;
  }

  function render(value) {
    return `<div class="jv">${node(value, undefined, '', 0)}</div>`;
  }

  function resolve(root, path) {
    let v = root;
    for (const part of path.split('/').slice(1)) {
      if (!isContainer(v)) return undefined;
      v = v[unseg(part)];
    }
    return v;
  }

  // Inner HTML of a lazy node's children (the .jv-kids wrapper).
  function renderChildren(root, path) {
    const v = resolve(root, path);
    if (!isContainer(v)) return '';
    return kids(v, path, path.split('/').length - 1);
  }

  function search(value, term, limit = Infinity) {
    const t = String(term == null ? '' : term).toLowerCase();
    const hits = [];
    if (t.trim().length < MIN_TERM) return hits;
    const has = (x) => String(x).toLowerCase().includes(t);
    (function walk(v, path) {
      if (!isContainer(v)) {
        if (has(v)) hits.push(path);
        return;
      }
      for (const [k, child] of entries(v)) {
        if (hits.length >= limit) return;
        const p = `${path}/${seg(k)}`;
        // Array indexes are not text the user sees as a key.
        if (!Array.isArray(v) && has(k)) hits.push(p);
        if (isContainer(child)) walk(child, p);
        else if (has(child) && hits[hits.length - 1] !== p) hits.push(p);
      }
    })(value, '');
    return hits.slice(0, limit);
  }

  // Container paths from the root down to the parent of `path`, root ('') first.
  function ancestors(path) {
    if (!path) return [];
    const parts = path.split('/').slice(1, -1);
    const out = [''];
    let cur = '';
    for (const p of parts) out.push((cur += '/' + p));
    return out;
  }

  function mount(el, bodyText) {
    // A previous mount's listener still holds that body's root and would fill
    // this body's lazy arrays from it.
    if (el._jvToggle) el.removeEventListener('toggle', el._jvToggle, true);
    el._jvToggle = null;
    const inert = { expandAll() {}, collapseAll() {}, find: () => ({ count: 0, more: false }) };
    if (!bodyText || !bodyText.trim()) {
      el.innerHTML = '<p class="jv-empty">empty body</p>';
      return inert;
    }
    let root;
    try { root = JSON.parse(bodyText); } catch {
      el.innerHTML = `<pre class="jv-text">${esc(bodyText)}</pre>`;
      return inert;
    }
    el.innerHTML = render(root);

    const fill = (d) => {
      if (!d.classList.contains('jv-lazy') || d.dataset.loaded) return;
      d.dataset.loaded = '1';
      d.insertAdjacentHTML('beforeend', renderChildren(root, d.dataset.path));
    };
    // toggle does not bubble, so listen in the capture phase on the container.
    el._jvToggle = (e) => {
      const d = e.target;
      if (d.open && d.classList && d.classList.contains('jv-lazy')) fill(d);
    };
    el.addEventListener('toggle', el._jvToggle, true);

    const nodes = () => Array.from(el.querySelectorAll('details'));

    function expandAll() {
      // Opening a lazy node adds more details, so loop until none are left closed.
      for (let pass = 0; pass < 50; pass++) {
        const closed = nodes().filter((d) => !d.open);
        if (!closed.length) break;
        for (const d of closed) {
          fill(d);
          d.open = true;
        }
      }
    }
    function collapseAll() { for (const d of nodes()) d.open = false; }

    function find(term) {
      el.querySelectorAll('.jv-hit').forEach((x) => x.classList.remove('jv-hit'));
      const found = search(root, term, FIND_CAP + 1);
      const paths = found.slice(0, FIND_CAP);
      // One index per search, grown as lazy nodes fill. Keyed by dataset value
      // rather than a selector because keys can hold quotes or newlines.
      const index = new Map();
      const indexIn = (scope) => scope.querySelectorAll('[data-path]').forEach((x) => index.set(x.dataset.path, x));
      indexIn(el);
      let first = null;
      let count = 0;
      for (const p of paths) {
        // Root first, then down, so lazy ancestors exist before we look for the match.
        for (const a of ancestors(p)) {
          const d = index.get(a);
          if (!d || d.tagName !== 'DETAILS') continue;
          if (d.classList.contains('jv-lazy') && !d.dataset.loaded) { fill(d); indexIn(d); }
          d.open = true;
        }
        const target = index.get(p);
        if (!target) continue;
        target.classList.add('jv-hit');
        count++;
        if (!first) first = target;
      }
      if (first) first.scrollIntoView({ block: 'center' });
      return { count, more: found.length > FIND_CAP };
    }

    return { expandAll, collapseAll, find };
  }

  globalThis.JsonView = { render, renderChildren, search, ancestors, mount, FIND_CAP, MIN_TERM };
})();
