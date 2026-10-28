// Impact preview: what the next sync would do to every person - and, with a proposal (unsaved role rules, mappings or
// settings), what would change compared with the saved configuration. A page of its own and a panel the editors open.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, errorBox, loading, field, inputText, inputCheck, inputSelect, act,
  dataTable, pager, notice, relTime, fmtTime, debounce, isObject, openModal, btn, uid,
} from '../ui.js';

const TYPE_LABELS = {
  none: 'nothing',
  create: 'create account',
  update: 'update account',
  disable: 'disable account',
  enable: 'enable account',
  delete: 'delete account',
  link: 'link to an existing account',
  error: 'cannot be planned',
};

const DIFF_LABELS = {
  new: 'newly affected',
  changed: 'treated differently',
  gone: 'no longer affected',
  same: 'same as now',
};

function typeLabel(t) {
  return TYPE_LABELS[t] || t || 'nothing';
}

function steps(side) {
  return side && Array.isArray(side.summary) ? side.summary : [];
}

function planCell(p) {
  if (!p) {
    return h('span', { class: 'muted' }, 'nothing');
  }
  const n = steps(p).length;
  return h('span', null, h('span', { class: `plan-type plan-${p.type || 'none'}` }, typeLabel(p.type)),
    n ? h('span', { class: 'muted small' }, ` (${n} step${n === 1 ? '' : 's'})`) : null);
}

/** Diff lines with a text marker and a screen-reader word, so the meaning never depends on colour. */
export function diffLines(lines) {
  return h('ul', { class: 'diff', 'data-test': 'impact-diff' }, lines.map((l) => {
    const add = l.op === '+';
    return h('li', { class: add ? 'diff-add' : 'diff-del' },
      h('span', { class: 'diff-mark', 'aria-hidden': 'true' }, add ? '+' : '−'),
      h('span', { class: 'sr-only' }, add ? 'Added: ' : 'Removed: '),
      h('span', { class: 'diff-text' }, l.text));
  }));
}

function stepList(side) {
  const s = steps(side);
  if (s.length === 0) {
    return h('span', { class: 'muted' }, side ? typeLabel(side.type) : 'nothing planned');
  }
  return h('ul', { class: 'impact-steps', 'data-test': 'impact-steps' }, s.map((t) => h('li', null, t)));
}

function countsTable(summary, hasProposal) {
  const base = isObject(summary.baseline) ? summary.baseline : {};
  const prop = isObject(summary.proposed) ? summary.proposed : {};
  const types = [...new Set([...Object.keys(base), ...Object.keys(prop)])].sort();
  if (types.length === 0) {
    return null;
  }
  const columns = [{ label: 'Change', render: (t) => typeLabel(t) }];
  if (hasProposal) {
    columns.push(
      { label: 'Saved configuration', className: 'num', render: (t) => String(base[t] || 0) },
      { label: 'With your changes', className: 'num', render: (t) => String(prop[t] || 0) },
      {
        label: 'Difference',
        className: 'num',
        render: (t) => {
          const d = (prop[t] || 0) - (base[t] || 0);
          return d === 0 ? h('span', { class: 'muted' }, '0') : h('strong', null, d > 0 ? `+${d}` : `−${-d}`);
        },
      },
    );
  }
  else {
    columns.push({ label: 'People', className: 'num', render: (t) => String(prop[t] || base[t] || 0) });
  }
  return dataTable(columns, types, { caption: 'Planned changes by type', wrapClass: 'compact' });
}

function countChips(list, kind) {
  if (!Array.isArray(list) || list.length === 0) {
    return h('span', { class: 'muted' }, 'none');
  }
  return list.map((g) => h('span', { class: `chip ${kind === '-' ? 'chip-del' : 'chip-add'}` },
    kind ? h('span', { 'aria-hidden': 'true' }, `${kind === '-' ? '−' : '+'} `) : null,
    `${g.group || g.attr} (${g.people} ${g.people === 1 ? 'person' : 'people'})`));
}

/**
 * impactPanel({ proposal, onlyChanged }) -> { el, load }
 * proposal: { roles?, mappings?, settings? } - the unsaved configuration; {} means "what the next sync would do".
 */
