// Settings: one JSON document (docs/ARCHITECTURE.md section 8). Admins edit; everyone else reads.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, errorBox, loading, field, inputText, inputNumber, inputArea,
  inputCheck, inputSelect, act, confirmDialog, toast, notice, linesToList, listToLines, isObject, pill,
} from '../ui.js';
import { previewPanel } from '../components.js';

const CHANGE_TYPES = [
  ['create', 'Create', 'New AD accounts for new hires.'],
  ['update', 'Update', 'Attribute, group and OU changes on existing accounts.'],
  ['disable', 'Disable', 'Disable accounts of people who left.'],
  ['enable', 'Enable', 'Re-enable accounts (rehires).'],
  ['delete', 'Delete', 'Permanently delete accounts after the grace period.'],
  ['link', 'Link', 'Link a person to an existing AD account found by Matching. Only full, unambiguous matches are ever approved automatically; partial matches always wait for a person.'],
];

const CONFLICT_MODES = [
  { value: 'manual', label: 'Ask a person (recommended)' },
  { value: 'number', label: 'Add a number (jdoe2, jdoe3 ...)' },
  { value: 'random', label: 'Add random digits (jdoe417)' },
  { value: 'template', label: 'Add a suffix from a template (e.g. part of the employee ID)' },
];

const CONFLICT_HELP = {
  manual: 'When the username, UPN, mail address or CN is already taken, the new account waits in Approvals and you get an "identity issues" notification. Someone types the name to use (with suggestions) before approving.',
  number: 'Tries name, name2, name3 ... up to the max candidates below. If every one is taken, the account waits for a person.',
  random: 'Tries the name, then the name with random digits. The digits are fixed per person, so the plan does not change from run to run. If every one is taken, the account waits for a person.',
  template: 'Tries the name, then the name followed by the suffix template below (letters and digits only). If both are taken, the account waits for a person.',
};

const SUFFIX_PRESETS = [
  ['Last 4 of EmployeeID', '{{ last .EmployeeID 4 }}'],
  ['Whole EmployeeID', '{{ .EmployeeID }}'],
  ['Middle initial', '{{ first .MiddleName 1 | lower }}'],
];

function section(doc, key) {
  if (!isObject(doc[key])) {
    doc[key] = {};
  }
  return doc[key];
}

function timezoneList() {
  try {
    if (typeof Intl.supportedValuesOf === 'function') {
      return Intl.supportedValuesOf('timeZone');
    }
  }
  catch (e) {
    // fall through
  }
  return ['UTC', 'America/New_York', 'America/Chicago', 'America/Denver', 'America/Los_Angeles', 'Europe/London', 'Europe/Berlin'];
}

// ---------------------------------------------------------------- notifications

const MASKED = '********';
const M365_SMTP_HOST = 'smtp.office365.com';

const PROVIDERS = [
  { value: 'smtp', label: 'Generic SMTP' },
  { value: 'm365_smtp', label: 'Microsoft 365 – SMTP AUTH (smtp.office365.com)' },
  { value: 'm365_graph', label: 'Microsoft 365 – Graph API' },
];

const AUTH_OPTIONS = {
  smtp: [
    { value: 'none', label: 'None (anonymous relay)' },
    { value: 'login', label: 'Username and password' },
  ],
  m365_smtp: [
    { value: 'oauth2', label: 'OAuth2 (recommended)' },
    { value: 'login', label: 'Basic - username and password (deprecated by Microsoft)' },
  ],
};

const NOTIFY_EVENTS = [
  ['run_failed', 'A sync run failed'],
  ['approvals_pending', 'Changes are waiting for approval'],
  ['changes_failed', 'Changes failed to apply in Active Directory'],
  ['threshold', 'A safety threshold was reached (bulk guard or delete limits held changes)'],
  ['run_summary', 'Summary of every run that planned or applied changes'],
  ['identity_issues', 'Identity issues: a username is taken, a match needs checking, or AD accounts match no person'],
];

function splitRecipients(text) {
  return String(text || '').split(/[\n,;]+/).map((s) => s.trim()).filter(Boolean);
}

function secretHelp(what) {
  return `Stored encrypted and never shown again. "${MASKED}" means a ${what} is saved: leave it to keep the saved one, or type a new one to replace it. Changing the server or app registration requires re-entering it.`;
}

