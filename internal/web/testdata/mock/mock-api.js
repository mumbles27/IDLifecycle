// mock-api.js - in-memory stand-in for the IDLifecycle HTTP API (docs/ARCHITECTURE.md section 7).
// Importing this module replaces window.fetch for URLs under /api/. It exists only so the UI can be
// exercised (headless Edge) before/without the Go backend. Not part of the product.

const now = () => new Date().toISOString();
const ago = (mins) => new Date(Date.now() - mins * 60000).toISOString();
let seq = 1000;
const newId = (p) => `${p}-${(seq += 1)}`;

export const db = {
  users: [
    { id: 'u1', username: 'admin', role: 'admin', disabled: false, created_at: ago(9000), last_login_at: ago(5), password: 'admin-pass' },
    { id: 'u2', username: 'approver1', role: 'approver', disabled: false, created_at: ago(8000), last_login_at: ago(500), password: 'approver-pass' },
  ],
  session: null,
  sources: [
    {
      id: 's1', name: 'HR export', type: 'csv', enabled: true,
      options: { external_id_field: 'EmployeeId', status_field: 'Status', terminated_values: ['T'], missing_means_terminated: true, missing_grace_runs: 3 },
      config: { path: 'C:\\feeds\\people.csv', delimiter: ',', api_key: '********' },
      last_sync_at: ago(60), last_status: 'ok', last_error: '',
    },
  ],
  mappings: [
    { id: 'm1', name: 'Display name', target_attr: 'displayName', template: '{{ .FirstName }} {{ .LastName }}', mode: 'sync', on_empty: 'skip', value_type: 'string', enabled: true, sort_order: 10, description: 'Full name' },
    { id: 'm2', name: 'Manager', target_attr: 'manager', template: '{{ .ManagerId }}', mode: 'immutable', on_empty: 'clear', value_type: 'user_ref', enabled: true, sort_order: 20, description: '' },
  ],
  roles: [
    { id: 'r1', name: 'All staff', description: 'Baseline', enabled: true, priority: 10, match: 'all', conditions: [], grants: { groups: ['GG-AllStaff'], ou: '', attributes: [] } },
    { id: 'r2', name: 'Finance', description: '', enabled: true, priority: 50, match: 'all', conditions: [{ attr: 'Department', op: 'eq', value: 'Finance' }], grants: { groups: ['GG-Finance'], ou: 'OU=Finance,DC=corp,DC=example,DC=com', attributes: [{ target_attr: 'department', template: '{{ .Department }}', mode: 'sync', on_empty: 'skip', value_type: 'string' }] } },
  ],
  settings: {
    schedule: { enabled: true, kind: 'interval', interval_minutes: 60, daily_time: '02:00', cron_expr: '0 */4 * * *', timezone: 'America/Chicago' },
    approval: { create: 'manual', update: 'auto', disable: 'manual', enable: 'manual', delete: 'manual' },
    engine: { dry_run: true, max_destructive_per_run: 10, delete_after_days: 30, remove_stale_groups: true, enforce_ou: false },
    ad: { server: '', search_bases: ['OU=Staff,DC=corp,DC=example,DC=com'], default_ou: 'OU=Staff,DC=corp,DC=example,DC=com', disabled_ou: '', id_attribute: 'employeeID', upn_suffix: 'corp.example.com', strip_groups_on_disable: true },
    naming: { sam_template: '{{ first .FirstName 1 | lower }}{{ .LastName | lower }}', upn_template: '{{ .FirstName | lower }}.{{ .LastName | lower }}', cn_template: '{{ .FirstName }} {{ .LastName }}', max_candidates: 9 },
    new_user: { password_length: 20, change_password_at_logon: true, enabled: true },
  },
  changes: [],
  runs: [
    { id: 'run1', trigger: 'schedule', triggered_by: 'scheduler', started_at: ago(62), finished_at: ago(61), status: 'ok', counters: { total: 120, created: 2, updated: 4, disabled: 1, enabled: 0, deleted: 0, pending: 3, failed: 1, skipped: 0, unchanged: 112, source_errors: 0 }, error: '' },
    { id: 'run2', trigger: 'manual', triggered_by: 'admin', started_at: ago(900), finished_at: ago(899), status: 'error', counters: {}, error: 'source "HR export": open C:\\feeds\\people.csv: file not found' },
  ],
  identities: [],
  audit: [],
};

