// mapper.js - field mapper: a source's fields next to the AD user attributes, with which mappings use each
// side and which mappings read fields the source does not send. Admin only (it fetches live HR data and
// reads the AD schema through PowerShell). Used by the Attribute mappings view.

import { api } from '../api.js';
import { h, clear, replaceContent, listItems, loading, errorBox, notice, pill, chip, mutBtn, btn, uid, reportError } from '../ui.js';

/** Template that reads one source field: {{ .Name }}, or index syntax for keys that are not identifiers. */
export function templateFor(field) {
  if (/^[A-Za-z_][A-Za-z0-9_]*$/.test(field)) {
    return `{{ .${field} }}`;
  }
  return `{{ index . ${JSON.stringify(field)} }}`;
}

/**
 * joinReferences(fields, refs) -> { usedBy: Map(field -> [ref]), problems: [{ ref, missing, suggestions }] }
 * Same rules as the server: names match exactly (Go templates are case-sensitive); a missing name that
 * matches a field in another case gets a suggestion.
 */
export function joinReferences(fields, refs) {
  const names = new Set(fields.map((f) => f.name));
  const lower = new Map(fields.map((f) => [f.name.toLowerCase(), f.name]));
  const usedBy = new Map(fields.map((f) => [f.name, []]));
  const problems = [];
  for (const ref of refs) {
    const missing = [];
    const suggestions = {};
    for (const name of ref.fields || []) {
      if (names.has(name)) {
        usedBy.get(name).push(ref);
        continue;
      }
      missing.push(name);
      if (lower.has(name.toLowerCase())) {
        suggestions[name] = lower.get(name.toLowerCase());
      }
    }
    if (missing.length > 0 || ref.error) {
      problems.push({ ref, missing, suggestions });
    }
  }
  return { usedBy, problems };
}

function refLabel(ref) {
  return ref.kind === 'role' ? `role rule "${ref.name}"` : `mapping "${ref.name}"`;
}

function problem(...children) {
  return h('div', { class: 'notice notice-warn', 'data-test': 'mapper-problem' }, children);
}

/**
 * fieldMapper({ getMappings, onMapField(field), onMapAttribute(attr) }) -> { el, refresh(), fields(), attributes(), adLive() }
 * refresh() re-reads the references (after a mapping was saved or deleted) and redraws.
 */
