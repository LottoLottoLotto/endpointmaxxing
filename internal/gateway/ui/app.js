// substrate admin UI: a static client of the management API (/substrate/v1). It adds no capabilities of
// its own; every action here is one API call an agent can make with the same key.
// Untrusted text (prompts' metadata, errors, model names) is only ever set through textContent.
'use strict';

const KEY = 'substrate-key';
const S = { key: sessionStorage.getItem(KEY) || '', who: null, timer: null, gen: 0, since: '24h' };

// ---------- DOM helpers ----------

function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (k === 'class') e.className = v;
    else e.setAttribute(k, v === true ? '' : v);
  }
  for (const k of kids.flat()) {
    if (k == null || k === false) continue;
    e.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
  return e;
}

function toast(msg, isErr) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast' + (isErr ? ' err' : '');
  t.hidden = false;
  clearTimeout(toast.t);
  toast.t = setTimeout(() => { t.hidden = true; }, isErr ? 9000 : 4000);
}

// ---------- formatting ----------

const nf = new Intl.NumberFormat();
const num = (v) => (v == null ? '–' : nf.format(Math.round(v)));
const dec = (v, d = 1) => (v == null ? '–' : Number(v).toFixed(d));
function ms(v) {
  if (v == null) return '–';
  if (v < 1000) return Math.round(v) + ' ms';
  if (v < 60000) return (v / 1000).toFixed(1) + ' s';
  if (v < 3600000) return (v / 60000).toFixed(1) + ' min';
  return (v / 3600000).toFixed(1) + ' h';
}
function when(v) {
  if (v == null || v === '' || v === 0) return '–';
  const d = typeof v === 'number' ? new Date(v) : new Date(v);
  if (isNaN(d) || d.getFullYear() < 2000) return '–';
  return d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
}
function ago(v) {
  if (!v) return '–';
  const t = typeof v === 'number' ? v : Date.parse(v);
  if (isNaN(t) || t <= 0) return '–';
  const s = (Date.now() - t) / 1000;
  if (s < 0) return 'in ' + ms(-s * 1000);
  if (s < 60) return Math.round(s) + 's ago';
  if (s < 3600) return Math.round(s / 60) + 'm ago';
  if (s < 86400) return Math.round(s / 3600) + 'h ago';
  return Math.round(s / 86400) + 'd ago';
}
let currency = 'USD';
function money(v) {
  if (v == null) return '–';
  try { return new Intl.NumberFormat(undefined, { style: 'currency', currency, maximumFractionDigits: 4 }).format(v); }
  catch { return dec(v, 4) + ' ' + currency; }
}
const blank = (v) => (v === '' || v == null ? el('span', { class: 'muted' }, '(none)') : v);

function pill(text, kind) { return el('span', { class: 'pill ' + (kind || '') }, text); }
function statusPill(s) {
  const kind = { ready: 'ok', starting: 'warn', loading: 'warn', exited: 'err', unhealthy: 'err', failed: 'err' }[s];
  return pill(s || '–', kind);
}
function sevPill(s) {
  if (!s) return '';
  return pill(s, { critical: 'err', high: 'err', medium: 'warn', low: '' }[s]);
}
function httpPill(code) {
  if (code == null) return '–';
  return pill(String(code), code < 400 ? 'ok' : code === 429 || code < 500 ? 'warn' : 'err');
}

// ---------- API ----------

class APIError extends Error {
  constructor(status, msg) { super(msg); this.status = status; }
}

async function api(method, path, body) {
  const headers = {};
  if (S.key) headers.Authorization = 'Bearer ' + S.key;
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const r = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  let data = null;
  const text = await r.text();
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!r.ok) {
    const msg = (data && data.error && data.error.message) || (typeof data === 'string' ? data : r.statusText);
    if (r.status === 401) showLogin(msg);
    throw new APIError(r.status, msg);
  }
  return data;
}
const get = (path) => api('GET', path);

function qs(obj) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(obj)) if (v !== '' && v != null && v !== false) p.set(k, v === true ? 'true' : v);
  const s = p.toString();
  return s ? '?' + s : '';
}

const isAdmin = () => S.who && S.who.role === 'admin';

// ---------- auth ----------