function seedChanges() {
  const mk = (n, type, status, display, ext, summary) => ({
    id: `c${n}`, run_id: 'run1', identity_id: `i${n}`, external_id: ext, display, type, status,
    payload: { external_id: ext, ou: 'OU=Staff,DC=corp,DC=example,DC=com', attributes: { givenName: display.split(' ')[0], sn: display.split(' ')[1] } },
    summary, created_at: ago(70 - n), decided_by: '', decided_at: null, decision_note: '', applied_at: null, error: status === 'failed' ? 'Set-ADUser : Access is denied' : '', attempts: status === 'failed' ? 1 : 0,
  });
  db.changes = [
    mk(1, 'create', 'pending', 'Jane Doe', 'E1001', ['Create account jdoe in OU=Staff', 'Add to GG-AllStaff', 'Set title = Analyst']),
    mk(2, 'update', 'pending', 'John Smith', 'E1002', ['Set title: "Analyst" -> "Senior Analyst"']),
    mk(3, 'disable', 'pending', 'Terry Left', 'E1003', ['Disable account tleft', 'Remove 4 groups']),
    mk(4, 'delete', 'failed', 'Old Timer', 'E1004', ['Delete account otimer']),
    mk(5, 'update', 'applied', 'Ann Lee', 'E1005', ['Set department = Finance']),
    mk(6, 'create', 'simulated', 'Sim User', 'E1006', ['Create account suser']),
  ];
  db.identities = db.changes.map((c, i) => ({
    id: c.identity_id, source_id: 's1', source_name: 'HR export', external_id: c.external_id, display: c.display, active: i !== 2 && i !== 3,
    state: ['new', 'linked', 'disabled', 'deleted', 'linked', 'new'][i], ad_guid: i === 1 || i === 4 ? `guid-${i}` : '', ad_dn: i === 1 || i === 4 ? `CN=${c.display},OU=Staff,DC=corp,DC=example,DC=com` : '',
    ad_sam: i === 1 || i === 4 ? c.display.toLowerCase().replace(' ', '.') : '', first_seen: ago(5000), last_seen: ago(60), disabled_at: null,
    attrs: { FirstName: c.display.split(' ')[0], LastName: c.display.split(' ')[1], EmployeeId: c.external_id, Department: 'Finance', Status: 'A' },
  }));
}

function audit(actor, action, entity_type, entity_id, detail) {
  db.audit.push({ seq: db.audit.length + 1, ts: now(), actor, action, entity_type, entity_id, detail, hash: Array.from({ length: 64 }, () => '0123456789abcdef'[Math.floor(Math.random() * 16)]).join('') });
}

seedChanges();
for (let i = 0; i < 30; i += 1) {
  audit(i % 3 === 0 ? 'system' : 'admin', i % 2 ? 'change.approve' : 'run.finish', i % 2 ? 'change' : 'run', `x${i}`, { note: `entry ${i}`, long: 'x'.repeat(i * 5) });
}

// ------------------------------------------------------------------ helpers

class HttpError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

const SOURCE_TYPES = [
  {
    type: 'csv', label: 'CSV file',
    fields: [
      { name: 'path', label: 'File path', type: 'string', required: true, help: 'Path of the CSV file readable by the service account.' },
      { name: 'delimiter', label: 'Delimiter', type: 'string', default: ',' },
      { name: 'skip_rows', label: 'Rows to skip', type: 'int', default: 0 },
      { name: 'has_header', label: 'First row is a header', type: 'bool', default: true },
      { name: 'api_key', label: 'Optional secret', type: 'password' },
      { name: 'notes', label: 'Notes', type: 'text' },
    ],
  },
  {
    type: 'paylocity', label: 'Paylocity API',
    fields: [
      { name: 'company_id', label: 'Company ID', type: 'string', required: true },
      { name: 'client_secret', label: 'Client secret', type: 'password', required: true },
    ],
  },
];

