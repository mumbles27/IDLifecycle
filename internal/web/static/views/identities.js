// Identities: people known from the HR sources and how they link to AD.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, pill, fmtTime, dataTable, pager, listItems, debounce,
  errorBox, loading, field, dl, truncCell, isObject, cellText, relTime, act, toast, confirmDialog, changeTypeName,
  historyHref, notice, formModal,
} from '../ui.js';
import { untilValue } from '../undo.js';
import { undoPills } from './approvals.js';
import { changesTable } from './audit.js';
import { fmtDate, daysUntil, retentionReasonText } from './leavers.js';
import { daysText } from './retention.js';

const STATES = ['new', 'linked', 'disabled', 'deleted', 'merged', 'error'];

function decisionText(d) {
  if (!d) {
    return '';
  }
  if (d === 'new') {
    return 'Create a new account (matching skipped)';
  }
  if (d.startsWith('guid:')) {
    return `Link to the AD account ${d.slice(5)} (chosen by a person)`;
  }
  return d;
}

const HOLD_KINDS = {
  attribute: 'Attribute',
  group: 'AD group',
  cloud_group: 'Cloud group',
  ou: 'OU',
  state: 'Account state',
  all: 'Whole person',
};

function holdWhat(hd) {
  if (hd.kind === 'all') {
    return 'Nothing is changed automatically for this person';
  }
  if (hd.kind === 'state') {
    return 'No create, link, disable, enable or delete';
  }
  if (hd.kind === 'ou') {
    return 'No OU moves';
  }
  return hd.key || '';
}

const activeHold = (hd) => !hd.until || new Date(hd.until).getTime() > Date.now();
const suppressedText = (hd) => (hd && hd.suppressed && typeof hd.suppressed === 'object' ? hd.suppressed.summary || '' : '');

/** "Set back to Auto": the sync manages the item again from the source data and rules. */
async function setBackToAuto(ident, hd, btnEl, reload, nextText = '') {
  const what = holdWhat(hd);
  const wouldDo = nextText || suppressedText(hd);
  const ok = await confirmDialog({
    title: 'Set back to Auto?',
    message: `IDLifecycle manages "${what}" for ${ident.display || ident.external_id} again from the source data and rules. ${wouldDo ? `The next sync: ${wouldDo}.` : 'The next sync may change it again (for example redo a change that was undone).'}`,
    confirmLabel: 'Set back to Auto',
    danger: true,
  });
  if (!ok) {
    return;
  }
  await act(btnEl, async () => {
    await api.del(`/identities/${encodeURIComponent(ident.id)}/holds/${encodeURIComponent(hd.id)}`);
    toast('Set back to Auto.', 'ok');
    await reload();
  });
}

/**
 * Make an item (or the whole person) Manual: the sync stops changing it until someone sets it back to Auto.
 * A reason is required; for the whole person or the account state of someone who has left HR, an extra tick.
 */
function makeManual(ident, { kind, key = '', label }, reload) {
  const inactive = !ident.active;
  const risky = (kind === 'all' || kind === 'state') && inactive;
  const model = { reason: '', until: '', ack: !risky };
  const reason = h('textarea', { rows: '3', required: true, minlength: '10', 'data-test': 'manual-reason', placeholder: 'Why is this managed by hand now?', oninput: (e) => { model.reason = e.target.value; validate(); } });
  const until = h('input', { type: 'date', 'data-test': 'manual-until', oninput: (e) => { model.until = e.target.value; } });
  const ack = h('input', { type: 'checkbox', 'data-test': 'manual-ack', onchange: (e) => { model.ack = e.target.checked; validate(); } });
  ack.id = `ack-${Math.random().toString(36).slice(2, 8)}`;
  let submitBtn = null;
  function validate() {
    const ok = model.reason.trim().length >= 10 && model.ack;
    if (submitBtn) {
      submitBtn.disabled = !ok;
    }
    return ok;
  }
  const content = h('div', { class: 'stack', 'data-test': 'manual-dialog' },
    h('p', null, 'IDLifecycle stops changing ', h('strong', null, label), ` for ${ident.display || ident.external_id} until someone sets it back to Auto here. What is in AD now stays as it is.`),
    kind === 'all' ? notice('warn', 'Nothing at all is changed automatically for this person: no attribute, group, OU or cloud group updates, and no disable, enable or delete.') : null,
    risky
      ? h('div', { class: 'notice notice-error', role: 'alert' },
        h('p', null, h('strong', null, 'This person is inactive in HR. '), 'While Manual, their account is not disabled or deleted automatically. Make sure someone handles it by hand.'),
        h('div', { class: 'check' }, ack, h('label', { for: ack.id }, 'I understand the account stays as it is although the person has left')))
      : null,
    field('Reason', reason, { required: true, help: 'Required, at least 10 characters. Shown next to the Manual item and in the audit log.' }),
    field('Until (optional)', until, { help: 'Leave empty to keep it Manual until someone sets it back to Auto.' }));
  return new Promise((resolve) => {
    let done = false;
    const modal = formModal({
      title: `Make ${label} Manual?`,
      danger: risky,
      submitLabel: 'Make Manual',
      content,
      initialFocus: 'textarea',
      onSubmit: async () => {
        if (!validate()) {
          return false;
        }
        const body = { kind, key, reason: model.reason.trim() };
        const end = untilValue(model.until);
        if (end) {
          body.until = end;
        }
        await api.post(`/identities/${encodeURIComponent(ident.id)}/holds`, body);
        done = true;
        return true;
      },
      onClose: async () => {
        if (done) {
          toast(`${label} is Manual now.`, 'ok');
          await reload();
        }
        resolve(done);
      },
    });
    submitBtn = modal.el.querySelector('button[type="submit"]');
    submitBtn.setAttribute('data-test', 'manual-submit');
    submitBtn.setAttribute('data-mutating', '');
    validate();
  });
}