function showLogin(msg) {
  const d = document.getElementById('login');
  document.getElementById('login-error').textContent = S.key && msg ? msg : '';
  if (!d.open) d.showModal();
  document.getElementById('login-key').focus();
}

async function signIn() {
  try {
    S.who = await get('/substrate/v1/whoami');
  } catch (e) {
    if (e.status !== 401) document.getElementById('main').replaceChildren(el('p', { class: 'error' }, e.message));
    return false;
  }
  const who = document.getElementById('who');
  if (S.who.open) who.textContent = 'open gateway (loopback, no keys)';
  else who.textContent = `${S.who.name} · ${S.who.role}` + (S.who.team ? ` · team ${S.who.team}` : '');
  document.getElementById('signout').hidden = S.who.open;
  if (S.who.role !== 'admin' && S.who.role !== 'auditor') {
    document.getElementById('main').replaceChildren(el('p', { class: 'error' },
      `Key ${S.who.name} has role ${S.who.role}. The admin UI needs an admin or auditor key.`));
    return false;
  }
  return true;
}

document.getElementById('login-form').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  S.key = document.getElementById('login-key').value.trim();
  sessionStorage.setItem(KEY, S.key);
  const ok = await signIn();
  if (ok || S.who) document.getElementById('login').close(); // a valid key with a lower role: the reason is on the page
  if (ok) {
    document.getElementById('login-key').value = '';
    route();
  }
});

document.getElementById('signout').addEventListener('click', () => {
  sessionStorage.removeItem(KEY);
  S.key = '';
  S.who = null;
  document.getElementById('who').textContent = '';
  document.getElementById('main').replaceChildren();
  showLogin();
});

// ---------- shared widgets ----------

// table renders rows with columns [{h, v(row), n?, cls?}]; detail(row) adds an expandable row.
function table(cols, rows, opts = {}) {
  const head = el('tr', {}, cols.map((c) => el('th', { class: c.n ? 'n' : '' }, c.h)));
  const body = el('tbody');
  if (!rows || rows.length === 0) {
    body.append(el('tr', {}, el('td', { class: 'empty', colspan: cols.length }, opts.empty || 'Nothing here.')));
  }
  for (const r of rows || []) {
    const tr = el('tr', { class: opts.detail || opts.onrow ? 'click' : '' },
      cols.map((c) => el('td', { class: (c.n ? 'n ' : '') + (c.cls || '') }, c.v(r))));
    if (opts.onrow) tr.addEventListener('click', (ev) => { if (ev.target.tagName !== 'BUTTON') opts.onrow(r); });
    else if (opts.detail) {
      let open = null;
      tr.addEventListener('click', (ev) => {
        if (ev.target.tagName === 'BUTTON') return;
        if (open) { open.remove(); open = null; return; }
        open = el('tr', { class: 'detail' }, el('td', { colspan: cols.length }, opts.detail(r)));
        tr.after(open);
      });
    }
    body.append(tr);
  }
  return el('table', {}, el('thead', {}, head), body);
}

const jsonBlock = (v) => el('pre', { class: 'json' }, JSON.stringify(v, null, 2));

function sinceSelect(onchange) {
  const s = el('select', { onchange: (e) => { S.since = e.target.value; onchange(); } },
    ['1h', '6h', '24h', '7d', '30d', '90d'].map((v) => el('option', { value: v, selected: v === S.since }, 'last ' + v)));
  return s;
}

function refreshToggle(fn) {
  const box = el('input', { type: 'checkbox', checked: S.autoRefresh !== false });
  const arm = () => {
    clearInterval(S.timer);
    S.timer = box.checked ? setInterval(fn, 10000) : null;
  };
  box.addEventListener('change', () => { S.autoRefresh = box.checked; arm(); });
  arm();
  return el('label', { class: 'muted' }, box, ' auto-refresh');
}

// page builds a page frame; load(gen) fills it and must check gen before writing.
function page(title, controls) {
  const body = el('div');
  const main = document.getElementById('main');
  main.replaceChildren(el('div', { class: 'bar' }, el('h1', {}, title), el('span', { class: 'spacer' }), controls || []), body);
  return body;
}

async function guarded(gen, target, fn) {
  try {
    const out = await fn();
    if (gen === S.gen) target.replaceChildren(...[].concat(out));
  } catch (e) {
    if (gen === S.gen && e.status !== 401) target.replaceChildren(el('p', { class: 'error' }, e.message));
  }
}

