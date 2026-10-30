// mock-api.js - in-memory stand-in for the IDLifecycle HTTP API (docs/ARCHITECTURE.md section 7).
// Importing this module replaces window.fetch for URLs under /api/. It exists only so the UI can be
// exercised (headless Edge) before/without the Go backend. Not part of the product.

const now = () => new Date().toISOString();
const ago = (mins) => new Date(Date.now() - mins * 60000).toISOString();
let seq = 1000;
const MOCK_MASK = '********';
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
    approval: { create: 'manual', update: 'auto', disable: 'manual', enable: 'manual', delete: 'manual', link: 'auto', cloud_groups: 'manual' },
    cloud: { enabled: true, environment: 'global', tenant_id: 'corp.onmicrosoft.com', client_id: '00000000-0000-0000-0000-0000000000c1', auth: 'certificate', cert_thumbprint: 'A1B2C3D4E5F60718293A4B5C6D7E8F9012345678', client_secret: '', user_match: 'sid', exchange: { enabled: false, organization: 'corp.onmicrosoft.com' } },
    undo: { enabled: true, second_person: false },
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

// Like the server's audit detail: {field: {from, to}} for every changed field; secrets only as {changed: true}.
const SECRET_FIELD = /(password|passphrase|secret|dsn|api_?key|token)/i;
function fieldChanges(before, after, prefix = '', out = {}) {
  const keys = new Set([...Object.keys(before || {}), ...Object.keys(after || {})]);
  for (const k of keys) {
    if (k === 'id' || k === 'created_at' || k === 'updated_at' || k === 'last_login_at') {
      continue;
    }
    const a = before ? before[k] : undefined;
    const b = after ? after[k] : undefined;
    const path = prefix ? `${prefix}.${k}` : k;
    const isObj = (v) => v && typeof v === 'object' && !Array.isArray(v);
    if ((isObj(a) || a === undefined) && (isObj(b) || b === undefined) && (isObj(a) || isObj(b))) {
      fieldChanges(a || {}, b || {}, path, out);
      continue;
    }
    if (JSON.stringify(a) === JSON.stringify(b)) {
      continue;
    }
    out[path] = SECRET_FIELD.test(k) ? { changed: true } : { from: a === undefined ? null : a, to: b === undefined ? null : b };
  }
  return out;
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

// A cloud group change (type cloud_groups) for John Smith: separate from his AD update.
db.changes.push({
  ...db.changes[1], id: 'c10', type: 'cloud_groups', status: 'pending', created_at: ago(20),
  payload: {
    cloud_user_id: '0b6e9a1c-1111-4222-8333-444455556666',
    cloud_add: ['6f1a2b3c-0000-4000-8000-000000000002', '6f1a2b3c-0000-4000-8000-000000000003'],
    cloud_remove: ['6f1a2b3c-0000-4000-8000-000000000004'],
    cloud_remove_all: false,
    cloud_keep: [],
  },
  summary: ['Add to Entra group Finance Team (Microsoft 365)', 'Add to Entra group SG-Cloud-Finance-Apps (security)', 'Remove from Entra group Finance Announcements (distribution list, Exchange Online)'],
});

// ---- undo: applied changes with their recorded previous state, holds, and what an undo would do.
const STAFF = 'OU=Staff,DC=corp,DC=example,DC=com';
db.holds = [
  {
    id: 'h1', identity_id: 'i2', kind: 'attribute', key: 'title', until: null, reason: 'Title set from a wrong HR record; waiting for HR to fix it', created_by: 'admin', created_at: ago(2000), change_id: 'cu0',
    suppressed: { summary: 'would set title to \'Senior Analyst\'', at: ago(60) },
  },
  // Terry left in HR, but someone manages the account by hand: the sync changes nothing for them.
  { id: 'h2', identity_id: 'i3', kind: 'all', key: '', until: null, reason: 'On extended leave; HR shows terminated by mistake', created_by: 'admin', created_at: ago(400), change_id: '', suppressed: { summary: 'would disable the account tleft', at: ago(60) } },
];
db.batches = [];
db.changes[4].applied_at = ago(55);
db.changes[4].decided_by = 'approver1';
db.changes[4].payload = { object_guid: 'guid-4', set: { department: { from: 'Sales', to: 'Finance', mode: 'sync' } }, add_groups: ['GG-Finance'], remove_groups: ['GG-Sales'] };
db.changes[4].summary = ['Set department: "Sales" -> "Finance"', 'Add to GG-Finance', 'Remove from GG-Sales'];
db.changes[4].applied_state = { before: { enabled: true, dn: `CN=Ann Lee,${STAFF}`, attributes: { department: 'Sales' }, groupsAdded: ['CN=GG-Finance,OU=Groups,DC=corp,DC=example,DC=com'], groupsRemoved: ['CN=GG-Sales,OU=Groups,DC=corp,DC=example,DC=com'] }, dn: `CN=Ann Lee,${STAFF}`, sam: 'ann.lee' };
db.changes.push(
  {
    id: 'c21', run_id: 'run1', identity_id: 'i21', external_id: 'E1021', display: 'Pat Gone', type: 'disable', status: 'applied',
    payload: { object_guid: 'guid-21', move_to: 'OU=Disabled,DC=corp,DC=example,DC=com', remove_groups: ['GG-AllStaff', 'GG-Sales'], description: 'Disabled by IDLifecycle 2026-10-20' },
    summary: ['Disable account pgone', 'Move to OU=Disabled,DC=corp,DC=example,DC=com', 'Remove from GG-AllStaff, GG-Sales', 'Set description "Disabled by IDLifecycle 2026-10-20"'],
    created_at: ago(13000), decided_by: 'admin', decided_at: ago(12990), decision_note: '', applied_at: ago(12980), error: '', attempts: 1,
    applied_state: { before: { enabled: true, dn: `CN=Pat Gone,${STAFF}`, attributes: { description: 'Sales team' }, groupsRemoved: ['CN=GG-AllStaff,OU=Groups,DC=corp,DC=example,DC=com', 'CN=GG-Sales,OU=Groups,DC=corp,DC=example,DC=com'] }, dn: 'CN=Pat Gone,OU=Disabled,DC=corp,DC=example,DC=com', sam: 'pgone' },
  },
  {
    id: 'c22', run_id: 'run1', identity_id: 'i22', external_id: 'E1022', display: 'Old Leaver', type: 'delete', status: 'applied',
    payload: { object_guid: 'guid-22' }, summary: ['Delete account oleaver (disabled 30 days ago)'],
    created_at: ago(9000), decided_by: 'admin', decided_at: ago(8990), decision_note: '', applied_at: ago(8980), error: '', attempts: 1,
  },
);
db.identities.push(
  { id: 'i21', source_id: 's1', source_name: 'HR export', external_id: 'E1021', display: 'Pat Gone', active: false, state: 'disabled', ad_guid: 'guid-21', ad_dn: 'CN=Pat Gone,OU=Disabled,DC=corp,DC=example,DC=com', ad_sam: 'pgone', first_seen: ago(90000), last_seen: ago(13000), disabled_at: ago(12980), attrs: { FirstName: 'Pat', LastName: 'Gone', EmployeeId: 'E1021', Department: 'Sales', Status: 'T' } },
  { id: 'i22', source_id: 's1', source_name: 'HR export', external_id: 'E1022', display: 'Old Leaver', active: false, state: 'deleted', ad_guid: '', ad_dn: '', ad_sam: '', first_seen: ago(90000), last_seen: ago(60000), disabled_at: ago(52000), attrs: { FirstName: 'Old', LastName: 'Leaver', EmployeeId: 'E1022', Status: 'T' } },
  { id: 'i23', source_id: 's1', source_name: 'HR export', external_id: 'E1023', display: 'Sam Patel', active: true, state: 'linked', ad_guid: 'guid-23', ad_dn: `CN=Sam Patel,OU=Finance,${STAFF}`, ad_sam: 'spatel', first_seen: ago(90000), last_seen: ago(60), disabled_at: null, attrs: { FirstName: 'Sam', LastName: 'Patel', EmployeeId: 'E1023', Department: 'Finance', Title: 'Lead', Status: 'A' } },
);
// Terry left in HR (a disable waits for approval) and is Manual as a whole person (hold h2).
Object.assign(db.identities.find((i) => i.id === 'i3'), { state: 'linked', ad_guid: 'guid-3', ad_dn: `CN=Terry Left,${STAFF}`, ad_sam: 'tleft' });
// Sam Patel moved to Finance in run1: one AD update with several items (someone has since edited the title by hand)
// and one cloud group change. Both can be undone item by item.
db.changes.push(
  {
    id: 'c23', run_id: 'run1', identity_id: 'i23', external_id: 'E1023', display: 'Sam Patel', type: 'update', status: 'applied',
    payload: {
      object_guid: 'guid-23',
      set: { title: { from: 'Analyst', to: 'Lead', mode: 'sync' }, department: { from: 'Sales', to: 'Finance', mode: 'sync' }, telephoneNumber: { from: '', to: '+1 555 0100', mode: 'sync' } },
      add_groups: ['GG-Finance'], remove_groups: ['GG-Sales'], move_to: `OU=Finance,${STAFF}`,
    },
    summary: ['Set title: "Analyst" -> "Lead"', 'Set department: "Sales" -> "Finance"', 'Set telephoneNumber = "+1 555 0100"', 'Add to GG-Finance', 'Remove from GG-Sales', `Move to OU=Finance,${STAFF}`],
    created_at: ago(48), decided_by: 'approver1', decided_at: ago(47), decision_note: '', applied_at: ago(46), error: '', attempts: 1,
    applied_state: { before: { enabled: true, dn: `CN=Sam Patel,${STAFF}`, attributes: { title: 'Analyst', department: 'Sales', telephoneNumber: null }, groupsAdded: ['CN=GG-Finance,OU=Groups,DC=corp,DC=example,DC=com'], groupsRemoved: ['CN=GG-Sales,OU=Groups,DC=corp,DC=example,DC=com'] }, dn: `CN=Sam Patel,OU=Finance,${STAFF}`, sam: 'spatel' },
  },
  {
    id: 'c24', run_id: 'run1', identity_id: 'i23', external_id: 'E1023', display: 'Sam Patel', type: 'cloud_groups', status: 'applied',
    payload: { cloud_user_id: '0b6e9a1c-2323-4222-8333-444455556666', cloud_add: ['6f1a2b3c-0000-4000-8000-000000000002'], cloud_remove: ['6f1a2b3c-0000-4000-8000-000000000005'], cloud_remove_all: false, cloud_keep: [] },
    summary: ['Add to Entra group Finance Team (Microsoft 365)', 'Remove from Entra group Sales Announcements (distribution list, Exchange Online)'],
    created_at: ago(47), decided_by: 'approver1', decided_at: ago(46), decision_note: '', applied_at: ago(45), error: '', attempts: 1,
    applied_state: { before: { cloudAdded: ['6f1a2b3c-0000-4000-8000-000000000002'], cloudRemoved: ['6f1a2b3c-0000-4000-8000-000000000005'] } },
  },
);

// ---- history: where each person came from, accounts gone from AD, and the per-identity timeline.
for (const ident of db.identities) {
  const mp = (db.manual || []).find((p) => p.source_id === ident.source_id && p.external_id === ident.external_id);
  ident.origin = mp ? 'manual' : 'source';
  ident.created_by = mp ? mp.created_by : '';
}
Object.assign(db.identities.find((i) => i.id === 'i22'), {
  ad_last_known: { guid: '7d3f0c2a-5b1e-4c8f-9a6d-22aa31f0e122', dn: 'CN=Old Leaver,OU=Disabled,DC=corp,DC=example,DC=com', sam: 'oleaver', upn: 'oleaver@corp.example.com', deleted_at: ago(8980) },
});

// Console activity with before/after detail, as the server records it.
db.audit.push(
  { seq: 0, ts: ago(3000), actor: 'admin', action: 'source.update', entity_type: 'source', entity_id: 's1', detail: { name: 'HR export', remote: '10.0.0.15', changes: { 'config.path': { from: 'C:\\feeds\\hr.csv', to: 'C:\\feeds\\people.csv' }, 'options.missing_grace_runs': { from: 1, to: 2 } } } },
  { seq: 0, ts: ago(2900), actor: 'admin', action: 'settings.update', entity_type: 'settings', entity_id: 'settings', detail: { remote: '10.0.0.15', changed: ['engine.max_destructive_per_run', 'notifications.smtp.password'], changes: { 'engine.max_destructive_per_run': { from: 25, to: 10 }, 'notifications.smtp.password': { changed: true } } } },
  { seq: 0, ts: ago(2800), actor: 'admin', action: 'auth.login', entity_type: 'user', entity_id: 'u1', detail: { remote: '10.0.0.15', method: 'password' } },
  { seq: 0, ts: ago(2700), actor: 'approver1', action: 'auth.login', entity_type: 'user', entity_id: 'u2', detail: { remote: '10.0.0.22', method: 'password' } },
  { seq: 0, ts: ago(9000), actor: 'approver1', action: 'manual.create', entity_type: 'manual_person', entity_id: 'mp1', detail: { external_id: 'C-7Q2K9A', remote: '10.0.0.22', changes: { FirstName: { from: null, to: 'Maria' }, LastName: { from: null, to: 'Lopez' }, Department: { from: null, to: 'Finance' }, Company: { from: null, to: 'Contoso Staffing' } } } },
);

/** Readable lines of a change's summary for the timeline. */
const changeTitle = {
  create: 'Account created in AD', update: 'AD account updated', disable: 'Account disabled', enable: 'Account enabled',
  delete: 'Account deleted from AD', link: 'Linked to an existing AD account', cloud_groups: 'Cloud groups changed', undo: 'Undo applied',
};

/** The person's timeline the way GET /api/identities/{id}/history returns it (oldest first here; reversed on output). */
function identityEvents(ident) {
  const ev = [];
  const add = (ts, actor, action, entity_type, entity_id, title, lines, detail) => ev.push({ ts, actor, action, entity_type, entity_id, title, lines: lines || [], detail: detail || {} });
  const src = ident.source_name || ident.source_id;
  const fields = ident.attrs || {};
  if (ident.origin === 'manual') {
    add(ident.first_seen, ident.created_by || 'system', 'identity.first_seen', 'identity', ident.id, `Added in the console by ${ident.created_by} (${src})`,
      Object.entries(fields).map(([k, v]) => `${k}: ${v}`), { source: src, external_id: ident.external_id, origin: 'manual', created_by: ident.created_by, fields });
  }
  else {
    add(ident.first_seen, 'system', 'identity.first_seen', 'identity', ident.id, `First seen in ${src}`,
      Object.entries(fields).map(([k, v]) => `${k}: ${v}`), { source: src, external_id: ident.external_id, origin: 'source', fields });
  }
  if (ident.id === 'i5') {
    add(ago(66), 'system', 'identity.source_changed', 'identity', ident.id, 'HR record changed', ['Department: "Sales" → "Finance"'], { changes: { Department: { from: 'Sales', to: 'Finance' } } });
  }
  if (ident.id === 'i2') {
    // An update that was later undone, with a hold so the sync does not redo it.
    add(ago(2300), 'system', 'identity.source_changed', 'identity', ident.id, 'HR record changed', ['Title: "Analyst" → "Team Lead"'], { changes: { Title: { from: 'Analyst', to: 'Team Lead' } } });
    add(ago(2200), 'approver1', 'change.applied', 'change', 'cx2', 'AD account updated (approved by approver1)', ['Set title: "Analyst" -> "Team Lead"'], { type: 'update' });
    add(ago(2050), 'admin', 'change.undo_requested', 'change', 'cx2', 'Undo requested', ['Reason: Title set from a wrong HR record; waiting for HR to fix it', 'Set title back to "Analyst"'], { undo: 'cu0', reason: 'Title set from a wrong HR record; waiting for HR to fix it' });
    add(ago(2010), 'approver1', 'change.undone', 'change', 'cx2', 'Change undone (approved by approver1)', ['Set title back to "Analyst"'], { undo: 'cu0' });
    add(ago(2000), 'approver1', 'identity.hold_added', 'identity', ident.id, 'Hold added: attribute title', ['Until released by hand'], { kind: 'attribute', key: 'title', change: 'cu0' });
  }
  if (ident.id === 'i22') {
    // A long career: monthly HR changes, then the account was disabled and finally deleted from AD.
    for (let n = 0; n < 60; n += 1) {
      const t = 89000 - n * 600;
      add(ago(t), 'system', 'identity.source_changed', 'identity', ident.id, 'HR record changed', [`Title: "Level ${n}" → "Level ${n + 1}"`], { changes: { Title: { from: `Level ${n}`, to: `Level ${n + 1}` } } });
    }
    add(ago(89500), 'admin', 'change.applied', 'change', 'cx22a', 'Account created in AD (approved by admin)', ['Create account oleaver in OU=Staff,DC=corp,DC=example,DC=com', 'Add to GG-AllStaff', 'Set title = Level 0'], { type: 'create' });
    add(ago(52100), 'system', 'identity.inactive', 'identity', ident.id, 'Inactive: terminated in the HR record', ['Status: "A" → "T"'], { reason: 'terminated' });
    add(ago(52000), 'admin', 'change.applied', 'change', 'cx22b', 'Account disabled (approved by admin)', ['Disable account oleaver', 'Move to OU=Disabled,DC=corp,DC=example,DC=com', 'Remove from GG-AllStaff'], { type: 'disable' });
    add(ago(52000), 'system', 'identity.state', 'identity', ident.id, 'State: linked → disabled', [], { from: 'linked', to: 'disabled', change: 'cx22b' });
    add(ago(8980), 'system', 'identity.account_deleted', 'identity', ident.id, 'Account deleted from AD',
      ['Last known: oleaver (oleaver@corp.example.com)', 'CN=Old Leaver,OU=Disabled,DC=corp,DC=example,DC=com', 'objectGUID 7d3f0c2a-5b1e-4c8f-9a6d-22aa31f0e122'],
      { guid: ident.ad_last_known.guid, dn: ident.ad_last_known.dn, sam: 'oleaver', upn: 'oleaver@corp.example.com', attributes: { title: 'Level 60', department: 'Sales' } });
    add(ago(8980), 'system', 'identity.state', 'identity', ident.id, 'State: disabled → deleted', [], { from: 'disabled', to: 'deleted', change: 'c22' });
  }
  // The person's changes in their current state.
  for (const c of db.changes.filter((x) => x.identity_id === ident.id)) {
    add(c.created_at, 'system', 'change.create', 'change', c.id, `Change planned: ${c.type}`, c.summary || [], { type: c.type, status: c.status });
    if (c.decided_by && c.decided_at && c.status !== 'rejected') {
      add(c.decided_at, c.decided_by, 'change.approve', 'change', c.id, `Approved by ${c.decided_by}`, c.decision_note ? [`Note: ${c.decision_note}`] : [], {});
    }
    if (c.applied_at && (c.status === 'applied' || c.undone_by)) {
      add(c.applied_at, 'system', 'change.applied', 'change', c.id, `${changeTitle[c.type] || 'Change applied'}${c.decided_by ? ` (approved by ${c.decided_by})` : ''}`, c.summary || [], { type: c.type });
    }
    if (c.status === 'failed') {
      add(c.created_at, 'system', 'change.failed', 'change', c.id, `Change failed: ${c.type}`, [c.error || 'failed'], { error: c.error });
    }
  }
  // What people did in this session (undo requests, holds released, matching decisions ...).
  const mine = new Set(db.changes.filter((x) => x.identity_id === ident.id).map((x) => x.id));
  const titles = {
    'change.undo_requested': 'Undo requested', 'change.undone': 'Change undone', 'identity.hold_released': 'Hold released',
    'identity.link_decision': 'Matching decision', 'change.set_name': 'Username chosen', 'change.set_link': 'Account chosen for the link',
    'identity.retention_set': 'Retention policy changed',
  };
  for (const a of db.audit) {
    const forMe = (a.entity_type === 'identity' && a.entity_id === ident.id) || (a.entity_type === 'change' && mine.has(a.entity_id));
    if (forMe && titles[a.action]) {
      const d = a.detail || {};
      add(a.ts, a.actor, a.action, a.entity_type, a.entity_id, titles[a.action], [d.reason ? `Reason: ${d.reason}` : '', d.kind ? `${d.kind}${d.key ? `: ${d.key}` : ''}` : ''].filter(Boolean), d);
    }
  }
  ev.sort((x, y) => (x.ts < y.ts ? -1 : (x.ts > y.ts ? 1 : 0)));
  return ev.map((e, i) => ({ seq: i + 1, ...e })).reverse();
}

function historyRoutes(method, path, params) {
  const m = path.match(/^\/identities\/([^/]+)\/history$/);
  if (!m || method !== 'GET') {
    return undefined;
  }
  need('viewer');
  const ident = db.identities.find((x) => x.id === m[1]);
  if (!ident) {
    throw new HttpError(404, 'not_found', 'Identity not found.');
  }
  const limit = Math.min(Math.max(Number(params.get('limit')) || 50, 1), 200);
  const before = Number(params.get('before_seq')) || 0;
  const all = identityEvents(ident).filter((e) => !before || e.seq < before);
  const items = all.slice(0, limit);
  return { items, next_before_seq: all.length > limit ? items[items.length - 1].seq : null };
}

// What the server's PlanUndo + dry run would say for changes undone as a whole, per original change id.
const UNDO_PLANS = {
  c21: {
    script: 'enable', confirm_text: 'pgone',
    drift: ['description is \'Left 2026-09 - kept for audit\', expected \'Disabled by IDLifecycle 2026-10-20\'', 'the account is in OU=Archive,DC=corp,DC=example,DC=com, expected OU=Disabled,DC=corp,DC=example,DC=com'],
    steps: ['Enable the account pgone', `Move back to ${STAFF}`, 'Add back to GG-AllStaff, GG-Sales', 'Set description back to "Sales team"'],
    dry_message: '',
    holds: [{ kind: 'state', key: '', label: 'Keep the account state Manual: no disable, enable, create, link or delete for this person', default: true }],
  },
};

// Changes whose items can be undone one by one: what each item wrote, what the undo restores, and a value changed
// by hand since (drift). Keys as the server names them.
const UNDO_ITEMS = {
  c5: {
    sam: 'ann.lee', script: 'set',
    items: [
      { key: 'attribute:department', from: 'Finance', to: 'Sales' },
      { key: 'group_add:GG-Finance' },
      { key: 'group_remove:GG-Sales' },
    ],
  },
  c23: {
    sam: 'spatel', script: 'set',
    items: [
      { key: 'attribute:title', from: 'Lead', to: 'Analyst', now: 'Manager' },
      { key: 'attribute:department', from: 'Finance', to: 'Sales' },
      { key: 'attribute:telephoneNumber', from: '+1 555 0100', to: null },
      { key: 'group_add:GG-Finance' },
      { key: 'group_remove:GG-Sales' },
      { key: 'ou', from: `OU=Finance,${STAFF}`, to: STAFF },
    ],
  },
  c24: {
    sam: 'spatel', script: 'cloud',
    items: [
      { key: 'cloud_add:6f1a2b3c-0000-4000-8000-000000000002', name: 'Finance Team' },
      { key: 'cloud_remove:6f1a2b3c-0000-4000-8000-000000000005', name: 'Sales Announcements' },
    ],
  },
};

const itemKind = (key) => key.split(':')[0];
const itemName = (key) => key.slice(key.indexOf(':') + 1);
const qv = (v) => (v === null || v === undefined || v === '' ? 'empty' : `'${v}'`);

function itemText(it) {
  const k = itemKind(it.key);
  const n = it.name || itemName(it.key);
  switch (k) {
    case 'attribute': return { label: `${n}: ${qv(it.from)} → back to ${qv(it.to)}`, step: it.to === null ? `Clear ${n} (now ${qv(it.now || it.from)})` : `Set ${n} back to ${qv(it.to)} (now ${qv(it.now || it.from)})` };
    case 'group_add': return { label: `Added to ${n} → remove again`, step: `Remove from group ${n}` };
    case 'group_remove': return { label: `Removed from ${n} → add back`, step: `Add back to group ${n}` };
    case 'ou': return { label: `Moved to ${it.from} → move back to ${it.to}`, step: `Move back to ${it.to}` };
    case 'cloud_add': return { label: `Added to Entra group ${n} → remove again`, step: `Remove from Entra group ${n}` };
    case 'cloud_remove': return { label: `Removed from Entra group ${n} → add back`, step: `Add back to Entra group ${n}` };
    default: return { label: it.key, step: it.key };
  }
}

function itemHold(it) {
  const k = itemKind(it.key);
  const n = it.name || itemName(it.key);
  if (k === 'attribute') {
    return { kind: 'attribute', key: n, label: `Keep ${n} Manual for this person`, default: true };
  }
  if (k === 'group_add' || k === 'group_remove') {
    return { kind: 'group', key: n, label: `Keep the group ${n} Manual for this person`, default: true };
  }
  if (k === 'ou') {
    return { kind: 'ou', key: '', label: 'Keep the OU Manual for this person', default: true };
  }
  return { kind: 'cloud_group', key: itemName(it.key), label: `Keep the Entra group ${n} Manual for this person`, default: true };
}

const itemDrift = (it) => (it.now !== undefined ? `${itemName(it.key)} is ${qv(it.now)}, expected ${qv(it.from)}` : '');

const activeHolds = (identityId) => db.holds.filter((x) => x.identity_id === identityId && (!x.until || new Date(x.until).getTime() > Date.now()));

function hasPin(identityId, hold) {
  return activeHolds(identityId).some((x) => x.kind === hold.kind && (x.key || '').toLowerCase() === (hold.key || '').toLowerCase());
}

/** Every item of a change the way GET /changes/{id}/undo lists them. */
function itemsOf(c) {
  const im = UNDO_ITEMS[c.id];
  if (!im) {
    return [];
  }
  const undone = c.undone_items || {};
  const pending = c.pending_items || {};
  return im.items.map((it) => {
    const t = itemText(it);
    const out = {
      key: it.key, kind: itemKind(it.key), label: t.label, from: it.from === undefined ? null : it.from, to: it.to === undefined ? null : it.to,
      selectable: !undone[it.key] && !pending[it.key], manual: hasPin(c.identity_id, itemHold(it)),
    };
    if (undone[it.key]) {
      out.undone_by = undone[it.key];
    }
    if (pending[it.key]) {
      out.pending_undo = pending[it.key];
    }
    return out;
  });
}

/** Steps, drift and Manual offers for the chosen items (or the whole change). */
function buildPlan(c, keys) {
  const im = UNDO_ITEMS[c.id];
  if (!im) {
    const plan = UNDO_PLANS[c.id];
    return plan ? { ...plan } : null;
  }
  const chosen = im.items.filter((it) => keys.includes(it.key));
  const holds = [];
  for (const it of chosen) {
    const hd = itemHold(it);
    if (!holds.some((x) => x.kind === hd.kind && x.key === hd.key)) {
      holds.push(hd);
    }
  }
  return {
    script: im.script, confirm_text: im.sam,
    steps: chosen.map((it) => itemText(it).step),
    drift: chosen.map(itemDrift).filter(Boolean),
    dry_message: chosen.map((it) => {
      const st = itemText(it).step;
      return `would ${st[0].toLowerCase()}${st.slice(1)}`;
    }).join('; '),
    holds,
  };
}

/** The undo preview for a change; keys = the chosen items (null = everything still undoable). Throws 400 like the server. */
function undoPreview(c, keys) {
  const no = (reason, extra = {}) => ({ undoable: false, reason, change: c, ...extra });
  const cfg = db.settings.undo || {};
  if (cfg.enabled === false) {
    return no('Undo is switched off in Settings.');
  }
  if (c.status !== 'applied') {
    return no(`Only applied changes can be undone; this one is ${c.status}.`);
  }
  if (c.type === 'delete') {
    return no('Deleted accounts can only be restored from the AD Recycle Bin (Restore-ADObject); IDLifecycle does not restore them.');
  }
  if (c.undone_by) {
    return no(`Already undone by the change ${c.undone_by}.`, { items: itemsOf(c) });
  }
  if (db.settings.engine.dry_run) {
    return no('Turn off dry run first: nothing is applied in dry run.');
  }
  const approvalMode = db.settings.approval[c.type] || 'manual';
  const warnings = [];
  const im = UNDO_ITEMS[c.id];
  let sel;
  let items = [];
  if (im) {
    items = itemsOf(c);
    const remaining = items.filter((i) => i.selectable).map((i) => i.key);
    if (remaining.length === 0) {
      return no('Every item of this change is already undone or has an undo waiting.', { items });
    }
    if (keys && keys.length) {
      for (const k of keys) {
        const it = items.find((i) => i.key === k);
        if (!it) {
          throw new HttpError(400, 'bad_request', `The change has no item "${k}".`);
        }
        if (!it.selectable) {
          throw new HttpError(400, 'bad_request', `The item "${k}" is already undone or has an undo waiting.`);
        }
      }
      sel = [...new Set(keys)];
    }
    else {
      sel = remaining;
    }
  }
  else {
    if (keys && keys.length && !(keys.length === 1 && keys[0] === '*')) {
      throw new HttpError(400, 'bad_request', `A ${c.type} change is undone as a whole; items cannot be chosen.`);
    }
    if (c.undo_pending) {
      return no(`An undo for this change is already waiting: ${c.undo_pending}.`);
    }
  }
  const plan = buildPlan(c, sel || []);
  if (!c.applied_state || !plan) {
    return no(c.type === 'undo' ? 'The mock API does not simulate undoing an undo.' : 'This change was applied before undo was available; there is no record of the previous values.');
  }
  if (plan.drift.length > 0) {
    warnings.push('Later changes would be overwritten (listed above).');
  }
  warnings.push(`Without Manual items, the next sync will plan this change again if the source data and rules still ask for it (it would then ${approvalMode === 'auto' ? `be applied automatically, because approval for ${c.type} is auto` : 'wait for approval'}).`);
  return {
    undoable: true, reason: '', change: c, script: plan.script, steps: plan.steps,
    dry_run: plan.drift.length ? { status: 'failed', message: '', error: `drift: ${plan.drift.join('; ')}` } : { status: 'would', message: plan.dry_message, error: '' },
    drift: plan.drift, holds: plan.holds, confirm_text: plan.confirm_text,
    approval: cfg.second_person ? 'second_person' : 'manual', warnings,
    preview_hash: `mock-${c.id}-${(sel || ['*']).join(',')}-${plan.drift.length}`,
    items, selected: sel || [],
  };
}

function refreshPending(c) {
  const ids = Object.values(c.pending_items || {});
  if (ids.length > 0) {
    c.undo_pending = ids[0];
  }
  else {
    delete c.undo_pending;
  }
}

/** Creates the pending undo change for a validated preview. */
function newUndoChange(c, pv, { reason, force, holds, me, batch }) {
  const items = UNDO_ITEMS[c.id] ? pv.selected : [];
  const u = {
    id: newId('cu'), run_id: '', identity_id: c.identity_id, external_id: c.external_id, display: c.display, type: 'undo', status: 'pending',
    payload: { undo: { of: c.id, of_type: c.type, script: pv.script, force: !!force, reason, holds: holds || [], requested_by: me, items, batch: batch || '' } },
    summary: [`UNDO of ${c.type} applied ${String(c.applied_at || '').slice(0, 10)} (requested by ${me}): ${reason}`, ...pv.steps],
    undo_of: c.id, created_at: now(), decided_by: '', decided_at: null, decision_note: '', applied_at: null, error: '', attempts: 0,
  };
  db.changes.push(u);
  if (items.length) {
    c.pending_items = { ...(c.pending_items || {}) };
    for (const k of items) {
      c.pending_items[k] = u.id;
    }
    refreshPending(c);
  }
  else {
    c.undo_pending = u.id;
  }
  audit(me, 'change.undo_requested', 'change', c.id, { undo: u.id, reason, force: !!force, items, batch: batch || undefined });
  return u;
}

/** The mock "executor": an approved undo is applied at once. */
function applyUndo(u, me, note) {
  decide(u, 'approved', me, note);
  u.status = 'applied';
  u.applied_at = now();
  const orig = db.changes.find((x) => x.id === u.undo_of);
  const keys = u.payload.undo.items || [];
  if (orig) {
    if (keys.length) {
      orig.undone_items = { ...(orig.undone_items || {}) };
      for (const k of keys) {
        orig.undone_items[k] = u.id;
        if (orig.pending_items) {
          delete orig.pending_items[k];
        }
      }
      refreshPending(orig);
      const im = UNDO_ITEMS[orig.id];
      if (im && im.items.every((it) => orig.undone_items[it.key])) {
        orig.undone_by = u.id;
      }
    }
    else {
      delete orig.undo_pending;
      orig.undone_by = u.id;
    }
  }
  for (const hd of u.payload.undo.holds || []) {
    if (!hasPin(u.identity_id, hd)) {
      db.holds.push({ id: newId('h'), identity_id: u.identity_id, kind: hd.kind, key: hd.key || '', until: hd.until || null, reason: u.payload.undo.reason, created_by: u.payload.undo.requested_by, created_at: now(), change_id: u.id });
    }
  }
  const ident = db.identities.find((i) => i.id === u.identity_id);
  if (ident && u.payload.undo.of_type === 'disable') {
    ident.state = 'linked';
    ident.disabled_at = null;
  }
  audit(me, 'change.undone', 'change', u.undo_of, { undo: u.id, items: keys });
}

function forgetPending(u) {
  const orig = db.changes.find((x) => x.id === u.undo_of);
  if (!orig) {
    return;
  }
  for (const k of u.payload.undo.items || []) {
    if (orig.pending_items) {
      delete orig.pending_items[k];
    }
  }
  if ((u.payload.undo.items || []).length) {
    refreshPending(orig);
  }
  else {
    delete orig.undo_pending;
  }
}

function checkReason(r) {
  if (String(r || '').trim().length < 10) {
    throw new HttpError(400, 'bad_request', 'A reason of at least 10 characters is required.');
  }
}

/** What GET /runs/{id}/undo/preview and POST /runs/{id}/undo both compute from a selection. */
function runPreview(run, selection) {
  if (!selection || typeof selection !== 'object' || Object.keys(selection).length === 0) {
    throw new HttpError(400, 'bad_request', 'Select at least one change.');
  }
  if (Object.keys(selection).length > 200) {
    throw new HttpError(400, 'bad_request', 'At most 200 changes can be undone in one batch.');
  }
  const entries = [];
  for (const [cid, keys] of Object.entries(selection)) {
    const c = db.changes.find((x) => x.id === cid && x.run_id === run.id && x.status === 'applied');
    if (!c) {
      throw new HttpError(400, 'bad_request', `The change ${cid} is not an applied change of this run.`);
    }
    const whole = !Array.isArray(keys) || keys.length === 0 || (keys.length === 1 && keys[0] === '*');
    const pv = undoPreview(c, whole ? null : keys);
    if (!pv.undoable) {
      throw new HttpError(409, 'conflict', `${c.display}: ${pv.reason}`);
    }
    entries.push({ c, pv });
  }
  const driftCount = entries.filter((e) => e.pv.drift.length > 0).length;
  const people = new Set(entries.map((e) => e.c.identity_id)).size;
  const warnings = [];
  if (driftCount) {
    warnings.push(`Later changes would be overwritten for ${driftCount} of these changes (listed in red).`);
  }
  const stable = JSON.stringify(Object.keys(selection).sort().map((k) => [k, [...(selection[k] || [])].sort()]));
  return {
    entries,
    out: {
      people, count: entries.length,
      changes: entries.map(({ c, pv }) => ({
        change_id: c.id, display: c.display, external_id: c.external_id, identity_id: c.identity_id, type: c.type,
        confirm_text: pv.confirm_text, steps: pv.steps, dry_run: pv.dry_run, drift: pv.drift, holds: pv.holds, items: pv.selected,
      })),
      drift_count: driftCount, warnings, confirm_text: `UNDO ${entries.length}`,
      preview_hash: `mockrun-${run.id}-${stable}-${driftCount}`,
    },
  };
}

function batchView(b) {
  const members = db.changes.filter((x) => x.type === 'undo' && x.payload.undo.batch === b.id);
  const pending = members.filter((x) => x.status === 'pending');
  const changes = pending.map((u) => {
    const orig = db.changes.find((x) => x.id === u.undo_of);
    const plan = (orig && buildPlan(orig, u.payload.undo.items || [])) || { steps: [], drift: [], confirm_text: '', dry_message: '' };
    const forced = u.payload.undo.force;
    return {
      change_id: u.id, change: u, display: u.display, external_id: u.external_id, steps: plan.steps,
      dry_run: plan.drift.length && !forced ? { status: 'failed', message: '', error: `drift: ${plan.drift.join('; ')}` } : { status: 'would', message: forced ? 'expect check skipped (force)' : plan.dry_message, error: '' },
      drift: forced ? [] : plan.drift, confirm_text: plan.confirm_text,
    };
  });
  return {
    batch: b.id, run_id: b.run_id, requested_by: b.requested_by, reason: b.reason, created_at: b.created_at, force: b.force,
    total: members.length, pending: pending.length, changes,
    drift_count: changes.filter((x) => x.drift.length > 0).length, confirm_text: `UNDO ${pending.length}`,
  };
}

const HOLD_KIND_SET = new Set(['attribute', 'group', 'cloud_group', 'ou', 'state', 'all']);

/** What IDLifecycle manages for a person, the way GET /identities/{id}/managed returns it. */
function managedOf(ident) {
  const holds = activeHolds(ident.id);
  const pin = (kind, key = '') => holds.find((x) => x.kind === kind && (x.key || '').toLowerCase() === key.toLowerCase()) || null;
  const all = pin('all');
  const linked = Boolean(ident.ad_guid);
  const a = ident.attrs || {};
  const dept = a.Department || 'Finance';
  const special = {
    i2: { title: ['Analyst', 'Senior Analyst'] },
    i23: { title: ['Manager', 'Lead'], department: ['Finance', 'Finance'], telephoneNumber: ['+1 555 0100', '+1 555 0100'] },
  }[ident.id] || {};
  const attr = (name, ad, desired) => {
    const [sad, sdes] = special[name] || [ad, desired];
    const hold = pin('attribute', name);
    return { name, ad_value: linked ? sad : '', desired: sdes, mode: hold || all ? 'manual' : 'auto', hold: hold || null };
  };
  const group = (g, member, desired) => {
    const hold = pin('group', g);
    return { group: g, member: linked && member, desired, mode: hold || all ? 'manual' : 'auto', hold: hold || null };
  };
  const ouHold = pin('ou');
  const parent = ident.ad_dn ? ident.ad_dn.slice(ident.ad_dn.indexOf(',') + 1) : '';
  return {
    attributes: [
      attr('displayName', `${a.FirstName || ''} ${a.LastName || ''}`.trim(), `${a.FirstName || ''} ${a.LastName || ''}`.trim()),
      attr('title', a.Title || 'Analyst', a.Title || 'Analyst'),
      attr('department', dept, dept),
      attr('telephoneNumber', '', ''),
    ],
    groups: [
      group('GG-AllStaff', true, ident.active),
      group(`GG-${dept}`, true, true),
      ...(ident.id === 'i23' ? [group('GG-Sales', false, false)] : []),
    ],
    ou: { current: parent, desired: ident.id === 'i23' ? `OU=Finance,${STAFF}` : STAFF, mode: ouHold || all ? 'manual' : 'auto', hold: ouHold },
    person: { mode: all ? 'manual' : (pin('state') ? 'state' : 'auto'), hold: all || pin('state') },
  };
}

function undoRoutes(method, path, params, body) {
  let m;
  if ((m = path.match(/^\/changes\/([^/]+)\/undo$/))) {
    need('admin');
    const c = db.changes.find((x) => x.id === m[1]);
    if (!c) {
      throw new HttpError(404, 'not_found', 'Change not found.');
    }
    if (method === 'GET') {
      // Like the server: ?items= is a JSON array (group DNs contain commas), or repeated ?item=.
      let keys = params.getAll('item');
      if (params.get('items')) {
        keys = JSON.parse(params.get('items'));
      }
      return undoPreview(c, keys.length ? keys : null);
    }
    if (method !== 'POST') {
      return undefined;
    }
    const keys = Array.isArray(body.items) ? body.items : [];
    const pv = undoPreview(c, keys.length ? keys : null);
    if (!pv.undoable) {
      throw new HttpError(409, 'conflict', pv.reason);
    }
    if ((body.preview_hash || '') !== pv.preview_hash) {
      throw new HttpError(409, 'conflict', 'The undo changed since you previewed it; preview again.');
    }
    if (pv.drift.length > 0 && !body.force) {
      throw new HttpError(409, 'conflict', 'Later changes would be overwritten; preview again and confirm overwriting.');
    }
    if (String(body.confirm || '').trim().toLowerCase() !== pv.confirm_text.toLowerCase()) {
      throw new HttpError(400, 'bad_request', `Type ${pv.confirm_text} to confirm.`);
    }
    checkReason(body.reason);
    const offered = new Set(pv.holds.map((x) => `${x.kind}|${x.key || ''}`));
    for (const hd of (Array.isArray(body.holds) ? body.holds : [])) {
      if (!offered.has(`${hd.kind}|${hd.key || ''}`)) {
        throw new HttpError(400, 'bad_request', `Manual "${hd.kind} ${hd.key || ''}" was not offered for this undo.`);
      }
    }
    return newUndoChange(c, pv, { reason: String(body.reason).trim(), force: body.force, holds: body.holds, me: db.session.user.username });
  }
  if ((m = path.match(/^\/changes\/([^/]+)\/undo-check$/)) && method === 'GET') {
    need('approver');
    const u = db.changes.find((x) => x.id === m[1]);
    if (!u || u.type !== 'undo') {
      throw new HttpError(404, 'not_found', 'Not an undo change.');
    }
    const orig = db.changes.find((x) => x.id === u.undo_of);
    const plan = (orig && buildPlan(orig, u.payload.undo.items || [])) || { drift: [], steps: [], confirm_text: '', dry_message: '' };
    const forced = u.payload.undo.force;
    return {
      dry_run: plan.drift.length && !forced ? { status: 'failed', message: '', error: `drift: ${plan.drift.join('; ')}` } : { status: 'would', message: forced ? 'expect check skipped (force); would apply every step' : plan.dry_message, error: '' },
      drift: forced ? [] : plan.drift, steps: plan.steps, confirm_text: plan.confirm_text,
    };
  }
  if ((m = path.match(/^\/changes\/([^/]+)\/(approve|reject)$/)) && method === 'POST') {
    const u = db.changes.find((x) => x.id === m[1]);
    if (!u || u.type !== 'undo') {
      return undefined;
    }
    need('approver');
    const me = db.session.user.username;
    if (m[2] === 'reject') {
      if (!(body.note || '').trim()) {
        throw new HttpError(400, 'note_required', 'A note is required to reject.');
      }
      decide(u, 'rejected', me, body.note);
      forgetPending(u);
      return u;
    }
    const orig = db.changes.find((x) => x.id === u.undo_of);
    const plan = (orig && buildPlan(orig, u.payload.undo.items || [])) || { confirm_text: '' };
    if (String(body.confirm || '').trim().toLowerCase() !== String(plan.confirm_text).toLowerCase() || !plan.confirm_text) {
      throw new HttpError(400, 'bad_request', `Type ${plan.confirm_text} to approve this undo.`);
    }
    if (db.settings.undo && db.settings.undo.second_person && u.payload.undo.requested_by === me) {
      throw new HttpError(403, 'forbidden', 'Another person must approve an undo you requested.');
    }
    applyUndo(u, me, body.note);
    return u;
  }
  // ---- several changes of one run, requested and approved as a batch
  if ((m = path.match(/^\/runs\/([^/]+)\/undo(\/preview)?$/))) {
    need('admin');
    const run = db.runs.find((x) => x.id === m[1]);
    if (!run) {
      throw new HttpError(404, 'not_found', 'Run not found.');
    }
    if (method === 'GET' && !m[2]) {
      const applied = db.changes.filter((c) => c.run_id === run.id && c.status === 'applied')
        .sort((a, b) => String(a.applied_at || '').localeCompare(String(b.applied_at || '')));
      return {
        run,
        changes: applied.map((c) => {
          const pv = undoPreview(c, null);
          return {
            change: { id: c.id, type: c.type, display: c.display, external_id: c.external_id, identity_id: c.identity_id, applied_at: c.applied_at, summary: c.summary, decided_by: c.decided_by },
            undoable: pv.undoable, reason: pv.reason || '', whole_only: !UNDO_ITEMS[c.id], items: pv.items || itemsOf(c),
          };
        }),
      };
    }
    if (method !== 'POST') {
      return undefined;
    }
    const { entries, out } = runPreview(run, body.selection);
    if (m[2]) {
      return out;
    }
    if ((body.preview_hash || '') !== out.preview_hash) {
      throw new HttpError(409, 'conflict', 'The selection or the accounts changed since you previewed; preview again.');
    }
    if (out.drift_count > 0 && !body.force) {
      throw new HttpError(409, 'conflict', 'Later changes would be overwritten; preview again and confirm overwriting.');
    }
    if (String(body.confirm || '').trim().replace(/\s+/g, ' ').toUpperCase() !== out.confirm_text) {
      throw new HttpError(400, 'bad_request', `Type ${out.confirm_text} to confirm.`);
    }
    checkReason(body.reason);
    const me = db.session.user.username;
    const reason = String(body.reason).trim();
    const batch = newId('ub');
    db.batches = db.batches || [];
    db.batches.push({ id: batch, run_id: run.id, requested_by: me, reason, created_at: now(), force: !!body.force });
    const created = entries.map(({ c, pv }) => newUndoChange(c, pv, {
      reason, force: body.force, me, batch,
      holds: body.manual === false ? [] : pv.holds.map((x) => ({ kind: x.kind, key: x.key || '' })),
    }));
    audit(me, 'undo.batch_requested', 'run', run.id, { batch, changes: created.map((x) => x.id), reason });
    return { batch, changes: created };
  }
  if ((m = path.match(/^\/undo-batches\/([^/]+)(\/(approve|reject))?$/))) {
    need('approver');
    const b = (db.batches || []).find((x) => x.id === m[1]);
    if (!b) {
      throw new HttpError(404, 'not_found', 'Batch not found.');
    }
    if (method === 'GET' && !m[2]) {
      return batchView(b);
    }
    if (method !== 'POST' || !m[3]) {
      return undefined;
    }
    const me = db.session.user.username;
    const view = batchView(b);
    const pending = view.changes.map((x) => x.change);
    if (pending.length === 0) {
      throw new HttpError(409, 'conflict', 'Nothing of this batch is waiting any more.');
    }
    if (m[3] === 'reject') {
      if (!(body.note || '').trim()) {
        throw new HttpError(400, 'note_required', 'A note is required to reject.');
      }
      for (const u of pending) {
        decide(u, 'rejected', me, body.note);
        forgetPending(u);
      }
      audit(me, 'undo.batch_rejected', 'run', b.run_id, { batch: b.id, note: body.note });
      return { batch: b.id, rejected: pending.length };
    }
    if (db.settings.undo && db.settings.undo.second_person && b.requested_by === me) {
      throw new HttpError(403, 'forbidden', 'Another person must approve a batch you requested.');
    }
    if (view.drift_count > 0) {
      throw new HttpError(409, 'conflict', `${view.drift_count} undo(s) of this batch now find later changes; approve or reject those one at a time.`);
    }
    if (String(body.confirm || '').trim().replace(/\s+/g, ' ').toUpperCase() !== view.confirm_text) {
      throw new HttpError(400, 'bad_request', `Type ${view.confirm_text} to approve this batch.`);
    }
    for (const u of pending) {
      applyUndo(u, me, body.note);
    }
    audit(me, 'undo.batch_approved', 'run', b.run_id, { batch: b.id, approved: pending.length });
    return { batch: b.id, approved: pending.length };
  }
  // ---- Manual items (holds): set by hand, set back to Auto, and what is managed
  if ((m = path.match(/^\/identities\/([^/]+)\/holds$/)) && method === 'POST') {
    need('approver');
    const ident = db.identities.find((x) => x.id === m[1]);
    if (!ident) {
      throw new HttpError(404, 'not_found', 'Identity not found.');
    }
    const kind = String(body.kind || '');
    const key = String(body.key || '').trim();
    if (!HOLD_KIND_SET.has(kind)) {
      throw new HttpError(400, 'bad_request', `Unknown kind "${kind}".`);
    }
    if (['attribute', 'group', 'cloud_group'].includes(kind) && !key) {
      throw new HttpError(400, 'bad_request', `A ${kind} needs a key.`);
    }
    if (kind === 'attribute' && !/^[A-Za-z][A-Za-z0-9-]*$/.test(key)) {
      throw new HttpError(400, 'bad_request', `"${key}" is not an LDAP attribute name.`);
    }
    if (['ou', 'state', 'all'].includes(kind) && key) {
      throw new HttpError(400, 'bad_request', `A ${kind} item has no key.`);
    }
    checkReason(body.reason);
    if (body.until && new Date(body.until).getTime() <= Date.now()) {
      throw new HttpError(400, 'bad_request', 'The end date is in the past.');
    }
    if (hasPin(ident.id, { kind, key })) {
      throw new HttpError(409, 'conflict', 'That item is already Manual.');
    }
    const hd = { id: newId('h'), identity_id: ident.id, kind, key, until: body.until || null, reason: String(body.reason).trim(), created_by: db.session.user.username, created_at: now(), change_id: '' };
    db.holds.push(hd);
    audit(db.session.user.username, 'identity.hold_added', 'identity', ident.id, { kind, key, manual: true, reason: hd.reason });
    return hd;
  }
  if ((m = path.match(/^\/identities\/([^/]+)\/holds\/([^/]+)$/)) && method === 'DELETE') {
    need('approver');
    const idx = db.holds.findIndex((x) => x.id === m[2] && x.identity_id === m[1]);
    if (idx < 0) {
      throw new HttpError(404, 'not_found', 'Hold not found.');
    }
    const [hd] = db.holds.splice(idx, 1);
    audit(db.session.user.username, 'identity.hold_released', 'identity', m[1], { kind: hd.kind, key: hd.key });
    return null;
  }
  if ((m = path.match(/^\/identities\/([^/]+)\/managed$/)) && method === 'GET') {
    need('viewer');
    const i = db.identities.find((x) => x.id === m[1]);
    if (!i) {
      throw new HttpError(404, 'not_found', 'Identity not found.');
    }
    return managedOf(i);
  }
  if ((m = path.match(/^\/identities\/([^/]+)$/)) && method === 'GET') {
    need('viewer');
    const i = db.identities.find((x) => x.id === m[1]);
    if (!i) {
      throw new HttpError(404, 'not_found', 'Identity not found.');
    }
    return { ...i, holds: db.holds.filter((x) => x.identity_id === i.id) };
  }
  return undefined;
}

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
// ---- leavers and retention (like the server's retention policies, design-retention.md)
db.retentionPolicies = [
  {
    id: 'rp1', name: 'Legal hold', description: 'Accounts under a legal hold are never deleted automatically.', enabled: true, priority: 10, match_mode: 'any',
    conditions: [{ attr: 'ad.memberOf', op: 'contains', value: 'GG-LegalHold' }],
    delete_after_days: 0, disabled_ou: 'OU=Legal Hold,OU=Disabled,DC=corp,DC=example,DC=com', extra_keep_groups: ['GG-LegalHold'], approval: 'manual',
    created_at: ago(20000), updated_at: ago(20000),
  },
  {
    id: 'rp2', name: 'Executives 365 days', description: 'Executive accounts are kept for a year.', enabled: true, priority: 20, match_mode: 'all',
    conditions: [{ attr: 'ad.memberOf', op: 'contains', value: 'GG-Executives' }],
    delete_after_days: 365, disabled_ou: '', extra_keep_groups: ['Mailbox-Retention'], approval: 'manual',
    created_at: ago(20000), updated_at: ago(20000),
  },
  {
    id: 'rp3', name: 'Contractors', description: 'People added in the console go 7 days after they are disabled.', enabled: true, priority: 30, match_mode: 'all',
    conditions: [{ attr: 'origin', op: 'eq', value: 'manual' }],
    delete_after_days: 7, disabled_ou: '', extra_keep_groups: [], approval: '',
    created_at: ago(20000), updated_at: ago(20000),
  },
];
// AD groups of the accounts (the server reads them from the snapshot).
db.adGroupsOf = { i21: ['GG-Executives', 'Mailbox-Retention'], i26: ['GG-LegalHold', 'GG-AllStaff'] };
{
  const DAY = 1440;
  const leaver = (id, ext, first, last, daysAgo, extra = {}) => ({
    id, source_id: 's1', source_name: 'HR export', external_id: ext, display: `${first} ${last}`, active: false, state: 'disabled', origin: 'source', created_by: '',
    ad_guid: `guid-${id}`, ad_dn: `CN=${first} ${last},OU=Disabled,DC=corp,DC=example,DC=com`, ad_sam: `${first[0]}${last}`.toLowerCase(),
    first_seen: ago(90000), last_seen: ago(daysAgo * DAY + 30), disabled_at: ago(daysAgo * DAY),
    attrs: { FirstName: first, LastName: last, EmployeeId: ext, Department: 'Sales', Status: 'T' },
    ...extra,
  });
  const today = new Date();
  const inDaysDate = (n) => {
    const d = new Date(today.getFullYear(), today.getMonth(), today.getDate() + n);
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  };
  db.identities.push(
    leaver('i24', 'E1024', 'Dana', 'Brooks', 26),
    leaver('i25', 'E1025', 'Lee', 'Quinn', 40),
    leaver('i26', 'E1026', 'Morgan', 'Hale', 20),
    leaver('i28', 'C-8R4T2B', 'Robin', 'Ash', 3, { source_id: 's2', source_name: 'Contractors', origin: 'manual', created_by: 'approver1', attrs: { FirstName: 'Robin', LastName: 'Ash', ExternalID: 'C-8R4T2B', Department: 'IT', Status: 'Terminated', Source: 'manual' } }),
    {
      id: 'i27', source_id: 's1', source_name: 'HR export', external_id: 'E1027', display: 'Chris Park', active: true, state: 'linked', origin: 'source', created_by: '',
      ad_guid: 'guid-i27', ad_dn: `CN=Chris Park,${STAFF}`, ad_sam: 'cpark', first_seen: ago(90000), last_seen: ago(60), disabled_at: null,
      attrs: { FirstName: 'Chris', LastName: 'Park', EmployeeId: 'E1027', Department: 'Sales', Status: 'A', TerminationDate: inDaysDate(10) },
    },
  );
  db.sources[0].options.termination_date_field = 'TerminationDate';
  db.sources[0].options.termination_date_format = 'auto';
  db.sources[0].options.termination_after_days = 0;
}

// The fields retention conditions see: the last source record plus ad.memberOf, ad.ou, ad.sam, source and origin.
function retentionRecord(ident) {
  const dn = ident.ad_dn || (ident.ad_last_known && ident.ad_last_known.dn) || '';
  return {
    ...(ident.attrs || {}),
    'ad.memberOf': (db.adGroupsOf[ident.id] || []).join(', '),
    'ad.ou': dn.replace(/^[^,]*,/, ''),
    'ad.sam': ident.ad_sam || '',
    source: ident.source_name || '',
    origin: ident.origin || 'source',
  };
}

// No conditions = only people assigned to the policy by hand.
function policyMatches(p, record, explain) {
  const conds = Array.isArray(p.conditions) ? p.conditions : [];
  if (conds.length === 0) {
    return false;
  }
  const res = conds.map((c, i) => evalCondition(c, record, `conditions[${i}]`, explain).matched);
  return p.match_mode === 'any' ? res.some(Boolean) : res.every(Boolean);
}

function parseLocalDate(v) {
  const s = String(v || '').trim();
  let m = /^(\d{4})-(\d{2})-(\d{2})/.exec(s);
  if (m) {
    return new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  }
  m = /^(\d{1,2})\/(\d{1,2})\/(\d{4})$/.exec(s);
  if (m) {
    return new Date(Number(m[3]), Number(m[1]) - 1, Number(m[2]));
  }
  return null;
}

// The termination date from the person's source (options.termination_date_field), as YYYY-MM-DD, or ''.
function leaveDateOf(ident) {
  const src = db.sources.find((s) => s.id === ident.source_id);
  const f = src && src.options && src.options.termination_date_field;
  const d = f ? parseLocalDate((ident.attrs || {})[f]) : null;
  if (!d) {
    return '';
  }
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

function resolveRetention(ident, policies) {
  const list = (policies || []).slice().sort((a, b) => Number(a.priority) - Number(b.priority));
  const dflt = { id: 'default', name: 'Default', delete_after_days: Number((db.settings.engine || {}).delete_after_days) || 0 };
  let pol = null;
  let reason = 'default';
  if (ident.retention_policy_id) {
    pol = ident.retention_policy_id === 'default' ? dflt : list.find((p) => p.id === ident.retention_policy_id) || null;
    reason = pol ? 'assigned' : 'default';
  }
  if (!pol) {
    const rec = retentionRecord(ident);
    pol = list.find((p) => p.enabled && policyMatches(p, rec, null)) || null;
    reason = pol ? 'rule' : 'default';
  }
  if (!pol) {
    pol = dflt;
  }
  const days = Number(pol.delete_after_days) || 0;
  let due = null;
  if (ident.state === 'disabled' && ident.disabled_at && days > 0) {
    due = new Date(new Date(ident.disabled_at).getTime() + days * 86400000).toISOString();
  }
  const out = { policy_id: pol.id || '', policy_name: pol.name, reason, delete_after_days: days, delete_due: due };
  if (reason === 'assigned') {
    out.assigned_by = ident.retention_assigned_by || '';
  }
  const leave = ident.active ? leaveDateOf(ident) : '';
  if (leave) {
    out.leave_date = leave;
  }
  return out;
}

function refreshRetention() {
  for (const ident of db.identities) {
    if (ident.state === 'merged') {
      delete ident.retention;
      continue;
    }
    ident.retention = resolveRetention(ident, db.retentionPolicies);
    if (ident.retention.leave_date) {
      ident.leave_date = ident.retention.leave_date;
    }
    else {
      delete ident.leave_date;
    }
  }
}

const RETENTION_OPS = new Set(['eq', 'ne', 'in', 'not_in', 'contains', 'not_contains', 'starts_with', 'ends_with', 'regex', 'not_regex', 'exists', 'empty', 'gt', 'gte', 'lt', 'lte', 'older_than_days', 'newer_than_days', 'template', 'all_of', 'any_of']);

function checkRetentionConditions(list, path = 'conditions') {
  if (!Array.isArray(list)) {
    throw new HttpError(400, 'invalid', `${path} must be a list`);
  }
  list.forEach((c, i) => {
    const p = `${path}[${i}]`;
    if (!c || !RETENTION_OPS.has(c.op)) {
      throw new HttpError(400, 'invalid', `${p}: unknown test "${c && c.op}"`);
    }
    if (c.op === 'all_of' || c.op === 'any_of') {
      if (!Array.isArray(c.conditions) || c.conditions.length === 0) {
        throw new HttpError(400, 'invalid', `${p}: a group needs at least one condition`);
      }
      checkRetentionConditions(c.conditions, `${p}.conditions`);
      return;
    }
    if (c.op !== 'template' && !String(c.attr || '').trim()) {
      throw new HttpError(400, 'invalid', `${p}: the field is required`);
    }
    if ((c.op === 'regex' || c.op === 'not_regex')) {
      try {
        new RegExp(String(c.value || '').replace(/^\(\?i\)/, '')); // eslint-disable-line no-new
      }
      catch (e) {
        throw new HttpError(400, 'invalid', `${p}: bad regex: ${e.message}`);
      }
    }
  });
}

function checkPolicy(b, existingId) {
  const name = String((b && b.name) || '').trim();
  if (!name) {
    throw new HttpError(400, 'invalid', 'name is required');
  }
  if (db.retentionPolicies.some((p) => p.id !== existingId && p.name.toLowerCase() === name.toLowerCase())) {
    throw new HttpError(409, 'duplicate', 'A retention policy with that name already exists.');
  }
  if (name.toLowerCase() === 'default') {
    throw new HttpError(400, 'invalid', '"Default" is the built-in policy; choose another name');
  }
  const days = Number(b.delete_after_days);
  if (!Number.isInteger(days) || days < 0) {
    throw new HttpError(400, 'invalid', 'delete_after_days must be a whole number of days >= 0 (0 = never delete)');
  }
  if (!Number.isInteger(Number(b.priority))) {
    throw new HttpError(400, 'invalid', 'priority must be a whole number');
  }
  if (!['all', 'any'].includes(b.match_mode || 'all')) {
    throw new HttpError(400, 'invalid', 'match_mode must be all or any');
  }
  if (!['', 'manual'].includes(b.approval || '')) {
    throw new HttpError(400, 'invalid', 'approval must be "" or "manual"');
  }
  const ou = String(b.disabled_ou || '').trim();
  if (ou && !/^(OU|CN)=[^,]+,.*DC=/i.test(ou)) {
    throw new HttpError(400, 'invalid', `disabled_ou "${ou}" is not a distinguished name (OU=...,DC=...)`);
  }
  if (b.extra_keep_groups !== undefined && (!Array.isArray(b.extra_keep_groups) || b.extra_keep_groups.some((g) => typeof g !== 'string' || !g.trim()))) {
    throw new HttpError(400, 'invalid', 'extra_keep_groups must be a list of group names');
  }
  checkRetentionConditions(b.conditions || []);
  return {
    name, description: String(b.description || ''), enabled: b.enabled !== false, priority: Number(b.priority), match_mode: b.match_mode || 'all',
    conditions: b.conditions || [], delete_after_days: days, disabled_ou: ou, extra_keep_groups: (b.extra_keep_groups || []).map((g) => g.trim()), approval: b.approval || '',
  };
}

const manualHold = (identityId) => activeHolds(identityId).some((x) => x.kind === 'all' || x.kind === 'state');

function retentionRoutes(method, path, params, body) {
  let m;
  const live = () => db.identities.filter((i) => i.state !== 'merged' && i.state !== 'deleted');
  if (path === '/retention-policies' && method === 'GET') {
    need('viewer');
    const items = db.retentionPolicies.slice().sort((a, b) => a.priority - b.priority)
      .map((p) => ({ ...p, people: live().filter((i) => i.retention && i.retention.policy_id === p.id).length }));
    return { items, total: items.length, default_people: live().filter((i) => i.retention && i.retention.policy_id === 'default').length };
  }
  if (path === '/retention-policies' && method === 'POST') {
    need('admin');
    const p = { id: newId('rp'), ...checkPolicy(body || {}, ''), created_at: now(), updated_at: now() };
    db.retentionPolicies.push(p);
    audit(db.session.user.username, 'retention_policy.create', 'retention_policy', p.id, { name: p.name, fields: fieldChanges({}, p) });
    return p;
  }
  if (path === '/retention-policies/test' && method === 'POST') {
    need('admin');
    const pol = body && body.policy;
    if (!pol || typeof pol !== 'object') {
      throw new HttpError(400, 'invalid', 'policy is required');
    }
    checkRetentionConditions(pol.conditions || []);
    let record = body.record;
    if (body.identity_id) {
      const ident = db.identities.find((i) => i.id === body.identity_id);
      if (!ident) {
        throw new HttpError(404, 'not_found', 'Identity not found.');
      }
      record = retentionRecord(ident);
    }
    if (!record || typeof record !== 'object') {
      throw new HttpError(400, 'invalid', 'identity_id or record is required');
    }
    const explain = [];
    const matched = policyMatches(pol, record, explain);
    return { matched, explain };
  }
  if ((m = path.match(/^\/retention-policies\/([^/]+)$/))) {
    if (method === 'GET') {
      need('viewer');
    }
    else {
      need('admin');
    }
    const p = db.retentionPolicies.find((x) => x.id === m[1]);
    if (!p) {
      throw new HttpError(404, 'not_found', 'Retention policy not found.');
    }
    if (method === 'GET') {
      return p;
    }
    if (method === 'PUT') {
      const old = JSON.parse(JSON.stringify(p));
      Object.assign(p, checkPolicy(body || {}, p.id), { updated_at: now() });
      audit(db.session.user.username, 'retention_policy.update', 'retention_policy', p.id, { name: p.name, changes: fieldChanges(old, p) });
      return p;
    }
    if (method === 'DELETE') {
      db.retentionPolicies.splice(db.retentionPolicies.indexOf(p), 1);
      for (const i of db.identities.filter((x) => x.retention_policy_id === p.id)) {
        i.retention_policy_id = '';
      }
      audit(db.session.user.username, 'retention_policy.delete', 'retention_policy', p.id, { name: p.name, fields: fieldChanges(p, {}) });
      return null;
    }
  }
  if (path === '/retention/upcoming' && method === 'GET') {
    need('viewer');
    const days = Number(params.get('days') || 30);
    if (!Number.isInteger(days) || days < 1 || days > 3650) {
      throw new HttpError(400, 'invalid', 'days must be 1-3650');
    }
    const kind = params.get('kind') || '';
    if (!['', 'delete', 'leaving'].includes(kind)) {
      throw new HttpError(400, 'invalid', 'kind must be delete or leaving');
    }
    const horizon = Date.now() + days * 86400000;
    const base = (i) => ({
      identity_id: i.id, display: i.display, external_id: i.external_id, ad_sam: i.ad_sam || '', disabled_at: i.disabled_at || null,
      policy_id: i.retention.policy_id, policy_name: i.retention.policy_name, reason: i.retention.reason, assigned_by: i.retention.assigned_by || '',
      manual_hold: manualHold(i.id),
    });
    const items = [];
    for (const i of live()) {
      if (!i.retention) {
        continue;
      }
      if (kind !== 'leaving' && i.state === 'disabled' && i.retention.delete_due && new Date(i.retention.delete_due).getTime() <= horizon) {
        items.push({ ...base(i), kind: 'delete', delete_due: i.retention.delete_due, sort: new Date(i.retention.delete_due).getTime() });
      }
      if (kind !== 'delete' && i.active && i.retention.leave_date) {
        const t = parseLocalDate(i.retention.leave_date).getTime();
        if (t <= horizon) {
          items.push({ ...base(i), kind: 'leaving', leave_date: i.retention.leave_date, sort: t });
        }
      }
    }
    items.sort((a, b) => a.sort - b.sort);
    return paginate(items.map(({ sort, ...rest }) => rest), params);
  }
  if ((m = path.match(/^\/identities\/([^/]+)\/retention$/)) && method === 'PUT') {
    need('approver');
    const ident = db.identities.find((i) => i.id === m[1]);
    if (!ident) {
      throw new HttpError(404, 'not_found', 'Identity not found.');
    }
    if (ident.state === 'deleted' || ident.state === 'merged') {
      throw new HttpError(409, 'conflict', `The account of this person is ${ident.state}; retention no longer applies.`);
    }
    const pid = String((body && body.policy_id) || '');
    const reason = String((body && body.reason) || '').trim();
    if (reason.length < 10) {
      throw new HttpError(400, 'invalid', 'reason must be at least 10 characters');
    }
    if (pid && pid !== 'default' && !db.retentionPolicies.some((p) => p.id === pid)) {
      throw new HttpError(400, 'invalid', `unknown retention policy "${pid}"`);
    }
    const from = ident.retention_policy_id || '';
    ident.retention_policy_id = pid;
    ident.retention_assigned_by = pid ? db.session.user.username : '';
    audit(db.session.user.username, 'identity.retention_set', 'identity', ident.id, { from, to: pid, reason });
    refreshRetention();
    return ident;
  }
  return undefined;
}

for (let i = 0; i < 30; i += 1) {
  audit(i % 3 === 0 ? 'system' : 'admin', i % 2 ? 'change.approve' : 'run.finish', i % 2 ? 'change' : 'run', `x${i}`, { note: `entry ${i}`, long: 'x'.repeat(i * 5) });
}
// Seeded rows above carry their own time: keep the log in time order with seq following it, like the hash chain.
db.audit.sort((x, y) => (x.ts < y.ts ? -1 : (x.ts > y.ts ? 1 : 0)));
db.audit.forEach((a, i) => { a.seq = i + 1; });

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


// ---- Microsoft 365 / Entra ID groups (GET /api/cloud/groups, /api/cloud/groups/resolve, POST /api/cloud/test)
const CLOUD_GROUPS = [
  { id: '6f1a2b3c-0000-4000-8000-000000000001', name: 'All Company', mail: 'allcompany@corp.example.com', kind: 'm365', description: 'Microsoft 365 group for everyone' },
  { id: '6f1a2b3c-0000-4000-8000-000000000002', name: 'Finance Team', mail: 'finance@corp.example.com', kind: 'm365', description: 'Finance Teams team' },
  { id: '6f1a2b3c-0000-4000-8000-000000000003', name: 'SG-Cloud-Finance-Apps', mail: '', kind: 'security', description: 'Access to finance SaaS apps' },
  { id: '6f1a2b3c-0000-4000-8000-000000000004', name: 'Finance Announcements', mail: 'finance-news@corp.example.com', kind: 'distribution', description: 'Finance mailing list' },
  { id: '6f1a2b3c-0000-4000-8000-000000000005', name: 'Finance Mail Security', mail: 'finance-sec@corp.example.com', kind: 'mail_security', description: 'Finance shared mailbox access' },
  { id: '6f1a2b3c-0000-4000-8000-000000000006', name: 'Finance Dynamic', mail: '', kind: 'security', description: 'Everyone in Finance (rule)', manageable: false, reason: 'dynamic' },
  { id: '6f1a2b3c-0000-4000-8000-000000000007', name: 'Finance Admins (PIM)', mail: '', kind: 'security', description: 'Role-assignable', manageable: false, reason: 'role_assignable' },
  { id: '6f1a2b3c-0000-4000-8000-000000000008', name: 'GG-Finance', mail: '', kind: 'security', description: 'Synced from AD', manageable: false, reason: 'synced' },
].map((g) => ({ ref: `entra:${g.id}`, manageable: true, reason: '', ...g }));

function cloudGroupView(g) {
  const ex = db.settings.cloud && db.settings.cloud.exchange && db.settings.cloud.exchange.enabled;
  if (g.manageable && !ex && (g.kind === 'mail_security' || g.kind === 'distribution')) {
    return { ...g, manageable: false, reason: 'exchange_off' };
  }
  return { ...g };
}

function cloudOn() {
  return Boolean(db.settings.cloud && db.settings.cloud.enabled);
}

function cloudGroups(params) {
  const q = String(params.get('q') || '').trim();
  if (q.length < 2) {
    throw new HttpError(400, 'bad_request', 'q must be at least 2 characters.');
  }
  if (!cloudOn()) {
    return { groups: [], error: 'Microsoft 365 connection is not configured' };
  }
  if (q === 'zzfail') {
    return { groups: [], error: 'Graph: Authorization_RequestDenied' };
  }
  const max = Math.min(Number(params.get('max')) || 25, 100);
  const ql = q.toLowerCase();
  const hits = CLOUD_GROUPS.filter((g) => [g.name, g.mail, g.description].some((s) => String(s).toLowerCase().includes(ql)))
    .map(cloudGroupView).sort((a, b) => a.name.localeCompare(b.name));
  return { groups: hits.slice(0, max), truncated: hits.length > max };
}

function cloudResolve(params) {
  const ids = String(params.get('ids') || '').split(',').map((s) => s.trim().toLowerCase()).filter(Boolean);
  if (ids.length > 50) {
    throw new HttpError(400, 'bad_request', 'At most 50 ids.');
  }
  return { groups: CLOUD_GROUPS.filter((g) => ids.includes(g.id)).map(cloudGroupView) };
}

function cloudTest(body) {
  const saved = db.settings.cloud || {};
  const c = body && body.cloud ? body.cloud : saved;
  const checks = [];
  const guid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
  const secretOk = c.auth === 'secret' ? Boolean(c.client_secret && (c.client_secret !== MOCK_MASK || db.cloudSecret)) : /^[0-9A-F]{40}$/i.test(c.cert_thumbprint || '');
  const signIn = Boolean(c.tenant_id) && guid.test(c.client_id || '') && secretOk;
  checks.push({ name: 'token', ok: signIn, detail: signIn ? 'Signed in as the app.' : 'AADSTS700016: application not found in the directory, or the credential is wrong.' });
  if (c.auth !== 'secret') {
    checks.push({ name: 'certificate', ok: secretOk, detail: secretOk ? 'Found in LocalMachine\My, private key readable, CSP key, expires in 712 days.' : 'No certificate with this thumbprint in LocalMachine\My.' });
  }
  checks.push({ name: 'graph_permissions', ok: signIn, detail: signIn ? 'GroupMember.ReadWrite.All, User.Read.All and Group.Read.All present.' : 'Not checked: no token.' });
  checks.push({ name: 'graph_read', ok: signIn, detail: signIn ? 'Read the organization "Contoso".' : 'Not checked: no token.' });
  if (c.exchange && c.exchange.enabled) {
    const exOk = signIn && c.auth !== 'secret' && /\./.test(c.exchange.organization || '');
    checks.push({ name: 'exchange_module', ok: true, detail: 'ExchangeOnlineManagement 3.7.1' });
    checks.push({ name: 'exchange_connect', ok: exOk, detail: exOk ? `Connected to ${c.exchange.organization}.` : 'Exchange Online needs certificate authentication and the .onmicrosoft.com organization.' });
  }
  audit(db.session.user.username, 'cloud.test', 'settings', 'cloud', { ok: checks.every((k) => k.ok) });
  return { ok: checks.every((k) => k.ok), checks };
}

/** "entra:<id>" -> "Entra: <name>" for plan lines; AD identities unchanged. */
function groupText(g) {
  const s = String(g);
  if (s.toLowerCase().startsWith('entra:')) {
    const cg = CLOUD_GROUPS.find((x) => x.id === s.slice(6).toLowerCase());
    return `Entra: ${cg ? cg.name : s.slice(6)}`;
  }
  return s;
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
        actions.push(`Add to group ${groupText(g)}`);
      }
    }
    return { type: 'disable', actions };
  }
  if (!cur) {
    const sam = `${String(ident.attrs.FirstName || '').slice(0, 1)}${ident.attrs.LastName || ''}`.toLowerCase();
    actions.push(`Create account ${sam}${desired.ou ? ` in ${desired.ou}` : ''}`);
    desired.groups.forEach((g) => actions.push(`Add to group ${groupText(g)}`));
    Object.entries(desired.attrs).forEach(([k, v]) => actions.push(`Set ${k} = "${v}"`));
    return { type: 'create', actions };
  }
  desired.groups.filter((g) => !cur.groups.includes(g)).forEach((g) => actions.push(`Add to group ${groupText(g)}`));
  if (cfg.settings.engine && cfg.settings.engine.remove_stale_groups) {
    cur.groups.filter((g) => !desired.groups.includes(g)).forEach((g) => actions.push(`Remove from group ${groupText(g)}`));
  }
  Object.entries(desired.attrs).filter(([k, v]) => cur.attrs[k] !== v).forEach(([k, v]) => actions.push(`Set ${k}: "${cur.attrs[k] || ''}" -> "${v}"`));
  return actions.length ? { type: 'update', actions } : null;
}