function paginate(items, params) {
  const limit = Math.min(Number(params.get('limit')) || 50, 500);
  const offset = Number(params.get('offset')) || 0;
  return { items: items.slice(offset, offset + limit), total: items.length };
}

function matchQ(obj, q) {
  if (!q) {
    return true;
  }
  return JSON.stringify(Object.values(obj)).toLowerCase().includes(q.toLowerCase());
}

const pubUser = (u) => ({ id: u.id, username: u.username, role: u.role, disabled: u.disabled, created_at: u.created_at, last_login_at: u.last_login_at });

function applyTemplate(template, rec) {
  const call = (expr) => {
    const parts = expr.trim().split(/\s+/);
    const val = (tok) => {
      if (tok.startsWith('.')) {
        const v = rec[tok.slice(1)];
        return v === undefined || v === null ? '' : String(v);
      }
      if (/^\d+$/.test(tok)) {
        return Number(tok);
      }
      return tok.replace(/^"|"$/g, '');
    };
    if (parts[0] === 'index') {
      return val(`.${val(parts[2])}`);
    }
    if (parts[0] === 'first') {
      return String(val(parts[1])).slice(0, val(parts[2]));
    }
    if (parts[0].startsWith('.')) {
      return val(parts[0]);
    }
    throw new Error(`function "${parts[0]}" not supported by the mock`);
  };
  const fns = {
    lower: (s) => s.toLowerCase(), upper: (s) => s.toUpperCase(), trim: (s) => s.trim(),
    title: (s) => s.replace(/\b\w/g, (c) => c.toUpperCase()),
  };
  return template.replace(/\{\{\s*([\s\S]*?)\s*\}\}/g, (_m, expr) => {
    const segs = expr.split('|').map((s) => s.trim());
    let v = call(segs[0]);
    for (const f of segs.slice(1)) {
      if (!fns[f]) {
        throw new Error(`function "${f}" not defined`);
      }
      v = fns[f](String(v));
    }
    return String(v);
  });
}

function rolePreview(record) {
  const matched = [];
  const groups = new Set();
  let ou = '';
  const attributes = {};
  for (const r of db.roles.filter((x) => x.enabled).sort((a, b) => a.priority - b.priority)) {
    const results = (r.conditions || []).map((c) => {
      const v = record[c.attr];
      if (c.op === 'exists') {
        return v !== undefined && v !== null && v !== '';
      }
      if (c.op === 'eq') {
        return String(v) === String(c.value);
      }
      if (c.op === 'ne') {
        return String(v) !== String(c.value);
      }
      return false;
    });
    const ok = results.length === 0 || (r.match === 'any' ? results.some(Boolean) : results.every(Boolean));
    if (!ok) {
      continue;
    }
    matched.push({ id: r.id, name: r.name });
    (r.grants.groups || []).forEach((g) => groups.add(g));
    if (r.grants.ou) {
      ou = r.grants.ou;
    }
    for (const a of r.grants.attributes || []) {
      attributes[a.target_attr] = applyTemplate(a.template, record);
    }
  }
  return { matched, groups: Array.from(groups), ou, attributes };
}

// ------------------------------------------------------------------ router

const LEVEL = { viewer: 1, approver: 2, admin: 3 };

function need(role) {
  if (!db.session) {
    throw new HttpError(401, 'unauthorized', 'Authentication required.');
  }
  if (LEVEL[db.session.user.role] < LEVEL[role]) {
    throw new HttpError(403, 'forbidden', `Requires role ${role}.`);
  }
}

function decide(change, to, actor, note) {
  if (to === 'approved' && change.status !== 'pending' && change.status !== 'failed') {
    throw new HttpError(409, 'conflict', `Cannot approve a change that is ${change.status}.`);
  }
  if (to === 'rejected' && change.status !== 'pending') {
    throw new HttpError(409, 'conflict', `Cannot reject a change that is ${change.status}.`);
  }
  change.status = to;
  change.decided_by = actor;
  change.decided_at = now();
  change.decision_note = note || '';
  audit(actor, `change.${to}`, 'change', change.id, { note });
}

