// undo.js - rolling back applied changes: the request dialog for one change (with a choice of items for updates and
// cloud group changes), the dialog for undoing several changes of one run, and the approval dialogs of an undo change
// and of a batch. All of them show a live dry run first and are deliberately hard to complete by accident: a reason,
// a typed confirmation, and two tick-boxes before later changes are overwritten.

import { api } from './api.js';
import {
  h, replaceContent, openModal, formModal, notice, field, toast, fmtTime, changeTypeName, loading, errorBox, uid, debounce,
  pill, typeTag,
} from './ui.js';

const MIN_REASON = 10;

/** The "until" date input value (YYYY-MM-DD) as the end of that local day, in RFC 3339. */
export function untilValue(date) {
  if (!date) {
    return undefined;
  }
  const d = new Date(`${date}T23:59:59`);
  return Number.isNaN(d.getTime()) ? undefined : d.toISOString();
}

function list(items, attrs = {}) {
  return h('ul', { class: 'summary-list', ...attrs }, items.map((s) => h('li', null, String(s))));
}

/** The live dry run of the undo against AD / the tenant: what it would do, or what changed since. */
export function dryRunBox(res) {
  const drift = Array.isArray(res.drift) ? res.drift : [];
  const dr = res.dry_run || {};
  if (drift.length > 0) {
    return h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'undo-drift' },
      h('p', null, h('strong', null, 'Changed since: '), 'the account no longer looks the way this change left it. Undoing now would overwrite these later changes:'),
      list(drift));
  }
  if (dr.status === 'failed') {
    return h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'undo-dryrun-failed' },
      h('strong', null, 'The live check failed: '), dr.error || dr.message || 'unknown error');
  }
  return h('div', { class: 'notice notice-ok', 'data-test': 'undo-dryrun-ok' },
    h('strong', null, 'Live check passed: '), 'the account is as this change left it.',
    dr.message ? h('div', { class: 'small' }, `Dry run: ${dr.message}`) : null);
}

/** One line per step; steps come from the server. */
function stepsBox(steps) {
  return Array.isArray(steps) && steps.length > 0
    ? list(steps, { 'data-test': 'undo-steps' })
    : h('p', { class: 'muted' }, 'No steps.');
}

const sameText = (a, b) => String(a).trim().replace(/\s+/g, ' ').toLowerCase() === String(b).trim().replace(/\s+/g, ' ').toLowerCase();

function typedConfirm(confirmText, onInput, testId, help = 'This makes sure the undo is not requested for the wrong account.') {
  const input = h('input', {
    type: 'text', class: 'mono', autocomplete: 'off', spellcheck: 'false', 'data-test': testId,
    oninput: onInput,
  });
  const wrap = field('', input, { required: true, help });
  const label = wrap.querySelector('label');
  const setText = (t) => {
    replaceContent(label, t
      ? ['Type ', h('code', { 'data-test': `${testId}-text` }, t), ' to confirm']
      : 'Confirmation (preview first: the text to type appears here)',
    h('span', { class: 'req', 'aria-hidden': 'true' }, ' *'));
  };
  let expected = confirmText;
  setText(expected);
  return {
    wrap,
    input,
    ok: () => Boolean(expected) && sameText(input.value, expected),
    set: (t) => { expected = t; setText(t); },
  };
}

/** The two tick-boxes that must both be ticked before later changes are overwritten. */
function forceRows(onChange, prefix = 'undo') {
  const state = { force: false, sure: false };
  const sureCb = h('input', { type: 'checkbox', 'data-test': `${prefix}-force-sure`, onchange: (e) => { state.sure = e.target.checked; onChange(); } });
  const sureRow = h('div', { class: 'check', hidden: true }, sureCb, h('label', null, 'I have checked the later changes and want them overwritten'));
  sureRow.querySelector('label').htmlFor = sureCb.id = uid('sure');
  const forceCb = h('input', {
    type: 'checkbox', 'data-test': `${prefix}-force`,
    onchange: (e) => {
      state.force = e.target.checked;
      sureRow.hidden = !state.force;
      if (!state.force) {
        sureCb.checked = false;
        state.sure = false;
      }
      onChange();
    },
  });
  const forceRow = h('div', { class: 'check' }, forceCb, h('label', null, 'Overwrite the later changes listed above'));
  forceRow.querySelector('label').htmlFor = forceCb.id = uid('force');
  const reset = () => {
    forceCb.checked = false;
    sureCb.checked = false;
    sureRow.hidden = true;
    state.force = false;
    state.sure = false;
  };
  return { el: h('div', { class: 'stack undo-force' }, forceRow, sureRow), confirmed: () => state.force && state.sure, reset };
}

