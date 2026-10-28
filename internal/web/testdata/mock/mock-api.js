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
    approval: { create: 'manual', update: 'auto', disable: 'manual', enable: 'manual', delete: 'manual', link: 'auto' },
    engine: { dry_run: true, max_destructive_per_run: 10, delete_after_days: 30, remove_stale_groups: true, enforce_ou: false, max_deletes_per_run: 5, max_deletes_per_day: 20 },
    ad: { server: '', search_bases: ['OU=Staff,DC=corp,DC=example,DC=com'], default_ou: 'OU=Staff,DC=corp,DC=example,DC=com', disabled_ou: '', id_attribute: 'employeeID', upn_suffix: 'corp.example.com', strip_groups_on_disable: true },
    lifecycle_groups: { active_add: ['GG-All'], disabled_add: [], disabled_remove: 'all_except', disabled_keep: ['Mailbox-Retention'] },
    naming: { sam_template: '{{ first .FirstName 1 | lower }}{{ .LastName | lower }}', upn_template: '{{ .FirstName | lower }}.{{ .LastName | lower }}', cn_template: '{{ .FirstName }} {{ .LastName }}', max_candidates: 9, mail_template: '', mail_domain: '', conflict_mode: 'manual', conflict_random_digits: 3, conflict_suffix_template: '{{ last .EmployeeID 4 }}' },
    correlation: {
      enabled: true, rename: 'transitions', max_auto_links_per_run: 25, report_uncorrelated: true, ignore_accounts: ['svc_*'],
      rules: [
        { field: 'FirstName', ad_attr: 'givenName', compare: 'name', required: true },
        { field: 'LastName', ad_attr: 'sn', compare: 'name', required: true },
        { field: 'Department', ad_attr: 'department', compare: 'ci', required: false },
      ],
    },
    backup: { enabled: false, interval_hours: 24, keep: 14, folder: '', passphrase: '' },
    new_user: { password_mode: 'random', password_length: 20, password_template: '{{ upper (first .LastName 1) }}{{ last .EmployeeId 4 }}!{{ random 6 }}', change_password_at_logon: true, enabled: true },
    notifications: {
      enabled: false, provider: 'm365_smtp', from: 'idlifecycle@corp.example.com', to: ['ops@corp.example.com'], subject_prefix: '[IDLifecycle]', gui_base_url: '',
      smtp: { host: 'smtp.office365.com', port: 587, security: 'starttls', auth: 'oauth2', username: '', password: '' },
      oauth: { tenant_id: 'corp.onmicrosoft.com', client_id: '00000000-0000-0000-0000-000000000001', client_secret: '********' },
      events: { run_failed: true, approvals_pending: true, changes_failed: true, threshold: true, run_summary: false, identity_issues: true },
    },
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