const usageCols = () => [
  { h: 'Requests', n: 1, v: (r) => num(r.requests) },
  { h: 'Errors', n: 1, v: (r) => (r.errors ? el('span', { class: 'error' }, num(r.errors)) : '0') },
  { h: 'Flagged', n: 1, v: (r) => num(r.flagged) },
  { h: 'Blocked', n: 1, v: (r) => num(r.blocked) },
  { h: 'Prompt tok', n: 1, v: (r) => num(r.prompt_tokens) },
  { h: 'Output tok', n: 1, v: (r) => num(r.completion_tokens) },
  { h: 'Avg TTFT', n: 1, v: (r) => ms(r.avg_ttft_ms) },
  { h: 'Avg time', n: 1, v: (r) => ms(r.avg_duration_ms) },
  { h: 'Decode tok/s', n: 1, v: (r) => dec(r.decode_tok_per_sec) },
  { h: 'Cost', n: 1, v: (r) => money(r.cost) },
];

function usageTable(label, rows, filterKey) {
  const cols = [{ h: label, v: (r) => blank(r.key) }, ...usageCols()];
  return table(cols, rows, {
    empty: 'No requests in this period.',
    onrow: filterKey ? (r) => go('requests', { [filterKey]: r.key, since: S.since }) : null,
  });
}

function go(name, params) {
  location.hash = name + (params ? qs(params) : '');
}

// ---------- pages ----------

const ALERT_KINDS = new Set(['budget_alert', 'config_reload_failed', 'key_expiring', 'start_failed', 'unhealthy', 'restart', 'exited']);

async function overview() {
  const body = page('Overview', [sinceSelect(() => load()), refreshToggle(() => load())]);
  const load = () => {
    const gen = S.gen;
    guarded(gen, body, async () => {
      const [rep, ins, evs] = await Promise.all([
        get('/substrate/v1/report' + qs({ since: S.since })),
        get('/substrate/v1/instances'),
        get('/substrate/v1/events' + qs({ since: S.since, limit: 500 })),
      ]);
      currency = rep.currency || 'USD';
      const t = rep.totals || {};
      const running = ins.filter((i) => i.status === 'ready').length;
      const card = (k, v, cls) => el('div', { class: 'card' }, el('div', { class: 'v ' + (cls || '') }, v), el('div', { class: 'k' }, k));
      const errRate = t.requests ? (100 * t.errors / t.requests).toFixed(1) + '%' : '–';
      const alerts = evs.filter((e) => ALERT_KINDS.has(e.kind)).slice(0, 15);
      return [
        el('div', { class: 'cards' },
          card('Requests', num(t.requests)),
          card('Errors (' + errRate + ')', num(t.errors), t.errors ? 'error' : ''),
          card('Flagged / blocked', `${num(t.flagged)} / ${num(t.blocked)}`),
          card('Prompt tokens', num(t.prompt_tokens)),
          card('Output tokens', num(t.completion_tokens)),
          card('Cached tokens', num(t.cached_tokens)),
          card('Avg TTFT', ms(t.avg_ttft_ms)),
          card('Decode tok/s', dec(t.decode_tok_per_sec)),
          card('Cost', money(t.cost)),
          card('Models used / clients', `${num(t.models)} / ${num(t.clients)}`),
          card('Instances ready', `${running} / ${ins.length}`)),
        el('h2', {}, 'Alerts and failures'),
        table([
          { h: 'Time', v: (e) => when(e.ts) },
          { h: 'Kind', v: (e) => pill(e.kind, ['start_failed', 'unhealthy', 'config_reload_failed'].includes(e.kind) ? 'err' : 'warn') },
          { h: 'Model / instance', v: (e) => e.instance || e.model || '' },
          { h: 'Detail', cls: 'wrap', v: (e) => (e.detail || '').split('\n').slice(-2).join('\n') },
        ], alerts, { empty: 'No alerts in this period.', detail: jsonBlock }),
        el('h2', {}, 'By model'), usageTable('Model', rep.by_model, 'model'),
        el('div', { class: 'grid2' },
          el('div', {}, el('h2', {}, 'By client'), usageTable('Client', rep.by_client, 'client')),
          el('div', {}, el('h2', {}, 'By team'), usageTable('Team', rep.by_team, 'team'))),
        el('h2', {}, 'By category'), usageTable('Category', rep.by_category, 'category'),
        el('div', { class: 'grid2' },
          el('div', {}, el('h2', {}, 'Policy flags'), flagsTable(rep.flags)),
          el('div', {}, el('h2', {}, 'Errors'), errorsTable(rep.errors))),
      ];
    });
  };
  load();
}