/** "Keep Manual" check-boxes: one per item the undo restores, each with an optional end date. */
function manualRows(holds) {
  return (Array.isArray(holds) ? holds : []).map((hd) => {
    const cb = h('input', { type: 'checkbox', checked: hd.default !== false, 'data-test': 'undo-hold', 'data-kind': hd.kind, 'data-key': hd.key || '' });
    const until = h('input', { type: 'date', 'aria-label': `Manual until (${hd.label || hd.kind})`, 'data-test': 'undo-hold-until' });
    cb.id = uid('hold');
    const row = h('div', { class: 'undo-hold' },
      h('div', { class: 'check' }, cb, h('label', { for: cb.id }, hd.label || `${hd.kind} ${hd.key || ''}`.trim())),
      h('label', { class: 'undo-until' }, h('span', { class: 'muted small' }, 'until (optional)'), until));
    const sync = () => { until.disabled = !cb.checked; };
    cb.addEventListener('change', sync);
    sync();
    return { hd, cb, until, row };
  });
}

function manualFieldset(rows) {
  return rows.length > 0
    ? h('fieldset', { class: 'undo-holds', 'data-test': 'undo-holds' },
      h('legend', null, 'Keep these Manual (the sync will not redo them)'),
      h('p', { class: 'help' }, 'A Manual item is left alone by the sync until someone sets it back to Auto on the person\'s page (or the optional date passes). Untick an item to let the next sync plan it again from the source data and rules.'),
      rows.map((r) => r.row))
    : null;
}

function chosenManual(rows) {
  return rows.filter((r) => r.cb.checked).map((r) => {
    const o = { kind: r.hd.kind, key: r.hd.key || '' };
    const u = untilValue(r.until.value);
    if (u) {
      o.until = u;
    }
    return o;
  });
}