// ---- naming conflicts, matching and manual people
const MANUAL_FIELDS = [
  { name: 'FirstName', label: 'First name', required: true, kind: 'text' },
  { name: 'LastName', label: 'Last name', required: true, kind: 'text' },
  { name: 'Department', label: 'Department', required: true, kind: 'select', options: ['Finance', 'IT', 'Sales'] },
  { name: 'Title', label: 'Job title', required: false, kind: 'text' },
  { name: 'Company', label: 'Company', required: false, kind: 'text', default: 'Contoso Staffing' },
  { name: 'Email', label: 'Contact e-mail', required: false, kind: 'email' },
  { name: 'StartDate', label: 'Start date', required: false, kind: 'date' },
  { name: 'EndDate', label: 'End date', required: false, kind: 'date' },
];
const TAKEN = {
  sam: { akim: 'CN=Alan Kim,OU=Staff,DC=corp,DC=example,DC=com', jdoe: 'CN=Jane Doe,OU=Staff,DC=corp,DC=example,DC=com' },
  upn: { 'alex.kim@corp.example.com': 'CN=Alan Kim,OU=Staff,DC=corp,DC=example,DC=com' },
  mail: { 'alex.kim@corp.example.com': 'CN=Alex Kim (Shared),OU=Shared,DC=corp,DC=example,DC=com' },
  cn: {},
};
function takenNames(names) {
  return (names || []).filter((n) => TAKEN[n.field] && TAKEN[n.field][String(n.value).toLowerCase()])
    .map((n) => ({ field: n.field, value: n.value, dn: TAKEN[n.field][String(n.value).toLowerCase()] }));
}
function seedIdentityFeatures() {
  const base = db.changes[0];
  db.changes.push({
    ...base, id: 'c7', identity_id: 'i7', external_id: 'E1007', display: 'Alex Kim', type: 'create', status: 'pending', created_at: ago(30),
    payload: {
      ou: 'OU=Staff,DC=corp,DC=example,DC=com', candidates: [], attributes: { givenName: 'Alex', sn: 'Kim' }, needs_name: true,
      name_conflicts: [
        { field: 'sam', value: 'akim', dn: 'CN=Alan Kim,OU=Staff,DC=corp,DC=example,DC=com' },
        { field: 'mail', value: 'alex.kim@corp.example.com', dn: 'CN=Alex Kim (Shared),OU=Shared,DC=corp,DC=example,DC=com' },
      ],
    },
    summary: ['WAITING for a username: choose one with Set name, then approve', 'Create account "akim" (Alex Kim) in OU=Staff,DC=corp,DC=example,DC=com'],
  }, {
    ...base, id: 'c8', identity_id: 'i8', external_id: 'E1008', display: 'Maria Lopez', type: 'link', status: 'pending', created_at: ago(29),
    payload: {
      object_guid: '', dn: '',
      link: {
        chosen: '', verify: true, rename: null, rename_note: '',
        candidates: [
          {
            object_guid: 'g-ml1', dn: 'CN=Maria Lopez,OU=Contractors,DC=corp,DC=example,DC=com', sam: 'c-mlopez', upn: 'c-mlopez@corp.example.com', enabled: true,
            external_id: 'C-7Q2K9A', identity_id: 'i9', identity_display: 'Maria Lopez', source_name: 'Contractors', full: false,
            fields: [
              { field: 'FirstName', ad_attr: 'givenName', value: 'Maria', ad_value: 'Maria', matched: true },
              { field: 'LastName', ad_attr: 'sn', value: 'Lopez', ad_value: 'López', matched: true },
              { field: 'Department', ad_attr: 'department', value: 'Finance', ad_value: 'Accounting', matched: false },
            ],
          },
          {
            object_guid: 'g-ml2', dn: 'CN=Maria Lopez 2,OU=Staff,DC=corp,DC=example,DC=com', sam: 'mlopez', upn: 'maria.lopez@corp.example.com', enabled: false,
            external_id: '', full: false,
            fields: [
              { field: 'FirstName', ad_attr: 'givenName', value: 'Maria', ad_value: 'Maria', matched: true },
              { field: 'LastName', ad_attr: 'sn', value: 'Lopez', ad_value: 'Lopez', matched: true },
              { field: 'Department', ad_attr: 'department', value: 'Finance', ad_value: '', matched: false },
            ],
          },
        ],
      },
    },
    summary: ['VERIFY the match: 2 AD accounts could be Maria Lopez', 'Link an existing account to Maria Lopez (E1008)'],
  });
  for (const c of db.changes.slice(-2)) {
    db.identities.push({
      id: c.identity_id, source_id: 's1', source_name: 'HR export', external_id: c.external_id, display: c.display, active: true, state: 'new',
      ad_guid: '', ad_dn: '', ad_sam: '', first_seen: ago(40), last_seen: ago(30), disabled_at: null,
      attrs: { FirstName: c.display.split(' ')[0], LastName: c.display.split(' ')[1], EmployeeId: c.external_id, Department: 'Finance', Status: 'A' },
    });
  }
  db.identities.push({
    id: 'i9', source_id: 's2', source_name: 'Contractors', external_id: 'C-7Q2K9A', display: 'Maria Lopez', active: true, state: 'merged', merged_into: 'i8',
    ad_guid: '', ad_dn: '', ad_sam: '', first_seen: ago(9000), last_seen: ago(30), disabled_at: null,
    attrs: { FirstName: 'Maria', LastName: 'Lopez', Department: 'Accounting', ExternalID: 'C-7Q2K9A', Status: 'Active', Source: 'manual' },
  });
  db.sources.push({
    id: 's2', name: 'Contractors', type: 'manual', enabled: true,
    options: { external_id_field: 'ExternalID', status_field: 'Status', terminated_values: ['Terminated'], missing_means_terminated: false, missing_grace_runs: 0 },
    config: { id_prefix: 'C-', end_date_field: 'EndDate', form_fields: 'FirstName | First name | required\nLastName | Last name | required' },
    last_sync_at: ago(60), last_status: 'ok', last_error: '',
  });
  db.manual = [
    { id: 'mp1', source_id: 's2', external_id: 'C-7Q2K9A', attrs: { FirstName: 'Maria', LastName: 'Lopez', Department: 'Finance', Company: 'Contoso Staffing' }, ended: false, created_by: 'approver1', created_at: ago(9000), updated_by: 'approver1', updated_at: ago(9000), state: 'linked', ad_sam: 'c-mlopez' },
    { id: 'mp2', source_id: 's2', external_id: 'C-3HD81Z', attrs: { FirstName: 'Sam', LastName: 'Patel', Department: 'IT', EndDate: '2026-12-31' }, ended: false, created_by: 'admin', created_at: ago(300), updated_by: 'admin', updated_at: ago(300), state: 'new', ad_sam: '' },
  ];
  db.report = {
    run_id: 'run1', generated_at: ago(61), enabled: true, unmatched_people: 1,
    uncorrelated: [
      { object_guid: 'g-u1', dn: 'CN=Old Contractor,OU=Staff,DC=corp,DC=example,DC=com', sam: 'ocontractor', upn: 'ocontractor@corp.example.com', display: 'Old Contractor', enabled: true, external_id: '' },
      { object_guid: 'g-u2', dn: 'CN=Print Kiosk,OU=Staff,DC=corp,DC=example,DC=com', sam: 'kiosk01', upn: '', display: 'Print Kiosk', enabled: true, external_id: '' },
      { object_guid: 'g-u3', dn: 'CN=Ex Employee,OU=Staff,DC=corp,DC=example,DC=com', sam: 'xemployee', upn: '', display: 'Ex Employee', enabled: false, external_id: 'E0099' },
    ],
    verify: [{ change_id: 'c8', identity_id: 'i8', display: 'Maria Lopez', external_id: 'E1008', candidates: 2 }],
    needs_name: [{ change_id: 'c7', display: 'Alex Kim', external_id: 'E1007' }],
  };
}
seedIdentityFeatures();

function namesFor(rec) {
  const first = String(rec.FirstName || '').toLowerCase().replace(/[^a-z0-9]/g, '');
  const last = String(rec.LastName || '').toLowerCase().replace(/[^a-z0-9]/g, '');
  const suffix = db.settings.ad.upn_suffix || 'corp.example.com';
  return { sam: `${first.slice(0, 1)}${last}`.slice(0, 20), upn: `${first}.${last}@${suffix}`, cn: `${rec.FirstName || ''} ${rec.LastName || ''}`.trim(), mail: `${first}.${last}@${suffix}` };
}

