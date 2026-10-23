// Approvals: the change queue. Filter, expand, approve / reject / retry, bulk actions.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, pill, fmtTime, relTime, dataTable, pager, listItems, debounce,
  act, confirmDialog, noteDialog, toast, errorBox, loading, field, codeBlock, dl, pretty, truncCell,
} from '../ui.js';

const STATUSES = ['pending', 'approved', 'applying', 'applied', 'failed', 'rejected', 'superseded', 'simulated'];
const TYPES = ['create', 'update', 'disable', 'enable', 'delete'];

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
      note = await noteDialog({
        title: 'Reject change',
        message: `Reject ${change.type} for ${change.display || change.external_id}? It will never be applied.`,
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
      replaceContent(box,
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

  function actionsFor(change) {
    if (!canDecide) {
      return '';
    }
    const cell = h('div', { class: 'row-actions' });
    if (change.status === 'pending') {
      cell.append(
        h('button', { type: 'button', class: 'btn btn-sm btn-primary', 'data-mutating': '', 'data-id': change.id, 'data-test': 'approve', onclick: (e) => decide(change, 'approve', e.currentTarget) }, 'Approve'),
        h('button', { type: 'button', class: 'btn btn-sm btn-danger', 'data-mutating': '', 'data-test': 'reject', onclick: (e) => decide(change, 'reject', e.currentTarget) }, 'Reject…'));
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