function notPossible(change, preview) {
  return new Promise((resolve) => {
    let modal = null;
    const items = Array.isArray(preview && preview.items) ? preview.items : [];
    modal = openModal({
      title: `This ${changeTypeName(change.type)} change cannot be undone`,
      content: h('div', { class: 'stack', 'data-test': 'undo-not-possible' },
        notice('error', (preview && preview.reason) || 'This change cannot be undone.'),
        items.length ? h('div', null, h('h3', null, 'Items of the change'), itemStateList(items)) : null,
        Array.isArray(change.summary) && change.summary.length ? h('div', null, h('h3', null, 'The change'), list(change.summary)) : null),
      footer: [h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn', 'data-test': 'undo-close', onclick: () => modal.close(false) }, 'Close')],
      onClose: () => resolve(false),
    });
  });
}

/** Read-only list of items with what happened to them (undone, an undo waiting, Manual). */
function itemStateList(items) {
  return h('ul', { class: 'summary-list' }, items.map((it) => h('li', null, it.label || it.key, ' ', itemBadges(it))));
}

function itemBadges(it) {
  return [
    it.undone_by ? h('span', { class: 'muted small', 'data-test': 'undo-item-undone' }, ` (undone by ${it.undone_by})`) : null,
    it.pending_undo ? h('span', { class: 'muted small' }, ` (undo waiting: ${it.pending_undo})`) : null,
    !it.undone_by && !it.pending_undo && it.selectable === false ? h('span', { class: 'muted small' }, ' (not selectable)') : null,
    it.manual ? h('span', null, ' ', pill('manual', 'Manual')) : null,
  ];
}

/**
 * Opens the undo request dialog for an applied change. Resolves true when an undo was requested.
 * For updates and cloud group changes the server lists the change's items; any subset can be undone.
 * The server re-checks everything (preview hash, typed confirmation, reason, drift, selection).
 */
export async function requestUndo(change) {
  const path = `/changes/${encodeURIComponent(change.id)}/undo`;
  let preview;
  try {
    preview = await api.get(path);
  }
  catch (e) {
    toast(e.message, 'error');
    return false;
  }
  const who = change.display || change.external_id;
  if (!preview || !preview.undoable) {
    return notPossible(change, preview);
  }

  const allItems = Array.isArray(preview.items) ? preview.items : [];
  const itemMode = allItems.length > 0;
  const selectable = allItems.filter((i) => i.selectable !== false);
  const startWith = Array.isArray(preview.selected) && preview.selected.length ? preview.selected : selectable.map((i) => i.key);
  const selected = new Set(startWith);
  let cur = preview;
  let busy = false;
  let seq = 0;
  let manual = [];
  const model = { reason: '' };

  const countLine = h('p', { class: 'undo-count', 'data-test': 'undo-item-count' });
  const stepsSlot = h('div');
  const checkSlot = h('div');
  const warnSlot = h('div', { class: 'stack' });
  const manualSlot = h('div');
  const force = forceRows(() => validate());
  const forceSlot = h('div', { hidden: true }, force.el);
  const confirm = typedConfirm(preview.confirm_text || '', () => validate(), 'undo-confirm');
  const errBox = h('div', { 'aria-live': 'polite' });
  const secondPerson = preview.approval === 'second_person';

  const reason = h('textarea', {
    rows: '3', required: true, minlength: String(MIN_REASON), 'data-test': 'undo-reason',
    placeholder: 'Why is this being rolled back?',
    oninput: (e) => { model.reason = e.target.value; validate(); },
  });

  const drift = () => (cur && Array.isArray(cur.drift) ? cur.drift : []);

  function showPreview(pv) {
    replaceContent(stepsSlot, stepsBox(pv.steps));
    replaceContent(checkSlot, dryRunBox(pv));
    force.reset();
    forceSlot.hidden = drift().length === 0;
    replaceContent(warnSlot, (Array.isArray(pv.warnings) ? pv.warnings : []).map((w) => notice('warn', w)));
    manual = manualRows(pv.holds);
    replaceContent(manualSlot, manualFieldset(manual));
    if (pv.confirm_text) {
      confirm.set(pv.confirm_text);
    }
  }

  function updateCount() {
    if (!itemMode) {
      return;
    }
    const others = allItems.length - selectable.length;
    replaceContent(countLine, h('strong', null, `Undo ${selected.size} of ${allItems.length} items`),
      others > 0 ? h('span', { class: 'muted' }, ` (${others} already undone or waiting)`) : null);
  }

  const refresh = debounce(async () => {
    const mine = ++seq;
    if (selected.size === 0) {
      cur = null;
      busy = false;
      replaceContent(stepsSlot, notice('warn', 'Pick at least one item to undo.'));
      replaceContent(checkSlot);
      replaceContent(manualSlot);
      replaceContent(warnSlot);
      forceSlot.hidden = true;
      validate();
      return;
    }
    try {
      // A JSON array: group DNs contain commas.
      const pv = await api.get(path, { items: JSON.stringify([...selected]) });
      if (mine !== seq) {
        return;
      }
      cur = pv && pv.undoable ? pv : null;
      if (cur) {
        showPreview(cur);
      }
      else {
        replaceContent(checkSlot, notice('error', (pv && pv.reason) || 'These items cannot be undone.'));
      }
    }
    catch (e) {
      if (mine !== seq) {
        return;
      }
      cur = null;
      replaceContent(checkSlot, errorBox(e));
    }
    busy = false;
    validate();
  }, 300);

  // The selection changed: the preview is stale until the server has checked the new selection.
  function kick() {
    updateCount();
    busy = true;
    cur = null;
    replaceContent(checkSlot, loading('Checking these items in AD…'));
    validate();
    refresh();
  }

  function onSelect(key, on) {
    if (on) {
      selected.add(key);
    }
    else {
      selected.delete(key);
    }
    kick();
  }

  const itemBoxes = allItems.map((it) => {
    const cb = h('input', {
      type: 'checkbox', checked: selected.has(it.key), disabled: it.selectable === false,
      'data-test': 'undo-item', 'data-key': it.key,
      onchange: (e) => onSelect(it.key, e.target.checked),
    });
    cb.id = uid('item');
    return { it, cb, row: h('div', { class: 'check undo-item-row' }, cb, h('label', { for: cb.id }, it.label || it.key), ' ', itemBadges(it)) };
  });
  const setAll = (on) => {
    for (const b of itemBoxes) {
      if (!b.cb.disabled && b.cb.checked !== on) {
        b.cb.checked = on;
        if (on) {
          selected.add(b.it.key);
        }
        else {
          selected.delete(b.it.key);
        }
      }
    }
    kick();
  };
  const itemsBox = itemMode
    ? h('fieldset', { class: 'undo-items', 'data-test': 'undo-items' },
      h('legend', null, 'What to undo'),
      h('p', { class: 'help' }, 'Untick what you want to keep. Only the ticked items are rolled back, checked and approved; the rest of the change stays as it is and can be undone later.'),
      h('div', { class: 'row-gap' },
        h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'undo-items-all', onclick: () => setAll(true) }, 'All'),
        h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'undo-items-none', onclick: () => setAll(false) }, 'None')),
      countLine,
      itemBoxes.map((b) => b.row))
    : null;

  const content = h('div', { class: 'stack undo-dialog', 'data-test': 'undo-dialog' },
    notice('warn', h('strong', null, 'This writes to Active Directory'), ` (or Microsoft 365) to put ${who} back the way they were before this change. Read every line below before you continue.`),
    h('div', { class: 'detail-cols' },
      h('div', null,
        h('h3', null, `The ${changeTypeName(change.type)} change being undone`),
        Array.isArray(change.summary) && change.summary.length ? list(change.summary) : h('p', { class: 'muted' }, 'No summary.'),
        h('p', { class: 'muted small' }, `Applied ${fmtTime(change.applied_at) || ''}${change.decided_by ? `, approved by ${change.decided_by}` : ''}.`)),
      h('div', null, h('h3', null, 'What the undo will do'), stepsSlot)),
    itemsBox,
    h('div', null, h('h3', null, 'Live check (dry run, nothing is changed)'), checkSlot),
    forceSlot,
    warnSlot,
    manualSlot,
    field('Reason', reason, { required: true, help: `Required, at least ${MIN_REASON} characters. Stored with the undo and in the audit log.` }),
    confirm.wrap,
    notice('info', 'The undo is not applied straight away: it waits in Approvals and is never approved automatically.',
      secondPerson ? ' Another person must approve it.' : ''),
    errBox);

  let submitBtn = null;
  function validate() {
    const ok = !busy && Boolean(cur)
      && (!itemMode || selected.size > 0)
      && model.reason.trim().length >= MIN_REASON && confirm.ok()
      && (drift().length === 0 || force.confirmed());
    if (submitBtn) {
      submitBtn.disabled = !ok;
    }
    return ok;
  }

  showPreview(preview);
  updateCount();

  let requested = false;
  return new Promise((resolve) => {
    const modal = formModal({
      title: itemMode ? `Undo items of the ${changeTypeName(change.type)} change for ${who}?` : `Undo the ${changeTypeName(change.type)} change for ${who}?`,
      wide: true,
      danger: true,
      submitLabel: 'Request undo',
      content,
      initialFocus: 'textarea',
      onSubmit: async () => {
        if (!validate()) {
          return false;
        }
        const body = {
          reason: model.reason.trim(),
          confirm: confirm.input.value.trim(),
          force: drift().length > 0 && force.confirmed(),
          holds: chosenManual(manual),
          preview_hash: cur.preview_hash || '',
        };
        if (itemMode) {
          body.items = [...selected];
        }
        try {
          await api.post(path, body);
        }
        catch (e) {
          if (e.status === 409 || e.status === 400) {
            replaceContent(errBox, h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'undo-error' }, e.message || 'The undo was refused.'));
            return false;
          }
          throw e;
        }
        requested = true;
        return true;
      },
      onClose: () => {
        refresh.cancel();
        if (requested) {
          toast('Undo requested. It waits in Approvals.', 'ok');
        }
        resolve(requested);
      },
    });
    submitBtn = modal.el.querySelector('button[type="submit"]');
    submitBtn.setAttribute('data-test', 'undo-submit');
    submitBtn.setAttribute('data-mutating', '');
    validate();
  });
}

