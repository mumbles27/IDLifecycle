// Dashboard: mode (live / dry run), scheduler, pending approvals, last run, source health.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, pill, fmtTime, relTime, fmtDuration, dataTable, listItems,
  mutBtn, act, confirmDialog, toast, errorBox, loading, truncCell, dl,
} from '../ui.js';
import { counterGrid } from '../components.js';

export async function render(root, ctx) {
  const body = h('div', { class: 'stack' }, loading());
  const runBtn = ctx.can('admin')
    ? mutBtn('Run sync now', {
      class: 'btn-primary',
      'data-test': 'run-now',
      onclick: () => startRun(runBtn),
    })
    : null;
  root.append(pageHeader('Dashboard', runBtn), body);

  async function startRun(button) {
    const ok = await confirmDialog({
      title: 'Run sync now',
      message: 'Start a sync run immediately? It fetches every enabled source and plans changes using the current settings.',
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
      else {
        await load();
      }
    });
  }

  async function load() {
    let status;
    try {
      status = await api.get('/status');
    }
    catch (e) {
      replaceContent(body, errorBox(e));
      return;
    }
    let sources = [];
    let sourcesError = null;
    try {
      sources = listItems(await api.get('/sources', { limit: 500 }));
    }
    catch (e) {
      sourcesError = e;
    }
    if (runBtn) {
      runBtn.disabled = Boolean(status.running_run);
      runBtn.title = status.running_run ? 'A run is already in progress' : '';
    }
    draw(status, sources, sourcesError);
    ctx.refreshStatus();
  }

  function modeCard(status) {
    const dry = status.mode === 'dry_run';
    return h('section', { class: `card mode-card ${dry ? 'mode-card-dry' : 'mode-card-live'}`, 'data-test': 'mode-card' },
      h('h2', null, 'Mode'),
      h('p', { class: 'mode-title', 'data-test': 'mode-text' }, dry ? 'DRY RUN' : 'LIVE'),
      h('p', null, dry
        ? 'The service plans and records changes as "simulated". Nothing is written to Active Directory until an admin switches off dry run.'
        : 'Approved changes are written to Active Directory.'),
      ctx.can('admin')
        ? h('p', null, h('a', { href: '#/settings' }, dry ? 'Review settings before going live' : 'Engine settings'))
        : null);
  }

  function schedulerCard(status) {
    const sch = status.scheduler || {};
    return h('section', { class: 'card', 'data-test': 'scheduler-card' },
      h('h2', null, 'Scheduler'),
      h('p', { class: 'big' }, sch.enabled ? 'Enabled' : 'Disabled'),
      sch.enabled
        ? dl([['Next run', sch.next_run ? `${fmtTime(sch.next_run)} (${relTime(sch.next_run)})` : 'Not scheduled']])
        : h('p', { class: 'muted' }, 'Automatic runs are off. Use "Run sync now" or enable a schedule in Settings.'));
  }

  function pendingCard(status) {
    const n = Number(status.pending_changes) || 0;
    return h('section', { class: 'card', 'data-test': 'pending-card' },
      h('h2', null, 'Pending approvals'),
      h('p', { class: 'big', 'data-test': 'pending-count' }, String(n)),
      h('p', null, h('a', { href: '#/approvals?status=pending' }, n > 0 ? 'Review pending changes' : 'Open approvals')));
  }

  function runCard(status) {
    const last = status.last_run;
    const running = status.running_run;
    const parts = [h('h2', null, 'Last run')];
    if (running) {
      const id = typeof running === 'object' ? running.id : running;
      parts.push(h('p', null, pill('running'), ' A run is in progress. ', id ? h('a', { href: `#/runs/${encodeURIComponent(id)}` }, 'Watch it') : null));
    }
    if (!last) {
      parts.push(h('p', { class: 'muted' }, 'No runs yet.'));
    }
    else {
      parts.push(
        h('p', null, pill(last.status), ' ', h('a', { href: `#/runs/${encodeURIComponent(last.id)}` }, fmtTime(last.started_at)),
          last.finished_at ? h('span', { class: 'muted' }, ` - took ${fmtDuration(last.started_at, last.finished_at)}`) : null),
        last.error ? h('p', { class: 'notice notice-error' }, last.error) : null,
        counterGrid(last.counters));
    }
    return h('section', { class: 'card card-wide', 'data-test': 'run-card' }, parts);
  }

  function sourcesCard(sources, err) {
    if (err) {
      return card('Sources', errorBox(err));
    }
    return card('Sources', dataTable([
      { label: 'Name', render: (s) => h('a', { href: '#/sources' }, s.name) },
      { label: 'Type', key: 'type' },
      { label: 'Enabled', render: (s) => pill(s.enabled ? 'enabled' : 'disabled') },
      { label: 'Last status', render: (s) => (s.last_status ? pill(s.last_status) : '') },
      { label: 'Last sync', render: (s) => fmtTime(s.last_sync_at) },
      { label: 'Last error', render: (s) => (s.last_error ? truncCell(s.last_error, 90) : '') },
    ], sources, { empty: 'No sources configured yet. Add one under Sources.', caption: 'Source health' }));
  }

  function draw(status, sources, sourcesError) {
    replaceContent(body,
      h('div', { class: 'grid grid-cards' },
        modeCard(status), schedulerCard(status), pendingCard(status)),
      h('div', { class: 'grid grid-cards' }, runCard(status)),
      sourcesCard(sources, sourcesError),
      h('p', { class: 'muted small' }, `IDLifecycle ${status.version || ''} - database: ${status.db_driver || 'unknown'}`));
  }

  await load();
  const timer = setInterval(load, 15000);
  return () => clearInterval(timer);
}