/** Every Manual item of this person (from undos or set by hand), with what the sync would otherwise do. */
function holdsCard(ident, ctx, reload) {
  const holds = Array.isArray(ident.holds) ? ident.holds : [];
  const canRelease = ctx.can('approver');
  return card('Manual items',
    h('p', { class: 'help' }, 'Items IDLifecycle does not change for this person until they are set back to Auto, usually because a change was undone or someone manages them by hand.'),
    h('div', { class: 'holds-list', 'data-test': 'ident-holds' }, dataTable([
      { label: 'Kind', render: (hd) => h('span', { class: 'hold-kind' }, HOLD_KINDS[hd.kind] || hd.kind) },
      { label: 'What', render: (hd) => truncCell(holdWhat(hd), 60) },
      { label: 'IDLifecycle would', render: (hd) => (suppressedText(hd) ? h('span', { class: 'small', 'data-test': 'hold-suppressed' }, suppressedText(hd)) : h('span', { class: 'muted small' }, 'nothing different so far')) },
      { label: 'Until', render: (hd) => (hd.until ? fmtTime(hd.until) : 'set back to Auto by hand') },
      { label: 'Reason', render: (hd) => truncCell(hd.reason || '', 80) },
      { label: 'Set by', render: (hd) => h('span', null, hd.created_by || '', hd.created_at ? h('div', { class: 'muted small' }, relTime(hd.created_at) || fmtTime(hd.created_at)) : null) },
      { label: 'From', render: (hd) => (hd.change_id ? h('span', { class: 'mono small' }, hd.change_id) : h('span', { class: 'muted small' }, 'set by hand')) },
      {
        label: h('span', { class: 'sr-only' }, 'Actions'),
        render: (hd) => (canRelease ? h('button', {
          type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'hold-release', 'data-id': hd.id,
          onclick: (e) => setBackToAuto(ident, hd, e.currentTarget, reload),
        }, 'Set back to Auto…') : ''),
      },
    ], holds, { caption: 'Manual items', empty: 'Nothing is Manual: IDLifecycle manages everything for this person.' })));
}

/** A person who left HR but is Manual is not disabled automatically: say so at the top of the page. */
function inactiveManualWarning(ident) {
  const holds = (Array.isArray(ident.holds) ? ident.holds : []).filter(activeHold);
  const pin = holds.find((hd) => hd.kind === 'all') || holds.find((hd) => hd.kind === 'state');
  if (ident.active || !pin) {
    return null;
  }
  return h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'manual-inactive-warning' },
    h('strong', null, 'This person is inactive in HR but Manual. '),
    `${pin.kind === 'all' ? 'Nothing is changed automatically for them' : 'Their account state is not changed automatically'}, so the account is not disabled or deleted. Set it back to Auto, or handle the account by hand.`);
}

const modePill = (row) => (row.mode === 'manual' ? pill('manual', 'Manual') : pill('auto', 'Auto'));
const valueText = (v) => (v === null || v === undefined || v === '' ? h('span', { class: 'muted' }, '(empty)') : truncCell(String(v), 60));
const yesNo = (v) => (v ? 'yes' : h('span', { class: 'muted' }, 'no'));