/**
 * Approval of an undo change: a fresh dry run, who asked and why, and a typed confirmation.
 * Resolves true when approved.
 */
export async function approveUndo(change, { me = '', secondPerson = false } = {}) {
  const p = change.payload || {};
  const u = p.undo || {};
  const who = change.display || change.external_id;
  const checkBox = h('div', null, loading('Checking the account in AD…'));
  const confirmText = { value: '' };
  let confirm = null;
  const confirmSlot = h('div');
  const errBox = h('div', { 'aria-live': 'polite' });
  const sameUser = secondPerson && u.requested_by && me && u.requested_by.toLowerCase() === me.toLowerCase();
  let submitBtn = null;
  let blocked = sameUser;
  const validate = () => {
    if (submitBtn) {
      submitBtn.disabled = blocked || !confirm || !confirm.ok();
    }
  };

  const content = h('div', { class: 'stack undo-dialog', 'data-test': 'undo-approve-dialog' },
    notice('warn', h('strong', null, 'Approving rolls back an applied change'), ` for ${who}. It is applied by the next executor pass.`),
    h('div', { class: 'detail-cols' },
      h('div', null,
        h('h3', null, 'Requested'),
        h('p', null, `By ${u.requested_by || 'unknown'}${change.created_at ? `, ${fmtTime(change.created_at)}` : ''}.`),
        h('p', null, h('strong', null, 'Reason: '), u.reason || change.decision_note || ''),
        h('p', { class: 'muted small' }, `Undoes the ${changeTypeName(u.of_type || '')} change ${u.of || change.undo_of || ''}.`),
        Array.isArray(u.items) && u.items.length
          ? h('p', { 'data-test': 'undo-approve-items' }, h('strong', null, 'Only these items: '), u.items.join(', '))
          : null,
        u.batch ? h('p', { class: 'muted small' }, `Part of the batch ${u.batch}; the whole batch can be approved at once from Approvals.`) : null,
        u.force ? notice('error', 'The requester chose to overwrite later changes to this account.') : null),
      h('div', null, h('h3', null, 'What the undo will do'), stepsBox(change.summary))),
    h('div', null, h('h3', null, 'Live check (dry run, nothing is changed)'), checkBox),
    sameUser ? notice('error', 'You requested this undo. Another person must approve it.') : null,
    confirmSlot,
    errBox);

  let approved = false;
  let modal = null;
  const done = new Promise((resolve) => {
    modal = formModal({
      title: `Approve the undo for ${who}?`,
      wide: true,
      danger: true,
      submitLabel: 'Approve undo',
      content,
      onSubmit: async () => {
        if (blocked || !confirm || !confirm.ok()) {
          return false;
        }
        try {
          await api.post(`/changes/${encodeURIComponent(change.id)}/approve`, { note: '', confirm: confirm.input.value.trim() });
        }
        catch (e) {
          if ([400, 403, 409].includes(e.status)) {
            replaceContent(errBox, h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'undo-error' }, e.message || 'Not approved.'));
            return false;
          }
          throw e;
        }
        approved = true;
        return true;
      },
      onClose: () => {
        if (approved) {
          toast('Undo approved.', 'ok');
        }
        resolve(approved);
      },
    });
  });
  submitBtn = modal.el.querySelector('button[type="submit"]');
  submitBtn.setAttribute('data-test', 'undo-approve-submit');
  submitBtn.setAttribute('data-mutating', '');
  validate();

  try {
    const res = await api.get(`/changes/${encodeURIComponent(change.id)}/undo-check`);
    replaceContent(checkBox, dryRunBox(res || {}), Array.isArray(res && res.steps) && res.steps.length ? h('div', { class: 'small muted' }, 'Steps now: ', res.steps.join('; ')) : null);
    confirmText.value = (res && res.confirm_text) || '';
  }
  catch (e) {
    replaceContent(checkBox, errorBox(e));
  }
  if (confirmText.value) {
    confirm = typedConfirm(confirmText.value, validate, 'undo-approve-confirm');
    replaceContent(confirmSlot, confirm.wrap);
    if (!sameUser) {
      confirm.input.focus();
    }
  }
  else {
    blocked = true;
    replaceContent(confirmSlot, notice('error', 'The account to confirm could not be determined; the undo cannot be approved from here.'));
  }
  validate();
  return done;
}