function identityRoutes(method, path, body) {
  let m;
  if ((m = path.match(/^\/changes\/([^/]+)\/names$/)) && method === 'GET') {
    need('approver');
    const c = db.changes.find((x) => x.id === m[1]);
    if (!c) {
      throw new HttpError(404, 'not_found', 'Change not found.');
    }
    const id = db.identities.find((i) => i.id === c.identity_id) || { attrs: {} };
    const n = namesFor(id.attrs);
    const sfx = (s) => ({ sam: `${n.sam}${s}`.slice(0, 20), upn: n.upn.replace('@', `${s}@`), cn: `${n.cn} ${s}`, mail: n.mail.replace('@', `${s}@`) });
    return {
      current: (c.payload.candidates || [])[0] || n,
      conflicts: c.payload.name_conflicts || [],
      suggestions: [{ mode: 'number', candidate: sfx('2') }, { mode: 'random', candidate: sfx('417') }, { mode: 'template', candidate: sfx('1007') }],
    };
  }
  if ((m = path.match(/^\/changes\/([^/]+)\/name$/)) && method === 'POST') {
    need('approver');
    const c = db.changes.find((x) => x.id === m[1]);
    if (!c || c.status !== 'pending') {
      throw new HttpError(409, 'conflict', 'Only a pending change can get a name.');
    }
    if (!/^[A-Za-z0-9._-]{1,20}$/.test(body.sam || '') || !/^[^@\s]+@[^@\s]+$/.test(body.upn || '') || !body.cn) {
      throw new HttpError(400, 'bad_request', 'sam, upn and cn are required (sam: 1-20 letters, digits, . _ -).');
    }
    const taken = takenNames([{ field: 'sam', value: body.sam }, { field: 'upn', value: body.upn }, { field: 'mail', value: body.mail || '' }, { field: 'cn', value: body.cn }]);
    if (taken.length > 0) {
      throw new HttpError(409, 'conflict', 'These names are already taken.', { conflicts: taken });
    }
    c.payload.candidates = [{ sam: body.sam, upn: body.upn, cn: body.cn, mail: body.mail || '' }];
    c.payload.needs_name = false;
    c.payload.name_conflicts = [];
    c.summary = c.summary.filter((s) => !s.startsWith('WAITING for a username'));
    audit(db.session.user.username, 'change.set_name', 'change', c.id, { sam: body.sam });
    return c;
  }
  if (method === 'POST' && path === '/ad/check-names') {
    need('approver');
    return { taken: takenNames(body.names) };
  }
  if ((m = path.match(/^\/changes\/([^/]+)\/link$/)) && method === 'POST') {
    need('approver');
    const c = db.changes.find((x) => x.id === m[1]);
    if (!c || c.type !== 'link' || c.status !== 'pending') {
      throw new HttpError(409, 'conflict', 'Only a pending link change can be changed.');
    }
    if (body.create_new) {
      decide(c, 'rejected', db.session.user.username, 'create a new account instead');
      const id = db.identities.find((i) => i.id === c.identity_id);
      if (id) {
        id.link_decision = 'new';
      }
      return c;
    }
    const cand = c.payload.link.candidates.find((x) => x.object_guid === body.object_guid);
    if (!cand) {
      throw new HttpError(400, 'bad_request', 'object_guid is not one of the candidates.');
    }
    c.payload.link.chosen = cand.object_guid;
    c.payload.object_guid = cand.object_guid;
    c.payload.dn = cand.dn;
    audit(db.session.user.username, 'change.set_link', 'change', c.id, { object_guid: cand.object_guid });
    return c;
  }
  if ((m = path.match(/^\/identities\/([^/]+)\/link$/)) && method === 'POST') {
    need('approver');
    const id = db.identities.find((x) => x.id === m[1]);
    if (!id) {
      throw new HttpError(404, 'not_found', 'Identity not found.');
    }
    if (id.ad_guid) {
      throw new HttpError(409, 'conflict', 'This person is already linked to an AD account.');
    }
    id.link_decision = body.clear ? '' : `guid:${body.object_guid}`;
    audit(db.session.user.username, 'identity.link_decision', 'identity', id.id, { decision: id.link_decision });
    return id;
  }
  if (method === 'GET' && path === '/placement/values') {
    need('viewer');
    return { field: 'Department', values: [{ value: 'Finance', people: 4, ou: 'OU=Finance,OU=Staff,DC=corp,DC=example,DC=com' }, { value: 'Sales', people: 2, ou: '' }] };
  }
  if (method === 'GET' && path === '/correlation/report') {
    need('viewer');
    const ignore = (db.settings.correlation && db.settings.correlation.ignore_accounts) || [];
    const ignored = (sam) => ignore.some((p) => new RegExp(`^${String(p).replace(/[.+?^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*')}$`, 'i').test(sam));
    return { ...db.report, enabled: !!(db.settings.correlation && db.settings.correlation.enabled), uncorrelated: db.report.uncorrelated.filter((a) => !ignored(a.sam)) };
  }
  if (method === 'GET' && path === '/manual/sources') {
    need('viewer');
    return db.sources.filter((s) => s.type === 'manual').map((s) => ({ id: s.id, name: s.name, enabled: s.enabled, id_prefix: s.config.id_prefix || 'C-', fields: MANUAL_FIELDS }));
  }
  if ((m = path.match(/^\/manual\/sources\/([^/]+)(\/.*)?$/))) {
    const src = db.sources.find((s) => s.id === m[1] && s.type === 'manual');
    if (!src) {
      throw new HttpError(404, 'not_found', 'Manual source not found.');
    }
    const rest = m[2] || '';
    const checkAttrs = (attrs) => {
      for (const f of MANUAL_FIELDS) {
        const v = String((attrs || {})[f.name] || '').trim();
        if (f.required && !v) {
          throw new HttpError(400, 'bad_request', `${f.label} is required.`);
        }
        if (v && f.kind === 'date' && !/^\d{4}-\d{2}-\d{2}$/.test(v)) {
          throw new HttpError(400, 'bad_request', `${f.label} must be a date (YYYY-MM-DD).`);
        }
      }
    };
    if (rest === '/people' && method === 'GET') {
      need('viewer');
      return db.manual.filter((p) => p.source_id === src.id).slice().sort((a, b) => b.created_at.localeCompare(a.created_at));
    }
    if (rest === '/preview' && method === 'POST') {
      need('approver');
      checkAttrs(body.attrs);
      const n = namesFor(body.attrs);
      const dept = body.attrs.Department;
      return {
        names: n, ou: dept === 'Sales' ? '' : `OU=${dept},OU=Staff,DC=corp,DC=example,DC=com`,
        ou_reason: dept === 'Sales' ? 'no OU placement rule for "Sales"' : `OU placement rule for "${dept}"`,
        conflicts: takenNames([{ field: 'sam', value: n.sam }, { field: 'upn', value: n.upn }, { field: 'mail', value: n.mail }]), warnings: [],
      };
    }
    if (rest === '/people' && method === 'POST') {
      need('approver');
      checkAttrs(body.attrs);
      const p = {
        id: newId('mp'), source_id: src.id, external_id: `${src.config.id_prefix || 'C-'}${Math.random().toString(36).slice(2, 8).toUpperCase()}`,
        attrs: body.attrs, ended: false, created_by: db.session.user.username, created_at: now(), updated_by: db.session.user.username, updated_at: now(), state: 'new', ad_sam: '',
      };
      db.manual.push(p);
      audit(db.session.user.username, 'manual.create', 'manual_person', p.id, { external_id: p.external_id });
      return body.sync ? { ...p, run_id: 'run-mock-sync' } : p;
    }
    const pm = rest.match(/^\/people\/([^/]+)(\/end)?$/);
    const person = pm ? db.manual.find((p) => p.id === pm[1] && p.source_id === src.id) : null;
    if (pm && !person) {
      throw new HttpError(404, 'not_found', 'Person not found.');
    }
    if (pm && !pm[2] && method === 'PUT') {
      need('approver');
      checkAttrs(body.attrs);
      Object.assign(person, { attrs: body.attrs, updated_by: db.session.user.username, updated_at: now() });
      return person;
    }
    if (pm && pm[2] && method === 'POST') {
      need('approver');
      person.ended = true;
      audit(db.session.user.username, 'manual.end', 'manual_person', person.id, { note: body.note || '' });
      return person;
    }
    if (pm && !pm[2] && method === 'DELETE') {
      need('admin');
      if (person.ad_sam) {
        throw new HttpError(409, 'conflict', 'This person has an AD account; end them instead.');
      }
      db.manual.splice(db.manual.indexOf(person), 1);
      return null;
    }
  }
  return undefined;
}
for (let i = 0; i < 30; i += 1) {
  audit(i % 3 === 0 ? 'system' : 'admin', i % 2 ? 'change.approve' : 'run.finish', i % 2 ? 'change' : 'run', `x${i}`, { note: `entry ${i}`, long: 'x'.repeat(i * 5) });
}