/**
 * What IDLifecycle manages for this person, item by item: what is in AD, what the rules would set, and whether
 * the sync may change it (Auto) or leaves it alone (Manual). Hidden when the server has no such endpoint.
 */
function managedCard(ident, ctx, reload) {
  const box = h('div', null, loading('Loading managed fields…'));
  const el = card('Managed fields', box);
  el.setAttribute('data-test', 'ident-managed');
  const can = ctx.can('approver');
  (async () => {
    let m;
    try {
      m = await api.get(`/identities/${encodeURIComponent(ident.id)}/managed`);
    }
    catch (e) {
      if (e.status === 404) {
        el.remove();
        return;
      }
      replaceContent(box, errorBox(e));
      return;
    }
    const action = (row, kind, key, label, nextText) => {
      if (!can) {
        return '';
      }
      if (row.mode === 'manual' && row.hold) {
        return h('button', {
          type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'set-auto', 'data-kind': kind, 'data-key': key,
          onclick: (e) => setBackToAuto(ident, row.hold, e.currentTarget, reload, nextText),
        }, 'Set back to Auto…');
      }
      if (row.mode === 'manual') {
        return h('span', { class: 'muted small' }, 'Manual through the whole person');
      }
      return h('button', {
        type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'make-manual', 'data-kind': kind, 'data-key': key,
        onclick: () => makeManual(ident, { kind, key, label }, reload),
      }, 'Make Manual…');
    };
    const manualNote = (row) => (row.mode === 'manual' && row.hold
      ? h('div', { class: 'muted small' }, `by ${row.hold.created_by || 'unknown'}${row.hold.created_at ? `, ${relTime(row.hold.created_at) || fmtTime(row.hold.created_at)}` : ''}${row.hold.reason ? `: ${row.hold.reason}` : ''}`,
        suppressedText(row.hold) ? h('div', { 'data-test': 'managed-suppressed' }, `IDLifecycle would ${suppressedText(row.hold).replace(/^would /, '')}`) : null)
      : null);
    const attrs = Array.isArray(m.attributes) ? m.attributes : [];
    const groups = Array.isArray(m.groups) ? m.groups : [];
    const person = m.person || { mode: 'auto' };
    const ou = m.ou || null;
    const rowAttrs = (kind, key) => ({ 'data-test': 'managed-row', 'data-kind': kind, 'data-key': key });
    replaceContent(box,
      h('div', { class: 'managed-person', 'data-test': 'managed-person', 'data-mode': person.mode },
        h('p', null, h('strong', null, 'Whole person: '), person.mode === 'manual' ? pill('manual', 'Manual') : (person.mode === 'state' ? pill('manual', 'Account state Manual') : pill('auto', 'Auto')),
          person.mode === 'manual' ? ' Nothing is changed automatically for this person.' : (person.mode === 'state' ? ' Their account is not created, linked, disabled, enabled or deleted automatically.' : ' The sync manages the items below that are Auto.')),
        person.hold ? manualNote({ mode: 'manual', hold: person.hold }) : null,
        can
          ? h('div', { class: 'row-gap' }, person.mode === 'auto'
            ? h('button', {
              type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'manual-person',
              onclick: () => makeManual(ident, { kind: 'all', key: '', label: 'the whole person' }, reload),
            }, 'Manage this person manually…')
            : (person.hold ? h('button', {
              type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'auto-person',
              onclick: (e) => setBackToAuto(ident, person.hold, e.currentTarget, reload),
            }, 'Set the person back to Auto…') : null))
          : null),
      h('h3', null, 'Attributes'),
      h('table', { class: 'data managed-table' },
        h('thead', null, h('tr', null, ['Attribute', 'In AD', 'Rules would set', 'Mode', h('span', { class: 'sr-only' }, 'Actions')].map((c) => h('th', null, c)))),
        h('tbody', null, attrs.length === 0
          ? h('tr', null, h('td', { colspan: '5', class: 'muted' }, 'No attributes are managed for this person.'))
          : attrs.map((a) => h('tr', { ...rowAttrs('attribute', a.name), class: a.mode === 'manual' ? 'is-manual' : '' },
            h('td', null, h('code', null, a.name)),
            h('td', null, valueText(a.ad_value)),
            h('td', null, valueText(a.desired), String(a.desired ?? '') !== String(a.ad_value ?? '') ? h('span', { class: 'pill pill-pending managed-diff' }, 'differs') : null),
            h('td', null, modePill(a), manualNote(a)),
            h('td', null, action(a, 'attribute', a.name, `the attribute ${a.name}`, `set ${a.name} to '${a.desired ?? ''}'`)))))),
      h('h3', null, 'Groups'),
      h('table', { class: 'data managed-table' },
        h('thead', null, h('tr', null, ['Group', 'Member now', 'Rules want', 'Mode', h('span', { class: 'sr-only' }, 'Actions')].map((c) => h('th', null, c)))),
        h('tbody', null, groups.length === 0
          ? h('tr', null, h('td', { colspan: '5', class: 'muted' }, 'No groups are managed for this person.'))
          : groups.map((g) => h('tr', { ...rowAttrs('group', g.group), class: g.mode === 'manual' ? 'is-manual' : '' },
            h('td', null, h('code', null, g.group)),
            h('td', null, yesNo(g.member)),
            h('td', null, yesNo(g.desired), Boolean(g.member) !== Boolean(g.desired) ? h('span', { class: 'pill pill-pending managed-diff' }, 'differs') : null),
            h('td', null, modePill(g), manualNote(g)),
            h('td', null, action(g, 'group', g.group, `the group ${g.group}`, g.desired ? `add to ${g.group}` : `remove from ${g.group}`)))))),
      ou ? h('div', { ...rowAttrs('ou', ''), class: 'managed-ou' },
        h('h3', null, 'OU'),
        dl([
          ['In AD', ou.current ? h('span', { class: 'mono wrap' }, ou.current) : h('span', { class: 'muted' }, '(none)')],
          ['Rules would place in', ou.desired ? h('span', { class: 'mono wrap' }, ou.desired) : h('span', { class: 'muted' }, '(not set)')],
          ['Mode', h('span', null, modePill(ou), manualNote(ou))],
        ]),
        h('div', { class: 'row-gap' }, action(ou, 'ou', '', 'the OU', ou.desired ? `move to ${ou.desired}` : ''))) : null);
  })();
  return el;
}