function route(method, path, params, body) {
  let m;
  if (method === 'POST' && path === '/auth/login') {
    const u = db.users.find((x) => x.username === body.username && x.password === body.password && !x.disabled);
    if (!u) {
      throw new HttpError(401, 'invalid_credentials', 'Invalid username or password.');
    }
    db.session = { user: u, csrf: newId('csrf') };
    u.last_login_at = now();
    return { user: { id: u.id, username: u.username, role: u.role }, csrf_token: db.session.csrf };
  }
  if (path === '/auth/me') {
    need('viewer');
    const u = db.session.user;
    return { user: { id: u.id, username: u.username, role: u.role }, csrf_token: db.session.csrf };
  }
  if (method === 'POST' && path === '/auth/logout') {
    db.session = null;
    return {};
  }
  if (method === 'POST' && path === '/auth/password') {
    need('viewer');
    if (body.current !== db.session.user.password) {
      throw new HttpError(400, 'bad_password', 'Current password is incorrect.');
    }
    db.session.user.password = body.new;
    return {};
  }
  if (path === '/status') {
    need('viewer');
    return {
      version: '0.0-mock', db_driver: 'sqlite', mode: db.settings.engine.dry_run ? 'dry_run' : 'live',
      scheduler: { enabled: db.settings.schedule.enabled, next_run: new Date(Date.now() + 25 * 60000).toISOString() },
      running_run: null, pending_changes: db.changes.filter((c) => c.status === 'pending').length, last_run: db.runs[0],
    };
  }
  if (path === '/source-types') {
    need('viewer');
    return SOURCE_TYPES;
  }

  // generic CRUD collections
  const crud = (name, coll, role, build) => {
    const idm = path.match(new RegExp(`^/${name}/([^/]+)$`));
    if (path === `/${name}`) {
      if (method === 'GET') {
        need('viewer');
        return paginate(coll(), params);
      }
      if (method === 'POST') {
        need(role);
        const item = { id: newId(name[0]), ...build(body) };
        coll().push(item);
        audit(db.session.user.username, `${name}.create`, name, item.id, {});
        return item;
      }
    }
    if (idm) {
      const item = coll().find((x) => x.id === idm[1]);
      if (method === 'GET') {
        need('viewer');
      }
      else {
        need(role);
      }
      if (!item) {
        throw new HttpError(404, 'not_found', `${name} not found.`);
      }
      if (method === 'GET') {
        return item;
      }
      if (method === 'PUT') {
        Object.assign(item, build(body));
        audit(db.session.user.username, `${name}.update`, name, item.id, {});
        return item;
      }
      if (method === 'DELETE') {
        coll().splice(coll().indexOf(item), 1);
        audit(db.session.user.username, `${name}.delete`, name, item.id, {});
        return null;
      }
    }
    return undefined;
  };

  const hasUnmaskedConfig = (cfg) => cfg || {};
  let r = crud('sources', () => db.sources, 'admin', (b) => ({
    name: b.name, type: b.type, enabled: b.enabled, options: b.options || {}, config: hasUnmaskedConfig(b.config),
    last_sync_at: b.last_sync_at || null, last_status: b.last_status || '', last_error: b.last_error || '',
  }));
  if (r !== undefined) {
    if (method === 'POST' && db.sources.filter((s) => s.name === body.name).length > 1) {
      db.sources.pop();
      throw new HttpError(409, 'duplicate', 'A source with that name already exists.');
    }
    return r;
  }
  if ((m = path.match(/^\/sources\/([^/]+)\/test$/)) && method === 'POST') {
    need('admin');
    const s = db.sources.find((x) => x.id === m[1]);
    if (!s) {
      throw new HttpError(404, 'not_found', 'Source not found.');
    }
    return {
      ok: true, count: 3, error: '', attributes: ['EmployeeId', 'FirstName', 'LastName', 'Department', 'Status'],
      sample: [
        { EmployeeId: 'E1001', FirstName: 'Jane', LastName: 'Doe', Department: 'Finance', Status: 'A' },
        { EmployeeId: 'E1002', FirstName: 'John', LastName: 'Smith', Department: 'IT', Status: 'A' },
        { EmployeeId: 'E1003', FirstName: 'Terry', LastName: 'Left', Department: 'HR', Status: 'T' },
      ],
    };
  }
  const mapBuild = (b) => ({
    name: b.name, target_attr: b.target_attr, template: b.template, mode: b.mode, on_empty: b.on_empty, value_type: b.value_type,
    enabled: b.enabled, sort_order: b.sort_order, description: b.description || '',
  });
  r = crud('mappings', () => db.mappings, 'admin', mapBuild);
  if (r !== undefined) {
    return r;
  }
  r = crud('roles', () => db.roles, 'admin', (b) => ({
    name: b.name, description: b.description || '', enabled: b.enabled, priority: b.priority, match: b.match, conditions: b.conditions || [], grants: b.grants || {},
  }));
  if (r !== undefined) {
    return r;
  }
  if (method === 'POST' && path === '/templates/preview') {
    need('admin');
    try {
      return { ok: true, output: applyTemplate(body.template, body.record || {}), error: '' };
    }
    catch (e) {
      return { ok: false, output: '', error: e.message };
    }
  }
  if (method === 'POST' && path === '/roles/preview') {
    need('admin');
    return rolePreview(body.record || {});
  }
  if (path === '/settings') {
    if (method === 'GET') {
      need('viewer');
      return db.settings;
    }
    if (method === 'PUT') {
      need('admin');
      db.settings = body;
      audit(db.session.user.username, 'settings.update', 'settings', 'all', {});
      return db.settings;
    }
  }
  if (method === 'GET' && path === '/changes') {
    need('viewer');
    let items = db.changes.filter((c) => (!params.get('status') || c.status === params.get('status'))
      && (!params.get('type') || c.type === params.get('type')) && matchQ({ d: c.display, e: c.external_id }, params.get('q')));
    items = items.slice().sort((a, b) => a.created_at.localeCompare(b.created_at));
    return paginate(items, params);
  }
  if (method === 'POST' && path === '/changes/bulk') {
    need('approver');
    const to = body.action === 'approve' ? 'approved' : 'rejected';
    if (to === 'rejected' && !(body.note || '').trim()) {
      throw new HttpError(400, 'note_required', 'A note is required to reject.');
    }
    const results = body.ids.map((id) => {
      try {
        decide(db.changes.find((c) => c.id === id), to, db.session.user.username, body.note);
        return { id, ok: true };
      }
      catch (e) {
        return { id, ok: false, error: e.message };
      }
    });
    return { results };
  }
  if ((m = path.match(/^\/changes\/([^/]+)$/)) && method === 'GET') {
    need('viewer');
    const c = db.changes.find((x) => x.id === m[1]);
    if (!c) {
      throw new HttpError(404, 'not_found', 'Change not found.');
    }
    return c;
  }
  if ((m = path.match(/^\/changes\/([^/]+)\/(approve|reject|retry)$/)) && method === 'POST') {
    need('approver');
    const c = db.changes.find((x) => x.id === m[1]);
    if (!c) {
      throw new HttpError(404, 'not_found', 'Change not found.');
    }
    if (m[2] === 'reject' && !(body.note || '').trim()) {
      throw new HttpError(400, 'note_required', 'A note is required to reject.');
    }
    decide(c, m[2] === 'reject' ? 'rejected' : 'approved', db.session.user.username, body.note);
    return c;
  }
  if (method === 'GET' && path === '/runs') {
    need('viewer');
    return paginate(db.runs, params);
  }
  if (method === 'POST' && path === '/runs') {
    need('admin');
    const run = { id: newId('run'), trigger: 'manual', triggered_by: db.session.user.username, started_at: now(), finished_at: now(), status: 'ok', counters: { total: 120, created: 0, updated: 0, unchanged: 120 }, error: '' };
    db.runs.unshift(run);
    return { id: run.id };
  }
  if ((m = path.match(/^\/runs\/([^/]+)$/)) && method === 'GET') {
    need('viewer');
    const run = db.runs.find((x) => x.id === m[1]);
    if (!run) {
      throw new HttpError(404, 'not_found', 'Run not found.');
    }
    return run;
  }
  if (method === 'GET' && path === '/identities') {
    need('viewer');
    return paginate(db.identities.filter((i) => (!params.get('state') || i.state === params.get('state')) && matchQ({ a: i.display, b: i.external_id, c: i.ad_sam }, params.get('q'))), params);
  }
  if ((m = path.match(/^\/identities\/([^/]+)$/)) && method === 'GET') {
    need('viewer');
    const i = db.identities.find((x) => x.id === m[1]);
    if (!i) {
      throw new HttpError(404, 'not_found', 'Identity not found.');
    }
    return i;
  }
  if (method === 'GET' && path === '/audit/verify') {
    need('viewer');
    return { ok: true, checked: db.audit.length, first_bad_seq: 0 };
  }
  if (method === 'GET' && path === '/audit') {
    need('viewer');
    const items = db.audit.filter((a) => (!params.get('actor') || a.actor.includes(params.get('actor')))
      && (!params.get('action') || a.action.includes(params.get('action'))) && (!params.get('entity_type') || a.entity_type === params.get('entity_type')))
      .slice().reverse();
    return paginate(items, params);
  }
  if (method === 'GET' && path === '/tables') {
    need('viewer');
    return ['sync_runs', 'changes', 'identities', 'audit_log'];
  }
  if ((m = path.match(/^\/tables\/([^/]+)$/)) && method === 'GET') {
    need('viewer');
    const src = { sync_runs: db.runs, changes: db.changes, identities: db.identities, audit_log: db.audit }[m[1]];
    if (!src) {
      throw new HttpError(404, 'not_found', 'Unknown table.');
    }
    const columns = src.length ? Object.keys(src[0]) : [];
    const rows = src.filter((o) => matchQ(o, params.get('q'))).map((o) => columns.map((c) => o[c]));
    const p = paginate(rows, params);
    return { name: m[1], columns, rows: p.items, total: p.total };
  }
  r = crud('users', () => db.users, 'admin', (b) => {
    const base = { username: b.username, role: b.role, disabled: Boolean(b.disabled) };
    if (b.password) {
      base.password = b.password;
    }
    if (method === 'POST') {
      base.created_at = now();
      base.last_login_at = null;
    }
    return base;
  });
  if (r !== undefined) {
    if (r === null) {
      return null;
    }
    return Array.isArray(r.items) ? { items: r.items.map(pubUser), total: r.total } : pubUser(r);
  }
  throw new HttpError(404, 'not_found', `No such endpoint: ${method} /api${path}`);
}

