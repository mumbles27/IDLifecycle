// Runs: history of sync runs with counters, plus a per-run detail page.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, pill, fmtTime, fmtDuration, dataTable, pager, listItems,
  act, confirmDialog, toast, errorBox, loading, truncCell, dl, mutBtn,
} from '../ui.js';
import { counterGrid } from '../components.js';

function compactCounters(c) {
  const entries = Object.entries(c || {}).filter(([, v]) => Number(v) > 0);
  if (entries.length === 0) {
    return h('span', { class: 'muted' }, 'no changes');
  }
  return h('span', { class: 'small' }, entries.slice(0, 5).map(([k, v]) => `${k.replace(/_/g, ' ')}: ${v}`).join(' · '));
}

export async function render(root, ctx) {
  if (ctx.params[0]) {
    return renderDetail(root, ctx, ctx.params[0]);
  }
  return renderList(root, ctx);
}

async function startRun(ctx, button) {
  const ok = await confirmDialog({
    title: 'Run sync now',
    message: 'Start a sync run immediately?',
    confirmLabel: 'Run sync',
  });
  if (!ok) {
    return;
  }
  await act(button, async () => {
    const res = await api.post('/runs', {});
    toast('Sync run started.', 'ok');
    if (res && res.id) {
      ctx.navigate(`#/runs/${encodeURIComponent(res.id)}`);
    }
  });
}

async function renderList(root, ctx) {
  const state = { limit: 25, offset: 0 };
  const listEl = h('div', null, loading());
  const runBtn = ctx.can('admin')
    ? mutBtn('Run sync now', { class: 'btn-primary', 'data-test': 'run-now', onclick: (e) => startRun(ctx, e.currentTarget) })
    : null;
  root.append(pageHeader('Sync runs', h('button', { type: 'button', class: 'btn', onclick: () => load() }, 'Refresh'), runBtn), listEl);

  async function load() {
    let resp;
    try {
      resp = await api.get('/runs', { limit: state.limit, offset: state.offset });
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const items = listItems(resp);
    const total = resp && Number.isFinite(resp.total) ? resp.total : items.length;
    replaceContent(listEl,
      dataTable([
        { label: 'Started', render: (r) => h('a', { href: `#/runs/${encodeURIComponent(r.id)}` }, fmtTime(r.started_at) || r.id) },
        { label: 'Trigger', render: (r) => [r.trigger, r.triggered_by ? h('span', { class: 'muted small' }, ` (${r.triggered_by})`) : null] },
        { label: 'Status', render: (r) => pill(r.status) },
        { label: 'Duration', render: (r) => fmtDuration(r.started_at, r.finished_at) },
        { label: 'Counters', render: (r) => compactCounters(r.counters) },
        { label: 'Error', render: (r) => (r.error ? truncCell(r.error, 70) : '') },
      ], items, { caption: 'Sync runs', empty: 'No runs yet.' }),
      pager({ total, limit: state.limit, offset: state.offset, onChange: (o, l) => { state.offset = o; state.limit = l; load(); } }));
  }
  await load();
}

async function renderDetail(root, ctx, id) {
  const body = h('div', { class: 'stack' }, loading());
  root.append(pageHeader('Run detail', h('a', { href: '#/runs', class: 'btn' }, 'Back to runs')), body);
  let timer = null;

  async function load() {
    let run;
    try {
      run = await api.get(`/runs/${encodeURIComponent(id)}`);
    }
    catch (e) {
      replaceContent(body, errorBox(e));
      return;
    }
    replaceContent(body,
      card('Summary', dl([
        ['Run ID', run.id],
        ['Status', pill(run.status)],
        ['Trigger', `${run.trigger || ''}${run.triggered_by ? ` by ${run.triggered_by}` : ''}`],
        ['Started', fmtTime(run.started_at)],
        ['Finished', run.finished_at ? fmtTime(run.finished_at) : (run.status === 'running' ? 'still running…' : '')],
        ['Duration', fmtDuration(run.started_at, run.finished_at)],
      ])),
      run.error ? card('Errors', h('pre', { class: 'code code-error', 'data-test': 'run-error' }, run.error)) : null,
      card('Counters', counterGrid(run.counters)),
      h('p', null, h('a', { href: '#/approvals?status=' }, 'Browse all changes'), ' (filter by identity to see what this run planned).'));
    if (run.status === 'running' && !timer) {
      timer = setInterval(load, 3000);
    }
    else if (run.status !== 'running' && timer) {
      clearInterval(timer);
      timer = null;
    }
  }
  await load();
  return () => {
    if (timer) {
      clearInterval(timer);
    }
  };
}