// With proposed retention policies: what happens to a disabled account's delete under the saved / proposed policies.
function withRetention(plan, ident, policies) {
  if (!plan || !policies || ident.state !== 'disabled') {
    return plan;
  }
  const r = resolveRetention(ident, policies);
  const sam = ident.ad_sam || ident.display;
  let line = `Keep account ${sam} (policy ${r.policy_name}: never deleted automatically)`;
  if (r.delete_due) {
    line = new Date(r.delete_due).getTime() <= Date.now()
      ? `Delete account ${sam} (due now, policy ${r.policy_name})`
      : `Delete account ${sam} on ${r.delete_due.slice(0, 10)} (policy ${r.policy_name})`;
  }
  return { ...plan, actions: [...plan.actions, line] };
}

function impact(body) {
  if (body.q === '__busy__') {
    throw new HttpError(409, 'conflict', 'A preview or sync is already running.');
  }
  const baseCfg = { roles: db.roles, mappings: db.mappings, settings: db.settings };
  const retentionOn = Array.isArray(body.retention_policies);
  const hasProposal = Array.isArray(body.roles) || Array.isArray(body.mappings) || Boolean(body.settings) || retentionOn;
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
    const base = withRetention(planFor(ident, baseCfg, baseCfg), ident, retentionOn ? db.retentionPolicies : null);
    const prop = hasProposal ? withRetention(planFor(ident, propCfg, baseCfg), ident, retentionOn ? body.retention_policies : null) : base;
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
  refreshRetention();
  const retOut = retentionRoutes(method, path, params, body);
  if (retOut !== undefined) {
    return retOut;
  }
  const histOut = historyRoutes(method, path, params);
  if (histOut !== undefined) {
    return histOut;
  }
  const undoOut = undoRoutes(method, path, params, body);
  if (undoOut !== undefined) {
    return undoOut;
  }
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
    const ent = name.replace(/s$/, ''); // like the server: source.update on entity "source"
    if (path === `/${name}`) {
      if (method === 'GET') {
        need('viewer');
        return paginate(coll(), params);
      }
      if (method === 'POST') {
        need(role);
        const item = { id: newId(name[0]), ...build(body) };
        coll().push(item);
        audit(db.session.user.username, `${ent}.create`, ent, item.id, { name: item.name || item.username || '', changes: fieldChanges({}, item) });
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
        const old = JSON.parse(JSON.stringify(item));
        Object.assign(item, build(body));
        audit(db.session.user.username, `${ent}.update`, ent, item.id, { name: item.name || item.username || '', changes: fieldChanges(old, item) });
        return item;
      }
      if (method === 'DELETE') {
        coll().splice(coll().indexOf(item), 1);
        audit(db.session.user.username, `${ent}.delete`, ent, item.id, { name: item.name || item.username || '', changes: fieldChanges(item, {}) });
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
  // Like the server: the termination date options of a source (design-retention.md section 1).
  if (/^\/sources(\/[^/]+)?$/.test(path) && (method === 'POST' || method === 'PUT') && body && body.options) {
    const o = body.options;
    const after = o.termination_after_days === undefined ? 0 : Number(o.termination_after_days);
    if (!Number.isInteger(after) || after < 0 || after > 3650) {
      throw new HttpError(400, 'invalid', 'options.termination_after_days must be a whole number 0-3650');
    }
    if (o.termination_date_format !== undefined && String(o.termination_date_format).trim() === '' && String(o.termination_date_field || '').trim() !== '') {
      throw new HttpError(400, 'invalid', 'options.termination_date_format is empty (use "auto" or a layout such as 2006-01-02)');
    }
  }
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
  if (method === 'GET' && path === '/cloud/groups') {
    need('admin');
    return cloudGroups(params);
  }
  if (method === 'GET' && path === '/cloud/groups/resolve') {
    need('admin');
    return cloudResolve(params);
  }
  if (method === 'POST' && path === '/cloud/test') {
    need('admin');
    return cloudTest(body || {});
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
      const oldSettings = JSON.parse(JSON.stringify(db.settings));
      db.settings = body;
      const settingsChanges = fieldChanges(oldSettings, body);
      // Like the server: a cloud client secret is kept aside and only ever returned masked.
      if (db.settings.cloud && db.settings.cloud.client_secret && db.settings.cloud.client_secret !== MOCK_MASK) {
        db.cloudSecret = db.settings.cloud.client_secret;
        db.settings.cloud.client_secret = MOCK_MASK;
      }
      audit(db.session.user.username, 'settings.update', 'settings', 'settings', { changed: Object.keys(settingsChanges), changes: settingsChanges });
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
      const ch = db.changes.find((c) => c.id === id);
      if (ch && ch.type === 'undo') {
        return { id, ok: false, error: 'undo changes are approved one at a time' };
      }
      try {
        decide(ch, to, db.session.user.username, body.note);
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
    // Like the server: manual_count = active Manual items; ?manual=1 keeps only people who have one.
    const rows = db.identities.map((i) => ({ ...i, manual_count: activeHolds(i.id).length }))
      .filter((i) => (!params.get('state') || i.state === params.get('state')) && matchQ({ a: i.display, b: i.external_id, c: i.ad_sam }, params.get('q'))
        && (params.get('manual') !== '1' || i.manual_count > 0));
    return paginate(rows, params);
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
    // Like the server: actor: and action: terms are each OR-ed together; everything else must all match.
    const actors = [];
    const actions = [];
    const rest = [];
    for (const raw of terms) {
      const quoted = raw.startsWith('"');
      const t = raw.replace(/"/g, '');
      const i = quoted ? -1 : t.indexOf(':');
      const k = i > 0 ? t.slice(0, i).toLowerCase() : '';
      const v = i > 0 ? t.slice(i + 1) : t;
      if (!k) {
        rest.push((a) => (t.startsWith('-') && t.length > 1
          ? !JSON.stringify(a).toLowerCase().includes(t.slice(1).toLowerCase())
          : JSON.stringify(a).toLowerCase().includes(t.toLowerCase())));
      }
      else if (k === '-actor') { rest.push((a) => a.actor !== v); }
      else if (k === '-action') { const n = v.toLowerCase(); rest.push((a) => !(n.endsWith('*') ? a.action.startsWith(n.slice(0, -1)) : a.action === n)); }
      else if (k === '-type') { rest.push((a) => a.entity_type !== v.toLowerCase()); }
      else if (k === 'actor') { actors.push(v); }
      else if (k === 'action') { actions.push(v.toLowerCase()); }
      else if (k === 'type') { rest.push((a) => a.entity_type === v.toLowerCase()); }
      else if (k === 'id') { rest.push((a) => a.entity_id === v); }
      else if (k === 'level') { rest.push((a) => /(failed|denied|rejected|blocked)$/.test(a.action)); }
      else if (!['since', 'before', 'after', 'until'].includes(k)) {
        throw new HttpError(400, 'bad_request', `unknown filter "${k}:" (use actor:, action:, type:, id:, level:, since:, before:, or plain words)`);
      }
    }
    const match = (a) => (actors.length === 0 || actors.includes(a.actor))
      && (actions.length === 0 || actions.some((v) => (v.endsWith('*') ? a.action.startsWith(v.slice(0, -1)) : a.action === v)))
      && rest.every((f) => f(a));
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