function flagsTable(rows) {
  return table([
    { h: 'Rule', v: (r) => r.rule },
    { h: 'Detector', v: (r) => r.detector },
    { h: 'Severity', v: (r) => sevPill(r.severity) },
    { h: 'Action', v: (r) => r.action },
    { h: 'Requests', n: 1, v: (r) => num(r.requests) },
    { h: 'Last', n: 1, v: (r) => ago(r.last_ts) },
  ], rows, { empty: 'No policy matches.', onrow: (r) => go('requests', { flagged: true, rule: r.rule, since: S.since }) });
}

function errorsTable(rows) {
  return table([
    { h: 'Kind', v: (r) => r.kind },
    { h: 'Count', n: 1, v: (r) => num(r.count) },
    { h: 'Last error', cls: 'wrap', v: (r) => r.last_error || '' },
    { h: 'Last', n: 1, v: (r) => ago(r.last_ts) },
  ], rows, { empty: 'No errors.', onrow: (r) => go('requests', { errors: true, error_kind: r.kind, since: S.since }) });
}

async function models() {
  const body = page('Models', [refreshToggle(() => load())]);
  const act = async (btn, label, fn) => {
    btn.disabled = true;
    const was = btn.textContent;
    btn.textContent = label + '…';
    try { await fn(); } catch (e) { toast(e.message, true); }
    btn.textContent = was;
    btn.disabled = false;
    load();
  };
  const ensure = (btn, model) => act(btn, 'Starting', async () => {
    toast(`Starting ${model}. Loading can take minutes.`);
    const r = await api('POST', '/substrate/v1/ensure', { model });
    toast(`${model} is ready on ${r.instance.id}`);
  });
  const stop = (btn, id) => {
    if (!confirm(`Stop ${id}? In-flight requests finish first.`)) return;
    act(btn, 'Stopping', async () => {
      await api('POST', '/substrate/v1/stop', { id });
      toast(`Stopped ${id}`);
    });
  };
  const adminBtn = (label, cls, onclick) => el('button', {
    class: cls, disabled: !isAdmin(), title: isAdmin() ? null : 'needs an admin key', onclick,
  }, label);

  const load = () => {
    const gen = S.gen;
    guarded(gen, body, async () => {
      const [ms_, ins] = await Promise.all([get('/v1/models'), get('/substrate/v1/instances')]);
      const alive = (s) => s === 'ready' || s === 'starting' || s === 'loading' || s === 'unhealthy';
      return [
        el('h2', {}, 'Registered models'),
        table([
          { h: 'Model', v: (m) => m.id },
          { h: 'Engine', v: (m) => m.substrate.engine || (m.substrate.alias_for ? '' : el('span', { class: 'muted' }, 'auto')) },
          { h: 'Format', v: (m) => m.substrate.format || '' },
          { h: 'State', v: (m) => {
            if (m.substrate.alias_for) return el('span', { class: 'muted' }, 'alias for ' + m.substrate.alias_for);
            const is = m.substrate.instances || [];
            return is.length ? is.map((i) => [statusPill(i.status), ' ']) : el('span', { class: 'muted' }, 'not running');
          } },
          { h: '', v: (m) => {
            if (m.substrate.alias_for || !m.substrate.registered) return '';
            const is = m.substrate.instances || [];
            if (is.some((i) => alive(i.status))) return is.filter((i) => alive(i.status)).map((i) => adminBtn('Stop', 'secondary', (e) => stop(e.target, i.id)));
            return adminBtn('Start', '', (e) => ensure(e.target, m.id));
          } },
        ], ms_.data, { empty: 'No models registered (substrate model add).', onrow: (m) => go('requests', { model: m.id, since: S.since }) }),
        el('h2', {}, 'Instances'),
        table([
          { h: 'Instance', v: (i) => i.instance.id },
          { h: 'Engine', v: (i) => i.instance.engine },
          { h: 'Status', v: (i) => statusPill(i.status) },
          { h: 'Owner', v: (i) => i.instance.owner || '' },
          { h: 'Devices', v: (i) => [i.instance.accelerator || (i.instance.gpus ? 'gpu' : ''), ' ', (i.instance.devices || i.instance.gpus || []).join(',')] },
          { h: 'Active', n: 1, v: (i) => num(i.active_requests) },
          { h: 'Started', n: 1, v: (i) => ago(i.instance.started_at) },
          { h: 'Last used', n: 1, v: (i) => ago(i.last_used) },
          { h: 'Endpoint', v: (i) => el('code', {}, i.api_url) },
          { h: '', v: (i) => (alive(i.status) ? adminBtn('Stop', 'secondary', (e) => stop(e.target, i.instance.id)) : '') },
        ], ins, { empty: 'No instances.', detail: jsonBlock }),
      ];
    });
  };
  load();
}

