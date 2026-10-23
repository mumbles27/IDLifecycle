// Audit log: append-only, hash-chained record of everything the service did.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, fmtTime, dataTable, pager, listItems, debounce, act,
  errorBox, loading, field, truncCell, notice, cellText,
} from '../ui.js';

export async function render(root, ctx) {
  const state = {
    actor: ctx.query.get('actor') || '',
    action: ctx.query.get('action') || '',
    entity_type: ctx.query.get('entity_type') || '',
    limit: 50,
    offset: 0,
  };
  const listEl = h('div', null, loading());
  const verifyOut = h('div', { 'aria-live': 'polite', 'data-test': 'verify-out' });

  const reload = debounce(() => { state.offset = 0; load(); }, 350);
  const mk = (key, label, placeholder) => {
    const input = h('input', {
      type: 'search',
      placeholder,
      value: state[key],
      'data-test': `audit-${key}`,
      oninput: () => { state[key] = input.value.trim(); reload(); },
    });
    return field(label, input);
  };

  const verifyBtn = h('button', {
    type: 'button',
    class: 'btn',
    'data-test': 'audit-verify',
    onclick: () => act(verifyBtn, async () => {
      replaceContent(verifyOut, h('p', { class: 'muted' }, 'Verifying hash chain…'));
      const res = await api.get('/audit/verify');
      if (res && res.ok) {
        replaceContent(verifyOut, notice('ok', h('strong', null, 'Audit log intact. '), `${res.checked} record(s) checked; the hash chain is unbroken.`));
      }
      else {
        replaceContent(verifyOut, notice('error', h('strong', null, 'Audit log integrity check FAILED. '),
          `The chain breaks at record #${res && res.first_bad_seq !== undefined ? res.first_bad_seq : '?'} (${res && res.checked !== undefined ? res.checked : '?'} checked). Treat records from that point on as untrustworthy and investigate.`));
      }
    }),
  }, 'Verify integrity');

  root.append(
    pageHeader('Audit log', verifyBtn),
    verifyOut,
    h('div', { class: 'filters' },
      mk('actor', 'Actor', 'e.g. admin or system:auto'),
      mk('action', 'Action', 'e.g. change.approve'),
      mk('entity_type', 'Entity type', 'e.g. change')),
    listEl);

  async function load() {
    let resp;
    try {
      resp = await api.get('/audit', { ...state });
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const items = listItems(resp);
    const total = resp && Number.isFinite(resp.total) ? resp.total : items.length;
    replaceContent(listEl,
      dataTable([
        { label: '#', key: 'seq', className: 'num' },
        { label: 'Time', render: (a) => fmtTime(a.ts) },
        { label: 'Actor', key: 'actor' },
        { label: 'Action', render: (a) => h('code', null, a.action) },
        { label: 'Entity', render: (a) => [a.entity_type, a.entity_id ? h('div', { class: 'muted small mono' }, truncCell(a.entity_id, 24)) : null] },
        { label: 'Detail', render: (a) => truncCell(typeof a.detail === 'string' ? a.detail : cellText(a.detail), 80) },
        { label: 'Hash', render: (a) => h('span', { class: 'mono small', title: a.hash }, a.hash ? String(a.hash).slice(0, 12) : '') },
      ], items, { caption: 'Audit entries', empty: 'No audit entries match.' }),
      pager({ total, limit: state.limit, offset: state.offset, onChange: (o, l) => { state.offset = o; state.limit = l; load(); } }));
  }
  await load();
}
