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
      inputCheck(ad, 'strip_groups_on_disable', 'Remove all group memberships when disabling an account'));

    // ---- naming
    const nPreview = previewPanel({
      title: 'Naming preview',
      getTemplates: () => [
        { label: 'sAMAccountName', template: naming.sam_template },
        { label: 'UPN (local part)', template: naming.upn_template },
        { label: 'CN', template: naming.cn_template },
      ],
    });
    const pv = () => nPreview.run();
    const namingCard = card('Naming (new accounts)',
      h('p', { class: 'help' }, 'Templates for new accounts. On a collision the service tries the next candidate (name, name2, name3 ...). sAMAccountName is cut to 20 characters before the number is added.'),
      field('sAMAccountName template', inputText(naming, 'sam_template', { class: 'mono', oninput: pv, 'data-test': 'set-sam' })),
      field('UPN local-part template', inputText(naming, 'upn_template', { class: 'mono', oninput: pv })),
      field('CN template', inputText(naming, 'cn_template', { class: 'mono', oninput: pv })),
      h('div', { class: 'grid grid-2' },
        field('Max name candidates', inputNumber(naming, 'max_candidates', { min: '1', step: '1' }), { help: 'How many numbered variants to try before giving up.' })),
      nPreview.el);

    // ---- new user
    const newUserCard = card('New accounts',
      h('div', { class: 'grid grid-2' },
        field('Initial password length', inputNumber(newUser, 'password_length', { min: '8', step: '1' }), { help: 'Passwords are random, never stored or logged; reset them with your normal process.' })),
      inputCheck(newUser, 'change_password_at_logon', 'User must change password at next logon'),
      inputCheck(newUser, 'enabled', 'Create accounts enabled'));

    const notifyCard = notificationsCard(doc, isAdmin);

    const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary', 'data-test': 'set-save', 'data-mutating': '' }, 'Save settings');
    const reloadBtn = h('button', { type: 'button', class: 'btn', onclick: () => load() }, 'Discard changes');

    const form = h('form', {
      class: 'stack settings-form',
      onsubmit: (ev) => {
        ev.preventDefault();
        act(saveBtn, async () => {
          ad.search_bases = linesToList(view.bases);
          notifyCard.collect();
          for (const [sec, key] of [[schedule, 'interval_minutes'], [engine, 'max_destructive_per_run'], [engine, 'delete_after_days'], [engine, 'max_deletes_per_run'], [engine, 'max_deletes_per_day'], [naming, 'max_candidates'], [newUser, 'password_length']]) {
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
    h('fieldset', { class: 'plain', disabled: !isAdmin }, h('div', { class: 'stack' }, scheduleCard, approvalCard, engineCard, adCard, namingCard, newUserCard, notifyCard.el)),
    isAdmin ? h('div', { class: 'savebar' }, reloadBtn, h('span', { class: 'spacer' }), saveBtn) : null);

    replaceContent(body, form);
    nPreview.run();
  }

  await load();
}