export async function render(root, ctx) {
  if (ctx.params[0]) {
    return renderDetail(root, ctx.params[0], ctx);
  }
  return renderList(root, ctx);
}

async function renderList(root, ctx) {
  const state = { q: ctx.query.get('q') || '', state: ctx.query.get('state') || '', manual: ctx.query.get('manual') === '1', limit: 25, offset: 0 };
  const listEl = h('div', null, loading());
  const search = h('input', {
    type: 'search',
    placeholder: 'Name, employee ID or sAMAccountName',
    'data-test': 'ident-q',
    value: state.q,
    oninput: debounce(() => { state.q = search.value.trim(); state.offset = 0; load(); }, 350),
  });
  const stateSel = h('select', {
    value: state.state,
    onchange: () => { state.state = stateSel.value; state.offset = 0; load(); },
  }, h('option', { value: '' }, 'Any state'), STATES.map((s) => h('option', { value: s }, s)));
  const manualSel = h('select', {
    'data-test': 'ident-manual',
    onchange: () => { state.manual = manualSel.value === '1'; state.offset = 0; load(); },
  }, h('option', { value: '' }, 'Everyone'), h('option', { value: '1' }, 'Only people with Manual items'));
  manualSel.value = state.manual ? '1' : '';

  root.append(pageHeader('Identities'),
    h('div', { class: 'filters' }, field('Search', search), field('State', stateSel), field('Managed', manualSel)),
    listEl);

  async function load() {
    let resp;
    try {
      resp = await api.get('/identities', { q: state.q, state: state.state, manual: state.manual ? '1' : '', limit: state.limit, offset: state.offset });
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const items = listItems(resp);
    const total = resp && Number.isFinite(resp.total) ? resp.total : items.length;
    replaceContent(listEl,
      dataTable([
        {
          label: 'Name',
          render: (i) => h('span', null, h('a', { href: `#/identities/${encodeURIComponent(i.id)}` }, i.display || i.external_id),
            Number(i.manual_count) > 0 ? h('span', { class: 'pill pill-manual manual-badge', 'data-test': 'manual-badge', title: 'Items IDLifecycle does not change for this person until set back to Auto' }, `Manual (${i.manual_count})`) : null),
        },
        { label: 'Employee ID', key: 'external_id' },
        { label: 'Source', key: 'source_name' },
        { label: 'State', render: (i) => pill(i.state) },
        { label: 'Active', render: (i) => pill(i.active ? 'active' : 'inactive') },
        { label: 'AD account', render: (i) => i.ad_sam || '' },
        { label: 'Last seen', render: (i) => h('span', { title: fmtTime(i.last_seen) }, relTime(i.last_seen) || fmtTime(i.last_seen)) },
      ], items, { caption: 'Identities', empty: 'No identities match.' }),
      pager({ total, limit: state.limit, offset: state.offset, onChange: (o, l) => { state.offset = o; state.limit = l; load(); } }));
  }
  await load();
}

const TABS = [['overview', 'Overview'], ['history', 'History'], ['changes', 'Changes']];

const inDays = (n) => {
  if (n === 0) {
    return 'today';
  }
  return n > 0 ? `in ${n} day${n === 1 ? '' : 's'}` : `${-n} day${n === -1 ? '' : 's'} ago`;
};

/** When the person leaves, when their disabled account is deleted, and under which retention policy. */
function retentionCard(ident, ctx, reload) {
  const r = isObject(ident.retention) ? ident.retention : null;
  const leave = (r && r.leave_date) || ident.leave_date || '';
  const lines = [];
  if (leave && ident.active) {
    lines.push(h('p', { class: 'retention-line', 'data-test': 'ident-leaving' }, pill('leaving', 'leaving'), ' ',
      h('strong', null, `Leaving on ${fmtDate(leave)}`), ` (${inDays(daysUntil(leave))}, from the termination date in the source).`));
  }
  if (ident.state === 'disabled') {
    const since = ident.disabled_at ? `Disabled on ${fmtDate(ident.disabled_at)}` : 'Disabled';
    let then = '';
    if (r && r.delete_due) {
      const d = daysUntil(r.delete_due);
      then = d <= 0 ? ` · deleted on the next sync (due ${fmtDate(r.delete_due)})` : ` · deleted on ${fmtDate(r.delete_due)} (${inDays(d)})`;
    }
    else if (r && r.policy_id) {
      then = ' · never deleted automatically';
    }
    lines.push(h('p', { class: 'retention-line', 'data-test': 'ident-delete-due' }, h('strong', null, `${since}${then}.`)));
  }
  if (r && (r.policy_name || r.policy_id)) {
    lines.push(h('p', { class: 'retention-line', 'data-test': 'ident-retention' },
      'Retention policy: ', h('strong', { 'data-test': 'ident-retention-policy' }, r.policy_name || r.policy_id),
      ` (${retentionReasonText(r)})`,
      r.delete_after_days !== undefined ? h('span', { class: 'muted' }, ` - ${daysText(r.delete_after_days)}`) : null, '.'));
  }
  const holds = Array.isArray(ident.holds) ? ident.holds : [];
  if (holds.some((hd) => (hd.kind === 'all' || hd.kind === 'state') && (!hd.until || new Date(hd.until).getTime() > Date.now()))) {
    lines.push(h('p', { class: 'retention-line muted small' }, 'Manual: the account is not disabled or deleted automatically while that Manual item stays.'));
  }
  if (lines.length === 0 && !r) {
    return null;
  }
  const canChange = ctx.can('approver') && ident.state !== 'deleted' && ident.state !== 'merged';
  return card('Leaving and retention',
    h('div', { 'data-test': 'ident-retention-card' }, lines.length ? lines : h('p', { class: 'muted' }, 'No retention information yet (it is worked out on the next sync).')),
    canChange
      ? h('div', { class: 'row-gap' }, h('button', {
        type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'ident-retention-change',
        onclick: () => changeRetention(ident, reload),
      }, 'Change retention…'))
      : null);
}

/** Assign a retention policy by hand (wins over the conditions), the Default, or back to Automatic. Reason required. */
async function changeRetention(ident, reload) {
  let policies = [];
  try {
    policies = listItems(await api.get('/retention-policies', { limit: 500 }));
  }
  catch (e) {
    toast(e.message, 'error');
    return;
  }
  const r = isObject(ident.retention) ? ident.retention : {};
  const model = { policy_id: r.reason === 'assigned' ? (r.policy_id || '') : '', reason: '' };
  const options = [
    { value: '', label: 'Automatic (the policy whose conditions match, else the Default)' },
    { value: 'default', label: 'Default' },
    ...policies.map((p) => ({ value: p.id, label: `${p.name} - ${daysText(p.delete_after_days)}${p.enabled ? '' : ' (disabled)'}` })),
  ];
  const sel = h('select', { 'data-test': 'retention-choose', onchange: () => { model.policy_id = sel.value; } },
    options.map((o) => h('option', { value: o.value }, o.label)));
  sel.value = model.policy_id;
  const reason = h('textarea', { rows: '3', required: true, minlength: '10', 'data-test': 'retention-reason', placeholder: 'Why does this person get this retention?', oninput: (e) => { model.reason = e.target.value; validate(); } });
  let submitBtn = null;
  function validate() {
    const ok = model.reason.trim().length >= 10;
    if (submitBtn) {
      submitBtn.disabled = !ok;
    }
    return ok;
  }
  let done = false;
  const modal = formModal({
    title: `Retention for ${ident.display || ident.external_id}`,
    submitLabel: 'Save retention',
    initialFocus: 'select',
    content: h('div', { class: 'stack', 'data-test': 'retention-dialog' },
      h('p', null, 'Now: ', h('strong', null, r.policy_name || 'not worked out yet'), r.reason ? ` (${retentionReasonText(r)})` : '', '.'),
      field('Retention policy', sel, { help: 'A policy chosen here wins over the conditions until it is set back to Automatic. Choosing a shorter period for someone disabled long ago plans their deletion on the next sync (it still needs approval as usual).' }),
      field('Reason', reason, { required: true, help: 'Required, at least 10 characters. Recorded in the person\'s history.' })),
    onSubmit: async () => {
      if (!validate()) {
        return false;
      }
      await api.put(`/identities/${encodeURIComponent(ident.id)}/retention`, { policy_id: model.policy_id, reason: model.reason.trim() });
      done = true;
      return true;
    },
    onClose: async () => {
      if (done) {
        toast('Retention saved.', 'ok');
        await reload();
      }
    },
  });
  submitBtn = modal.el.querySelector('button[type="submit"]');
  submitBtn.setAttribute('data-test', 'retention-save');
  submitBtn.setAttribute('data-mutating', '');
  validate();
}

/** Where the person record came from: an HR source, or typed into the console by someone. */
function originText(ident) {
  const src = ident.source_name || ident.source_id || 'a source';
  if (ident.origin === 'manual') {
    return `Added in the console${ident.created_by ? ` by ${ident.created_by}` : ''} (${src})`;
  }
  if (ident.origin === 'source') {
    return `From the HR source ${src}`;
  }
  return src;
}

/** The permanent record of an account that is gone from AD. */
function removedBox(ident) {
  const k = ident.ad_last_known;
  if (!k || typeof k !== 'object' || (!k.guid && !k.dn && !k.sam)) {
    return null;
  }
  return h('div', { class: 'notice notice-warn removed-box', 'data-test': 'ident-removed' },
    h('p', null, h('strong', null, k.deleted_at ? `Removed from AD on ${fmtTime(k.deleted_at)}. ` : 'No longer in AD. '),
      'The person record and its history are kept in IDLifecycle. Last known account:'),
    dl([
      ['sAMAccountName', k.sam || ''],
      ['UPN', k.upn || ''],
      ['Distinguished name', k.dn ? h('span', { class: 'mono wrap' }, k.dn) : ''],
      ['objectGUID', k.guid ? h('span', { class: 'mono' }, k.guid) : ''],
    ]));
}

const historyKind = (action) => {
  if (/(failed|denied|rejected)$/.test(action)) {
    return 'bad';
  }
  if (/(deleted|account_missing|inactive|undone|undo_requested)$/.test(action)) {
    return 'warn';
  }
  if (/(first_seen|reactivated|applied|create)$/.test(action)) {
    return 'good';
  }
  return 'info';
};

/** One history entry: when, who, what, the readable lines, and the recorded detail on request. */
function historyItem(it) {
  const lines = Array.isArray(it.lines) ? it.lines.filter(Boolean) : [];
  const detail = it.detail && typeof it.detail === 'object' && Object.keys(it.detail).length > 0 ? it.detail : null;
  const action = it.action || '';
  return h('li', { class: `tl-item tl-${historyKind(action)}`, 'data-test': 'history-item', 'data-action': action },
    h('div', { class: 'tl-when' },
      h('span', null, fmtTime(it.ts)),
      h('span', { class: 'muted small' }, relTime(it.ts) || '')),
    h('div', { class: 'tl-body' },
      h('div', { class: 'tl-title' }, it.title || action, ' ',
        h('span', { class: 'muted small' }, it.actor === 'system' ? 'by the service' : `by ${it.actor || 'unknown'}`)),
      lines.length ? h('ul', { class: 'tl-lines' }, lines.map((l) => h('li', null, l))) : null,
      h('div', { class: 'tl-meta muted small' },
        h('code', null, action),
        it.entity_type === 'change' && it.entity_id
          ? h('span', null, ' · change ', h('a', { href: `#/approvals?status=&q=${encodeURIComponent(it.entity_id)}` }, it.entity_id))
          : null,
        it.seq ? ` · #${it.seq}` : ''),
      detail
        ? h('details', { class: 'tl-detail' }, h('summary', { class: 'small' }, 'Recorded detail'),
          changesTable(detail.changes), h('pre', { class: 'detail-json' }, JSON.stringify(detail, null, 2)))
        : null));
}

/** The History tab: the person's whole timeline, newest first, a page at a time. */
function historyTab(ident) {
  const list = h('ol', { class: 'timeline', 'data-test': 'ident-history' });
  const more = h('div', { class: 'row-gap' });
  const status = h('div', { 'aria-live': 'polite' }, loading('Loading history…'));
  let before = '';
  let count = 0;
  async function page() {
    let resp;
    try {
      resp = await api.get(`/identities/${encodeURIComponent(ident.id)}/history`, { limit: 50, before_seq: before });
    }
    catch (e) {
      replaceContent(status, errorBox(e));
      return;
    }
    const items = listItems(resp);
    count += items.length;
    list.append(...items.map(historyItem));
    replaceContent(status, count === 0 ? h('p', { class: 'empty' }, 'Nothing recorded for this person yet.') : null);
    before = resp && resp.next_before_seq ? String(resp.next_before_seq) : '';
    if (before) {
      replaceContent(more, h('button', { type: 'button', class: 'btn', 'data-test': 'history-older', onclick: (e) => act(e.currentTarget, page) }, 'Load older'));
    }
    else {
      replaceContent(more, count > 0 ? h('p', { class: 'muted small', 'data-test': 'history-end' }, 'That is everything recorded for this person.') : null);
    }
  }
  page();
  return h('div', { class: 'stack' },
    h('p', { class: 'help' }, 'Everything recorded about this person, newest first: when they first appeared and from where, changes to their HR record, every AD and group change (who approved it and what was set), undos, and the account being disabled or removed. The record is kept even after the account is gone from AD.'),
    h('p', { class: 'small' }, h('a', { href: historyHref('identity', ident.id) }, 'Open these entries on the History page')),
    status, list, more);
}

function changesTab(ident) {
  const box = h('div', null, loading());
  (async () => {
    try {
      const resp = await api.get('/changes', { q: ident.external_id, limit: 100 });
      const mine = listItems(resp).filter((c) => c.identity_id === ident.id || c.external_id === ident.external_id);
      replaceContent(box, dataTable([
        { label: 'Created', render: (c) => fmtTime(c.created_at) },
        { label: 'Type', render: (c) => changeTypeName(c.type) },
        { label: 'Status', render: (c) => h('span', null, pill(c.status), ' ', undoPills(c)) },
        { label: 'Decided by', render: (c) => c.decided_by || '' },
        { label: 'Applied', render: (c) => (c.applied_at ? fmtTime(c.applied_at) : '') },
        { label: 'Summary', render: (c) => truncCell((c.summary || []).join('; '), 90) },
      ], mine, { caption: 'Changes', empty: 'No changes recorded for this identity.' }),
      h('p', null, h('a', { href: `#/approvals?status=&q=${encodeURIComponent(ident.external_id)}` }, 'Open in approvals')));
    }
    catch (e) {
      replaceContent(box, errorBox(e));
    }
  })();
  return card('Changes', box);
}

function overviewTab(root, id, ident, ctx) {
  const attrs = isObject(ident.attrs) ? Object.entries(ident.attrs).sort((a, b) => a[0].localeCompare(b[0])) : [];
  const reload = async () => { replaceContent(root); await renderDetail(root, id, ctx, 'overview'); };
  let notLinked = 'Not linked to an AD account yet.';
  if (ident.state === 'merged') {
    notLinked = 'Not linked: the account was handed over to another person record.';
  }
  else if (ident.ad_last_known) {
    notLinked = 'Not linked: the account was removed from AD (see above).';
  }
  return h('div', { class: 'stack' },
    inactiveManualWarning(ident),
    removedBox(ident),
    h('div', { class: 'grid grid-cards' },
      card('Source record', dl([
        ['Employee ID', ident.external_id],
        ['Origin', h('span', { 'data-test': 'ident-origin' }, originText(ident))],
        ['First seen', fmtTime(ident.first_seen)],
        ['Last seen', fmtTime(ident.last_seen)],
        ['Disabled at', fmtTime(ident.disabled_at)],
      ])),
      retentionCard(ident, ctx, reload),
      card('Active Directory link', ident.ad_guid || ident.ad_dn || ident.ad_sam
        ? dl([
          ['sAMAccountName', ident.ad_sam],
          ['Distinguished name', ident.ad_dn ? h('span', { class: 'mono wrap' }, ident.ad_dn) : ''],
          ['objectGUID', ident.ad_guid ? h('span', { class: 'mono' }, ident.ad_guid) : ''],
        ])
        : h('p', { class: 'muted' }, notLinked),
      ident.merged_into || ident.link_decision || (Array.isArray(ident.link_rejects) && ident.link_rejects.length > 0)
        ? h('div', { class: 'stack', 'data-test': 'ident-matching' },
          h('h3', null, 'Matching'),
          dl([
            ['Merged into', ident.merged_into ? h('a', { href: `#/identities/${encodeURIComponent(ident.merged_into)}`, 'data-test': 'ident-merged-into' }, 'the person record that now owns the account') : ''],
            ['Decision', decisionText(ident.link_decision)],
            ['Not this person', Array.isArray(ident.link_rejects) && ident.link_rejects.length > 0 ? h('span', { class: 'mono small wrap' }, ident.link_rejects.join(', ')) : ''],
          ]),
          ident.link_decision && !ident.ad_guid && ctx.can('approver')
            ? h('div', { class: 'row-gap' }, h('button', {
              type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'ident-clear-decision',
              onclick: async (e) => {
                const ok = await confirmDialog({
                  title: 'Clear the matching decision?',
                  message: 'The next sync matches this person again with the matching rules (it may propose a link or a new account).',
                  confirmLabel: 'Clear',
                });
                if (!ok) {
                  return;
                }
                await act(e.currentTarget, async () => {
                  await api.post(`/identities/${encodeURIComponent(ident.id)}/link`, { clear: true });
                  toast('Decision cleared.', 'ok');
                  await reload();
                });
              },
            }, 'Clear decision'))
            : null)
        : null)),
    card('Source attributes', dataTable([
      { label: 'Attribute', render: (r) => h('code', null, r[0]) },
      { label: 'Value', render: (r) => truncCell(cellText(r[1]), 120) },
    ], attrs, { caption: 'Source attributes', empty: 'No attributes stored.' })),
    managedCard(ident, ctx, reload),
    Array.isArray(ident.holds) ? holdsCard(ident, ctx, reload) : null);
}

async function renderDetail(root, id, ctx, initialTab) {
  const body = h('div', { class: 'stack' }, loading());
  root.append(pageHeader('Identity', h('a', { href: '#/identities', class: 'btn' }, 'Back to identities')), body);
  let ident;
  try {
    ident = await api.get(`/identities/${encodeURIComponent(id)}`);
  }
  catch (e) {
    replaceContent(body, errorBox(e));
    return;
  }
  const known = (k) => TABS.some(([t]) => t === k);
  let tab = 'overview';
  if (known(initialTab)) {
    tab = initialTab;
  }
  else if (known(ctx.query.get('tab'))) {
    tab = ctx.query.get('tab');
  }
  const tabsEl = h('div', { class: 'tabs', role: 'tablist', 'aria-label': 'Identity' });
  const panel = h('div', { role: 'tabpanel', 'data-test': 'ident-panel' });
  function show(next) {
    tab = next;
    replaceContent(tabsEl, TABS.map(([k, label]) => h('button', {
      type: 'button',
      role: 'tab',
      class: `tab${k === tab ? ' tab-active' : ''}`,
      'aria-selected': String(k === tab),
      'data-test': `ident-tab-${k}`,
      onclick: () => show(k),
    }, label)));
    if (tab === 'history') {
      replaceContent(panel, historyTab(ident));
    }
    else if (tab === 'changes') {
      replaceContent(panel, changesTab(ident));
    }
    else {
      replaceContent(panel, overviewTab(root, id, ident, ctx));
    }
  }
  replaceContent(body,
    h('h2', { class: 'ident-title' }, ident.display || ident.external_id, ' ', pill(ident.state), ' ', pill(ident.active ? 'active' : 'inactive')),
    tabsEl, panel);
  show(tab);
}