export function fieldMapper({ getMappings, onMapField, onMapAttribute }) {
  const state = { sourceId: '', fields: [], refs: [], result: null, ad: null, adError: null, showAll: false, filter: '' };

  const sourceSel = h('select', { 'aria-label': 'Source', 'data-test': 'mapper-source' }, h('option', { value: '' }, 'Choose a source…'));
  const status = h('span', { class: 'muted small', role: 'status', 'data-test': 'mapper-status' });
  const loadBtn = btn('Load fields', { class: 'btn-primary btn-sm', 'data-test': 'mapper-load', onclick: () => loadFields() });
  const problemsEl = h('div', { 'data-test': 'mapper-problems' });
  const fieldsEl = h('div', { class: 'mapper-list', 'data-test': 'mapper-fields' },
    h('p', { class: 'muted' }, 'Choose a source and load its fields. This fetches the source once, the same way a sync does.'));
  const adEl = h('div', { class: 'mapper-list', 'data-test': 'mapper-ad' }, loading('Reading AD attributes…'));
  const adNote = h('p', { class: 'muted small', 'data-test': 'mapper-ad-source' });

  const filterId = uid('adf');
  const filter = h('input', {
    id: filterId,
    type: 'search',
    placeholder: 'Filter attributes',
    'data-test': 'mapper-ad-filter',
    oninput: () => { state.filter = filter.value.trim().toLowerCase(); drawAD(); },
  });
  const showAllId = uid('ada');
  const showAll = h('input', { id: showAllId, type: 'checkbox', onchange: () => { state.showAll = showAll.checked; drawAD(); } });
  const refreshBtn = btn('Refresh from AD', { class: 'btn-sm', 'data-test': 'mapper-ad-refresh', onclick: () => loadAD(true) });

  const el = h('section', { class: 'card mapper', 'data-test': 'field-mapper' },
    h('h2', null, 'Field mapper'),
    h('p', { class: 'help' }, 'See the fields a source sends next to the attributes AD accepts. Use Map… on either side to start a mapping with the template or attribute filled in. A renamed or removed source field shows up here as a problem before the next sync.'),
    h('div', { class: 'row-gap' }, sourceSel, loadBtn, status),
    problemsEl,
    h('div', { class: 'mapper-grid' },
      h('div', { class: 'mapper-pane' },
        h('h3', null, 'Source fields'),
        fieldsEl),
      h('div', { class: 'mapper-pane' },
        h('h3', null, 'AD user attributes'),
        h('div', { class: 'row-gap' },
          h('label', { for: filterId, class: 'sr-only' }, 'Filter attributes'), filter,
          h('label', { class: 'check inline-label', for: showAllId }, showAll, ' Show attributes that cannot be mapped'),
          refreshBtn),
        adNote,
        adEl)));

  async function populateSources() {
    try {
      const sources = listItems(await api.get('/sources', { limit: 500 }));
      for (const s of sources) {
        sourceSel.appendChild(h('option', { value: s.id }, `${s.name} (${s.type})`));
      }
      if (sources.length === 1) {
        sourceSel.value = sources[0].id;
      }
      if (sources.length === 0) {
        status.textContent = 'No sources configured yet.';
      }
    }
    catch (e) {
      reportError(e);
    }
  }

  async function loadFields() {
    if (!sourceSel.value) {
      status.textContent = 'Choose a source first.';
      return;
    }
    loadBtn.disabled = true;
    status.textContent = 'Fetching the source…';
    replaceContent(fieldsEl, loading('Fetching the source…'));
    try {
      const res = await api.post(`/mapper/sources/${encodeURIComponent(sourceSel.value)}/fields`, {});
      state.sourceId = sourceSel.value;
      state.result = res;
      state.fields = res.fields || [];
      state.refs = res.references || [];
      status.textContent = res.ok
        ? `${res.count} record(s), ${state.fields.length} field(s).`
        : (res.error || 'The source could not be read.');
    }
    catch (e) {
      state.result = null;
      state.fields = [];
      status.textContent = '';
      replaceContent(fieldsEl, errorBox(e));
      loadBtn.disabled = false;
      return;
    }
    loadBtn.disabled = false;
    draw();
  }

  async function loadAD(refresh = false) {
    refreshBtn.disabled = true;
    replaceContent(adEl, loading('Reading AD attributes…'));
    try {
      state.ad = await api.get('/mapper/ad-attributes', refresh ? { refresh: '1' } : undefined);
      state.adError = null;
    }
    catch (e) {
      state.ad = null;
      state.adError = e;
    }
    refreshBtn.disabled = false;
    drawAD();
    drawProblems();
  }

  async function refresh() {
    try {
      const res = await api.get('/mapper/references');
      state.refs = res.references || [];
    }
    catch (e) {
      reportError(e);
    }
    draw();
  }

  function draw() {
    drawFields();
    drawAD();
    drawProblems();
  }

  function drawProblems() {
    clear(problemsEl);
    const out = [];
    if (state.result && state.result.ok === false && state.result.error) {
      out.push(notice('error', `The source could not be read: ${state.result.error}`));
    }
    if (state.fields.length > 0) {
      const { problems } = joinReferences(state.fields, state.refs);
      for (const p of problems) {
        const parts = [h('strong', null, refLabel(p.ref)), p.ref.enabled === false ? ' (disabled)' : ''];
        if (p.ref.error) {
          parts.push(` has a template error: ${p.ref.error}`);
        }
        if (p.missing.length > 0) {
          parts.push(' reads ', ...p.missing.flatMap((m, i) => [i > 0 ? ', ' : '', h('code', null, m),
            p.suggestions[m] ? [' (the source sends ', h('code', null, p.suggestions[m]), ')'] : '']),
          ', which this source does not send. It will render empty for everyone from this source.');
        }
        out.push(problem(parts));
      }
    }
    if (state.ad && state.ad.source === 'live') {
      const known = new Map(state.ad.attributes.map((a) => [a.name.toLowerCase(), a]));
      for (const m of getMappings()) {
        const a = known.get(String(m.target_attr).toLowerCase());
        if (!a) {
          out.push(problem(h('strong', null, `mapping "${m.name}"`),
            ' targets ', h('code', null, m.target_attr), ', which the AD schema does not have for users.'));
        }
        else if (!a.mappable) {
          out.push(problem(h('strong', null, `mapping "${m.name}"`),
            ' targets ', h('code', null, a.name), `, which cannot be written: ${a.reason}.`));
        }
      }
    }
    if (out.length > 0) {
      problemsEl.append(...out);
    }
  }

  function drawFields() {
    if (!state.result) {
      return;
    }
    if (state.fields.length === 0) {
      replaceContent(fieldsEl, h('p', { class: 'muted' }, state.result.ok ? 'The source returned no fields.' : 'No fields to show.'));
      return;
    }
    const { usedBy } = joinReferences(state.fields, state.refs);
    const total = state.result.count || 0;
    const rows = state.fields.map((f) => {
      const users = usedBy.get(f.name) || [];
      const pct = total > 0 ? Math.round((100 * f.filled) / total) : 0;
      return h('div', { class: 'mapper-row', 'data-test': 'mapper-field', 'data-name': f.name },
        h('div', { class: 'mapper-main' },
          h('code', { class: 'mapper-name' }, f.name),
          f.role === 'external_id' ? pill('linked', 'link key') : null,
          f.role === 'status' ? pill('neutral', 'status') : null,
          h('span', { class: 'muted small mapper-fill', title: `${f.filled} of ${total} records have a value` }, `${pct}% filled`)),
        f.samples.length > 0
          ? h('div', { class: 'mapper-samples mono small' }, f.samples.join('  ·  '))
          : h('div', { class: 'mapper-samples muted small' }, 'always empty'),
        h('div', { class: 'mapper-used' },
          users.length > 0
            ? users.map((r) => chip(r.kind === 'role' ? `role: ${r.name}` : `${r.name} → ${r.target_attr}`))
            : h('span', { class: 'muted small', 'data-test': 'not-mapped' }, 'not used by any mapping or role rule')),
        h('div', { class: 'mapper-act' },
          mutBtn('Map…', { class: 'btn-sm', 'data-test': 'map-field', onclick: () => onMapField(f) })));
    });
    replaceContent(fieldsEl, rows);
  }

  function drawAD() {
    if (state.adError) {
      replaceContent(adEl, errorBox(state.adError));
      adNote.textContent = '';
      return;
    }
    if (!state.ad) {
      return;
    }
    adNote.textContent = state.ad.source === 'live'
      ? `From the live AD schema (${state.ad.attributes.length} attributes of the user class).`
      : `Built-in list of common attributes. The live schema is not available: ${state.ad.live_error}`;
    adNote.classList.toggle('mapper-fallback', state.ad.source !== 'live');

    const byTarget = new Map();
    for (const m of getMappings()) {
      const key = String(m.target_attr).toLowerCase();
      if (!byTarget.has(key)) {
        byTarget.set(key, []);
      }
      byTarget.get(key).push(m);
    }
    const list = state.ad.attributes.filter((a) => (state.showAll || a.mappable || byTarget.has(a.name.toLowerCase()))
      && (!state.filter || a.name.toLowerCase().includes(state.filter) || String(a.description || '').toLowerCase().includes(state.filter)));
    if (list.length === 0) {
      replaceContent(adEl, h('p', { class: 'muted' }, 'No attribute matches.'));
      return;
    }
    replaceContent(adEl, list.map((a) => {
      const maps = byTarget.get(a.name.toLowerCase()) || [];
      return h('div', { class: `mapper-row${a.mappable ? '' : ' mapper-off'}`, 'data-test': 'mapper-attr', 'data-name': a.name },
        h('div', { class: 'mapper-main' },
          h('code', { class: 'mapper-name' }, a.name),
          a.description ? h('span', { class: 'muted small' }, a.description) : null),
        a.mappable ? (a.hint ? h('div', { class: 'muted small' }, a.hint) : null) : h('div', { class: 'muted small' }, `Cannot be mapped: ${a.reason}.`),
        h('div', { class: 'mapper-used' },
          maps.map((m) => h('span', { class: 'mapper-mapped' }, chip(m.name), pill(m.mode)))),
        h('div', { class: 'mapper-act' },
          a.mappable ? mutBtn('Map…', { class: 'btn-sm', 'data-test': 'map-attr', onclick: () => onMapAttribute(a) }) : null));
    }));
  }

  populateSources();
  loadAD();

  return {
    el,
    refresh,
    redraw: draw,
    fields: () => state.fields,
    attributes: () => (state.ad ? state.ad.attributes : []),
    adLive: () => Boolean(state.ad && state.ad.source === 'live'),
  };
}