// ---------------------------------------------------------------- several changes of one run

/**
 * Undo some or all changes of one sync run: pick changes (and items of updates / cloud changes), preview them all with
 * one live dry run, then request one undo per change as a batch. Resolves the batch id, or false.
 */
export async function requestRunUndo(run) {
  const base = `/runs/${encodeURIComponent(run.id)}/undo`;
  let data;
  try {
    data = await api.get(base);
  }
  catch (e) {
    toast(e.message, 'error');
    return false;
  }
  const rows = Array.isArray(data && data.changes) ? data.changes : [];
  // selection: change id -> Set of item keys, or the string '*' for a change undone as a whole
  const sel = new Map();
  const model = { reason: '', manual: true };
  let cur = null;
  const resultSlot = h('div', { class: 'stack', 'data-test': 'run-undo-result' });
  const errBox = h('div', { 'aria-live': 'polite' });
  const force = forceRows(() => validate(), 'run-undo');
  const forceSlot = h('div', { hidden: true }, force.el);
  const confirm = typedConfirm('', () => validate(), 'run-undo-confirm', 'The number of undo changes this creates. It makes sure you meant all of them.');
  const reason = h('textarea', {
    rows: '3', required: true, minlength: String(MIN_REASON), 'data-test': 'run-undo-reason',
    placeholder: 'Why are these changes being rolled back?',
    oninput: (e) => { model.reason = e.target.value; validate(); },
  });
  const manualCb = h('input', { type: 'checkbox', checked: true, 'data-test': 'run-undo-manual', onchange: (e) => { model.manual = e.target.checked; } });
  manualCb.id = uid('manual');
  const countLine = h('p', { class: 'muted', 'data-test': 'run-undo-count' });
  let submitBtn = null;
  const previewBtn = h('button', { type: 'button', class: 'btn btn-primary', 'data-test': 'run-undo-preview', onclick: () => doPreview() }, 'Preview');

  const selection = () => {
    const out = {};
    for (const [id, v] of sel) {
      if (v === '*') {
        out[id] = ['*'];
      }
      else if (v.size > 0) {
        out[id] = [...v];
      }
    }
    return out;
  };
  const selectedCount = () => Object.keys(selection()).length;

  function changed() {
    cur = null;
    force.reset();
    forceSlot.hidden = true;
    confirm.set('');
    const n = selectedCount();
    replaceContent(countLine, n === 0 ? 'Nothing selected.' : `${n} change${n === 1 ? '' : 's'} selected.`);
    replaceContent(resultSlot, n > 0 ? notice('info', 'Click Preview to check the selection against AD before you can request it.') : null);
    previewBtn.disabled = n === 0;
    if (submitBtn) {
      submitBtn.textContent = 'Request undo';
    }
    validate();
  }

  // One block per person, one row per change, item check-boxes below updates and cloud changes.
  const byPerson = new Map();
  for (const r of rows) {
    const c = r.change || {};
    const k = c.identity_id || c.external_id || c.id;
    if (!byPerson.has(k)) {
      byPerson.set(k, { name: c.display || c.external_id || k, rows: [] });
    }
    byPerson.get(k).rows.push(r);
  }
  const allChangeBoxes = [];
  const people = [...byPerson.values()].sort((a, b) => a.name.localeCompare(b.name)).map((p) => h('div', { class: 'run-undo-person' },
    h('h3', null, p.name),
    p.rows.map((r) => {
      const c = r.change || {};
      const items = Array.isArray(r.items) ? r.items : [];
      const itemMode = !r.whole_only && items.length > 0;
      const selectable = items.filter((i) => i.selectable !== false);
      const changeCb = h('input', { type: 'checkbox', disabled: !r.undoable, 'data-test': 'run-undo-change', 'data-id': c.id });
      changeCb.id = uid('chg');
      const itemCbs = itemMode ? items.map((it) => {
        const cb = h('input', { type: 'checkbox', disabled: !r.undoable || it.selectable === false, 'data-test': 'run-undo-item', 'data-change': c.id, 'data-key': it.key });
        cb.id = uid('it');
        cb.addEventListener('change', () => {
          const set = sel.get(c.id) instanceof Set ? sel.get(c.id) : new Set();
          if (cb.checked) {
            set.add(it.key);
          }
          else {
            set.delete(it.key);
          }
          sel.set(c.id, set);
          changeCb.checked = set.size > 0;
          changeCb.indeterminate = set.size > 0 && set.size < selectable.length;
          changed();
        });
        return { it, cb, row: h('div', { class: 'check' }, cb, h('label', { for: cb.id }, it.label || it.key), ' ', itemBadges(it)) };
      }) : [];
      const setChange = (on) => {
        if (!r.undoable) {
          return;
        }
        changeCb.checked = on;
        changeCb.indeterminate = false;
        if (!itemMode) {
          if (on) {
            sel.set(c.id, '*');
          }
          else {
            sel.delete(c.id);
          }
          return;
        }
        const set = new Set();
        for (const b of itemCbs) {
          if (!b.cb.disabled) {
            b.cb.checked = on;
            if (on) {
              set.add(b.it.key);
            }
          }
        }
        sel.set(c.id, set);
      };
      changeCb.addEventListener('change', () => { setChange(changeCb.checked); changed(); });
      allChangeBoxes.push(setChange);
      return h('div', { class: `run-undo-change${r.undoable ? '' : ' is-disabled'}`, 'data-test': 'run-undo-row', 'data-id': c.id },
        h('div', { class: 'check' }, changeCb,
          h('label', { for: changeCb.id }, typeTag(c.type), ' ', (Array.isArray(c.summary) && c.summary[0]) || c.id),
          c.applied_at ? h('span', { class: 'muted small' }, ` applied ${fmtTime(c.applied_at)}`) : null),
        !r.undoable ? h('p', { class: 'muted small', 'data-test': 'run-undo-reason-not' }, r.reason || 'Cannot be undone.') : null,
        itemMode ? h('div', { class: 'run-undo-items' }, itemCbs.map((b) => b.row)) : null);
    })));

  const content = h('div', { class: 'stack undo-dialog', 'data-test': 'run-undo-dialog' },
    notice('warn', h('strong', null, 'This writes to Active Directory'), ' (or Microsoft 365) for every change you select: each one is put back the way it was before this run. Select only what you mean to roll back.'),
    rows.length === 0
      ? notice('info', 'This run applied no changes.')
      : h('div', null,
        h('div', { class: 'row-gap' },
          h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'run-undo-select-all', onclick: () => { allChangeBoxes.forEach((f) => f(true)); changed(); } }, 'Select every undoable change'),
          h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'run-undo-clear', onclick: () => { allChangeBoxes.forEach((f) => f(false)); changed(); } }, 'Clear')),
        h('div', { class: 'run-undo-list' }, people)),
    h('div', { class: 'row-gap' }, previewBtn, countLine),
    resultSlot,
    forceSlot,
    h('div', { class: 'check' }, manualCb, h('label', { for: manualCb.id }, 'Mark every undone item as Manual, so the next sync does not simply redo it (set back to Auto on each person\'s page)')),
    field('Reason', reason, { required: true, help: `Required, at least ${MIN_REASON} characters. Stored with every undo and in the audit log.` }),
    confirm.wrap,
    notice('info', 'Nothing is applied straight away: the undos wait in Approvals as one batch and are never approved automatically.'),
    errBox);

  async function doPreview() {
    const body = { selection: selection() };
    if (Object.keys(body.selection).length === 0) {
      return;
    }
    replaceContent(resultSlot, loading('Checking every selected change in AD…'));
    previewBtn.disabled = true;
    try {
      const res = await api.post(`${base}/preview`, body);
      cur = { ...res, selection: JSON.stringify(body.selection) };
    }
    catch (e) {
      cur = null;
      replaceContent(resultSlot, h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'run-undo-preview-error' }, e.message || 'The preview failed.'));
      previewBtn.disabled = false;
      validate();
      return;
    }
    previewBtn.disabled = false;
    const list2 = Array.isArray(cur.changes) ? cur.changes : [];
    const driftN = Number(cur.drift_count) || list2.filter((x) => Array.isArray(x.drift) && x.drift.length).length;
    replaceContent(resultSlot,
      h('p', { 'data-test': 'run-undo-summary' }, h('strong', null, `${list2.length} undo change${list2.length === 1 ? '' : 's'}`), ` for ${cur.people || list2.length} ${(cur.people || list2.length) === 1 ? 'person' : 'people'}.`,
        driftN ? h('span', { class: 'text-danger' }, ` ${driftN} find${driftN === 1 ? 's' : ''} later changes.`) : null),
      (Array.isArray(cur.warnings) ? cur.warnings : []).map((w) => notice('warn', w)),
      h('div', { class: 'run-undo-preview-list' }, list2.map((x) => h('div', { class: 'run-undo-preview-item', 'data-test': 'run-undo-entry', 'data-id': x.change_id },
        h('h4', null, x.display || x.external_id || x.change_id),
        stepsBox(x.steps),
        dryRunBox(x)))));
    forceSlot.hidden = driftN === 0;
    confirm.set(cur.confirm_text || `UNDO ${list2.length}`);
    if (submitBtn) {
      submitBtn.textContent = `Request undo for ${list2.length} change${list2.length === 1 ? '' : 's'}`;
    }
    validate();
  }

  function validate() {
    const fresh = Boolean(cur) && cur.selection === JSON.stringify(selection());
    const driftN = cur ? (Number(cur.drift_count) || 0) : 0;
    const ok = fresh && model.reason.trim().length >= MIN_REASON && confirm.ok() && (driftN === 0 || force.confirmed());
    if (submitBtn) {
      submitBtn.disabled = !ok;
    }
    return ok;
  }

  let batch = false;
  return new Promise((resolve) => {
    const modal = formModal({
      title: `Undo changes from the run of ${fmtTime(run.started_at) || run.id}?`,
      wide: true,
      danger: true,
      submitLabel: 'Request undo',
      content,
      onSubmit: async () => {
        if (!validate()) {
          return false;
        }
        const body = {
          selection: selection(),
          reason: model.reason.trim(),
          confirm: confirm.input.value.trim(),
          force: (Number(cur.drift_count) || 0) > 0 && force.confirmed(),
          manual: model.manual,
          preview_hash: cur.preview_hash || '',
        };
        try {
          const res = await api.post(base, body);
          batch = (res && res.batch) || true;
        }
        catch (e) {
          if (e.status === 409 || e.status === 400) {
            replaceContent(errBox, h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'run-undo-error' }, e.message || 'The undo was refused.'));
            return false;
          }
          throw e;
        }
        return true;
      },
      onClose: () => {
        if (batch) {
          toast('Undo requested for the selected changes. The batch waits in Approvals.', 'ok');
        }
        resolve(batch);
      },
    });
    submitBtn = modal.el.querySelector('button[type="submit"]');
    submitBtn.setAttribute('data-test', 'run-undo-submit');
    submitBtn.setAttribute('data-mutating', '');
    changed();
  });
}

