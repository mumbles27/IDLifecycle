// Leavers: accounts due for deletion under their retention policy, and people whose termination date is coming up.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, pill, fmtTime, dataTable, pager, listItems, errorBox, loading, field,
} from '../ui.js';

const WINDOWS = [[7, 'next 7 days'], [30, 'next 30 days'], [90, 'next 90 days'], [365, 'next year']];
const KINDS = [['', 'Deletions and leavers'], ['delete', 'Deletions only'], ['leaving', 'Leavers only']];

// A plain YYYY-MM-DD (a termination date) is a local date, not UTC midnight.
function toDate(v) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(v || ''));
  return m ? new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3])) : new Date(v);
}

export function fmtDate(v) {
  if (!v) {
    return '';
  }
  const d = toDate(v);
  return Number.isNaN(d.getTime()) ? String(v) : d.toLocaleDateString();
}

/** Whole days from today until v (negative = past). */
export function daysUntil(v) {
  const d = toDate(v);
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const day = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  return Math.round((day.getTime() - today.getTime()) / 86400000);
}

export function retentionReasonText(r) {
  if (!r) {
    return '';
  }
  if (r.reason === 'assigned') {
    return `assigned${r.assigned_by ? ` by ${r.assigned_by}` : ' by hand'}`;
  }
  if (r.reason === 'rule') {
    return 'matched its conditions';
  }
  if (r.reason === 'default') {
    return 'no other policy applies';
  }
  return r.reason || '';
}

function dueCell(it) {
  const when = it.kind === 'leaving' ? it.leave_date : it.delete_due;
  if (!when) {
    return '';
  }
  const days = daysUntil(when);
  let rel = `in ${days} day${days === 1 ? '' : 's'}`;
  if (days <= 0) {
    rel = it.kind === 'leaving' ? 'today' : 'due now (next sync)';
  }
  return h('span', { class: `leavers-due${days <= 0 && it.kind !== 'leaving' ? ' leavers-overdue' : ''}`, title: fmtTime(when) },
    fmtDate(when), h('div', { class: 'muted small' }, rel));
}

export async function render(root, ctx) {
  const state = {
    days: Number(ctx.query.get('days')) || 30,
    kind: ctx.query.get('kind') || '',
    limit: 50,
    offset: 0,
  };
  const listEl = h('div', null, loading());
  const daysSel = h('select', { 'data-test': 'leavers-days', onchange: () => { state.days = Number(daysSel.value); state.offset = 0; load(); } },
    WINDOWS.map(([v, l]) => h('option', { value: String(v) }, l)));
  daysSel.value = String(state.days);
  const kindSel = h('select', { 'data-test': 'leavers-kind', onchange: () => { state.kind = kindSel.value; state.offset = 0; load(); } },
    KINDS.map(([v, l]) => h('option', { value: v }, l)));
  kindSel.value = state.kind;

  root.append(
    pageHeader('Leavers', ctx.can('viewer') ? h('a', { class: 'btn', href: '#/retention' }, 'Retention policies') : null),
    h('p', { class: 'help' }, 'Disabled accounts that their retention policy deletes in this window, and people whose termination date falls in it. Deletes still need approval as usual and respect the delete limits; Manual people are not deleted.'),
    h('div', { class: 'filters' }, field('When', daysSel), field('Show', kindSel)),
    listEl);

  async function load() {
    let resp;
    try {
      resp = await api.get('/retention/upcoming', { days: state.days, kind: state.kind, limit: state.limit, offset: state.offset });
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const items = listItems(resp);
    const total = resp && Number.isFinite(resp.total) ? resp.total : items.length;
    replaceContent(listEl,
      dataTable([
        { label: 'When', render: dueCell },
        {
          label: 'Person',
          render: (it) => h('span', null,
            h('a', { href: `#/identities/${encodeURIComponent(it.identity_id)}`, 'data-test': 'leavers-person' }, it.display || it.external_id),
            h('div', { class: 'muted small' }, it.external_id || '')),
        },
        { label: 'What', render: (it) => (it.kind === 'leaving' ? pill('leaving', 'leaves (termination date)') : pill('deletion', 'account deleted')) },
        { label: 'AD account', render: (it) => it.ad_sam || '' },
        { label: 'Disabled since', render: (it) => (it.disabled_at ? fmtDate(it.disabled_at) : h('span', { class: 'muted small' }, it.kind === 'leaving' ? 'still active' : '')) },
        {
          label: 'Policy',
          render: (it) => (it.policy_name
            ? h('span', null, h('strong', null, it.policy_name), h('div', { class: 'muted small' }, retentionReasonText(it)))
            : ''),
        },
        { label: 'Manual', render: (it) => (it.manual_hold ? h('span', { class: 'pill pill-manual', title: 'Manual: not deleted automatically until set back to Auto' }, 'Manual') : '') },
      ], items, {
        caption: 'Upcoming deletions and leavers',
        empty: `Nothing due in the ${(WINDOWS.find(([v]) => v === state.days) || [0, 'chosen window'])[1]}.`,
        rowClass: (it) => `leavers-${it.kind || 'delete'}`,
      }),
      pager({ total, limit: state.limit, offset: state.offset, onChange: (o, l) => { state.offset = o; state.limit = l; load(); } }));
  }
  await load();
}