/** The "Notifications (email)" card bound to doc.notifications. Returns { el, collect }. */
function notificationsCard(doc, isAdmin) {
  const n = section(doc, 'notifications');
  for (const k of ['smtp', 'oauth', 'events']) {
    section(n, k);
  }
  const { smtp, oauth, events } = n;
  if (!n.provider) {
    n.provider = 'smtp';
  }
  if (!smtp.security) {
    smtp.security = 'starttls';
  }
  if (!smtp.auth) {
    smtp.auth = n.provider === 'm365_smtp' ? 'oauth2' : 'none';
  }
  const view = { to: listToLines(Array.isArray(n.to) ? n.to : []) };

  const hostInput = inputText(smtp, 'host', { placeholder: 'mail.corp.example.com', 'data-test': 'set-notify-host' });
  const portInput = inputNumber(smtp, 'port', { min: '1', max: '65535', step: '1', 'data-test': 'set-notify-port' });
  const securitySel = inputSelect(smtp, 'security', [
    { value: 'starttls', label: 'STARTTLS (usually port 587)' },
    { value: 'tls', label: 'Implicit TLS (usually port 465)' },
    { value: 'none', label: 'None - plain text (no authentication unless the host is local)' },
  ], { 'data-test': 'set-notify-security' });
  const authSel = h('select', {
    'data-test': 'set-notify-auth',
    onchange: (e) => {
      smtp.auth = e.target.value;
      sync();
    },
  });
  const fillAuth = () => {
    const opts = AUTH_OPTIONS[n.provider] || AUTH_OPTIONS.smtp;
    if (!opts.some((o) => o.value === smtp.auth)) {
      smtp.auth = opts[0].value;
    }
    replaceContent(authSel, opts.map((o) => h('option', { value: o.value }, o.label)));
    authSel.value = smtp.auth;
  };

  const userBox = field('Username', inputText(smtp, 'username', { autocomplete: 'off', 'data-test': 'set-notify-user' }),
    { help: 'For Microsoft 365 basic auth this is the mailbox UPN.' });
  const passBox = field('Password', inputText(smtp, 'password', { type: 'password', autocomplete: 'new-password', 'data-test': 'set-notify-pass' }),
    { help: secretHelp('password') });
  const basicWarn = notice('warn', 'Microsoft is retiring Basic authentication for SMTP AUTH in Exchange Online. Prefer OAuth2 (below) or the Graph API provider.');

  const smtpBox = h('div', { class: 'stack' },
    h('div', { class: 'grid grid-2' },
      field('SMTP host', hostInput),
      field('Port', portInput),
      field('Security', securitySel, { help: 'STARTTLS is required when offered; the message is never sent if the server does not offer it.' }),
      field('Authentication', authSel)),
    basicWarn,
    h('div', { class: 'grid grid-2' }, userBox, passBox));

  const oauthBox = h('div', { class: 'stack' },
    h('div', { class: 'grid grid-2' },
      field('Tenant ID', inputText(oauth, 'tenant_id', { placeholder: '00000000-0000-0000-0000-000000000000 or contoso.onmicrosoft.com', 'data-test': 'set-notify-tenant' })),
      field('Application (client) ID', inputText(oauth, 'client_id', { placeholder: '00000000-0000-0000-0000-000000000000', 'data-test': 'set-notify-client' })),
      field('Client secret', inputText(oauth, 'client_secret', { type: 'password', autocomplete: 'new-password', 'data-test': 'set-notify-secret' }),
        { help: secretHelp('client secret') })));

  const smtpHelp = h('details', { class: 'help' },
    h('summary', null, 'Entra ID setup for Microsoft 365 SMTP AUTH (OAuth2)'),
    h('p', null, 'Register an app in Entra ID, create a client secret, and add the API permission ',
      h('strong', null, 'Office 365 Exchange Online › SMTP.SendAsApp'), ' (application) with admin consent. Then, in Exchange Online PowerShell:'),
    h('p', null, h('code', null, 'New-ServicePrincipal -AppId <client id> -ObjectId <enterprise app object id>')),
    h('p', null, h('code', null, 'Add-MailboxPermission -Identity <sender mailbox> -User <service principal id> -AccessRights FullAccess')),
    h('p', null, 'SMTP AUTH must be enabled for the sender mailbox (', h('code', null, 'Set-CASMailbox <sender> -SmtpClientAuthenticationDisabled $false'), '). The From address is used as the XOAUTH2 user unless a username is set.'));
  const graphHelp = h('details', { class: 'help' },
    h('summary', null, 'Entra ID setup for the Graph API'),
    h('p', null, 'Register an app in Entra ID, create a client secret, and add the API permission ',
      h('strong', null, 'Microsoft Graph › Mail.Send'), ' (application) with admin consent.'),
    h('p', null, 'Mail.Send lets the app send as any mailbox. Scope it to the sender mailbox with an Application Access Policy (',
      h('code', null, 'New-ApplicationAccessPolicy -AppId <client id> -PolicyScopeGroupId <group with the sender> -AccessRight RestrictAccess'),
      ') or RBAC for Applications in Exchange Online.'));

  const sync = () => {
    fillAuth();
    const p = n.provider;
    smtpBox.hidden = p === 'm365_graph';
    const login = smtp.auth === 'login';
    userBox.hidden = !login;
    passBox.hidden = !login;
    basicWarn.hidden = !(p === 'm365_smtp' && login);
    oauthBox.hidden = !(p === 'm365_graph' || (p === 'm365_smtp' && smtp.auth === 'oauth2'));
    smtpHelp.hidden = !(p === 'm365_smtp' && smtp.auth === 'oauth2');
    graphHelp.hidden = p !== 'm365_graph';
  };

  const onProvider = () => {
    if (n.provider === 'm365_smtp') {
      if (!String(smtp.host || '').trim()) {
        smtp.host = M365_SMTP_HOST;
        hostInput.value = smtp.host;
      }
      if (smtp.port === undefined || smtp.port === null || smtp.port === '' || smtp.port === 0) {
        smtp.port = 587;
        portInput.value = '587';
      }
      if (!smtp.security || smtp.security === 'none') {
        smtp.security = 'starttls';
        securitySel.value = 'starttls';
      }
      if (smtp.auth !== 'login') {
        smtp.auth = 'oauth2';
      }
    }
    sync();
  };

  const status = h('div', { role: 'status', 'aria-live': 'polite', 'data-test': 'set-notify-result' });

  /** Normalizes the bound object (recipients, empty numbers) and returns it. */
  const collect = () => {
    n.to = splitRecipients(view.to);
    if (smtp.port === '') {
      delete smtp.port;
    }
    return n;
  };

  const testBtn = isAdmin ? h('button', {
    type: 'button',
    class: 'btn',
    'data-test': 'set-notify-test',
    'data-mutating': '',
    onclick: () => act(testBtn, async () => {
      replaceContent(status);
      const payload = JSON.parse(JSON.stringify(collect()));
      if (payload.to.length === 0) {
        replaceContent(status, notice('error', 'Add at least one recipient first.'));
        return;
      }
      replaceContent(status, h('p', { class: 'muted' }, 'Sending…'));
      const res = await api.post('/notifications/test', { notifications: payload });
      if (res && res.ok) {
        replaceContent(status, notice('ok', `Test email sent to ${payload.to.join(', ')}. Check the inbox (and the spam folder).`));
        toast('Test email sent.', 'ok');
      }
      else {
        replaceContent(status, notice('error', `Test email failed: ${(res && res.error) || 'unknown error'}`));
      }
    }).finally(() => {
      if (status.textContent === 'Sending…') {
        replaceContent(status);
      }
    }),
  }, 'Send test email') : null;

  const el = card('Notifications (email)',
    h('p', { class: 'help' }, 'Email operators when something needs attention. Messages are plain text and never contain secrets or passwords.'),
    inputCheck(n, 'enabled', 'Send email notifications', { 'data-test': 'set-notify-enabled' }),
    h('div', { class: 'grid grid-2' },
      field('Provider', inputSelect(n, 'provider', PROVIDERS, { onchange: onProvider, 'data-test': 'set-notify-provider' })),
      field('From address', inputText(n, 'from', { placeholder: 'idlifecycle@corp.example.com', 'data-test': 'set-notify-from' }),
        { help: 'For Microsoft 365 this must be a real mailbox (a shared mailbox works) that the app may send as.' }),
      field('Subject prefix', inputText(n, 'subject_prefix', { placeholder: '[IDLifecycle]' })),
      field('GUI link base URL (optional)', inputText(n, 'gui_base_url', { type: 'url', placeholder: 'https://idl.corp.example.com:8443', 'data-test': 'set-notify-gui' }),
        { help: 'When set, messages link back to the run or approval queue.' })),
    field('Recipients', inputArea(view, 'to', { rows: '3', placeholder: 'ops@corp.example.com\nidentity-team@corp.example.com', 'data-test': 'set-notify-to' }),
      { help: 'One address per line, or comma separated (at most 50).' }),
    smtpBox,
    oauthBox,
    smtpHelp,
    graphHelp,
    h('h3', null, 'Send a message when'),
    h('div', { class: 'grid grid-2' }, NOTIFY_EVENTS.map(([key, label]) => inputCheck(events, key, label, { 'data-test': `set-notify-ev-${key}` }))),
    testBtn ? h('div', { class: 'row-gap' }, testBtn,
      h('span', { class: 'help' }, `Uses the values in this form, even unsaved ones. Fields showing "${MASKED}" use the saved secret.`)) : null,
    status);

  sync();
  return { el, collect };
}