/**
 * Approving (or rejecting) every waiting undo of a batch at once: a fresh dry run of each, who asked and why,
 * and a typed "UNDO n". Resolves true when the batch was approved or rejected.
 */
export async function approveBatch(batchId, { me = '', secondPerson = false } = {}) {
  const path = `/undo-batches/${encodeURIComponent(batchId)}`;
  const body = h('div', null, loading('Checking every account of the batch in AD…'));
  const confirmSlot = h('div');
  const errBox = h('div', { 'aria-live': 'polite' });
  let confirm = null;
  let blocked = true;
  let submitBtn = null;
  let outcome = false;
  let modal = null;
  const validate = () => {
    if (submitBtn) {
      submitBtn.disabled = blocked || !confirm || !confirm.ok();
    }
  };

  const note = h('textarea', { rows: '2', 'data-test': 'batch-reject-note', placeholder: 'Why are these undos rejected?', oninput: () => { rejectBtn.disabled = !note.value.trim(); } });
  const rejectBtn = h('button', {
    type: 'button', class: 'btn btn-danger', disabled: true, 'data-mutating': '', 'data-test': 'batch-reject-submit',
    onclick: async () => {
      rejectBtn.disabled = true;
      try {
        await api.post(`${path}/reject`, { note: note.value.trim() });
        outcome = true;
        toast('Batch rejected.', 'ok');
        modal.close(true);
      }
      catch (e) {
        replaceContent(errBox, h('div', { class: 'notice notice-error', role: 'alert' }, e.message));
        rejectBtn.disabled = false;
      }
    },
  }, 'Reject every waiting undo of the batch');
  const rejectBox = h('details', { class: 'batch-reject' }, h('summary', { 'data-test': 'batch-reject' }, 'Reject the batch instead…'),
    h('div', { class: 'stack' }, field('Note', note, { help: 'Required to reject.' }), h('div', null, rejectBtn)));

  const content = h('div', { class: 'stack undo-dialog', 'data-test': 'batch-approve-dialog' },
    notice('warn', h('strong', null, 'Approving rolls back every applied change listed below'), '. They are applied by the next executor pass.'),
    body, confirmSlot, rejectBox, errBox);

  const done = new Promise((resolve) => {
    modal = formModal({
      title: 'Approve the undo batch?',
      wide: true,
      danger: true,
      submitLabel: 'Approve batch',
      content,
      onSubmit: async () => {
        if (blocked || !confirm || !confirm.ok()) {
          return false;
        }
        try {
          await api.post(`${path}/approve`, { confirm: confirm.input.value.trim(), note: '' });
        }
        catch (e) {
          if ([400, 403, 409].includes(e.status)) {
            replaceContent(errBox, h('div', { class: 'notice notice-error', role: 'alert', 'data-test': 'batch-error' }, e.message || 'Not approved.'));
            return false;
          }
          throw e;
        }
        outcome = true;
        toast('Undo batch approved.', 'ok');
        return true;
      },
      onClose: () => resolve(outcome),
    });
  });
  submitBtn = modal.el.querySelector('button[type="submit"]');
  submitBtn.setAttribute('data-test', 'batch-approve-submit');
  submitBtn.setAttribute('data-mutating', '');
  validate();

  let b;
  try {
    b = await api.get(path);
  }
  catch (e) {
    replaceContent(body, errorBox(e));
    return done;
  }
  const entries = Array.isArray(b && b.changes) ? b.changes : [];
  const driftN = Number(b.drift_count) || entries.filter((x) => Array.isArray(x.drift) && x.drift.length).length;
  const sameUser = secondPerson && b.requested_by && me && String(b.requested_by).toLowerCase() === me.toLowerCase();
  replaceContent(body,
    h('div', { class: 'detail-cols' },
      h('div', null,
        h('h3', null, 'Requested'),
        h('p', null, `By ${b.requested_by || 'unknown'}${b.created_at ? `, ${fmtTime(b.created_at)}` : ''}.`),
        h('p', null, h('strong', null, 'Reason: '), b.reason || '')),
      h('div', null,
        h('h3', null, 'Waiting'),
        h('p', { 'data-test': 'batch-count' }, `${entries.length} undo change${entries.length === 1 ? '' : 's'} waiting${b.total && b.total !== entries.length ? ` (of ${b.total} in the batch; the others were already decided)` : ''}.`),
        b.force ? notice('error', 'The requester chose to overwrite later changes.') : null)),
    h('div', { class: 'run-undo-preview-list' }, entries.map((x) => h('div', { class: 'run-undo-preview-item', 'data-test': 'batch-entry', 'data-id': x.change_id },
      h('h4', null, x.display || x.external_id || x.change_id),
      stepsBox(x.steps),
      dryRunBox(x)))),
    driftN > 0 ? notice('error', `${driftN} of these now find later changes to the account. Approve or reject those one at a time; the batch cannot be approved as a whole.`) : null,
    sameUser ? notice('error', 'You requested this batch. Another person must approve it.') : null);
  blocked = entries.length === 0 || driftN > 0 || sameUser;
  if (b.confirm_text && !blocked) {
    confirm = typedConfirm(b.confirm_text, validate, 'batch-approve-confirm', 'The number of undo changes you approve.');
    replaceContent(confirmSlot, confirm.wrap);
    confirm.input.focus();
  }
  validate();
  return done;
}
