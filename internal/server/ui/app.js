/* Token usage dashboard. Vanilla JS, talks to /api/*. */
(() => {
  'use strict';

  const $ = (sel, el = document) => el.querySelector(sel);
  const $$ = (sel, el = document) => Array.from(el.querySelectorAll(sel));
  const el = (tag, attrs = {}, ...children) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'class') n.className = v;
      else if (k === 'html') n.innerHTML = v;
      else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
      else if (v !== undefined && v !== null) n.setAttribute(k, v);
    }
    for (const c of children.flat()) {
      if (c === null || c === undefined) continue;
      n.append(c.nodeType ? c : document.createTextNode(String(c)));
    }
    return n;
  };

  // ---------- formatting ----------
  const fmt = (n) => {
    n = Number(n) || 0;
    const a = Math.abs(n);
    if (a >= 1e9) return (n / 1e9).toFixed(2) + 'B';
    if (a >= 1e6) return (n / 1e6).toFixed(a >= 1e8 ? 0 : 1) + 'M';
    if (a >= 1e3) return (n / 1e3).toFixed(a >= 1e5 ? 0 : 1) + 'K';
    return String(Math.round(n));
  };
  const fmtFull = (n) => (Number(n) || 0).toLocaleString();
  const pct = (n) => (Number(n) || 0).toFixed(n >= 10 ? 0 : 1) + '%';
  const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  const pad = (n) => String(n).padStart(2, '0');
  const fmtTime = (ms, withTime = true) => {
    if (!ms) return '';
    const d = new Date(ms);
    const day = `${MONTHS[d.getMonth()]} ${d.getDate()}`;
    return withTime ? `${day} ${pad(d.getHours())}:${pad(d.getMinutes())}` : day;
  };
  const fmtDur = (ms) => {
    if (ms <= 0) return 'now';
    const m = Math.round(ms / 60000);
    if (m < 60) return `${m}m`;
    const h = Math.floor(m / 60);
    if (h < 48) return `${h}h ${pad(m % 60)}m`;
    return `${Math.floor(h / 24)}d ${h % 24}h`;
  };
  const ago = (ms) => (ms ? fmtDur(Date.now() - ms) + ' ago' : '');

  // ---------- state ----------
  const state = {
    range: '5h', source: '', model: '',
    tsBy: 'source', tsView: 'chart',
    tab: 'workspace', drill: [],
  };
  const loadHash = () => {
    try {
      const h = decodeURIComponent(location.hash.slice(1));
      if (h) Object.assign(state, JSON.parse(h));
    } catch (e) { /* ignore */ }
  };
  const saveHash = () => {
    const { tsView, ...rest } = state;
    history.replaceState(null, '', '#' + encodeURIComponent(JSON.stringify(rest)));
  };

  let sources = [];       // [{name,label}]
  let limits = [];        // from /api/limits
  const sourceLabel = (n) => (sources.find((s) => s.name === n) || { label: n }).label || n;

  // ---------- colors: follow the entity, never its rank ----------
  const SLOTS = 7;
  const colorMaps = { source: new Map([['claude', 1], ['codex', 2]]) };
  const freeSlotsOfHiddenSeries = (m, visible) => { for (const k of [...m.keys()]) if (!visible.has(k)) m.delete(k); };
  const lowestFreeSlot = (used) => { let slot = 1; while (used.has(slot)) slot++; return slot; };
  const assignColors = (dim, series) => {
    const m = colorMaps[dim] || (colorMaps[dim] = new Map());
    const visible = new Set(series.filter((s) => s !== 'Other'));
    if (dim !== 'source') freeSlotsOfHiddenSeries(m, visible);
    const used = new Set(m.values());
    for (const s of visible) {
      if (m.has(s)) continue;
      const slot = lowestFreeSlot(used);
      m.set(s, slot);
      used.add(slot);
    }
  };
  const colorOf = (dim, key) => {
    if (key === 'Other') return 'var(--other)';
    const slot = (colorMaps[dim] || new Map()).get(key);
    return slot && slot <= 8 ? `var(--series-${slot})` : 'var(--other)';
  };

  // ---------- api ----------
  const api = async (path, params = {}) => {
    const u = new URL(path, location.href);
    for (const [k, v] of Object.entries(params)) if (v !== '' && v !== undefined && v !== null) u.searchParams.set(k, v);
    const r = await fetch(u);
    if (!r.ok) throw new Error(`${path}: ${r.status} ${await r.text()}`);
    return r.json();
  };
  const baseParams = () => ({ range: state.range, source: state.source, model: state.model });

  // ---------- tooltip ----------
  const tip = $('#tooltip');
  const showTip = (x, y, node) => {
    tip.innerHTML = '';
    tip.append(node);
    tip.classList.remove('hidden');
    const w = tip.offsetWidth, h = tip.offsetHeight;
    let left = x + 14, top = y + 14;
    if (left + w > innerWidth - 8) left = x - w - 14;
    if (top + h > innerHeight - 8) top = y - h - 14;
    tip.style.left = left + 'px';
    tip.style.top = top + 'px';
  };
  const hideTip = () => tip.classList.add('hidden');
  const totalsTable = (t) => el('table', {}, ...[
    ['Total', t.total], ['Input', t.input], ['Cache write', t.cache_create], ['Cache read', t.cache_read], ['Output', t.output], ['Requests', t.requests],
  ].map(([k, v]) => el('tr', {}, el('td', {}, k), el('td', { class: 'num' }, fmtFull(v)))));

  // ---------- status ----------
  const renderStatus = async () => {
    try {
      const s = await api('/api/status');
      sources = s.sources || [];
      const sel = $('#source');
      const cur = sel.value;
      sel.innerHTML = '';
      sel.append(el('option', { value: '' }, 'All sources'));
      for (const src of sources) sel.append(el('option', { value: src.name }, src.label));
      sel.value = state.source || cur || '';
      const st = s.stats;
      $('#status').textContent = `${fmtFull(st.events)} requests indexed across ${fmtFull(st.sessions)} sessions · last scan ${ago(Date.parse(s.scan_at))}${s.scanning ? ' · scanning…' : ''}`;
    } catch (e) {
      $('#status').textContent = 'status unavailable: ' + e.message;
    }
  };

  // ---------- limits ----------
  const level = (u) => (u >= 90 ? 'critical' : u >= 75 ? 'serious' : u >= 50 ? 'warning' : '');
  const levelText = { critical: '⚠ near limit', serious: '▲ high', warning: '● half used', '': '' };
  const renderLimits = async () => {
    try { limits = await api('/api/limits'); } catch (e) { limits = []; }
    const box = $('#limits');
    box.innerHTML = '';
    for (const l of limits) {
      const card = el('div', { class: 'limit' });
      card.append(el('div', { class: 'who' },
        el('b', {}, sourceLabel(l.source)),
        el('span', { class: 'muted small' }, [l.plan ? l.plan : '', l.fetched_at ? 'as of ' + ago(Date.parse(l.fetched_at)) : ''].filter(Boolean).join(' · ')),
      ));
      if (l.error) card.append(el('div', { class: 'err' }, l.error));
      for (const w of l.windows || []) {
        const lv = level(w.utilization);
        const resets = w.resets_at && !w.resets_at.startsWith('0001') ? Date.parse(w.resets_at) : 0;
        const win = el('div', { class: 'win' },
          el('div', { class: 'row' },
            el('span', { class: 'lbl' }, w.label + (w.scope ? ` · ${w.scope}` : ''), w.active ? el('span', { class: 'status-tag', title: 'The provider reports this as the binding limit right now' }, 'active') : null),
            el('span', {}, lv ? el('span', { class: 'status-tag ' + lv }, levelText[lv]) : null, ' ', el('span', { class: 'pct' }, pct(w.utilization))),
          ),
          el('div', { class: 'meter ' + lv }, el('span', { style: `width:${Math.min(100, Math.max(0, w.utilization))}%` })),
          el('div', { class: 'sub' },
            el('span', {}, resets ? (resets > Date.now() ? `resets in ${fmtDur(resets - Date.now())} (${fmtTime(resets)})` : `reset ${fmtTime(resets)}; nothing newer reported`) : 'reset time unknown'),
            w.local && w.window_minutes ? el('a', {
              href: '#', title: 'Use this window as the range below',
              onclick: (e) => { e.preventDefault(); setRange(`window:${l.source}:${w.key}`); },
            }, `${fmt(w.local.total)} tokens · ${fmtFull(w.local.requests)} req here`) : null,
          ),
        );
        card.append(win);
      }
      if (!(l.windows || []).length && !l.error) card.append(el('div', { class: 'muted small' }, 'no windows reported'));
      if ((l.shares || []).length) card.append(el('div', { class: 'muted small shares' }, 'weekly by product: ' + l.shares.filter((s) => s.percent > 0).map((s) => `${s.label} ${pct(s.percent)}`).join(' · ')));
      for (const n of l.notes || []) card.append(el('div', { class: 'muted small' }, n));
      box.append(card);
    }
    // range options that align to provider windows
    const sel = $('#range');
    const keep = sel.value || state.range;
    sel.innerHTML = '';
    const add = (v, label) => sel.append(el('option', { value: v }, label));
    for (const l of limits) for (const w of l.windows || []) if (w.window_minutes && !w.scope) add(`window:${l.source}:${w.key}`, `${sourceLabel(l.source)} ${w.label.toLowerCase()} window`);
    add('5h', 'Last 5 hours'); add('24h', 'Last 24 hours'); add('today', 'Today'); add('7d', 'Last 7 days'); add('30d', 'Last 30 days'); add('all', 'All time');
    sel.value = keep;
    if (sel.value !== keep) { sel.value = '5h'; state.range = '5h'; }
  };

  // ---------- summary tiles + model select ----------
  const renderSummary = async () => {
    const s = await api('/api/summary', baseParams());
    const t = s.totals;
    $('#range-desc').textContent = `${s.range.label}: ${fmtTime(s.range.from)} → ${fmtTime(s.range.to)}`;
    const tiles = $('#tiles');
    tiles.innerHTML = '';
    const tile = (k, v, sub) => el('div', { class: 'tile', title: fmtFull(v) + ' tokens' }, el('div', { class: 'v' }, fmt(v)), el('div', { class: 'k' }, k), sub ? el('div', { class: 's' }, sub) : null);
    const share = (v) => (t.total ? pct((v / t.total) * 100) + ' of total' : '');
    tiles.append(
      tile('Total tokens', t.total, `${fmtFull(t.requests)} requests`),
      tile('Input', t.input, share(t.input)),
      tile('Cache write', t.cache_create, share(t.cache_create)),
      tile('Cache read', t.cache_read, share(t.cache_read)),
      tile('Output', t.output, t.reasoning ? `${fmt(t.reasoning)} thinking` : share(t.output)),
    );
    for (const g of s.by_source) tiles.append(tile(sourceLabel(g.key), g.totals.total, `${fmtFull(g.totals.requests)} requests`));
    // model select: keep the current selection even if it has no rows in this range
    const sel = $('#model');
    const cur = state.model;
    const seen = new Set(s.by_model.map((g) => g.key));
    if (cur && !seen.has(cur)) seen.add(cur);
    sel.innerHTML = '';
    sel.append(el('option', { value: '' }, 'All models'));
    for (const m of [...seen].sort()) sel.append(el('option', { value: m }, m));
    sel.value = cur;
  };

  // ---------- time series ----------
  const bucketFor = (from, to) => {
    const span = to - from;
    if (span <= 6 * 3600e3) return '15m';
    if (span <= 48 * 3600e3) return 'hour';
    return 'day';
  };
  let tsData = null;
  const renderTimeseries = async () => {
    const probe = await api('/api/summary', baseParams()); // cheap way to learn the resolved range
    const bucket = bucketFor(probe.range.from, probe.range.to);
    const drillFilters = { ...current().filters };
    delete drillFilters.source; // the session drill pins a source; keep the global source filter authoritative
    const d = await api('/api/timeseries', { ...baseParams(), ...drillFilters, ...(current().filters.source ? { source: current().filters.source } : {}), by: state.tsBy, bucket });
    // top-N series by total, rest folded into Other
    const totals = new Map();
    for (const r of d.rows) totals.set(r.series, (totals.get(r.series) || 0) + r.totals.total);
    const ranked = [...totals.entries()].sort((a, b) => b[1] - a[1]).map((e) => e[0]);
    const keep = new Set(ranked.slice(0, SLOTS));
    const fold = ranked.length > SLOTS;
    const seriesName = (k) => (keep.has(k) ? k : 'Other');
    const buckets = new Map();
    for (const r of d.rows) {
      const b = buckets.get(r.ts) || (buckets.set(r.ts, new Map()), buckets.get(r.ts));
      const name = seriesName(r.series);
      const cur = b.get(name) || { total: 0, input: 0, cache_create: 0, cache_read: 0, output: 0, requests: 0 };
      for (const k of Object.keys(cur)) cur[k] += r.totals[k] || 0;
      b.set(name, cur);
    }
    const series = ranked.slice(0, SLOTS).concat(fold ? ['Other'] : []);
    // fill empty buckets so gaps show
    const step = d.bucket_ms;
    const start = Math.floor(d.range.from / step) * step, end = d.range.to;
    const xs = [];
    for (let t = start; t < end; t += step) xs.push(t);
    assignColors(state.tsBy, series);
    tsData = { series, buckets, xs, step, dim: state.tsBy, label: d.range.label };
    drawChart();
    drawTable();
    // legend
    const lg = $('#ts-legend');
    lg.innerHTML = '';
    if (state.drill.length) lg.append(el('span', { class: 'muted' }, `showing ${current().label} · `));
    if (series.length >= 2) {
      for (const s of series) lg.append(el('span', {}, el('span', { class: 'sw', style: `background:${colorOf(tsData.dim, s)}` }), labelSeries(s)));
    } else if (series.length === 1) {
      lg.append(el('span', {}, el('span', { class: 'sw', style: `background:${colorOf(tsData.dim, series[0])}` }), labelSeries(series[0])));
    }
  };
  const labelSeries = (s) => (tsData && tsData.dim === 'source' ? sourceLabel(s) : s || '(none)');

  const drawChart = () => {
    const box = $('#ts-chart');
    box.innerHTML = '';
    if (!tsData) return;
    const { series, buckets, xs, step, dim } = tsData;
    const W = Math.max(320, box.clientWidth || 800), H = 240, padL = 48, padR = 8, padT = 10, padB = 24;
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    svg.setAttribute('width', W); svg.setAttribute('height', H);
    const ns = (tag, attrs) => { const n = document.createElementNS('http://www.w3.org/2000/svg', tag); for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v); return n; };
    const maxY = Math.max(1, ...xs.map((t) => { const b = buckets.get(t); return b ? [...b.values()].reduce((a, v) => a + v.total, 0) : 0; }));
    if (!buckets.size) {
      svg.append(Object.assign(ns('text', { x: padL, y: H / 2, class: 'empty' }), { textContent: 'no usage in this range' }));
      box.append(svg);
      return;
    }
    const plotW = W - padL - padR, plotH = H - padT - padB;
    const x = (i) => padL + (i / xs.length) * plotW;
    const y = (v) => padT + plotH - (v / maxY) * plotH;
    const grid = ns('g', { class: 'grid' }), axis = ns('g', { class: 'axis' });
    for (let i = 0; i <= 3; i++) {
      const v = (maxY * i) / 3;
      grid.append(ns('line', { x1: padL, x2: W - padR, y1: y(v), y2: y(v) }));
      const t = ns('text', { x: padL - 6, y: y(v) + 4, 'text-anchor': 'end' }); t.textContent = fmt(v); axis.append(t);
    }
    svg.append(grid);
    const bw = Math.max(1, plotW / xs.length - 2);
    const every = Math.max(1, Math.ceil(xs.length / Math.floor(plotW / 90)));
    xs.forEach((t, i) => {
      if (i % every === 0) {
        const tx = ns('text', { x: x(i) + bw / 2, y: H - 6, 'text-anchor': 'middle' });
        tx.textContent = fmtTime(t, step < 86400e3); axis.append(tx);
      }
      const b = buckets.get(t);
      let acc = 0;
      if (b) for (const s of series) {
        const v = b.get(s); if (!v || !v.total) continue;
        const y1 = y(acc + v.total), y0 = y(acc);
        const r = ns('rect', { x: x(i) + 1, y: y1, width: bw, height: Math.max(0, y0 - y1), fill: colorOf(dim, s), class: 'bar', rx: acc === 0 && series.indexOf(s) === series.length - 1 ? 0 : 0 });
        svg.append(r);
        acc += v.total;
      }
      const hit = ns('rect', { x: x(i), y: padT, width: plotW / xs.length, height: plotH, class: 'hit' });
      hit.addEventListener('mousemove', (e) => {
        const rows = b ? series.filter((s) => b.get(s) && b.get(s).total).map((s) => [s, b.get(s)]) : [];
        const total = rows.reduce((a, [, v]) => a + v.total, 0);
        const node = el('div', {}, el('div', { class: 't' }, `${fmtTime(t, step < 86400e3)} · ${fmt(total)} tokens`),
          el('table', {}, ...rows.map(([s, v]) => el('tr', {}, el('td', {}, el('span', { class: 'sw', style: `background:${colorOf(dim, s)}` }), labelSeries(s)), el('td', { class: 'num' }, fmtFull(v.total)), el('td', { class: 'num muted' }, `${fmtFull(v.requests)} req`))),
            rows.length ? el('tr', {}, el('td', { class: 'muted' }, 'in / cache w / cache r / out'), el('td', { class: 'num muted', colspan: 2 }, rows.reduce((a, [, v]) => [a[0] + v.input, a[1] + v.cache_create, a[2] + v.cache_read, a[3] + v.output], [0, 0, 0, 0]).map(fmt).join(' / '))) : el('tr', {}, el('td', { class: 'muted' }, 'no usage'))));
        showTip(e.clientX, e.clientY, node);
      });
      hit.addEventListener('mouseleave', hideTip);
      svg.append(hit);
    });
    svg.append(axis);
    box.append(svg);
  };

  const drawTable = () => {
    const box = $('#ts-table');
    box.innerHTML = '';
    if (!tsData) return;
    const { series, buckets, xs, step } = tsData;
    const tbl = el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, 'Bucket'), ...series.map((s) => el('th', { class: 'num' }, labelSeries(s))), el('th', { class: 'num' }, 'Total'))));
    const body = el('tbody');
    for (const t of xs) {
      const b = buckets.get(t);
      if (!b) continue;
      const total = [...b.values()].reduce((a, v) => a + v.total, 0);
      body.append(el('tr', {}, el('td', {}, fmtTime(t, step < 86400e3)), ...series.map((s) => el('td', { class: 'num' }, b.get(s) ? fmtFull(b.get(s).total) : '')), el('td', { class: 'num' }, fmtFull(total))));
    }
    tbl.append(body);
    box.append(buckets.size ? tbl : el('div', { class: 'empty-note' }, 'no usage in this range'));
  };

  // ---------- breakdown / drill-down ----------
  const rootView = () => {
    switch (state.tab) {
      case 'directory': return { by: 'directory', filters: { dir: '' }, label: 'All directories' };
      case 'model': return { by: 'model', filters: {}, label: 'All models' };
      case 'session': return { by: 'session', filters: {}, label: 'All sessions' };
      default: return { by: 'workspace', filters: {}, label: 'All workspaces' };
    }
  };
  const views = () => [rootView(), ...state.drill];
  const current = () => views()[views().length - 1];
  const push = (v) => { state.drill.push(v); saveHash(); renderBreakdown(); renderTimeseries(); };
  const popTo = (i) => { state.drill = state.drill.slice(0, i); saveHash(); renderBreakdown(); renderTimeseries(); };

  const COLS = [
    ['Total', 'total'], ['Input', 'input'], ['Cache write', 'cache_create'], ['Cache read', 'cache_read'], ['Output', 'output'], ['Requests', 'requests'],
  ];
  const rowFor = (g, grand, onClick, subtitle) => {
    const t = g.totals;
    const share = grand ? (t.total / grand) * 100 : 0;
    const tr = el('tr', { class: onClick ? 'link' : '' },
      el('td', { class: 'name', title: g.key }, el('span', { class: 'share' }, el('span', { style: `width:${Math.min(100, share)}%` })), g.label || g.key || '(none)', subtitle ? el('span', { class: 'sub' }, subtitle) : null),
      el('td', { class: 'num muted' }, pct(share)),
      ...COLS.map(([, k]) => el('td', { class: 'num', title: fmtFull(t[k]) }, k === 'requests' ? fmtFull(t[k]) : fmt(t[k]))),
      el('td', { class: 'num muted' }, ago(t.last_ts)),
    );
    if (onClick) tr.addEventListener('click', onClick);
    return tr;
  };
  const table = (rows) => el('table', {}, el('thead', {}, el('tr', {}, el('th', {}, 'Name'), el('th', { class: 'num' }, 'Share'), ...COLS.map(([h]) => el('th', { class: 'num' }, h)), el('th', { class: 'num' }, 'Last active'))), el('tbody', {}, ...rows));

  const sessionSubtitle = (m) => {
    if (!m || !m.cwd) return '';
    const bits = [sourceLabel(m.source), m.workspace, m.git_branch ? '⎇ ' + m.git_branch : '', m.started_at ? fmtTime(m.started_at) : '', m.kind === 'subagent' ? 'sub-agent thread' : ''];
    return bits.filter(Boolean).join(' · ');
  };

  const renderCrumbs = () => {
    const nav = $('#crumbs');
    nav.innerHTML = '';
    views().forEach((v, i, all) => {
      if (i) nav.append(el('span', { class: 'sep' }, '›'));
      if (i < all.length - 1) nav.append(el('a', { onclick: () => popTo(i) }, v.label));
      else nav.append(el('span', {}, v.label));
    });
  };

  let breakdownSeq = 0;
  const renderBreakdown = async () => {
    const seq = ++breakdownSeq;
    renderCrumbs();
    const v = current();
    const box = $('#breakdown');
    const detail = $('#detail');
    detail.classList.add('hidden');
    box.innerHTML = '<div class="empty-note">loading…</div>';
    const params = { ...baseParams(), by: v.by, ...v.filters };
    if (v.filters.source) params.source = v.filters.source;
    if (v.by === 'session') params.limit = 300;
    let d;
    try { d = await api('/api/breakdown', params); } catch (e) { box.innerHTML = ''; box.append(el('div', { class: 'empty-note' }, e.message)); return; }
    if (seq !== breakdownSeq) return;
    const grand = d.totals.total;
    const rows = [];
    for (const g of d.groups) {
      let onClick = null, subtitle = '';
      switch (v.by) {
        case 'workspace':
          onClick = () => push({ by: 'session', filters: { ...v.filters, workspace: g.key }, label: g.key });
          break;
        case 'directory': {
          const leaf = g.meta && g.meta.leaf;
          subtitle = g.meta && g.meta.cwds > 1 ? `${g.meta.cwds} working directories` : '';
          if (g.key === '' || g.label === '(this directory)' || leaf) onClick = () => push({ by: 'session', filters: { ...v.filters, dir: g.key }, label: g.label === '(this directory)' ? 'sessions here' : g.label });
          else onClick = () => push({ by: 'directory', filters: { ...v.filters, dir: g.key }, label: g.label });
          break;
        }
        case 'model':
          onClick = () => push({ by: 'session', filters: { ...v.filters, model: g.key }, label: g.key });
          break;
        case 'session': {
          const m = g.meta || {};
          subtitle = sessionSubtitle(m);
          const src = m.source || state.source || v.filters.source;
          if (src) onClick = () => push({ by: 'agent', filters: { session: g.key, source: src }, label: g.label, session: g.key, source: src });
          break;
        }
        case 'agent':
          break;
      }
      rows.push(rowFor(g, grand, onClick, subtitle));
    }
    box.innerHTML = '';
    if (!rows.length) box.append(el('div', { class: 'empty-note' }, 'nothing in this range'));
    else box.append(table(rows));
    if (v.by === 'agent') renderSessionDetail(v, seq);
  };

  const renderSessionDetail = async (v, seq) => {
    const detail = $('#detail');
    let d;
    try { d = await api('/api/session', { source: v.source, id: v.session }); } catch (e) { return; }
    if (seq !== breakdownSeq) return;
    const s = d.session, t = d.totals;
    detail.innerHTML = '';
    detail.append(
      el('dl', {},
        el('dt', {}, 'Session'), el('dd', {}, s.title || '(untitled)', ' ', el('code', {}, s.id)),
        el('dt', {}, 'Tool'), el('dd', {}, sourceLabel(s.source), s.version ? ` ${s.version}` : ''),
        el('dt', {}, 'Directory'), el('dd', {}, el('code', {}, s.cwd || '?'), s.git_branch ? ` on ${s.git_branch}` : ''),
        el('dt', {}, 'Workspace'), el('dd', {}, s.workspace || '?'),
        el('dt', {}, 'Active'), el('dd', {}, `${fmtTime(s.started_at)} → ${fmtTime(s.last_at)} (${fmtDur(s.last_at - s.started_at)})`),
        el('dt', {}, 'All time'), el('dd', {}, `${fmt(t.total)} tokens in ${fmtFull(t.requests)} requests`, d.agents.length > 1 ? `, ${d.agents.length - 1} sub-agent${d.agents.length > 2 ? 's' : ''}` : ''),
        el('dt', {}, 'Models'), el('dd', {}, d.models.map((m) => `${m.key} ${fmt(m.totals.total)}`).join(' · ')),
      ),
      el('div', { class: 'muted small' }, 'The table below applies the range filter above; the "All time" line does not.'),
    );
    detail.classList.remove('hidden');
  };

  // ---------- wiring ----------
  const setRange = (r) => { state.range = r; $('#range').value = r; saveHash(); refreshData(); };
  let refreshing = false;
  const refreshData = async () => {
    if (refreshing) return;
    refreshing = true;
    try {
      await renderStatus();
      await renderLimits();
      await Promise.all([renderSummary(), renderTimeseries(), renderBreakdown()]);
    } catch (e) {
      $('#status').textContent = 'error: ' + e.message;
    } finally { refreshing = false; }
  };

  $('#range').addEventListener('change', (e) => setRange(e.target.value));
  $('#source').addEventListener('change', (e) => { state.source = e.target.value; state.drill = []; saveHash(); refreshData(); });
  $('#model').addEventListener('change', (e) => { state.model = e.target.value; state.drill = []; saveHash(); refreshData(); });
  $$('.seg [data-by]').forEach((b) => b.addEventListener('click', () => { $$('.seg [data-by]').forEach((x) => x.classList.toggle('on', x === b)); state.tsBy = b.dataset.by; saveHash(); renderTimeseries(); }));
  $$('.seg [data-view]').forEach((b) => b.addEventListener('click', () => {
    $$('.seg [data-view]').forEach((x) => x.classList.toggle('on', x === b));
    state.tsView = b.dataset.view;
    $('#ts-chart').classList.toggle('hidden', state.tsView !== 'chart');
    $('#ts-table').classList.toggle('hidden', state.tsView !== 'table');
  }));
  $$('.seg [data-tab]').forEach((b) => b.addEventListener('click', () => { $$('.seg [data-tab]').forEach((x) => x.classList.toggle('on', x === b)); state.tab = b.dataset.tab; state.drill = []; saveHash(); renderBreakdown(); renderTimeseries(); }));
  $('#refresh').addEventListener('click', async () => { await fetch('/api/refresh', { method: 'POST' }); setTimeout(refreshData, 1500); });
  addEventListener('resize', () => drawChart());
  addEventListener('hashchange', () => { loadHash(); syncControls(); refreshData(); });

  const syncControls = () => {
    $$('.seg [data-by]').forEach((x) => x.classList.toggle('on', x.dataset.by === state.tsBy));
    $$('.seg [data-tab]').forEach((x) => x.classList.toggle('on', x.dataset.tab === state.tab));
    $('#source').value = state.source;
    $('#model').value = state.model;
  };

  loadHash();
  syncControls();
  refreshData();
  setInterval(refreshData, 30000);
})();
