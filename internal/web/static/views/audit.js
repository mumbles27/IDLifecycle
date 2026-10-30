// History: search the append-only, hash-chained audit log of everything people and the service did.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, fmtTime, pager, listItems, debounce, act,
  errorBox, loading, field, notice, cellText, clear,
} from '../ui.js';

// Category -> action filter added to the query (q syntax: action:<prefix>*).
// What people did in the console: everything except the service itself, accounts created at Entra ID sign-in and
// failed sign-ins of unknown names (those are under Sign-ins).
const CONSOLE_ACTIVITY = '-actor:system -actor:oidc -actor:anonymous';

const CATEGORIES = [
  ['', 'Everything'],
  [CONSOLE_ACTIVITY, 'Console activity (what people did)'],
  ['action:auth.login action:auth.login_failed action:auth.logout', 'Sign-ins'],
  ['action:change.*', 'Changes (proposals, approvals, AD results)'],
  ['action:identity.*', 'People (identity events)'],
  ['action:sync.*', 'Sync runs'],
  ['action:auth.*', 'Sign-in and sessions (all)'],
  ['action:settings.* action:mapping.* action:role.* action:source.*', 'Configuration'],
  ['action:user.*', 'GUI users'],
  ['action:backup.*', 'Backups and restores'],
  ['action:service.* action:notifications.*', 'Service and notifications'],
];

// ?preset=... links (e.g. from the dashboard or the docs) pick a category.
const PRESETS = { console: CONSOLE_ACTIVITY, signins: CATEGORIES[2][0] };

const RANGES = [
  ['', 'Any time'],
  ['since:24h', 'Last 24 hours'],
  ['since:7d', 'Last 7 days'],
  ['since:30d', 'Last 30 days'],
  ['since:90d', 'Last 90 days'],
];

const HELP = [
  ['timeout dc01', 'entries that mention both words (actor, action, entity or detail)'],
  ['"password expired"', 'an exact phrase'],
  ['-dryrun', 'leave out entries that mention a word'],
  ['actor:alice', 'done by alice (actor:system = the service itself)'],
  ['action:change.failed', 'one kind of event; action:change.* = every change event'],
  ['type:change id:<id>', 'everything about one change, run, source, user ...'],
  ['level:error', 'only failures and errors'],
  ['since:7d   since:2026-10-01   before:2026-10-25T08:00', 'a time range (your local time)'],
];

const shown = (v) => {
  if (v === null || v === undefined || v === '') {
    return h('span', { class: 'muted' }, '(empty)');
  }
  return h('span', { class: 'mono small wrap' }, typeof v === 'string' ? v : cellText(v));
};

/** `changes: {field: {from, to}}` from a create/update entry as a field / before / after table (secrets: "changed"). */
export function changesTable(changes) {
  if (!changes || typeof changes !== 'object' || Array.isArray(changes) || Object.keys(changes).length === 0) {
    return null;
  }
  const rows = Object.entries(changes).sort((x, y) => x[0].localeCompare(y[0]));
  return h('table', { class: 'data compact audit-changes', 'data-test': 'audit-changes' },
    h('caption', { class: 'sr-only' }, 'What changed'),
    h('thead', null, h('tr', null, h('th', null, 'Field'), h('th', null, 'Before'), h('th', null, 'After'))),
    h('tbody', null, rows.map(([k, v]) => {
      const secret = v && typeof v === 'object' && v.changed === true && !('from' in v) && !('to' in v);
      const pair = v && typeof v === 'object' && ('from' in v || 'to' in v);
      return h('tr', null,
        h('td', null, h('code', null, k)),
        secret
          ? h('td', { colspan: '2', class: 'muted' }, 'changed (secret value not recorded)')
          : h('td', null, shown(pair ? v.from : undefined)),
        secret ? null : h('td', null, shown(pair ? v.to : v)));
    })));
}