// filterPage renders a filter form bound to the hash's query and a results area.
function filterForm(fields, values, onsubmit) {
  const form = el('form', { class: 'filters' });
  for (const f of fields) {
    if (f.type === 'check') {
      form.append(el('label', { class: 'check' }, el('input', { type: 'checkbox', name: f.name, checked: values[f.name] === 'true' }), f.label));
    } else if (f.options) {
      form.append(el('label', {}, f.label, el('select', { name: f.name },
        f.options.map((o) => el('option', { value: o, selected: (values[f.name] || f.def || '') === o }, o || 'any')))));
    } else {
      form.append(el('label', {}, f.label, el('input', { name: f.name, value: values[f.name] || f.def || '', placeholder: f.ph || '' })));
    }
  }
  form.append(el('button', { type: 'submit' }, 'Apply'),
    el('button', { type: 'button', class: 'secondary', onclick: () => { form.reset(); for (const i of form.querySelectorAll('input:not([type=checkbox])')) i.value = ''; form.requestSubmit(); } }, 'Clear'));
  form.addEventListener('submit', (ev) => {
    ev.preventDefault();
    const out = {};
    for (const f of fields) {
      const i = form.elements[f.name];
      const v = f.type === 'check' ? (i.checked ? 'true' : '') : i.value.trim();
      if (v) out[f.name] = v;
    }
    onsubmit(out);
  });
  return form;
}

function hashParams() {
  const q = location.hash.split('?')[1] || '';
  return Object.fromEntries(new URLSearchParams(q));
}

async function requests() {
  const body = page('Requests');
  const p = hashParams();
  const fields = [
    { name: 'since', label: 'Since', def: '24h', ph: '24h, 7d, 2026-10-01' },
    { name: 'model', label: 'Model' },
    { name: 'client', label: 'Client' },
    { name: 'team', label: 'Team' },
    { name: 'category', label: 'Category' },
    { name: 'error_kind', label: 'Error kind' },
    { name: 'rule', label: 'Policy rule' },
    { name: 'min_severity', label: 'Min severity', options: ['', 'low', 'medium', 'high', 'critical'] },
    { name: 'limit', label: 'Limit', def: '100' },
    { name: 'errors', label: 'errors only', type: 'check' },
    { name: 'flagged', label: 'flagged only', type: 'check' },
  ];
  const results = el('div');
  body.append(filterForm(fields, p, (v) => go('requests', v)), results);
  const gen = S.gen;
  guarded(gen, results, async () => {
    const rows = await get('/substrate/v1/requests' + qs({ since: '24h', limit: 100, ...p }));
    return [
      el('p', { class: 'muted' }, `${rows.length} request${rows.length === 1 ? '' : 's'}, newest first. Click a row for the full record.`),
      table([
        { h: 'Time', v: (r) => when(r.ts) },
        { h: 'Model', v: (r) => r.model },
        { h: 'Client', v: (r) => blank(r.client) },
        { h: 'Status', v: (r) => httpPill(r.status) },
        { h: 'Error', v: (r) => r.error_kind || '' },
        { h: 'Prompt', n: 1, v: (r) => num(r.prompt_tokens) },
        { h: 'Output', n: 1, v: (r) => num(r.completion_tokens) },
        { h: 'TTFT', n: 1, v: (r) => ms(r.ttft_ms) },
        { h: 'Time', n: 1, v: (r) => ms(r.duration_ms) },
        { h: 'Category', v: (r) => r.category || '' },
        { h: 'Flags', v: (r) => (r.flags ? [sevPill(r.flag_severity), ' ', r.flags.map((f) => f.detector).join(', ')] : '') },
        { h: 'Cost', n: 1, v: (r) => money(r.cost) },
      ], rows, { empty: 'No requests match.', detail: jsonBlock }),
    ];
  });
}

