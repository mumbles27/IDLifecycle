// Approvals: the change queue. Filter, expand, approve / reject / retry, bulk actions.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, pill, fmtTime, relTime, dataTable, pager, listItems, debounce,
  act, confirmDialog, noteDialog, toast, errorBox, loading, field, codeBlock, dl, pretty, truncCell,
  formModal, notice, openModal,
} from '../ui.js';
import { ouPicker, conflictList, fieldMatches, NAME_FIELDS } from '../components.js';

const STATUSES = ['pending', 'approved', 'applying', 'applied', 'failed', 'rejected', 'superseded', 'simulated'];
const TYPES = ['create', 'update', 'disable', 'enable', 'delete', 'link'];

const SAM_PATTERN = '[A-Za-z0-9._\\-]{1,20}';

function linkOf(change) {
  const p = change.payload || {};
  return p.link && typeof p.link === 'object' ? p.link : null;
}

/** Why Approve is not possible yet ('' = it is). */
function approveBlocker(change) {
  const p = change.payload || {};
  if (p.needs_ou) {
    return 'Choose the OU first';
  }
  if (p.needs_name) {
    return 'Set the username first';
  }
  const link = linkOf(change);
  if (change.type === 'link' && link && !link.chosen) {
    return 'Choose the matching account first';
  }
  return '';
}

/** Read-only view of a link change: the candidates and the planned rename. */
export function linkCandidatesTable(link, { onChoose } = {}) {
  const cands = Array.isArray(link.candidates) ? link.candidates : [];
  const chosen = cands.find((c) => c.object_guid === link.chosen) || null;
  const rows = cands.map((c) => {
    const isChosen = c.object_guid === link.chosen;
    return h('tr', { class: isChosen ? 'row-chosen' : '', 'data-test': 'link-candidate', 'data-guid': c.object_guid },
      h('td', { 'data-label': 'Account' },
        h('div', null, h('strong', null, c.sam || '(no sAMAccountName)'), isChosen ? [' ', h('span', { class: 'pill pill-approved', 'data-test': 'link-chosen' }, 'chosen')] : null),
        h('div', { class: 'mono small wrap' }, c.dn || ''),
        c.upn ? h('div', { class: 'muted small' }, c.upn) : null),
      h('td', { 'data-label': 'Current ID' }, c.external_id ? h('code', null, c.external_id) : h('span', { class: 'muted' }, 'none')),
      h('td', { 'data-label': 'Enabled' }, pill(c.enabled ? 'enabled' : 'disabled')),
      h('td', { 'data-label': 'Belongs to' }, c.identity_id
        ? h('div', null, h('a', { href: `#/identities/${encodeURIComponent(c.identity_id)}` }, c.identity_display || c.identity_id), c.source_name ? h('div', { class: 'muted small' }, `source: ${c.source_name}`) : null)
        : h('span', { class: 'muted' }, 'nobody')),
      h('td', { 'data-label': 'Match' },
        c.manual ? h('span', { class: 'pill pill-neutral' }, 'chosen by hand')
          : h('span', { class: c.full ? 'pill pill-ok' : 'pill pill-pending', 'data-test': c.full ? 'match-full' : 'match-partial' }, c.full ? 'full match' : 'partial match'),
        fieldMatches(c.fields)),
      onChoose ? h('td', { 'data-label': '' }, isChosen && !link.verify ? null : h('button', {
        type: 'button', class: 'btn btn-sm btn-primary', 'data-mutating': '', 'data-test': 'link-choose',
        onclick: (e) => onChoose(c, e.currentTarget),
      }, isChosen ? 'Confirm: this is the person' : 'This is the person')) : null);
  });
  let rename;
  if (link.rename) {
    const r = link.rename;
    rename = h('div', { class: 'notice notice-info', 'data-test': 'link-rename' },
      h('strong', null, 'Rename on link: '),
      chosen ? `${chosen.sam || '?'} -> ${r.sam}` : r.sam,
      chosen && chosen.upn && r.upn && chosen.upn.toLowerCase() !== r.upn.toLowerCase() ? `, ${chosen.upn} -> ${r.upn}` : '',
      r.mail ? `, mail ${r.mail}` : '',
      r.cn ? `, CN "${r.cn}"` : '');
  }
  else if (link.rename_note) {
    rename = h('p', { class: 'help', 'data-test': 'link-rename-note' }, link.rename_note);
  }
  return h('div', { class: 'stack' },
    link.verify || !link.chosen
      ? notice('warn', !link.chosen
        ? 'Several AD accounts could be this person. Choose the right one, or create a new account if none of them is.'
        : 'This is a partial match: check the differences below before approving.')
      : null,
    h('div', { class: 'table-wrap' }, h('table', { class: 'data', 'data-test': 'link-candidates' },
      h('caption', { class: 'sr-only' }, 'Possible AD accounts for this person'),
      h('thead', null, h('tr', null,
        h('th', { scope: 'col' }, 'Account'), h('th', { scope: 'col' }, 'Current ID'), h('th', { scope: 'col' }, 'Enabled'),
        h('th', { scope: 'col' }, 'Belongs to'), h('th', { scope: 'col' }, 'Match'), onChoose ? h('th', { scope: 'col' }, h('span', { class: 'sr-only' }, 'Actions')) : null)),
      h('tbody', null, rows))),
    rename);
}