// ------------------------------------------------------------------ fetch interceptor

const realFetch = window.fetch.bind(window);

window.fetch = async (input, init = {}) => {
  const url = new URL(typeof input === 'string' ? input : input.url, location.href);
  if (!url.pathname.startsWith('/api/')) {
    return realFetch(input, init);
  }
  const method = (init.method || 'GET').toUpperCase();
  const headers = init.headers || {};
  const path = url.pathname.slice(4);
  const respond = (status, payload) => new Response(payload === null ? null : JSON.stringify(payload), {
    status, headers: { 'Content-Type': 'application/json' },
  });
  await new Promise((r) => setTimeout(r, 5));
  try {
    if (method !== 'GET' && path !== '/auth/login') {
      if (!db.session) {
        throw new HttpError(401, 'unauthorized', 'Authentication required.');
      }
      if (headers['X-CSRF-Token'] !== db.session.csrf) {
        throw new HttpError(403, 'csrf', 'Missing or invalid CSRF token.');
      }
    }
    const body = init.body ? JSON.parse(init.body) : {};
    const out = route(method, path, url.searchParams, body);
    return respond(out === null ? 204 : (method === 'POST' && path === '/runs' ? 202 : 200), out);
  }
  catch (e) {
    if (e instanceof HttpError) {
      return respond(e.status, { error: { code: e.code, message: e.message } });
    }
    return respond(500, { error: { code: 'internal', message: `mock failure: ${e.message}` } });
  }
};