export async function render(root, ctx) {
  const state = {
    q: ctx.query.get('q') || '',
    category: PRESETS[ctx.query.get('preset')] || '',
    range: '',
    errors: ctx.query.get('errors') === '1',
    limit: 50,
    offset: 0,
  };
  // Older links (?actor=...&action=...&entity_type=...) become query terms.
  for (const [param, key] of [['actor', 'actor'], ['action', 'action'], ['entity_type', 'type'], ['entity_id', 'id']]) {
    const v = ctx.query.get(param);
    if (v) {
      state.q = `${state.q} ${key}:${v.includes(' ') ? `"${v}"` : v}`.trim();
    }
  }

  const listEl = h('div', null, loading());
  const verifyOut = h('div', { 'aria-live': 'polite', 'data-test': 'verify-out' });
  const problem = h('div', { 'aria-live': 'polite' });

  const fullQuery = () => [state.q, state.category, state.range, state.errors ? 'level:error' : ''].filter(Boolean).join(' ');
  const reload = debounce(() => { state.offset = 0; load(); }, 350);

  const search = h('input', {
    type: 'search',
    class: 'mono',
    value: state.q,
    placeholder: 'e.g. actor:alice action:change.* level:error since:7d "timeout"',
    'aria-label': 'Search the history',
    'data-test': 'history-q',
    oninput: () => { state.q = search.value.trim(); reload(); },
  });
  const select = (options, key, test) => {
    const s = h('select', { 'data-test': test, onchange: () => { state[key] = s.value; reload(); } },
      options.map(([v, label]) => h('option', { value: v }, label)));
    s.value = state[key];
    return s;
  };
  const catSel = select(CATEGORIES, 'category', 'history-category');
  const presetBtn = (label, value, test) => h('button', {
    type: 'button', class: 'btn btn-sm', 'data-test': test,
    onclick: () => { state.category = value; catSel.value = value; state.offset = 0; load(); },
  }, label);
  const errorsBox = h('input', { type: 'checkbox', id: 'history-errors', checked: state.errors, 'data-test': 'history-errors', onchange: () => { state.errors = errorsBox.checked; reload(); } });

  // Clicking an actor, action or entity adds it to the search.
  const addTerm = (term) => {
    if (!state.q.split(/\s+/).includes(term)) {
      state.q = `${state.q} ${term}`.trim();
      search.value = state.q;
    }
    state.offset = 0;
    load();
  };
  const termLink = (text, term, cls) => h('button', { type: 'button', class: `linklike ${cls || ''}`, title: `Search for ${term}`, onclick: () => addTerm(term) }, text);
  const quote = (v) => (/\s/.test(v) ? `"${v}"` : v);

  const verifyBtn = h('button', {
    type: 'button',
    class: 'btn',
    'data-test': 'audit-verify',
    onclick: () => act(verifyBtn, async () => {
      replaceContent(verifyOut, h('p', { class: 'muted' }, 'Verifying hash chain…'));
      const res = await api.get('/audit/verify');
      if (res && res.ok) {
        replaceContent(verifyOut, notice('ok', h('strong', null, 'History intact. '), `${res.checked} record(s) checked; the hash chain is unbroken.`));
      }
      else {
        replaceContent(verifyOut, notice('error', h('strong', null, 'History integrity check FAILED. '),
          `The chain breaks at record #${res && res.first_bad_seq !== undefined ? res.first_bad_seq : '?'} (${res && res.checked !== undefined ? res.checked : '?'} checked). Treat records from that point on as untrustworthy and investigate.`));
      }
    }),
  }, 'Verify integrity');

  const exportLink = h('a', { class: 'btn', 'data-test': 'history-export', download: 'idlifecycle-history.csv' }, 'Export CSV');
  const syncExport = () => {
    exportLink.href = `/api/audit?format=csv&q=${encodeURIComponent(fullQuery())}`;
  };

  root.append(
    pageHeader('History', exportLink, verifyBtn),
    h('p', { class: 'help' }, 'Every change, approval, sync run, sign-in, setting change and backup is recorded here, in a tamper-evident chain. Search it below; click an actor, action or entity to narrow the search.'),
    verifyOut,
    h('div', { class: 'stack' },
      field('Search', search),
      h('details', { class: 'help' },
        h('summary', null, 'Search syntax'),
        h('table', { class: 'data compact' }, h('tbody', null, HELP.map(([ex, what]) => h('tr', null, h('td', null, h('code', null, ex)), h('td', null, what)))))),
      h('div', { class: 'filters' },
        field('What', catSel),
        h('div', { class: 'row-gap history-presets' },
          presetBtn('Console activity', PRESETS.console, 'history-preset-console'),
          presetBtn('Sign-ins', PRESETS.signins, 'history-preset-signins')),
        field('When', select(RANGES, 'range', 'history-range')),
        h('div', { class: 'check' }, errorsBox, h('label', { for: 'history-errors' }, 'Errors and failures only')))),
    problem,
    listEl);

  function detailView(a) {
    const d = a.detail;
    if (!d || (typeof d === 'object' && Object.keys(d).length === 0)) {
      return h('span', { class: 'muted' }, '—');
    }
    if (typeof d === 'object' && changesTable(d.changes)) {
      const rest = { ...d };
      delete rest.changes;
      return h('div', { class: 'stack-tight' }, changesTable(d.changes), Object.keys(rest).length ? detailView({ detail: rest }) : null);
    }
    const text = typeof d === 'string' ? d : JSON.stringify(d, null, 2);
    const short = typeof d === 'string' ? d : cellText(d);
    if (text.length <= 90 && !text.includes('\n')) {
      return h('span', { class: 'mono small' }, short);
    }
    return h('details', null, h('summary', { class: 'mono small' }, `${short.slice(0, 90)}…`), h('pre', { class: 'detail-json' }, text));
  }

  // Only the newest search may draw: an older, slower response must not replace it.
  let loadSeq = 0;
  async function load() {
    syncExport();
    const seq = ++loadSeq;
    let resp;
    try {
      resp = await api.get('/audit', { q: fullQuery(), limit: state.limit, offset: state.offset });
      if (seq !== loadSeq) {
        return;
      }
      clear(problem);
    }
    catch (e) {
      if (seq !== loadSeq) {
        return;
      }
      replaceContent(problem, notice('error', e.message));
      clear(listEl);
      return;
    }
    const items = listItems(resp);
    const total = resp && Number.isFinite(resp.total) ? resp.total : items.length;
    const failed = (a) => /(failed|denied|rejected|blocked)$/.test(a.action) || (a.detail && typeof a.detail === 'object' && a.detail.error);
    replaceContent(listEl,
      h('p', { class: 'muted small', 'data-test': 'history-total' }, `${total} matching record(s)`),
      items.length === 0
        ? h('p', { class: 'empty' }, 'Nothing matches this search.')
        : h('table', { class: 'data', 'data-test': 'history-table' },
          h('caption', { class: 'sr-only' }, 'History'),
          h('thead', null, h('tr', null, ['#', 'Time', 'Actor', 'Action', 'Entity', 'Detail'].map((c) => h('th', null, c)))),
          h('tbody', null, items.map((a) => h('tr', { class: failed(a) ? 'row-error' : '' },
            h('td', { class: 'num' }, String(a.seq)),
            h('td', { class: 'nowrap' }, fmtTime(a.ts)),
            h('td', null, termLink(a.actor, `actor:${quote(a.actor)}`)),
            h('td', null, termLink(h('code', null, a.action), `action:${a.action}`)),
            h('td', null, a.entity_type ? termLink(a.entity_type, `type:${a.entity_type}`) : '',
              a.entity_id ? h('div', null, termLink(a.entity_id.length > 28 ? `${a.entity_id.slice(0, 28)}…` : a.entity_id, `id:${quote(a.entity_id)}`, 'muted small mono')) : null),
            h('td', null, detailView(a)))))),
      pager({ total, limit: state.limit, offset: state.offset, onChange: (o, l) => { state.offset = o; state.limit = l; load(); } }));
  }
  await load();
}