export async function render(root, ctx) {
  const canDecide = ctx.can('approver');
  const state = {
    status: ctx.query.has('status') ? ctx.query.get('status') : 'pending',
    type: ctx.query.get('type') || '',
    q: ctx.query.get('q') || '',
    limit: 25,
    offset: 0,
  };
  const selected = new Set();

  const statusSel = h('select', {
    id: 'flt-status',
    'data-test': 'flt-status',
    value: state.status,
    onchange: () => { state.status = statusSel.value; state.offset = 0; load(); },
  }, h('option', { value: '' }, 'All statuses'), STATUSES.map((s) => h('option', { value: s }, s)));

  const typeSel = h('select', {
    id: 'flt-type',
    'data-test': 'flt-type',
    value: state.type,
    onchange: () => { state.type = typeSel.value; state.offset = 0; load(); },
  }, h('option', { value: '' }, 'All types'), TYPES.map((s) => h('option', { value: s }, s)));

  const search = h('input', {
    id: 'flt-q',
    type: 'search',
    'data-test': 'flt-q',
    placeholder: 'Name or employee ID',
    value: state.q,
    oninput: debounce(() => { state.q = search.value.trim(); state.offset = 0; load(); }, 350),
  });

  const filters = h('div', { class: 'filters' },
    field('Status', statusSel),
    field('Type', typeSel),
    field('Search', search),
    h('button', { type: 'button', class: 'btn', onclick: () => load() }, 'Refresh'));

  const bulkBar = h('div', { class: 'bulk-bar', hidden: true, 'data-test': 'bulk-bar' });
  const listEl = h('div', null, loading());
  root.append(pageHeader('Approvals'), filters, bulkBar, listEl);

  function updateBulkBar() {
    if (!canDecide || selected.size === 0) {
      bulkBar.hidden = true;
      return;
    }
    bulkBar.hidden = false;
    replaceContent(bulkBar,
      h('strong', null, `${selected.size} selected`),
      h('button', { type: 'button', class: 'btn btn-primary btn-sm', 'data-mutating': '', 'data-test': 'bulk-approve', onclick: (e) => bulk('approve', e.currentTarget) }, 'Approve selected'),
      h('button', { type: 'button', class: 'btn btn-danger btn-sm', 'data-mutating': '', 'data-test': 'bulk-reject', onclick: (e) => bulk('reject', e.currentTarget) }, 'Reject selected…'),
      h('button', { type: 'button', class: 'btn btn-sm', onclick: () => { selected.clear(); load(); } }, 'Clear selection'));
  }

  async function bulk(action, button) {
    const ids = Array.from(selected);
    let note = '';
    if (action === 'reject') {
      note = await noteDialog({
        title: `Reject ${ids.length} change(s)`,
        message: 'These changes will be rejected and never applied. The note is stored with each decision.',
        label: 'Reason for rejection',
        required: true,
        confirmLabel: 'Reject',
        danger: true,
      });
      if (note === null) {
        return;
      }
    }
    else {
      const ok = await confirmDialog({
        title: `Approve ${ids.length} change(s)`,
        message: 'Approved changes are applied to Active Directory by the next executor pass (or simulated when dry run is on).',
        confirmLabel: 'Approve',
      });
      if (!ok) {
        return;
      }
    }
    await act(button, async () => {
      const res = await api.post('/changes/bulk', { ids, action, note });
      const results = (res && res.results) || [];
      const failed = results.filter((r) => r.ok === false);
      const good = results.length - failed.length;
      if (good > 0) {
        toast(`${good} change(s) ${action === 'approve' ? 'approved' : 'rejected'}.`, 'ok');
      }
      if (failed.length > 0) {
        toast(`${failed.length} failed: ${failed.slice(0, 3).map((f) => f.error || f.id).join('; ')}`, 'error');
      }
      selected.clear();
      await load();
      ctx.refreshStatus();
    });
  }

  async function decide(change, action, button) {
    let note = '';
    if (action === 'reject') {
      const link = change.type === 'link' ? linkOf(change) : null;
      note = await noteDialog({
        title: link ? 'Not this person' : 'Reject change',
        message: link
          ? `Rejecting records that ${link.chosen ? 'the chosen account is' : 'none of these accounts is'} ${change.display || change.external_id}. The next sync tries any remaining candidates, or proposes a new account.`
          : `Reject ${change.type} for ${change.display || change.external_id}? It will never be applied.`,
        label: 'Reason for rejection',
        required: true,
        confirmLabel: 'Reject',
        danger: true,
      });
      if (note === null) {
        return;
      }
    }
    await act(button, async () => {
      await api.post(`/changes/${encodeURIComponent(change.id)}/${action}`, { note });
      const verb = { approve: 'approved', reject: 'rejected', retry: 'queued for retry' }[action];
      toast(`Change ${verb}.`, 'ok');
      selected.delete(change.id);
      await load();
      ctx.refreshStatus();
    });
  }

  function detail(change) {
    const box = h('div', { class: 'change-detail' });
    const fill = (c) => {
      const summary = Array.isArray(c.summary) ? c.summary : [];
      const p = c.payload || {};
      const link = linkOf(c);
      replaceContent(box,
        link ? h('div', { class: 'stack' }, h('h3', null, 'Possible AD accounts'), linkCandidatesTable(link)) : null,
        p.needs_name ? h('div', { class: 'stack' }, h('h3', null, 'Names already taken'), conflictList(p.name_conflicts, { empty: h('p', { class: 'muted' }, 'No candidate name is free.') })) : null,
        h('div', { class: 'detail-cols' },
          h('div', null,
            h('h3', null, 'What will happen'),
            summary.length > 0
              ? h('ul', { class: 'summary-list' }, summary.map((s) => h('li', null, String(s))))
              : h('p', { class: 'muted' }, 'No summary available.'),
            c.error ? h('div', { class: 'notice notice-error' }, h('strong', null, 'Error: '), c.error) : null,
            dl([
              ['Change ID', c.id],
              ['Run', c.run_id ? h('a', { href: `#/runs/${encodeURIComponent(c.run_id)}` }, c.run_id) : ''],
              ['Identity', c.identity_id ? h('a', { href: `#/identities/${encodeURIComponent(c.identity_id)}` }, c.external_id || c.identity_id) : c.external_id],
              ['Created', fmtTime(c.created_at)],
              ['Decided by', c.decided_by],
              ['Decided at', fmtTime(c.decided_at)],
              ['Decision note', c.decision_note],
              ['Applied at', fmtTime(c.applied_at)],
              ['Attempts', c.attempts === undefined ? '' : String(c.attempts)],
            ])),
          h('div', null,
            h('h3', null, 'Raw payload'),
            codeBlock(c.payload === undefined ? '(not provided)' : pretty(c.payload)))));
    };
    if (change.payload === undefined && change.summary === undefined) {
      replaceContent(box, loading());
      api.get(`/changes/${encodeURIComponent(change.id)}`).then(fill, (e) => replaceContent(box, errorBox(e)));
    }
    else {
      fill(change);
    }
    return box;
  }

  // A create always has an OU; an update or enable only when it moves the account (or waits for an OU).
  function hasOU(change) {
    const p = change.payload || {};
    return change.type === 'create' || p.needs_ou || !!p.move_to;
  }

  async function chooseOU(change, button) {
    const p = change.payload || {};
    const value = p.placement || '';
    const res = await ouPicker({
      title: p.needs_ou ? 'Choose the OU' : 'Change the OU',
      current: p.ou || p.move_to || '',
      note: `${change.display || change.external_id}: ${p.needs_ou ? `${p.ou_reason || 'no OU could be chosen automatically'}. ` : ''}The choice is kept for this person until their department changes.`,
      ruleLabel: ctx.can('admin') && value ? `Also use this OU for everyone in "${value}" (adds an OU placement rule)` : null,
    });
    if (!res) {
      return;
    }
    await act(button, async () => {
      await api.post(`/changes/${encodeURIComponent(change.id)}/ou`, { ou: res.ou, add_rule: res.addRule });
      toast(res.addRule ? 'OU set, and the placement rule added.' : 'OU set for this person.', 'ok');
      await load();
    });
  }

  // ---- names (needs_name) ----

  async function setName(change, button) {
    let info;
    try {
      info = await api.get(`/changes/${encodeURIComponent(change.id)}/names`);
    }
    catch (e) {
      toast(e.message, 'error');
      return;
    }
    const p = change.payload || {};
    const cur = (info && info.current) || ((p.candidates || [])[0]) || {};
    const suggestions = (info && Array.isArray(info.suggestions)) ? info.suggestions : [];
    const withMail = !!(cur.mail || suggestions.some((s) => s.candidate && s.candidate.mail) || (p.candidates || []).some((c) => c.mail));
    const model = { sam: cur.sam || '', upn: cur.upn || '', cn: cur.cn || '', mail: cur.mail || '' };
    const inputs = {
      sam: h('input', { type: 'text', required: true, maxlength: '20', pattern: SAM_PATTERN, class: 'mono', autocomplete: 'off', 'data-test': 'name-sam', value: model.sam, oninput: (e) => { model.sam = e.target.value.trim(); } }),
      upn: h('input', { type: 'text', required: true, pattern: '[^@\\s]+@[^@\\s]+', class: 'mono', autocomplete: 'off', 'data-test': 'name-upn', value: model.upn, oninput: (e) => { model.upn = e.target.value.trim(); } }),
      cn: h('input', { type: 'text', required: true, maxlength: '64', autocomplete: 'off', 'data-test': 'name-cn', value: model.cn, oninput: (e) => { model.cn = e.target.value.trim(); } }),
      mail: h('input', { type: 'email', required: withMail, class: 'mono', autocomplete: 'off', 'data-test': 'name-mail', value: model.mail, oninput: (e) => { model.mail = e.target.value.trim(); } }),
    };
    const fill = (c) => {
      for (const k of ['sam', 'upn', 'cn', 'mail']) {
        model[k] = c[k] || '';
        inputs[k].value = model[k];
      }
      checkBox.textContent = '';
    };
    const checkBox = h('div', { 'aria-live': 'polite', 'data-test': 'name-check' });
    const names = () => {
      const out = [{ field: 'sam', value: model.sam }, { field: 'upn', value: model.upn }];
      if (model.mail) {
        out.push({ field: 'mail', value: model.mail });
      }
      if (model.cn && p.ou) {
        out.push({ field: 'cn', value: model.cn, ou: p.ou });
      }
      return out.filter((n) => n.value);
    };
    const showTaken = (taken) => {
      replaceContent(checkBox, taken.length === 0
        ? notice('ok', 'All of these names are free.')
        : h('div', { class: 'notice notice-error' }, h('p', null, 'Already taken:'), conflictList(taken)));
    };
    const check = async (btnEl) => act(btnEl, async () => {
      const res = await api.post('/ad/check-names', { names: names() });
      showTaken((res && Array.isArray(res.taken)) ? res.taken : []);
    });
    const content = [
      h('p', null, `${change.display || change.external_id}: the generated name is already in use. Type the names to use, or pick a suggestion. The choice is kept for this person.`),
      conflictList(p.name_conflicts),
      suggestions.length > 0
        ? h('div', { class: 'stack' }, h('h3', null, 'Free suggestions'),
          h('div', { class: 'row-gap', 'data-test': 'name-suggestions' }, suggestions.map((s) => h('button', {
            type: 'button', class: 'btn btn-sm', 'data-test': 'name-suggestion',
            title: `${s.mode}: ${s.candidate.upn || ''}`,
            onclick: () => fill(s.candidate || {}),
          }, `${s.candidate.sam} (${s.mode})`))))
        : h('p', { class: 'muted' }, 'No free suggestion was found; type the names.'),
      h('div', { class: 'grid grid-2' },
        field(NAME_FIELDS.sam, inputs.sam, { required: true, help: 'Up to 20 characters: letters, digits, . _ -' }),
        field(NAME_FIELDS.upn, inputs.upn, { required: true, help: 'name@suffix' }),
        field(NAME_FIELDS.cn, inputs.cn, { required: true, help: 'The object name shown in AD (up to 64 characters).' }),
        withMail ? field(NAME_FIELDS.mail, inputs.mail, { required: true }) : null),
      h('div', { class: 'row-gap' }, h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'name-check-btn', onclick: (e) => check(e.currentTarget) }, 'Check in AD')),
      checkBox,
    ];
    let saved = false;
    formModal({
      title: 'Set the username',
      wide: true,
      submitLabel: 'Use these names',
      content,
      onSubmit: async () => {
        const body = { sam: model.sam, upn: model.upn, cn: model.cn };
        if (withMail || model.mail) {
          body.mail = model.mail;
        }
        try {
          await api.post(`/changes/${encodeURIComponent(change.id)}/name`, body);
        }
        catch (e) {
          if (e.status === 409) {
            const taken = (e.data && Array.isArray(e.data.conflicts)) ? e.data.conflicts : [];
            replaceContent(checkBox, h('div', { class: 'notice notice-error', role: 'alert' }, h('p', null, e.message || 'Already taken.'), conflictList(taken)));
            return false;
          }
          throw e;
        }
        saved = true;
        return true;
      },
      onClose: async () => {
        if (saved) {
          toast('Username set. Approve the change when ready.', 'ok');
          await load();
        }
      },
    });
    if (button) {
      button.blur();
    }
  }

  // ---- links ----

  async function reviewLink(change) {
    const link = linkOf(change) || { candidates: [] };
    let modal = null;
    let changed = false;
    const choose = async (cand, btnEl) => {
      await act(btnEl, async () => {
        await api.post(`/changes/${encodeURIComponent(change.id)}/link`, { object_guid: cand.object_guid });
        changed = true;
        toast(`${cand.sam || 'Account'} chosen. Approve the change to link it.`, 'ok');
        modal.close(true);
      });
    };
    const createNew = async (btnEl) => {
      const ok = await confirmDialog({
        title: 'Create a new account instead?',
        message: `None of these accounts is ${change.display || change.external_id}. This change is closed, and the next sync proposes a new account for this person (it is not matched again).`,
        confirmLabel: 'Create a new account',
        danger: true,
      });
      if (!ok) {
        return;
      }
      await act(btnEl, async () => {
        await api.post(`/changes/${encodeURIComponent(change.id)}/link`, { create_new: true });
        changed = true;
        toast('The next sync proposes a new account for this person.', 'ok');
        modal.close(true);
      });
    };
    const body = h('div', { class: 'stack' },
      h('p', null, `Who is ${change.display || change.external_id} in Active Directory? Linking keeps the existing account (with its mailbox and files) instead of creating a new one.`),
      linkCandidatesTable(link, { onChoose: choose }));
    const newBtn = h('button', { type: 'button', class: 'btn btn-danger', 'data-mutating': '', 'data-test': 'link-create-new', onclick: (e) => createNew(e.currentTarget) }, 'None of these: create a new account');
    const closeBtn = h('button', { type: 'button', class: 'btn', onclick: () => modal.close(false) }, 'Close');
    modal = openLinkModal(body, [newBtn, h('span', { class: 'spacer' }), closeBtn], async () => {
      if (changed) {
        await load();
        ctx.refreshStatus();
      }
    });
  }

  function actionsFor(change) {
    if (!canDecide) {
      return '';
    }
    const cell = h('div', { class: 'row-actions' });
    if (change.status === 'pending') {
      const p = change.payload || {};
      const needs = !!p.needs_ou;
      const blocker = approveBlocker(change);
      if (hasOU(change)) {
        cell.append(h('button', {
          type: 'button', class: needs ? 'btn btn-sm btn-primary' : 'btn btn-sm', 'data-mutating': '', 'data-test': 'choose-ou',
          onclick: (e) => chooseOU(change, e.currentTarget),
        }, needs ? 'Choose OU…' : 'Change OU…'));
      }
      if (p.needs_name) {
        cell.append(h('button', {
          type: 'button', class: 'btn btn-sm btn-primary', 'data-mutating': '', 'data-test': 'set-name', 'data-id': change.id,
          onclick: (e) => setName(change, e.currentTarget),
        }, 'Set username…'));
      }
      if (change.type === 'link') {
        const link = linkOf(change) || {};
        cell.append(h('button', {
          type: 'button', class: !link.chosen ? 'btn btn-sm btn-primary' : 'btn btn-sm', 'data-mutating': '', 'data-test': 'review-link', 'data-id': change.id,
          onclick: () => reviewLink(change),
        }, !link.chosen ? 'Choose account…' : 'Review match…'));
      }
      cell.append(
        h('button', { type: 'button', class: blocker ? 'btn btn-sm' : 'btn btn-sm btn-primary', 'data-mutating': '', 'data-id': change.id, 'data-test': 'approve', disabled: !!blocker, title: blocker, onclick: (e) => decide(change, 'approve', e.currentTarget) }, 'Approve'),
        h('button', { type: 'button', class: 'btn btn-sm btn-danger', 'data-mutating': '', 'data-test': 'reject', onclick: (e) => decide(change, 'reject', e.currentTarget) }, change.type === 'link' ? 'Not this person…' : 'Reject…'));
    }
    else if (change.status === 'failed') {
      cell.append(h('button', { type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'retry', onclick: (e) => decide(change, 'retry', e.currentTarget) }, 'Retry'));
    }
    return cell;
  }

  async function load() {
    let resp;
    try {
      resp = await api.get('/changes', {
        status: state.status, type: state.type, q: state.q, limit: state.limit, offset: state.offset,
      });
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const items = listItems(resp);
    const total = resp && Number.isFinite(resp.total) ? resp.total : items.length;
    const pageIds = items.filter((c) => c.status === 'pending').map((c) => c.id);
    for (const id of Array.from(selected)) {
      if (!pageIds.includes(id)) {
        selected.delete(id);
      }
    }

    const checkboxes = [];
    const selectAll = h('input', {
      type: 'checkbox',
      'aria-label': 'Select all pending changes on this page',
      'data-test': 'select-all',
      onchange: () => {
        for (const cb of checkboxes) {
          cb.checked = selectAll.checked;
          if (selectAll.checked) {
            selected.add(cb.dataset.id);
          }
          else {
            selected.delete(cb.dataset.id);
          }
        }
        updateBulkBar();
      },
    });

    const columns = [];
    if (canDecide) {
      columns.push({
        label: pageIds.length > 0 ? selectAll : h('span', { class: 'sr-only' }, 'Select'),
        className: 'col-check',
        render: (c) => {
          if (c.status !== 'pending') {
            return '';
          }
          const cb = h('input', {
            type: 'checkbox',
            checked: selected.has(c.id),
            'aria-label': `Select ${c.display || c.external_id}`,
            'data-id': c.id,
            'data-test': 'select-row',
            onchange: () => {
              if (cb.checked) {
                selected.add(c.id);
              }
              else {
                selected.delete(c.id);
              }
              selectAll.checked = checkboxes.every((x) => x.checked);
              updateBulkBar();
            },
          });
          checkboxes.push(cb);
          return cb;
        },
      });
    }
    columns.push(
      { label: 'Created', render: (c) => h('span', { title: fmtTime(c.created_at) }, relTime(c.created_at) || fmtTime(c.created_at)) },
      { label: 'Type', render: (c) => h('span', { class: `type-tag type-${c.type}` }, c.type) },
      {
        label: 'Identity',
        render: (c) => h('div', null, h('div', null, c.display || c.external_id), c.display && c.external_id ? h('div', { class: 'muted small' }, c.external_id) : null),
      },
      { label: 'Status', render: (c) => pill(c.status) },
      {
        label: 'Summary',
        render: (c) => {
          const s = Array.isArray(c.summary) ? c.summary : [];
          if (s.length === 0) {
            return c.error ? truncCell(c.error, 80) : '';
          }
          return h('div', null, truncCell(s[0], 90), s.length > 1 ? h('span', { class: 'muted small' }, ` (+${s.length - 1} more)`) : null);
        },
      },
      { label: 'Decided by', render: (c) => c.decided_by || '' },
      { label: 'Actions', className: 'col-actions', render: actionsFor },
    );

    const table = dataTable(columns, items, {
      caption: 'Changes',
      empty: 'No changes match these filters.',
      expand: detail,
      rowClass: (c) => `status-row-${c.status}`,
    });
    replaceContent(listEl, table, pager({
      total, limit: state.limit, offset: state.offset,
      onChange: (off, lim) => { state.offset = off; state.limit = lim; load(); },
    }));
    // keep the page-size select in sync
    const sizeSel = listEl.querySelector('.pager select');
    if (sizeSel) {
      sizeSel.value = String(state.limit);
    }
    updateBulkBar();
  }

  await load();
}

function openLinkModal(body, footer, onClose) {
  return openModal({ title: 'Match this person to an AD account', content: body, footer, wide: true, onClose });
}