async function events() {
  const body = page('Events');
  const p = hashParams();
  const fields = [
    { name: 'since', label: 'Since', def: '7d' },
    { name: 'model', label: 'Model' },
    { name: 'instance', label: 'Instance' },
    { name: 'kind', label: 'Kind', options: ['', 'admin', 'start', 'ready', 'stop', 'exited', 'restart', 'unhealthy', 'start_failed',
      'load', 'unload', 'budget_alert', 'key_expiring', 'config_reloaded', 'config_reload_failed'] },
    { name: 'limit', label: 'Limit', def: '200' },
  ];
  const results = el('div');
  body.append(filterForm(fields, p, (v) => go('events', v)), results);
  const gen = S.gen;
  guarded(gen, results, async () => {
    // The API has no kind filter: with a kind, read further back so the newest matches still fill the page.
    const { kind, ...q } = p;
    const limit = Number(q.limit || 200);
    let rows = await get('/substrate/v1/events' + qs({ since: '7d', ...q, limit: kind ? Math.max(limit * 10, 2000) : limit }));
    if (kind) rows = rows.filter((e) => e.kind === kind).slice(0, limit);
    return table([
      { h: 'Time', v: (e) => when(e.ts) },
      { h: 'Kind', v: (e) => e.kind },
      { h: 'Instance / model', v: (e) => e.instance || e.model || '' },
      { h: 'Owner / actor', v: (e) => e.owner || '' },
      { h: 'Took', n: 1, v: (e) => ms(e.duration_ms) },
      { h: 'Detail', cls: 'wrap', v: (e) => { const d = e.detail || ''; return d.length > 220 ? d.slice(0, 220) + '…' : d; } },
    ], rows, { empty: 'No events match.', detail: jsonBlock });
  });
}

async function policy() {
  const body = page('Policy', [sinceSelect(() => load())]);
  const stats = el('div');
  const result = el('div');
  const text = el('textarea', { placeholder: 'A prompt to check against the gateway\'s policy and classifier. Nothing is sent to a model.' });
  const model = el('input', { placeholder: 'model (optional)' });
  const client = el('input', { placeholder: 'client (optional)' });
  const run = async (ev) => {
    ev.preventDefault();
    if (!text.value.trim()) return;
    try {
      const r = await api('POST', '/substrate/v1/policy/test', { text: text.value, model: model.value.trim(), client: client.value.trim() });
      result.replaceChildren(
        el('p', {}, r.blocked ? pill('would be blocked', 'err') : pill('allowed', 'ok'), ' category ', el('b', {}, r.category || '–'),
          r.flag_severity ? [' · severity ', sevPill(r.flag_severity)] : '',
          !r.policy_enabled ? el('span', { class: 'muted' }, ' · no policy configured') : '',
          !r.classify_enabled ? el('span', { class: 'muted' }, ' · classifier off') : ''),
        table([
          { h: 'Rule', v: (f) => f.rule },
          { h: 'Detector', v: (f) => f.detector },
          { h: 'Severity', v: (f) => sevPill(f.severity) },
          { h: 'Action', v: (f) => f.action },
          { h: 'Matches', n: 1, v: (f) => num(f.count) },
          { h: 'Sample', v: (f) => el('code', {}, f.sample || '') },
        ], r.flags || [], { empty: 'No rule matched.' }));
    } catch (e) {
      result.replaceChildren(el('p', { class: 'error' }, e.message));
    }
  };
  body.append(stats,
    el('h2', {}, 'Test a prompt'),
    el('form', { onsubmit: run }, text, el('div', { class: 'bar' }, model, client, el('button', { type: 'submit' }, 'Check'))),
    result);
  const load = () => {
    const gen = S.gen;
    guarded(gen, stats, async () => {
      const [flags, errors] = await Promise.all([
        get('/substrate/v1/flags' + qs({ since: S.since })),
        get('/substrate/v1/errors' + qs({ since: S.since })),
      ]);
      return el('div', { class: 'grid2' },
        el('div', {}, el('h2', {}, 'Policy matches'), flagsTable(flags)),
        el('div', {}, el('h2', {}, 'Errors by kind'), errorsTable(errors)));
    });
  };
  load();
}