// ------------------------------------------------------------------ helpers

class HttpError extends Error {
  constructor(status, code, message, extra) {
    super(message);
    this.status = status;
    this.code = code;
    this.extra = extra || null; // extra top-level fields of the error body (e.g. conflicts)
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
    type: 'rest', label: 'HR system API (REST/JSON)',
    fields: [
      { name: 'base_url', label: 'Base URL', type: 'string', required: true },
      { name: 'path', label: 'Path', type: 'string' },
      { name: 'method', label: 'Method', type: 'select', options: ['GET', 'POST'], default: 'GET' },
      { name: 'body', label: 'Request body (JSON)', type: 'text', show_if: { field: 'method', values: ['POST'] } },
      { name: 'auth', label: 'Authentication', type: 'select', options: ['none', 'api_key', 'basic', 'bearer', 'oauth2_client_credentials'], default: 'none' },
      { name: 'api_key_name', label: 'API key header / parameter name', type: 'string', default: 'X-API-Key', show_if: { field: 'auth', values: ['api_key'] } },
      { name: 'api_key', label: 'API key', type: 'password', show_if: { field: 'auth', values: ['api_key'] } },
      { name: 'client_secret', label: 'Client secret', type: 'password', show_if: { field: 'auth', values: ['oauth2_client_credentials'] } },
      { name: 'records_path', label: 'Records path', type: 'string' },
      { name: 'pagination', label: 'Pagination', type: 'select', options: ['none', 'page', 'offset', 'cursor', 'next_link'], default: 'none' },
      { name: 'page_size', label: 'Page size', type: 'int', default: 100, show_if: { field: 'pagination', values: ['page', 'offset', 'cursor'] } },
    ],
  },
  {
    type: 'manual', label: 'Manual entry (Add person)',
    fields: [
      { name: 'id_prefix', label: 'Employee ID prefix', type: 'string', default: 'C-' },
      { name: 'form_fields', label: 'Form fields', type: 'text', default: 'FirstName | First name | required\nLastName | Last name | required' },
      { name: 'end_date_field', label: 'End date field', type: 'string', default: 'EndDate' },
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

// Conditions as the engine evaluates them (internal/engine/conditions.go), with the explain trail.
function parseWhen(v) {
  const s = String(v === undefined || v === null ? '' : v).trim();
  let m = s.match(/^(\d{1,2})\/(\d{1,2})\/(\d{4})$/);
  if (m) {
    return Date.UTC(Number(m[3]), Number(m[1]) - 1, Number(m[2]));
  }
  m = s.match(/^\d{4}-\d{2}-\d{2}/);
  if (!m) {
    return NaN;
  }
  return Date.parse(s.length === 10 ? `${s}T00:00:00Z` : s);
}

function compare(a, b) {
  const na = Number(a);
  const nb = Number(b);
  if (String(a).trim() !== '' && String(b).trim() !== '' && Number.isFinite(na) && Number.isFinite(nb)) {
    return na - nb;
  }
  const da = parseWhen(a);
  const dbb = parseWhen(b);
  if (Number.isNaN(da) || Number.isNaN(dbb)) {
    return NaN;
  }
  return da - dbb;
}

function evalCondition(c, record, path, explain) {
  const list = (s) => String(s).split(',').map((x) => x.trim().toLowerCase()).filter(Boolean);
  if (c.op === 'all_of' || c.op === 'any_of') {
    const kids = (c.conditions || []).map((k, i) => evalCondition(k, record, `${path}.conditions[${i}]`, null));
    const ok = c.op === 'any_of' ? kids.some((k) => k.matched) : kids.length > 0 && kids.every((k) => k.matched);
    if (explain) {
      explain.push({ path, attr: '', op: c.op, value: '', actual: `${kids.filter((k) => k.matched).length} of ${kids.length} matched`, matched: ok });
      (c.conditions || []).forEach((k, i) => evalCondition(k, record, `${path}.conditions[${i}]`, explain));
    }
    return { matched: ok };
  }
  let actual = c.attr === '*' ? '' : record[c.attr];
  actual = actual === undefined || actual === null ? '' : String(actual);
  const v = String(c.value === undefined || c.value === null ? '' : c.value);
  const al = actual.toLowerCase();
  const vl = v.toLowerCase();
  let ok = false;
  switch (c.op) {
    case 'eq': ok = al === vl; break;
    case 'ne': ok = al !== vl; break;
    case 'in': ok = list(v).includes(al); break;
    case 'not_in': ok = !list(v).includes(al); break;
    case 'contains': ok = al.includes(vl); break;
    case 'not_contains': ok = !al.includes(vl); break;
    case 'starts_with': ok = al.startsWith(vl); break;
    case 'ends_with': ok = al.endsWith(vl); break;
    case 'regex': case 'not_regex': {
      let re;
      try {
        re = new RegExp(v.replace(/^\(\?i\)/, ''), v.startsWith('(?i)') ? 'i' : '');
      }
      catch (e) {
        throw new HttpError(400, 'bad_request', `${path}: bad regex: ${e.message}`);
      }
      ok = re.test(actual) === (c.op === 'regex');
      break;
    }
    case 'exists': ok = actual.trim() !== ''; break;
    case 'empty': ok = actual.trim() === ''; break;
    case 'gt': ok = compare(actual, v) > 0; break;
    case 'gte': ok = compare(actual, v) >= 0; break;
    case 'lt': ok = compare(actual, v) < 0; break;
    case 'lte': ok = compare(actual, v) <= 0; break;
    case 'older_than_days': case 'newer_than_days': {
      const t = parseWhen(actual);
      const days = (Date.now() - t) / 86400000;
      ok = !Number.isNaN(t) && (c.op === 'older_than_days' ? days > Number(v) : days >= 0 && days < Number(v));
      break;
    }
    case 'template':
      try {
        actual = applyTemplate(v, record).trim();
      }
      catch (e) {
        actual = `template error: ${e.message}`;
      }
      ok = ['true', 'yes', '1'].includes(actual.toLowerCase());
      break;
    default:
      throw new HttpError(400, 'bad_request', `${path}: unknown op "${c.op}"`);
  }
  if (explain) {
    explain.push({ path, attr: c.attr, op: c.op, value: v, actual, matched: ok });
  }
  return { matched: ok };
}

function ruleMatches(r, record, explain) {
  const results = (r.conditions || []).map((c, i) => evalCondition(c, record, `conditions[${i}]`, explain).matched);
  return results.length === 0 || (r.match === 'any' ? results.some(Boolean) : results.every(Boolean));
}

function rolePreview(record, rule, roles = db.roles) {
  const matched = [];
  const groups = new Set();
  let ou = '';
  const attributes = {};
  let all = roles;
  let explain;
  const out0 = {};
  if (rule) {
    explain = [];
    out0.rule_matched = ruleMatches(rule, record, explain);
    all = roles.filter((r) => !rule.id || r.id !== rule.id).concat([{ ...rule, id: rule.id || '(this rule)' }]);
  }
  for (const r of all.filter((x) => x.enabled).sort((a, b) => a.priority - b.priority)) {
    if (!ruleMatches(r, record, null)) {
      continue;
    }
    matched.push({ id: r.id, name: r.name });
    ((r.grants && r.grants.groups) || []).forEach((g) => groups.add(g));
    if (r.grants && r.grants.ou) {
      ou = r.grants.ou;
    }
    for (const a of (r.grants && r.grants.attributes) || []) {
      attributes[a.target_attr] = applyTemplate(a.template, record);
    }
  }
  const out = { matched, groups: Array.from(groups), ou, attributes, ...out0 };
  if (explain) {
    out.explain = explain;
  }
  return out;
}

// ---- AD group search (GET /api/ad/groups)
const AD_GROUPS = [
  { sam: 'GG-All', name: 'GG-All', description: 'Every employee', category: 'security', scope: 'global' },
  { sam: 'GG-AllStaff', name: 'GG-AllStaff', description: 'All staff (baseline role)', category: 'security', scope: 'global' },
  { sam: 'GG-Finance', name: 'GG-Finance', description: 'Finance department', category: 'security', scope: 'global' },
  { sam: 'GG-Mkt', name: 'GG-Mkt', description: 'Marketing team', category: 'security', scope: 'global' },
  { sam: 'ALLUSERS', name: 'ALLUSERS', description: 'All active user accounts', category: 'security', scope: 'universal' },
  { sam: 'Finance-Readers', name: 'Finance-Readers', description: 'Read access to the finance share', category: 'security', scope: 'domainlocal' },
  { sam: 'AllStaffDL', name: 'All Staff DL', description: 'All staff mailing list', category: 'distribution', scope: 'universal' },
  { sam: 'Disabled Users', name: 'Disabled Users', description: 'Accounts that are disabled', category: 'security', scope: 'global' },
  { sam: 'Mailbox-Retention', name: 'Mailbox-Retention', description: 'Keeps the mailbox of leavers (legal hold)', category: 'security', scope: 'global' },
  { sam: 'Domain Admins', name: 'Domain Admins', description: 'Designated administrators of the domain', category: 'security', scope: 'global', privileged: true },
].map((g) => ({ dn: `CN=${g.name},OU=Groups,DC=corp,DC=example,DC=com`, privileged: false, ...g }));

function adGroups(params) {
  const q = String(params.get('q') || '').trim();
  if (q.length < 2) {
    throw new HttpError(400, 'bad_request', 'q must be at least 2 characters.');
  }
  const max = Math.min(Number(params.get('max')) || 25, 100);
  const ql = q.toLowerCase();
  const hits = AD_GROUPS.filter((g) => [g.sam, g.name, g.description].some((s) => String(s).toLowerCase().includes(ql)));
  return { groups: hits.slice(0, max), truncated: hits.length > max };
}

// ---- impact preview (POST /api/preview/impact): baseline = saved configuration, proposed = the body's overrides.
const PRIVILEGED = new Set(AD_GROUPS.filter((g) => g.privileged).map((g) => g.sam.toLowerCase()));

function desiredFor(ident, cfg) {
  const rec = ident.attrs || {};
  const rp = rolePreview(rec, null, cfg.roles);
  const lg = cfg.settings.lifecycle_groups || {};
  const groups = [...new Set([...rp.groups, ...(lg.active_add || [])])].filter((g) => !PRIVILEGED.has(String(g).toLowerCase()));
  const attrs = {};
  for (const m of cfg.mappings.filter((x) => x.enabled)) {
    try {
      attrs[m.target_attr] = applyTemplate(m.template, rec);
    }
    catch (e) {
      // a template the mock cannot run is skipped
    }
  }
  Object.assign(attrs, rp.attributes);
  return { groups, attrs, ou: rp.ou };
}

// The AD account as it is now: in step with the saved configuration, except John Smith (i2) misses a group.
function currentAD(ident, baseDesired) {
  if (ident.state === 'disabled') {
    return { sam: ident.ad_sam || 'tleft', groups: ['GG-AllStaff', 'GG-Finance', 'Mailbox-Retention', 'GG-Mkt'], attrs: {} };
  }
  if (!ident.ad_sam) {
    return null;
  }
  const groups = baseDesired.groups.filter((g) => !(ident.id === 'i2' && g === 'GG-AllStaff'));
  return { sam: ident.ad_sam, groups, attrs: { ...baseDesired.attrs } };
}

function planFor(ident, cfg, baseCfg) {
  if (ident.state === 'deleted') {
    return null;
  }
  const desired = desiredFor(ident, cfg);
  const cur = currentAD(ident, desiredFor(ident, baseCfg));
  const lg = cfg.settings.lifecycle_groups || {};
  const actions = [];
  if (ident.state === 'disabled') {
    const mode = lg.disabled_remove || (cfg.settings.ad && cfg.settings.ad.strip_groups_on_disable ? 'all_except' : 'none');
    const keep = new Set([...(lg.disabled_keep || []), ...(lg.disabled_add || [])].map((g) => g.toLowerCase()));
    const managed = new Set([...desired.groups, ...(lg.disabled_add || [])].map((g) => g.toLowerCase()));
    actions.push(`Disable account ${cur.sam}`);
    for (const g of cur.groups) {
      if ((mode === 'all_except' && !keep.has(g.toLowerCase())) || (mode === 'managed' && managed.has(g.toLowerCase()))) {
        actions.push(`Remove from group ${g}`);
      }
    }
    for (const g of lg.disabled_add || []) {
      if (!cur.groups.some((x) => x.toLowerCase() === g.toLowerCase())) {
        actions.push(`Add to group ${g}`);
      }
    }
    return { type: 'disable', actions };
  }
  if (!cur) {
    const sam = `${String(ident.attrs.FirstName || '').slice(0, 1)}${ident.attrs.LastName || ''}`.toLowerCase();
    actions.push(`Create account ${sam}${desired.ou ? ` in ${desired.ou}` : ''}`);
    desired.groups.forEach((g) => actions.push(`Add to group ${g}`));
    Object.entries(desired.attrs).forEach(([k, v]) => actions.push(`Set ${k} = "${v}"`));
    return { type: 'create', actions };
  }
  desired.groups.filter((g) => !cur.groups.includes(g)).forEach((g) => actions.push(`Add to group ${g}`));
  if (cfg.settings.engine && cfg.settings.engine.remove_stale_groups) {
    cur.groups.filter((g) => !desired.groups.includes(g)).forEach((g) => actions.push(`Remove from group ${g}`));
  }
  Object.entries(desired.attrs).filter(([k, v]) => cur.attrs[k] !== v).forEach(([k, v]) => actions.push(`Set ${k}: "${cur.attrs[k] || ''}" -> "${v}"`));
  return actions.length ? { type: 'update', actions } : null;
}

function impact(body) {
  if (body.q === '__busy__') {
    throw new HttpError(409, 'conflict', 'A preview or sync is already running.');
  }
  const baseCfg = { roles: db.roles, mappings: db.mappings, settings: db.settings };
  const hasProposal = Array.isArray(body.roles) || Array.isArray(body.mappings) || Boolean(body.settings);
  const propCfg = {
    roles: Array.isArray(body.roles) ? body.roles : db.roles,
    mappings: Array.isArray(body.mappings) ? body.mappings : db.mappings,
    settings: body.settings && typeof body.settings === 'object' ? body.settings : db.settings,
  };
  const summary = { changed_people: 0, baseline: {}, proposed: {}, groups_added: [], groups_removed: [], attributes: [] };
  const gAdd = {};
  const gDel = {};
  const attrs = {};
  const bump = (o, k) => { o[k] = (o[k] || 0) + 1; };
  const all = db.identities.map((ident) => {
    const base = planFor(ident, baseCfg, baseCfg);
    const prop = hasProposal ? planFor(ident, propCfg, baseCfg) : base;
    if (!base && !prop) {
      return null;
    }
    const bActs = base ? base.actions : [];
    const pActs = prop ? prop.actions : [];
    const lines = hasProposal ? [...pActs.filter((a) => !bActs.includes(a)).map((text) => ({ op: '+', text })), ...bActs.filter((a) => !pActs.includes(a)).map((text) => ({ op: '-', text }))] : [];
    let difference = 'same';
    if (!base && prop) {
      difference = 'new';
    }
    else if (base && !prop) {
      difference = 'gone';
    }
    else if (lines.length) {
      difference = 'changed';
    }
    if (base) {
      bump(summary.baseline, base.type);
    }
    if (prop) {
      bump(summary.proposed, prop.type);
    }
    for (const l of lines) {
      const g = l.text.match(/^Add to group (.+)$/);
      const r = l.text.match(/^Remove from group (.+)$/);
      const a = l.text.match(/^Set ([^ :=]+)/);
      if (g) {
        bump(l.op === '+' ? gAdd : gDel, g[1]);
      }
      if (r) {
        bump(l.op === '+' ? gDel : gAdd, r[1]);
      }
      if (a && l.op === '+') {
        bump(attrs, a[1]);
      }
    }
    if (difference !== 'same') {
      summary.changed_people += 1;
    }
    const brief = (p) => (p ? { type: p.type, summary: p.actions } : null);
    return {
      identity_id: ident.id, external_id: ident.external_id, display: ident.display, ad_sam: ident.ad_sam || '', state: ident.state,
      difference, baseline: brief(base), proposed: brief(prop), lines,
    };
  }).filter(Boolean);
  const toList = (o, k) => Object.entries(o).map(([name, people]) => ({ [k]: name, people })).sort((a, b) => b.people - a.people);
  summary.groups_added = toList(gAdd, 'group');
  summary.groups_removed = toList(gDel, 'group');
  summary.attributes = toList(attrs, 'attr');
  const onlyChanged = body.only_changed !== false;
  const q = String(body.q || '').trim().toLowerCase();
  const filtered = all.filter((it) => (!onlyChanged || it.difference !== 'same')
    && (!body.type || (it.baseline && it.baseline.type === body.type) || (it.proposed && it.proposed.type === body.type))
    && (!q || [it.display, it.external_id, it.ad_sam].some((s) => String(s).toLowerCase().includes(q))));
  const limit = Math.min(Number(body.limit) || 50, 500);
  const offset = Number(body.offset) || 0;
  return {
    generated_at: now(), snapshot_at: ago(body.refresh ? 0 : 42), people: all.length, names_checked: false, notes: [],
    summary, items: filtered.slice(offset, offset + limit), total: filtered.length,
  };
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

  if (method === 'POST' && path === '/sources/test') {
    need('admin');
    if (!body || !body.type) {
      throw new HttpError(400, 'invalid', 'type is required');
    }
    return {
      ok: true, count: 2, error: '', attributes: ['id', 'name.first'],
      sample: [{ id: 'E1', 'name.first': 'Jane' }, { id: 'E2', 'name.first': 'Bob' }],
    };
  }
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
  if (method === 'POST' && path === '/roles/preview') {
    need('admin');
    return rolePreview(body.record || {}, body.rule || null);
  }
  r = crud('roles', () => db.roles, 'admin', (b) => ({
    name: b.name, description: b.description || '', enabled: b.enabled, priority: b.priority, match: b.match, conditions: b.conditions || [], grants: b.grants || {},
  }));
  if (r !== undefined) {
    return r;
  }
  if (path.startsWith('/mapper/')) {
    need('admin');
    // Rough stand-in for engine.TemplateFields: .Name and index . "key".
    const tplFields = (t) => [...new Set([...String(t || '').matchAll(/\.([A-Za-z_][A-Za-z0-9_]*)|index \. "([^"]+)"/g)].map((m) => m[1] || m[2]))].sort();
    const refs = () => [
      ...db.mappings.map((m) => ({ kind: 'mapping', id: m.id, name: m.name, target_attr: m.target_attr, enabled: m.enabled, fields: tplFields(m.template), dynamic: false })),
      ...db.roles.map((r) => ({ kind: 'role', id: r.id, name: r.name, enabled: r.enabled, dynamic: false,
        fields: [...new Set([...(r.conditions || []).map((c) => c.attr), ...((r.grants && r.grants.attributes) || []).flatMap((g) => tplFields(g.template))])].sort() })),
    ];
    if (method === 'GET' && path === '/mapper/references') {
      return { references: refs() };
    }
    if (method === 'GET' && path === '/mapper/ad-attributes') {
      const s = (name, description, extra = {}) => ({ name, description, syntax: 'string', single_valued: true, mappable: true, ...extra });
      return {
        source: 'builtin', live_error: 'mock: no AD here', fetched_at: now(),
        attributes: [
          s('department', 'Department'), s('displayName', 'Display name shown in address lists'), s('givenName', 'First name'),
          s('manager', 'Manager (use value type user_ref)', { syntax: 'dn', hint: 'holds a reference to another object: use value type user_ref' }),
          s('sAMAccountName', 'Logon name', { mappable: false, reason: 'managed by IDLifecycle or security-sensitive' }),
          s('sn', 'Last name (surname)'), s('title', 'Job title'),
        ],
      };
    }
    const m = /^\/mapper\/sources\/([^/]+)\/fields$/.exec(path);
    if (method === 'POST' && m) {
      const rows = [
        { EmployeeId: 'E1001', FirstName: 'Jane', LastName: 'Doe', Department: 'Finance', Status: 'A', ManagerID: 'E1002' },
        { EmployeeId: 'E1002', FirstName: 'John', LastName: 'Smith', Department: 'IT', Status: 'A', ManagerID: '' },
        { EmployeeId: 'E1003', FirstName: 'Terry', LastName: 'Left', Department: 'HR', Status: 'T', ManagerID: 'E1002' },
      ];
      const names = Object.keys(rows[0]).sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()));
      const fields = names.map((n) => ({
        name: n, filled: rows.filter((r) => r[n]).length, samples: [...new Set(rows.map((r) => r[n]).filter(Boolean))].slice(0, 3), used_by: [],
        role: n === 'EmployeeId' ? 'external_id' : (n === 'Status' ? 'status' : undefined),
      }));
      return { ok: true, error: '', count: rows.length, id_field: 'EmployeeId', status_field: 'Status', fields, references: refs().map((r) => ({ ...r, missing: [], suggestions: {} })) };
    }
  }
  if (method === 'POST' && path === '/templates/preview') {
    need('admin');
    try {
      // The mock has no random(): a password preview strips it and reports the check like the server would.
      if (body.kind === 'password') {
        const out = applyTemplate(String(body.template || '').replace(/\{\{\s*random\w*\s+\d+\s*\}\}/g, 'Xy7#'), body.record || {});
        return { ok: true, output: out, error: '', check: out.length < 8 ? `the initial password is ${out.length} characters; at least 8 are needed` : '' };
      }
      return { ok: true, output: applyTemplate(body.template, body.record || {}), error: '' };
    }
    catch (e) {
      return { ok: false, output: '', error: e.message };
    }
  }
  if (method === 'GET' && path === '/ad/groups') {
    need('admin');
    return adGroups(params);
  }
  if (method === 'POST' && path === '/preview/impact') {
    need('admin');
    const res = impact(body || {});
    audit(db.session.user.username, 'preview.impact', 'preview', 'impact', { q: body.q || '' });
    return res;
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
  if (method === 'POST' && path === '/notifications/test') {
    need('admin');
    const n = (body && body.notifications) || {};
    if (!Array.isArray(n.to) || n.to.length === 0) {
      return { ok: false, error: 'notifications.to needs at least one recipient' };
    }
    return { ok: true };
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
  if (path === '/backups') {
    need('admin');
    db.backups = db.backups || [];
    if (method === 'POST') {
      if (!db.settings.backup || !db.settings.backup.passphrase) {
        throw new HttpError(400, 'bad_request', 'set a backup passphrase in Settings -> Backups first');
      }
      const f = { name: `IDLifecycle_MOCK_${Date.now()}.idlbak`, size: 24576, created: now() };
      db.backups.unshift(f);
      audit(db.session.user.username, 'backup.create', 'backup', f.name, { size: f.size });
      return f;
    }
    const b = db.settings.backup || {};
    return { folder: 'C:\\ProgramData\\IDLifecycle\\backups', files: db.backups, enabled: !!b.enabled, interval_hours: b.interval_hours || 24, keep: b.keep || 14, passphrase_set: !!b.passphrase, next_due: b.enabled ? now() : null };
  }
  if (method === 'GET' && path === '/audit/verify') {
    need('viewer');
    return { ok: true, checked: db.audit.length, first_bad_seq: 0 };
  }
  if (method === 'GET' && path === '/audit') {
    need('viewer');
    // A small subset of the server's q syntax: plain words, actor:, action:(prefix*), level:error; since:/before: are ignored.
    const terms = (params.get('q') || '').match(/"[^"]*"|\S+/g) || [];
    const match = (a) => terms.every((raw) => {
      const t = raw.replace(/"/g, '');
      const [k, ...rest] = t.split(':');
      const v = rest.join(':');
      if (k === 'actor') { return a.actor === v; }
      if (k === 'action') { return v.endsWith('*') ? a.action.startsWith(v.slice(0, -1)) : a.action === v; }
      if (k === 'level') { return /(failed|denied|rejected|blocked)$/.test(a.action); }
      if (['since', 'before', 'after', 'until'].includes(k)) { return true; }
      return JSON.stringify(a).toLowerCase().includes(t.toLowerCase());
    });
    const items = db.audit.filter((a) => match(a) && (!params.get('actor') || a.actor.includes(params.get('actor')))
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
  r = identityRoutes(method, path, body);
  if (r !== undefined) {
    return r;
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

window.__IDL_MOCK = true;

window.fetch = async (input, init = {}) => {
  const url = new URL(typeof input === 'string' ? input : input.url, location.href);
  if (url.pathname === '/branding/brand.json') {
    // A replaced brand, as an override folder on the server would give.
    return new Response(JSON.stringify({ product_name: 'Acme Identity (mock)', accent: '#0a7d5a', custom: true }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  }
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
      return respond(e.status, { ...(e.extra || {}), error: { code: e.code, message: e.message } });
    }
    return respond(500, { error: { code: 'internal', message: `mock failure: ${e.message}` } });
  }
};
