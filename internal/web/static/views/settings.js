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
        field('Delete after (days)', inputNumber(engine, 'delete_after_days', { min: '0', step: '1' }), { help: 'Grace period between disabling an account and deleting it.' })),
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

    const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary', 'data-test': 'set-save', 'data-mutating': '' }, 'Save settings');
    const reloadBtn = h('button', { type: 'button', class: 'btn', onclick: () => load() }, 'Discard changes');

    const form = h('form', {
      class: 'stack settings-form',
      onsubmit: (ev) => {
        ev.preventDefault();
        act(saveBtn, async () => {
          ad.search_bases = linesToList(view.bases);
          for (const [sec, key] of [[schedule, 'interval_minutes'], [engine, 'max_destructive_per_run'], [engine, 'delete_after_days'], [naming, 'max_candidates'], [newUser, 'password_length']]) {
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
    h('fieldset', { class: 'plain', disabled: !isAdmin }, h('div', { class: 'stack' }, scheduleCard, approvalCard, engineCard, adCard, namingCard, newUserCard)),
    isAdmin ? h('div', { class: 'savebar' }, reloadBtn, h('span', { class: 'spacer' }), saveBtn) : null);

    replaceContent(body, form);
    nPreview.run();
  }

  await load();
}