async function access() {
  const body = page('Keys & teams');
  const gen = S.gen;
  guarded(gen, body, async () => {
    const [ks, teams] = await Promise.all([get('/substrate/v1/keys'), get('/substrate/v1/teams')]);
    const limit = (v) => (v ? num(v) : el('span', { class: 'muted' }, '∞'));
    return [
      el('p', { class: 'muted' }, 'Keys and teams are managed with the CLI (substrate key …, substrate team …) or its MCP tools; changes appear here and in the admin trail.'),
      el('h2', {}, 'API keys'),
      table([
        { h: 'Name', v: (k) => k.name },
        { h: 'Prefix', v: (k) => el('code', {}, k.prefix + '…') },
        { h: 'Role', v: (k) => k.role },
        { h: 'Team', v: (k) => k.team || '' },
        { h: 'Models', v: (k) => (k.models || []).join(', ') || el('span', { class: 'muted' }, 'all') },
        { h: 'RPM', n: 1, v: (k) => limit(k.rpm) },
        { h: 'Tokens/day', n: 1, v: (k) => limit(k.tokens_per_day) },
        { h: 'Expires', n: 1, v: (k) => (k.expires ? when(k.expires) : el('span', { class: 'muted' }, 'never')) },
        { h: 'Status', v: (k) => pill(k.status, k.status === 'active' ? 'ok' : 'err') },
        { h: 'Last used', n: 1, v: (k) => ago(k.last_used) },
      ], ks, { empty: 'No keys in keys.yaml (substrate key add).', detail: jsonBlock }),
      el('h2', {}, 'Teams'),
      table([
        { h: 'Team', v: (t) => t.name },
        { h: 'Keys', n: 1, v: (t) => num(t.keys) },
        { h: 'Models', v: (t) => (t.config.models || []).join(', ') || el('span', { class: 'muted' }, 'all') },
        { h: 'Limits', cls: 'wrap', v: (t) => (t.limits || []).map((l) => el('div', {},
          `${l.limit}: ${num(l.used)} of ${num(l.max)} this ${l.period} `, pill((100 * l.ratio).toFixed(0) + '%', l.ratio >= 1 ? 'err' : l.ratio >= (t.config.alert_at || 0.8) ? 'warn' : 'ok'))) },
      ], teams, { empty: 'No teams configured.' }),
    ];
  });
}

async function config() {
  const body = page('Config');
  const show = (st) => [
    el('h2', {}, 'Configuration reload'),
    el('p', { class: 'muted' }, 'The gateway re-reads config.yaml and models.yaml when they change. An invalid file is refused and the running configuration stays.'),
    table([{ h: 'Field', v: (r) => r[0] }, { h: 'Value', cls: 'wrap', v: (r) => (r[0].endsWith('_at') ? when(r[1]) : String(r[1])) }],
      Object.entries(st)),
    el('p', {}, el('button', { disabled: !isAdmin(), title: isAdmin() ? null : 'needs an admin key', onclick: async () => {
      try {
        const r = await api('POST', '/substrate/v1/reload');
        toast('Configuration reloaded');
        body.replaceChildren(...show(r), ...who());
      } catch (e) { toast(e.message, true); }
    } }, 'Reload now')),
  ];
  const who = () => [el('h2', {}, 'Signed in as'), jsonBlock(S.who)];
  const gen = S.gen;
  guarded(gen, body, async () => [...show(await get('/substrate/v1/reload')), ...who()]);
}

const PAGES = { overview, models, requests, events, policy, access, config };

function route() {
  S.gen++;
  clearInterval(S.timer);
  S.timer = null;
  const name = (location.hash.slice(1).split('?')[0]) || 'overview';
  for (const a of document.querySelectorAll('#nav a')) a.classList.toggle('active', a.getAttribute('href') === '#' + name);
  if (!S.who || (S.who.role !== 'admin' && S.who.role !== 'auditor')) return;
  (PAGES[name] || overview)();
}

window.addEventListener('hashchange', route);

(async () => {
  if (await signIn()) route();
})();
