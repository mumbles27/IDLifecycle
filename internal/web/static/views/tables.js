// Data browser: read-only view of allow-listed database tables.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, dataTable, pager, listItems, debounce, errorBox, loading, field, truncCell,
} from '../ui.js';

function tableNames(resp) {
  let list = [];
  if (Array.isArray(resp)) {
    list = resp;
  }
  else if (resp && Array.isArray(resp.tables)) {
    list = resp.tables;
  }
  else {
    list = listItems(resp);
  }
  return list.map((t) => (typeof t === 'string' ? t : (t && (t.name || t.table)))).filter(Boolean);
}

export async function render(root, ctx) {
  const state = { name: ctx.params[0] || '', q: '', limit: 50, offset: 0 };
  const tabs = h('div', { class: 'tabs', role: 'tablist', 'aria-label': 'Tables' });
  const listEl = h('div', null, loading());
  const search = h('input', {
    type: 'search',
    placeholder: 'Search rows',
    'data-test': 'table-q',
    oninput: debounce(() => { state.q = search.value.trim(); state.offset = 0; load(); }, 350),
  });
  root.append(
    pageHeader('Data browser'),
    h('p', { class: 'muted' }, 'Read-only view of selected database tables, for troubleshooting. Long values are shortened; click one to expand it.'),
    tabs,
    h('div', { class: 'filters' }, field('Search', search)),
    listEl);

  let names = [];
  try {
    names = tableNames(await api.get('/tables'));
  }
  catch (e) {
    replaceContent(listEl, errorBox(e));
    return;
  }
  if (names.length === 0) {
    replaceContent(listEl, h('p', { class: 'empty' }, 'No tables are available.'));
    return;
  }
  if (!names.includes(state.name)) {
    state.name = names[0];
  }

  function drawTabs() {
    replaceContent(tabs, names.map((n) => h('button', {
      type: 'button',
      role: 'tab',
      class: `tab${n === state.name ? ' tab-active' : ''}`,
      'aria-selected': String(n === state.name),
      'data-test': `tab-${n}`,
      onclick: () => { state.name = n; state.offset = 0; state.q = ''; search.value = ''; drawTabs(); load(); },
    }, n)));
  }

  async function load() {
    replaceContent(listEl, loading());
    let resp;
    try {
      resp = await api.get(`/tables/${encodeURIComponent(state.name)}`, { q: state.q, limit: state.limit, offset: state.offset });
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const columns = Array.isArray(resp.columns) ? resp.columns : [];
    const rows = Array.isArray(resp.rows) ? resp.rows : [];
    const total = Number.isFinite(resp.total) ? resp.total : rows.length;
    replaceContent(listEl,
      dataTable(columns.map((c, i) => ({
        label: c,
        render: (row) => truncCell(Array.isArray(row) ? row[i] : (row ? row[c] : ''), 60),
      })), rows, { caption: `Rows of ${state.name}`, empty: 'No rows.', wrapClass: 'nowrap' }),
      pager({ total, limit: state.limit, offset: state.offset, onChange: (o, l) => { state.offset = o; state.limit = l; load(); } }));
  }

  drawTabs();
  await load();
}
