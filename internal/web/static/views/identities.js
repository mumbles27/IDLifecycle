// Identities: people known from the HR sources and how they link to AD.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, pill, fmtTime, dataTable, pager, listItems, debounce,
  errorBox, loading, field, dl, truncCell, isObject, cellText, relTime,
} from '../ui.js';

const STATES = ['new', 'linked', 'disabled', 'deleted', 'error'];

export async function render(root, ctx) {
  if (ctx.params[0]) {
    return renderDetail(root, ctx.params[0]);
  }
  return renderList(root, ctx);
}

async function renderList(root, ctx) {
  const state = { q: ctx.query.get('q') || '', state: ctx.query.get('state') || '', limit: 25, offset: 0 };
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

  root.append(pageHeader('Identities'),
    h('div', { class: 'filters' }, field('Search', search), field('State', stateSel)),
    listEl);

  async function load() {
    let resp;
    try {
      resp = await api.get('/identities', { q: state.q, state: state.state, limit: state.limit, offset: state.offset });
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const items = listItems(resp);
    const total = resp && Number.isFinite(resp.total) ? resp.total : items.length;
    replaceContent(listEl,
      dataTable([
        { label: 'Name', render: (i) => h('a', { href: `#/identities/${encodeURIComponent(i.id)}` }, i.display || i.external_id) },
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

async function renderDetail(root, id) {
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
  const attrs = isObject(ident.attrs) ? Object.entries(ident.attrs).sort((a, b) => a[0].localeCompare(b[0])) : [];

  const changesBox = h('div', null, loading());
  replaceContent(body,
    h('h2', { class: 'ident-title' }, ident.display || ident.external_id, ' ', pill(ident.state), ' ', pill(ident.active ? 'active' : 'inactive')),
    h('div', { class: 'grid grid-cards' },
      card('Source record', dl([
        ['Employee ID', ident.external_id],
        ['Source', ident.source_name || ident.source_id],
        ['First seen', fmtTime(ident.first_seen)],
        ['Last seen', fmtTime(ident.last_seen)],
        ['Disabled at', fmtTime(ident.disabled_at)],
      ])),
      card('Active Directory link', ident.ad_guid || ident.ad_dn || ident.ad_sam
        ? dl([
          ['sAMAccountName', ident.ad_sam],
          ['Distinguished name', ident.ad_dn ? h('span', { class: 'mono wrap' }, ident.ad_dn) : ''],
          ['objectGUID', ident.ad_guid ? h('span', { class: 'mono' }, ident.ad_guid) : ''],
        ])
        : h('p', { class: 'muted' }, 'Not linked to an AD account yet.'))),
    card('Source attributes', dataTable([
      { label: 'Attribute', render: (r) => h('code', null, r[0]) },
      { label: 'Value', render: (r) => truncCell(cellText(r[1]), 120) },
    ], attrs, { caption: 'Source attributes', empty: 'No attributes stored.' })),
    card('Recent changes', changesBox));

  try {
    const resp = await api.get('/changes', { q: ident.external_id, limit: 50 });
    const mine = listItems(resp).filter((c) => c.identity_id === ident.id || c.external_id === ident.external_id).slice(0, 10);
    replaceContent(changesBox, dataTable([
      { label: 'Created', render: (c) => fmtTime(c.created_at) },
      { label: 'Type', key: 'type' },
      { label: 'Status', render: (c) => pill(c.status) },
      { label: 'Summary', render: (c) => truncCell((c.summary || []).join('; '), 90) },
    ], mine, { caption: 'Recent changes', empty: 'No changes recorded for this identity.' }),
    h('p', null, h('a', { href: `#/approvals?status=&q=${encodeURIComponent(ident.external_id)}` }, 'Open in approvals')));
  }
  catch (e) {
    replaceContent(changesBox, errorBox(e));
  }
}