// ---------------------------------------------------------------- sign-in (Entra ID)

const CALLBACK_PATH = '/api/auth/oidc/callback';

const LOCAL_LOGIN = [
  { value: 'all', label: 'Every local account (default)' },
  { value: 'admins_only', label: 'Administrators only (break-glass); everyone else signs in with Entra ID' },
];

// ---------------------------------------------------------------- backups

function fmtBytes(n) {
  if (!Number.isFinite(n)) {
    return '';
  }
  return n >= 1048576 ? `${(n / 1048576).toFixed(1)} MB` : `${Math.max(1, Math.round(n / 1024))} KB`;
}

/** The "Backups" card bound to doc.backup (schedule + passphrase), plus Back up now and the file list. */
function backupsCard(doc, isAdmin) {
  const b = section(doc, 'backup');
  if (!b.interval_hours) {
    b.interval_hours = 24;
  }
  if (!b.keep) {
    b.keep = 14;
  }
  const listEl = h('div', { 'data-test': 'backup-list' }, loading());
  const status = h('div', { 'aria-live': 'polite' });

  const pass = h('input', {
    type: 'password',
    autocomplete: 'new-password',
    value: b.passphrase || '',
    placeholder: b.passphrase ? 'stored - type to replace' : 'at least 12 characters',
    'data-test': 'set-backup-pass',
    oninput: () => { b.passphrase = pass.value; },
  });

  const loadList = async () => {
    if (!isAdmin) {
      replaceContent(listEl, h('p', { class: 'muted' }, 'Only administrators can see and download backups.'));
      return;
    }
    let res;
    try {
      res = await api.get('/backups');
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const files = (res && Array.isArray(res.files)) ? res.files : [];
    replaceContent(listEl,
      h('p', { class: 'help' }, 'Folder: ', h('code', null, res.folder || ''),
        res.next_due ? ['. Next scheduled backup: ', new Date(res.next_due).toLocaleString()] : '. Scheduled backups are off.'),
      files.length === 0
        ? h('p', { class: 'muted' }, 'No backups yet.')
        : h('table', { class: 'data' },
          h('thead', null, h('tr', null, h('th', null, 'File'), h('th', null, 'Created'), h('th', { class: 'num' }, 'Size'), h('th', null, ''))),
          h('tbody', null, files.map((f) => h('tr', null,
            h('td', { class: 'mono small' }, f.name),
            h('td', null, new Date(f.created).toLocaleString()),
            h('td', { class: 'num' }, fmtBytes(f.size)),
            h('td', null, h('a', { href: `/api/backups/${encodeURIComponent(f.name)}`, download: f.name, 'data-test': 'backup-download' }, 'Download')))))));
  };

  const nowBtn = h('button', {
    type: 'button',
    class: 'btn',
    'data-test': 'backup-now',
    'data-mutating': '',
    onclick: () => act(nowBtn, async () => {
      replaceContent(status, h('p', { class: 'muted' }, 'Writing the backup…'));
      try {
        const f = await api.post('/backups', {});
        replaceContent(status, notice('ok', `Backup written: ${f.name} (${fmtBytes(f.size)}).`));
      }
      catch (e) {
        replaceContent(status, notice('error', e.message));
      }
      await loadList();
    }),
  }, 'Back up now');

  const el = card('Backups',
    h('p', { class: 'help' }, 'A backup is one encrypted .idlbak file with the database, config.json and the encryption key, so it can be restored on this or another server (also into another database type) with the setup window or ',
      h('code', null, 'idlifecycled restore'), '. Without the passphrase it cannot be read or restored - keep a copy of the passphrase somewhere safe, outside this server.'),
    field('Backup passphrase', pass, { help: 'Stored encrypted. Changing it affects new backups only; older ones keep the passphrase they were made with.' }),
    inputCheck(b, 'enabled', 'Make backups automatically', { 'data-test': 'set-backup-enabled' }),
    h('div', { class: 'grid grid-2' },
      field('Every (hours)', inputNumber(b, 'interval_hours', { min: '1', max: '720', step: '1' })),
      field('Keep the newest', inputNumber(b, 'keep', { min: '1', max: '1000', step: '1' }), { help: 'Older backups in the folder are deleted after each backup.' })),
    field('Folder', inputText(b, 'folder', { placeholder: 'empty = backups folder inside the data folder' }),
      { help: 'A full path. The service account must be able to write there, and only administrators should be able to read it. Copy backups off this server regularly.' }),
    isAdmin ? h('div', { class: 'row-gap' }, nowBtn, h('span', { class: 'help' }, ' Uses the saved passphrase - save first after changing it.')) : null,
    status,
    listEl);
  loadList();
  return { el };
}

/** The "Sign-in" card bound to doc.auth. Returns { el, collect }. */
function signInCard(doc, isAdmin) {
  const auth = section(doc, 'auth');
  if (!auth.local_login) {
    auth.local_login = 'all';
  }
  const o = section(auth, 'oidc');
  const view = {
    admin: listToLines(o.admin_values),
    approver: listToLines(o.approver_values),
    viewer: listToLines(o.viewer_values),
  };
  const status = h('div', { 'aria-live': 'polite', 'data-test': 'set-oidc-status' });

  const collect = () => {
    o.admin_values = linesToList(view.admin);
    o.approver_values = linesToList(view.approver);
    o.viewer_values = linesToList(view.viewer);
    return auth;
  };

  const redirect = inputText(o, 'redirect_url', { type: 'url', placeholder: `https://idl.corp.example.com:8443${CALLBACK_PATH}`, 'data-test': 'set-oidc-redirect' });
  const suggested = `${location.origin}${CALLBACK_PATH}`;
  const useHere = isAdmin ? h('button', {
    type: 'button',
    class: 'btn btn-sm',
    'data-test': 'set-oidc-redirect-here',
    onclick: () => { redirect.value = suggested; redirect.dispatchEvent(new Event('input', { bubbles: true })); },
  }, 'Use this server\'s address') : null;

  const testBtn = isAdmin ? h('button', {
    type: 'button',
    class: 'btn',
    'data-test': 'set-oidc-test',
    'data-mutating': '',
    onclick: () => act(testBtn, async () => {
      replaceContent(status, h('p', { class: 'muted' }, 'Checking…'));
      const res = await api.post('/auth/oidc/test', { auth: JSON.parse(JSON.stringify(collect())) });
      if (res && res.ok) {
        replaceContent(status, notice('ok', 'Entra ID accepted the tenant, client ID and secret. Save, then try Sign in with Microsoft in a private window before restricting password sign-in.'));
      }
      else {
        replaceContent(status, notice('error', `Check failed: ${(res && res.error) || 'unknown error'}`));
      }
    }),
  }, 'Test sign-in settings') : null;

  const setup = h('details', { class: 'help' },
    h('summary', null, 'How to set this up in Entra ID'),
    h('ol', null,
      h('li', null, 'Entra admin center → App registrations → New registration. Supported account types: this organizational directory only.'),
      h('li', null, 'Redirect URI: platform Web, value ', h('code', null, suggested), ' (or your server\'s HTTPS address with that path).'),
      h('li', null, 'Certificates & secrets → New client secret. Paste the value below (not the secret ID).'),
      h('li', null, 'App roles → create three roles for Users/Groups with the values ', h('code', null, 'IDLifecycle.Admin'), ', ', h('code', null, 'IDLifecycle.Approver'), ' and ', h('code', null, 'IDLifecycle.Viewer'), '.'),
      h('li', null, 'Enterprise applications → this app → Users and groups: assign people or groups to those roles. Turn on "Assignment required" so nobody else can sign in.'),
      h('li', null, 'Require MFA with a Conditional Access policy that targets this app.')),
    h('p', null, 'No API permissions are needed. Group object IDs also work in the role lists if you emit group claims, but app roles avoid the 200-group token limit.'));

  const el = card('Sign-in',
    h('p', { class: 'help' }, 'Local accounts always work for administrators, so a broken Entra ID configuration cannot lock you out. Accounts that sign in with Entra ID are created on first sign-in; their name and role follow Entra ID on every sign-in.'),
    field('Password sign-in', inputSelect(auth, 'local_login', LOCAL_LOGIN, { 'data-test': 'set-local-login' }),
      { help: 'Administrators only needs Entra ID sign-in turned on. Switching it signs out local non-admins.' }),
    h('h3', null, 'Microsoft Entra ID (OpenID Connect)'),
    inputCheck(o, 'enabled', 'Offer "Sign in with Microsoft"', { 'data-test': 'set-oidc-enabled' }),
    setup,
    h('div', { class: 'grid grid-2' },
      field('Directory (tenant) ID', inputText(o, 'tenant_id', { placeholder: '00000000-0000-0000-0000-000000000000', class: 'mono', 'data-test': 'set-oidc-tenant' })),
      field('Application (client) ID', inputText(o, 'client_id', { placeholder: '00000000-0000-0000-0000-000000000000', class: 'mono', 'data-test': 'set-oidc-client' })),
      field('Client secret', inputText(o, 'client_secret', { type: 'password', autocomplete: 'new-password', 'data-test': 'set-oidc-secret' }), { help: secretHelp('client secret') }),
      field('Button text', inputText(o, 'button_label', { placeholder: 'Sign in with Microsoft' }))),
    field('Redirect URI', redirect, { help: 'Must match the app registration exactly. HTTPS is required except for localhost.' }),
    useHere,
    h('h3', null, 'Who gets which role'),
    h('p', { class: 'help' }, 'One app role value or group object ID per line. Someone matching several lists gets the highest role; someone matching none cannot sign in.'),
    h('div', { class: 'grid grid-2' },
      field('Admin', inputArea(view, 'admin', { rows: '3', class: 'mono', 'data-test': 'set-oidc-admins' })),
      field('Approver', inputArea(view, 'approver', { rows: '3', class: 'mono' })),
      field('Viewer', inputArea(view, 'viewer', { rows: '3', class: 'mono' }))),
    testBtn ? h('div', { class: 'row-gap' }, testBtn,
      h('span', { class: 'help' }, `Uses the values in this form, even unsaved ones. "${MASKED}" uses the saved secret.`)) : null,
    status);

  return { el, collect };
}

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const body = h('div', null, loading());
  root.append(pageHeader('Settings'), body);

  async function load() {
    let doc;
    try {
      doc = await api.get('/settings');
    }
    catch (e) {
      replaceContent(body, errorBox(e));
      return;
    }
    draw(isObject(doc) ? JSON.parse(JSON.stringify(doc)) : {});
  }

  function draw(doc) {
    const schedule = section(doc, 'schedule');
    const approval = section(doc, 'approval');
    const engine = section(doc, 'engine');
    const ad = section(doc, 'ad');
    const naming = section(doc, 'naming');
    const newUser = section(doc, 'new_user');
    const view = { bases: listToLines(ad.search_bases) };

    // ---- schedule
    if (!schedule.kind) {
      schedule.kind = 'interval';
    }
    const intervalBox = field('Interval (minutes)', inputNumber(schedule, 'interval_minutes', { min: '1', step: '1', 'data-test': 'set-interval' }), { help: 'Run every N minutes.' });
    const dailyBox = field('Time of day', inputText(schedule, 'daily_time', { type: 'time', 'data-test': 'set-daily' }), { help: 'Runs once a day at this local time (in the timezone below).' });
    const cronBox = field('Cron expression', inputText(schedule, 'cron_expr', { placeholder: '0 */4 * * *', class: 'mono' }), { help: 'Five fields: minute hour day-of-month month day-of-week.' });
    const syncKind = () => {
      intervalBox.hidden = schedule.kind !== 'interval';
      dailyBox.hidden = schedule.kind !== 'daily';
      cronBox.hidden = schedule.kind !== 'cron';
    };
    const tzList = h('datalist', { id: 'tz-list' }, timezoneList().map((z) => h('option', { value: z })));
    const tzInput = inputText(schedule, 'timezone', { list: 'tz-list', placeholder: 'America/Chicago' });
    const scheduleCard = card('Schedule',
      inputCheck(schedule, 'enabled', 'Run automatically on a schedule', { 'data-test': 'set-sched-enabled' }),
      field('Schedule type', inputSelect(schedule, 'kind', [
        { value: 'interval', label: 'Every N minutes' },
        { value: 'daily', label: 'Daily at a set time' },
        { value: 'cron', label: 'Cron expression' },
      ], { onchange: syncKind, 'data-test': 'set-kind' })),
      intervalBox, dailyBox, cronBox,
      field('Timezone', tzInput, { help: 'IANA name, e.g. America/Chicago. Applies to daily and cron schedules.' }),
      tzList);
    syncKind();

    // ---- approval
    const selects = {};
    const allAutoToggle = inputCheck({ v: false }, 'v', 'Auto-approve everything', { 'data-test': 'set-auto-all' });
    const allAutoInput = allAutoToggle.querySelector('input');
    const refreshAll = () => {
      allAutoInput.checked = CHANGE_TYPES.every(([k]) => approval[k] === 'auto');
    };
    const rows = CHANGE_TYPES.map(([key, label, help]) => {
      if (!approval[key]) {
        approval[key] = 'manual';
      }
      const sel = inputSelect(approval, key, [
        { value: 'manual', label: 'manual - a human must approve' },
        { value: 'auto', label: 'auto - approve automatically' },
      ], { 'data-test': `set-approval-${key}`, onchange: refreshAll });
      selects[key] = sel;
      return field(`${label} changes`, sel, { help });
    });
    refreshAll();
    allAutoInput.addEventListener('change', async () => {
      if (allAutoInput.checked) {
        const ok = await confirmDialog({
          title: 'Auto-approve everything?',
          message: 'Every change type - including DISABLE and DELETE - will be approved automatically with no human review. The bulk-destructive guard (max destructive per run) still forces manual approval when tripped. Continue?',
          confirmLabel: 'Auto-approve everything',
          danger: true,
        });
        if (!ok) {
          allAutoInput.checked = false;
          return;
        }
        for (const [k] of CHANGE_TYPES) {
          approval[k] = 'auto';
          selects[k].value = 'auto';
        }
      }
      else {
        for (const [k] of CHANGE_TYPES) {
          approval[k] = 'manual';
          selects[k].value = 'manual';
        }
      }
    });
    const approvalCard = card('Approval policy',
      h('p', { class: 'help' }, 'Decide per change type whether a person must approve it. New installs default to manual for everything.'),
      h('div', { class: 'quick-toggle' }, allAutoToggle,
        h('p', { class: 'help' }, 'Quick switch: sets every type below to auto (or back to manual).')),
      h('div', { class: 'grid grid-2' }, rows));

    // ---- engine
    const dryToggle = inputCheck(engine, 'dry_run', 'Dry run (plan and simulate only; never write to Active Directory)', { 'data-test': 'set-dry-run' });
    const dryInput = dryToggle.querySelector('input');
    dryInput.addEventListener('change', async () => {
      if (dryInput.checked) {
        return;
      }
      const ok = await confirmDialog({
        title: 'Turn OFF dry run?',
        message: 'The service will start WRITING to Active Directory: creating, changing, disabling and deleting accounts (subject to your approval policy). Are you sure you are ready to go live?',
        confirmLabel: 'Go live',
        danger: true,
      });
      if (!ok) {
        dryInput.checked = true;
        engine.dry_run = true;
      }
    });
    const engineCard = card('Engine',
      h('div', { class: 'dry-toggle' }, dryToggle,
        h('p', { class: 'help' }, 'While on, plans are recorded as "simulated" and nothing changes in AD. Changes are saved with the Save button below.')),
      h('div', { class: 'grid grid-2' },
        field('Max destructive changes per run', inputNumber(engine, 'max_destructive_per_run', { min: '0', step: '1', 'data-test': 'set-max-destructive' }), { help: 'Disable/delete batches larger than this are forced to manual approval - a safety net against a bad feed.' }),
        field('Delete after (days)', inputNumber(engine, 'delete_after_days', { min: '0', step: '1' }), { help: 'Grace period between disabling an account and deleting it.' }),
        field('Hold all deletes for manual approval when one run plans more than', inputNumber(engine, 'max_deletes_per_run', { min: '0', step: '1', 'data-test': 'set-max-deletes-run' }), { help: 'If a single run plans more deletes than this, every delete of that run waits for a human - even when deletes are auto-approved. 0 = no limit.' }),
        field('Maximum deletes completed per 24 hours', inputNumber(engine, 'max_deletes_per_day', { min: '0', step: '1', 'data-test': 'set-max-deletes-day' }), { help: 'Hard cap on deletes applied to AD in any rolling 24 hours. Approved deletes beyond it stay approved and are applied once the window allows. 0 = no limit.' })),
      inputCheck(engine, 'remove_stale_groups', 'Remove groups that are no longer granted by any role rule'),
      inputCheck(engine, 'enforce_ou', 'Enforce OU placement (move existing accounts to the OU their role rule dictates)'));

    // ---- AD
    const adCard = card('Active Directory',
      h('div', { class: 'grid grid-2' },
        field('Domain controller (optional)', inputText(ad, 'server', { placeholder: 'dc01.corp.example.com' }), { help: 'Leave blank to let the PowerShell AD module choose.' }),
        field('ID attribute', inputText(ad, 'id_attribute', { placeholder: 'employeeID', required: true }), { help: 'The AD attribute that stores the HR employee ID (the link key).' }),
        field('Default OU', inputText(ad, 'default_ou', { placeholder: 'OU=Staff,DC=corp,DC=example,DC=com' }), { help: 'Where new accounts go when no role rule sets an OU.' }),
        field('Disabled OU', inputText(ad, 'disabled_ou', { placeholder: 'OU=Disabled,DC=corp,DC=example,DC=com' }), { help: 'Disabled accounts are moved here. Blank leaves them in place.' }),
        field('UPN suffix', inputText(ad, 'upn_suffix', { placeholder: 'corp.example.com' }))),
      field('Search bases (one per line)', inputArea(view, 'bases', { rows: '3', placeholder: 'OU=Staff,DC=corp,DC=example,DC=com', 'data-test': 'set-bases' }), { help: 'Only accounts under these OUs are managed.' }),
      h('p', { class: 'help', 'data-test': 'set-lifecycle-link' }, 'Which groups a disabled account loses or keeps, and the groups every active account gets, are set on ', h('a', { href: '#/lifecycle' }, 'Lifecycle groups'), '.'));

    // ---- naming
    const nPreview = previewPanel({
      title: 'Naming preview',
      getTemplates: () => [
        { label: 'sAMAccountName', template: naming.sam_template },
        { label: 'UPN (local part)', template: naming.upn_template },
        { label: 'CN', template: naming.cn_template },
        { label: 'Mail (local part)', template: naming.mail_template },
      ],
    });
    const pv = () => nPreview.run();
    if (!naming.conflict_mode) {
      naming.conflict_mode = 'manual';
    }
    const sufPreview = previewPanel({
      title: 'Suffix preview',
      getTemplates: () => [{ label: 'Suffix', template: naming.conflict_suffix_template }],
    });
    const suffixInput = inputText(naming, 'conflict_suffix_template', { class: 'mono', oninput: () => sufPreview.run(), placeholder: '{{ last .EmployeeID 4 }}', 'data-test': 'set-conflict-suffix' });
    const conflictHelp = h('p', { class: 'help', 'aria-live': 'polite', 'data-test': 'conflict-help' });
    const maxBox = field('Max name candidates', inputNumber(naming, 'max_candidates', { min: '1', step: '1' }), { help: 'How many variants to try before the account waits for a person.' });
    const digitsBox = field('Random digits', inputNumber(naming, 'conflict_random_digits', { min: '2', max: '6', step: '1', 'data-test': 'set-conflict-digits' }), { help: '2 to 6 digits.' });
    const suffixBox = h('div', { class: 'stack' },
      field('Suffix template', suffixInput, { help: 'Rendered from the person\'s source record; anything but letters and digits is dropped.' }),
      h('div', { class: 'row-gap' }, h('span', { class: 'help' }, 'Presets:'),
        SUFFIX_PRESETS.map(([label, tpl]) => h('button', {
          type: 'button', class: 'btn btn-sm', 'data-test': 'suffix-preset',
          onclick: () => { naming.conflict_suffix_template = tpl; suffixInput.value = tpl; sufPreview.run(); },
        }, label))),
      sufPreview.el);
    const syncConflict = () => {
      const m = naming.conflict_mode;
      conflictHelp.textContent = CONFLICT_HELP[m] || '';
      maxBox.hidden = !(m === 'number' || m === 'random');
      digitsBox.hidden = m !== 'random';
      suffixBox.hidden = m !== 'template';
      if (m === 'template') {
        sufPreview.run();
      }
    };
    const conflictBox = h('div', { class: 'stack' },
      h('h3', null, 'When a name is already taken'),
      field('Conflict handling', inputSelect(naming, 'conflict_mode', CONFLICT_MODES, { onchange: syncConflict, 'data-test': 'set-conflict-mode' })),
      conflictHelp,
      h('p', { class: 'help' }, 'Every sync checks the planned sAMAccountName, UPN, mail address (mail and proxyAddresses) and CN against the whole directory, and against the other people planned in the same run.'),
      h('div', { class: 'grid grid-2' }, maxBox, digitsBox),
      suffixBox);
    syncConflict();
    const namingCard = card('Naming (new accounts)',
      h('p', { class: 'help' }, 'Templates for new accounts. sAMAccountName is cut to 20 characters before any suffix is added.'),
      field('sAMAccountName template', inputText(naming, 'sam_template', { class: 'mono', oninput: pv, 'data-test': 'set-sam' })),
      field('UPN local-part template', inputText(naming, 'upn_template', { class: 'mono', oninput: pv })),
      field('CN template', inputText(naming, 'cn_template', { class: 'mono', oninput: pv })),
      h('div', { class: 'grid grid-2' },
        field('Mail local-part template (optional)', inputText(naming, 'mail_template', { class: 'mono', oninput: pv, placeholder: '{{ .FirstName | lower | alnum }}.{{ .LastName | lower | alnum }}', 'data-test': 'set-mail-template' }),
          { help: 'Sets the mail attribute when the account is created, with the same number as the account name (jane.doe2@...). Leave empty to skip. A mapping that writes mail takes precedence and keeps it in sync.' }),
        field('Mail domain', inputText(naming, 'mail_domain', { placeholder: 'defaults to the UPN suffix', 'data-test': 'set-mail-domain' }))),
      conflictBox,
      nPreview.el);

    // ---- new user
    if (!newUser.password_mode) {
      newUser.password_mode = 'random';
    }
    const pwPreview = previewPanel({
      title: 'Initial password preview (sample record only)',
      getTemplates: () => [{ label: 'Initial password', template: newUser.password_template, kind: 'password' }],
    });
    const lengthBox = field('Initial password length', inputNumber(newUser, 'password_length', { min: '14', step: '1' }), { help: 'Random passwords are never stored or logged; reset them with your normal process.' });
    const templateBox = h('div', { class: 'stack' },
      notice('warn', 'A password built from HR data can be guessed by anyone who knows that data. Include a random part (e.g. ', h('code', null, '{{ random 6 }}'), '), deliver it through a trusted channel, and keep "must change at next logon" (it is forced on in this mode).'),
      field('Initial password template', inputText(newUser, 'password_template', { class: 'mono', oninput: () => pwPreview.run(), 'data-test': 'set-pw-template' }),
        { help: 'Rendered per account when it is created; never stored or logged. It must give 8+ characters with 3 of: uppercase, lowercase, digits, symbols, and must not contain the account name, or that account fails with the reason.' }),
      pwPreview.el);
    const mustChange = inputCheck(newUser, 'change_password_at_logon', 'User must change password at next logon', { 'data-test': 'set-must-change' });
    const syncPwMode = () => {
      const tpl = newUser.password_mode === 'template';
      lengthBox.hidden = tpl;
      templateBox.hidden = !tpl;
      const box = mustChange.querySelector('input');
      if (tpl) {
        newUser.change_password_at_logon = true;
        box.checked = true;
      }
      box.disabled = tpl;
      if (tpl) {
        pwPreview.run();
      }
    };
    const newUserCard = card('New accounts',
      field('Initial password', inputSelect(newUser, 'password_mode', [
        { value: 'random', label: 'Random (recommended)' },
        { value: 'template', label: 'Built from a template' },
      ], { onchange: syncPwMode, 'data-test': 'set-pw-mode' })),
      lengthBox,
      templateBox,
      mustChange,
      inputCheck(newUser, 'enabled', 'Create accounts enabled'));
    syncPwMode();

    const notifyCard = notificationsCard(doc, isAdmin);
    const authCard = signInCard(doc, isAdmin);
    const backupCard = backupsCard(doc, isAdmin);

    const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary', 'data-test': 'set-save', 'data-mutating': '' }, 'Save settings');
    const reloadBtn = h('button', { type: 'button', class: 'btn', onclick: () => load() }, 'Discard changes');

    const form = h('form', {
      class: 'stack settings-form',
      onsubmit: (ev) => {
        ev.preventDefault();
        act(saveBtn, async () => {
          ad.search_bases = linesToList(view.bases);
          notifyCard.collect();
          authCard.collect();
          for (const [sec, key] of [[schedule, 'interval_minutes'], [engine, 'max_destructive_per_run'], [engine, 'delete_after_days'], [engine, 'max_deletes_per_run'], [engine, 'max_deletes_per_day'], [naming, 'max_candidates'], [naming, 'conflict_random_digits'], [newUser, 'password_length'], [section(doc, 'backup'), 'interval_hours'], [section(doc, 'backup'), 'keep']]) {
            if (sec[key] === '') {
              delete sec[key];
            }
          }
          const saved = await api.put('/settings', doc);
          toast('Settings saved.', 'ok');
          ctx.refreshStatus();
          if (isObject(saved) && isObject(saved.engine)) {
            draw(JSON.parse(JSON.stringify(saved)));
          }
          else {
            await load();
          }
        });
      },
    },
    isAdmin ? null : notice('info', 'Read-only: only administrators can change settings. Your role is ', pill(ctx.role), '.'),
    h('fieldset', { class: 'plain', disabled: !isAdmin }, h('div', { class: 'stack' }, scheduleCard, approvalCard, engineCard, adCard, namingCard, newUserCard, notifyCard.el, authCard.el, backupCard.el)),
    isAdmin ? h('div', { class: 'savebar' }, reloadBtn, h('span', { class: 'spacer' }), saveBtn) : null);

    replaceContent(body, form);
    nPreview.run();
  }

  await load();
}