export function impactPanel({ proposal = {}, onlyChanged = true } = {}) {
  const hasProposal = Object.keys(proposal).length > 0;
  // Without a proposal both sides are the same, so "only changed" would hide everyone.
  const state = { q: '', type: '', only_changed: hasProposal && onlyChanged, offset: 0, limit: 25 };
  const summaryEl = h('div', { 'data-test': 'impact-summary' });
  const metaEl = h('div', { class: 'impact-meta' });
  const countEl = h('p', { class: 'impact-count', 'aria-live': 'polite', 'data-test': 'impact-count' });
  const resultsEl = h('div', { 'data-test': 'impact-results' }, loading('Working out the changes…'));
  const typeSel = inputSelect(state, 'type', [{ value: '', label: 'any change' }], {
    'data-test': 'impact-type',
    onchange: () => { state.offset = 0; load(); },
  });
  let seq = 0;
  let knownTypes = [];

  const refreshBtn = btn('Refresh from AD', {
    class: 'btn-sm',
    'data-test': 'impact-refresh',
    title: 'Read the accounts from AD again before working out the changes (slower)',
    onclick: () => act(refreshBtn, () => load(true)),
  });

  function setTypes(summary) {
    const types = [...new Set([...Object.keys(summary.baseline || {}), ...Object.keys(summary.proposed || {})])].sort();
    if (types.join() === knownTypes.join()) {
      return;
    }
    knownTypes = types;
    replaceContent(typeSel, h('option', { value: '' }, 'any change'), types.map((t) => h('option', { value: t }, typeLabel(t))));
    typeSel.value = types.includes(state.type) ? state.type : '';
  }

  function drawSummary(res) {
    const s = isObject(res.summary) ? res.summary : {};
    setTypes(s);
    replaceContent(metaEl,
      h('span', { class: 'small muted' },
        `${res.people || 0} people checked`,
        res.snapshot_at ? h('span', { title: fmtTime(res.snapshot_at) }, ` · AD as read ${relTime(res.snapshot_at)}`) : null,
        res.generated_at ? h('span', { title: fmtTime(res.generated_at) }, ` · worked out ${relTime(res.generated_at)}`) : null),
      refreshBtn);
    const planned = Object.entries(isObject(s.proposed) ? s.proposed : {}).reduce((n, [t, c]) => n + (t === 'none' ? 0 : c), 0);
    const n = hasProposal ? s.changed_people || 0 : planned;
    replaceContent(summaryEl,
      h('p', { class: 'impact-headline', 'data-test': 'impact-changed' },
        h('strong', null, String(n)),
        hasProposal
          ? ` ${n === 1 ? 'person would be' : 'people would be'} treated differently than with the saved configuration.`
          : ` ${n === 1 ? 'person has' : 'people have'} a change planned for the next sync.`),
      countsTable(s, hasProposal),
      hasProposal
        ? h('dl', { class: 'kv impact-groups' },
          h('dt', null, 'Group memberships added'), h('dd', { 'data-test': 'impact-groups-added' }, countChips(s.groups_added, '+')),
          h('dt', null, 'Group memberships removed'), h('dd', { 'data-test': 'impact-groups-removed' }, countChips(s.groups_removed, '-')),
          h('dt', null, 'Attributes that change'), h('dd', null, countChips(s.attributes, '')))
        : null,
      (Array.isArray(res.notes) ? res.notes : []).map((t) => notice('info', t)),
      res.names_checked === false
        ? notice('info', 'Account names for new people are not checked against AD in a preview; the real sync may pick a different name when one is taken.')
        : null);
  }

  function drawItems(res) {
    const items = Array.isArray(res.items) ? res.items : [];
    const total = Number(res.total) || 0;
    let filter = '';
    if (state.only_changed) {
      filter = ' and would be treated differently';
    }
    countEl.textContent = `${total} ${total === 1 ? 'person matches' : 'people match'}${state.q.trim() ? ` "${state.q.trim()}"` : ''}${filter}.`;
    const columns = [
      { label: 'Person', render: (r) => h('div', null, h('strong', null, r.display || r.external_id || r.identity_id), r.external_id ? h('div', { class: 'muted small mono' }, r.external_id) : null) },
      { label: 'AD account', render: (r) => (r.ad_sam ? h('code', null, r.ad_sam) : h('span', { class: 'muted' }, 'none yet')) },
      { label: 'State', key: 'state' },
    ];
    if (hasProposal) {
      columns.push(
        { label: 'Difference', render: (r) => h('span', { class: `diff-kind diff-kind-${r.difference}`, 'data-test': 'impact-difference' }, DIFF_LABELS[r.difference] || r.difference) },
        { label: 'Saved configuration', render: (r) => planCell(r.baseline) },
        { label: 'With your changes', render: (r) => planCell(r.proposed) },
      );
    }
    else {
      columns.push({ label: 'Next sync', render: (r) => planCell(r.proposed || r.baseline) });
    }
    const detail = (r) => {
      const lines = Array.isArray(r.lines) ? r.lines : [];
      if (!hasProposal) {
        return h('div', { class: 'impact-detail' }, h('h3', null, 'Planned steps'), stepList(r.proposed || r.baseline));
      }
      return h('div', { class: 'impact-detail' },
        h('h3', null, 'What changes'),
        lines.length ? diffLines(lines) : h('p', { class: 'muted small' }, 'The same steps as with the saved configuration.'),
        h('div', { class: 'grid grid-2' },
          h('div', null, h('h4', null, 'Saved configuration'), stepList(r.baseline)),
          h('div', null, h('h4', null, 'With your changes'), stepList(r.proposed))));
    };
    replaceContent(resultsEl,
      dataTable(columns, items, {
        caption: 'People and their changes',
        empty: state.q.trim() || state.type ? 'Nobody matches this search.' : 'Nobody would be affected.',
        rowClass: (r) => (hasProposal && r.difference !== 'same' ? 'impact-row-changed' : ''),
        expand: detail,
      }),
      total > state.limit || state.offset > 0
        ? pager({ total, limit: state.limit, offset: state.offset, onChange: (offset, limit) => { state.offset = offset; state.limit = limit; load(); } })
        : null);
  }

  async function load(refresh = false) {
    seq += 1;
    const mine = seq;
    const body = {
      ...proposal, refresh: Boolean(refresh), q: state.q.trim(), type: state.type, only_changed: Boolean(state.only_changed), offset: state.offset, limit: state.limit,
    };
    let res;
    try {
      res = await api.post('/preview/impact', body);
    }
    catch (e) {
      if (mine !== seq) {
        return;
      }
      countEl.textContent = '';
      if (e.status === 409) {
        const retry = btn('Try again', { class: 'btn-sm', onclick: () => load(refresh) });
        replaceContent(resultsEl, notice('warn', 'Another preview or a sync is running right now. Try again in a moment. ', retry));
      }
      else {
        replaceContent(resultsEl, errorBox(e));
      }
      return;
    }
    if (mine !== seq) {
      return;
    }
    drawSummary(res || {});
    drawItems(res || {});
  }

  const search = debounce(() => { state.offset = 0; load(); }, 300);
  const controls = h('div', { class: 'impact-controls', role: 'search' },
    field('Search people', inputText(state, 'q', {
      id: uid('impact-q'), type: 'search', placeholder: 'Name, employee ID or AD account', 'data-test': 'impact-search', oninput: search,
    })),
    field('Show', typeSel),
    hasProposal
      ? inputCheck(state, 'only_changed', 'Only people who would be treated differently', {
        'data-test': 'impact-only-changed',
        onchange: () => { state.offset = 0; load(); },
      })
      : null);

  const el = h('div', { class: 'impact', 'data-test': 'impact' }, metaEl, summaryEl, controls, countEl, resultsEl);
  load();
  return { el, load };
}

/** The panel in a wide dialog, stacked over the editor that opened it. */
export function openImpactModal({ title = 'Preview impact', proposal = {} } = {}) {
  const panel = impactPanel({ proposal, onlyChanged: true });
  let modal = null;
  const close = btn('Close', { onclick: () => modal.close(undefined) });
  modal = openModal({
    title,
    wide: true,
    content: h('div', null,
      h('p', { class: 'help' }, 'Nothing is saved or changed: this compares what the next sync would do with your unsaved changes against what it would do with the saved configuration.'),
      panel.el),
    footer: [h('span', { class: 'spacer' }), close],
    initialFocus: '[data-test="impact-search"]',
  });
  return modal;
}

export async function render(root) {
  const panel = impactPanel({ proposal: {} });
  root.append(
    pageHeader('Preview changes'),
    card(null,
      h('p', { class: 'help' }, 'Everything the next sync would do, person by person, worked out from the latest HR data and AD - nothing is changed. Search for a person to see exactly what would happen to their account. The role rule, mapping and lifecycle group editors have a "Preview impact" button that shows the effect of unsaved changes before you save them.'),
      panel.el));
}
