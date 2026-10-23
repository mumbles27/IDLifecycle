// components.js - widgets shared by several views: attribute-mapping fields, sample record picker,
// live template preview.

import { api } from './api.js';
import {
  h, uid, clear, field, inputText, inputArea, inputSelect, inputCheck, inputNumber,
  listItems, debounce, reportError, pretty, isObject, cellText,
} from './ui.js';

export const MODE_OPTIONS = [
  { value: 'create_only', label: 'create_only - set once at account creation' },
  { value: 'sync', label: 'sync - push when the source value changes' },
  { value: 'immutable', label: 'immutable - enforce on every run' },
];

export const MODE_INFO = {
  create_only: 'Set once, when the AD account is created. It is never touched again: later changes in the HR source or edits made in AD are ignored.',
  sync: 'Pushed to AD only when the value in the HR source changes. A manual edit made in AD survives until the source value changes again.',
  immutable: 'Enforced on every run: if AD differs from the value derived from the source, AD is overwritten back. Use for fields only HR should control.',
};

export const ON_EMPTY_OPTIONS = [
  { value: 'skip', label: 'skip - leave the AD attribute alone' },
  { value: 'clear', label: 'clear - empty the AD attribute' },
];

export const ON_EMPTY_INFO = {
  skip: 'If the template renders an empty string, nothing is written and any existing AD value is kept.',
  clear: 'If the template renders an empty string, the attribute is cleared in AD.',
};

export const VALUE_TYPE_OPTIONS = [
  { value: 'string', label: 'string - plain text' },
  { value: 'user_ref', label: 'user_ref - reference to another user' },
];

export const VALUE_TYPE_INFO = {
  string: 'The rendered text is written to the AD attribute as-is.',
  user_ref: 'The rendered text identifies another person (for example the manager\'s employee ID). The service resolves it to that person\'s AD account before writing.',
};

/** A select plus a live explanation paragraph underneath. */
export function selectWithHelp(label, model, key, options, info, attrs = {}) {
  const help = h('p', { class: 'help', 'aria-live': 'polite' }, info[model[key]] || '');
  const sel = inputSelect(model, key, options, {
    ...attrs,
    onchange: (e) => { help.textContent = info[e.target.value] || ''; },
  });
  if (!sel.id) {
    sel.id = uid('f');
  }
  sel.setAttribute('aria-describedby', `${sel.id}-help`);
  help.id = `${sel.id}-help`;
  return h('div', { class: 'field' }, h('label', { for: sel.id }, label), sel, help);
}

export function defaultAttrModel() {
  return {
    target_attr: '',
    template: '',
    mode: 'sync',
    on_empty: 'skip',
    value_type: 'string',
  };
}

/** Fields of one attribute grant (used inside role editor rows). Returns a Node. */
export function attributeGrantFields(model, onTemplateInput) {
  const tpl = inputArea(model, 'template', {
    rows: '2',
    class: 'mono',
    placeholder: '{{ .FirstName }} {{ .LastName }}',
    oninput: () => onTemplateInput && onTemplateInput(),
  });
  return h('div', { class: 'grant-fields' },
    field('Target attribute (LDAP name)', inputText(model, 'target_attr', { required: true, placeholder: 'title' }), { required: true }),
    field('Template', tpl, { required: true }),
    selectWithHelp('Mode', model, 'mode', MODE_OPTIONS, MODE_INFO),
    selectWithHelp('When empty', model, 'on_empty', ON_EMPTY_OPTIONS, ON_EMPTY_INFO),
    selectWithHelp('Value type', model, 'value_type', VALUE_TYPE_OPTIONS, VALUE_TYPE_INFO));
}

export { inputCheck, inputNumber };

// ---------------------------------------------------------------- sample record

const DEFAULT_SAMPLE = {
  FirstName: 'Jane',
  LastName: 'Doe',
  EmployeeId: 'E1001',
  Title: 'Analyst',
  Department: 'Finance',
  Email: 'jane.doe@example.com',
};

const shared = { text: pretty(DEFAULT_SAMPLE) };

let sourcesCache = null;

async function fetchSources() {
  if (!sourcesCache) {
    sourcesCache = listItems(await api.get('/sources', { limit: 500 }));
  }
  return sourcesCache;
}

export function invalidateSourceCache() {
  sourcesCache = null;
}

/**
 * Sample record editor: JSON textarea + "load sample from source".
 * getRecord() throws Error with a friendly message when the JSON is invalid.
 */
export function sampleRecordPicker({ onChange } = {}) {
  const area = h('textarea', {
    rows: '7',
    class: 'mono',
    spellcheck: 'false',
    'data-test': 'sample-json',
    value: shared.text,
    oninput: () => {
      shared.text = area.value;
      if (onChange) {
        onChange();
      }
    },
  });
  const areaField = field('Sample record (JSON object of source attributes)', area);

  const sourceSel = h('select', { 'aria-label': 'Source to load a sample from' }, h('option', { value: '' }, 'Load sample from source…'));
  const rowSel = h('select', { 'aria-label': 'Sample row', hidden: true });
  const status = h('span', { class: 'muted small', role: 'status' });
  let rows = [];
  let populated = false;

  const populateSources = async () => {
    if (populated) {
      return;
    }
    populated = true;
    try {
      const sources = await fetchSources();
      for (const s of sources) {
        sourceSel.appendChild(h('option', { value: s.id }, `${s.name} (${s.type})`));
      }
      if (sources.length === 0) {
        status.textContent = 'No sources configured yet.';
      }
    }
    catch (e) {
      populated = false;
      reportError(e);
    }
  };
  sourceSel.addEventListener('focus', populateSources);
  sourceSel.addEventListener('mousedown', populateSources);
  populateSources();

  const loadBtn = h('button', {
    type: 'button',
    class: 'btn btn-sm',
    'data-test': 'load-sample',
    onclick: async () => {
      if (!sourceSel.value) {
        status.textContent = 'Choose a source first.';
        return;
      }
      loadBtn.disabled = true;
      status.textContent = 'Fetching sample rows…';
      try {
        const res = await api.post(`/sources/${encodeURIComponent(sourceSel.value)}/test`, {});
        rows = res && Array.isArray(res.sample) ? res.sample : [];
        clear(rowSel);
        rows.forEach((row, i) => {
          const vals = isObject(row) ? Object.values(row).slice(0, 3).map(cellText).join(', ') : '';
          rowSel.appendChild(h('option', { value: String(i) }, `Row ${i + 1}: ${vals}`));
        });
        rowSel.hidden = rows.length === 0;
        if (res && res.ok === false) {
          status.textContent = res.error || 'The source test failed.';
        }
        else if (rows.length === 0) {
          status.textContent = 'The source returned no sample rows.';
        }
        else {
          status.textContent = `Loaded ${rows.length} sample row(s).`;
          applyRow(0);
        }
      }
      catch (e) {
        status.textContent = '';
        reportError(e);
      }
      finally {
        loadBtn.disabled = false;
      }
    },
  }, 'Fetch');

  function applyRow(i) {
    if (!rows[i]) {
      return;
    }
    area.value = pretty(rows[i]);
    shared.text = area.value;
    if (onChange) {
      onChange();
    }
  }
  rowSel.addEventListener('change', () => applyRow(Number(rowSel.value)));

  const el = h('div', { class: 'sample-picker' },
    areaField,
    h('div', { class: 'row-gap' }, sourceSel, loadBtn, rowSel, status));

  return {
    el,
    getRecord() {
      let parsed;
      try {
        parsed = JSON.parse(area.value);
      }
      catch (e) {
        throw new Error(`Sample record is not valid JSON: ${e.message}`);
      }
      if (!isObject(parsed)) {
        throw new Error('Sample record must be a JSON object, e.g. {"FirstName": "Jane"}.');
      }
      return parsed;
    },
  };
}

// ---------------------------------------------------------------- template preview

/**
 * previewPanel({ getTemplates }) -> { el, run }
 * getTemplates() returns [{ label, template }]. Output is rendered by POST /api/templates/preview.
 */
export function previewPanel({ getTemplates, title = 'Live template preview' }) {
  const out = h('div', { class: 'preview-out', 'aria-live': 'polite', 'data-test': 'preview-out' });
  let seq = 0;

  const run = async () => {
    seq += 1;
    const mine = seq;
    let record;
    try {
      record = picker.getRecord();
    }
    catch (e) {
      clear(out).appendChild(h('p', { class: 'preview-error' }, e.message));
      return;
    }
    const templates = getTemplates();
    if (templates.length === 0) {
      clear(out).appendChild(h('p', { class: 'muted' }, 'Nothing to preview yet.'));
      return;
    }
    const results = await Promise.all(templates.map(async (t) => {
      if (!String(t.template || '').trim()) {
        return { label: t.label, empty: true };
      }
      try {
        const res = await api.post('/templates/preview', { template: t.template, record });
        return { label: t.label, res };
      }
      catch (e) {
        return { label: t.label, err: e.message };
      }
    }));
    if (mine !== seq) {
      return;
    }
    clear(out);
    for (const r of results) {
      let body;
      if (r.empty) {
        body = h('span', { class: 'muted' }, '(empty template)');
      }
      else if (r.err) {
        body = h('span', { class: 'preview-error' }, r.err);
      }
      else if (r.res && r.res.ok === false) {
        body = h('span', { class: 'preview-error' }, r.res.error || 'Template error');
      }
      else {
        const text = r.res ? r.res.output : '';
        body = text === '' || text === undefined || text === null
          ? h('span', { class: 'muted' }, '(renders empty)')
          : h('code', { class: 'preview-value' }, String(text));
      }
      out.appendChild(h('div', { class: 'preview-row' }, h('span', { class: 'preview-label' }, r.label), body));
    }
  };

  const debounced = debounce(run, 400);
  const picker = sampleRecordPicker({ onChange: debounced });
  const button = h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'preview-run', onclick: () => run() }, 'Preview now');

  const el = h('section', { class: 'preview-panel' },
    h('h3', null, title),
    h('p', { class: 'help' }, 'Templates use Go text/template syntax against the source attributes, e.g. ', h('code', null, '{{ .FirstName | lower }}'), '. Missing keys render empty.'),
    h('details', { class: 'help' },
      h('summary', null, 'Template functions'),
      h('p', null, h('code', null, 'lower upper title trim first(s,n) last(s,n) substr(s,start,len) replace(s,old,new) default(v,fallback) coalesce(a,b,...) join(sep,a,b,..) ascii(s) digits(s) alnum(s) split(s,sep)')),
      h('p', null, 'Pipe style works: ', h('code', null, '{{ .First | lower }}'), '. For keys with dots use ', h('code', null, '{{ index . "currentStatus.statusType" }}'), '.')),
    picker.el,
    h('div', { class: 'row-gap' }, button),
    out);

  return { el, run: debounced, runNow: run };
}

// ---------------------------------------------------------------- run counters

export function counterGrid(counters) {
  const entries = Object.entries(counters || {});
  if (entries.length === 0) {
    return h('p', { class: 'muted' }, 'No counters recorded.');
  }
  return h('dl', { class: 'counters' }, entries.map(([k, v]) => h('div', { class: 'counter' },
    h('dt', null, k.replace(/_/g, ' ')),
    h('dd', null, cellText(v)))));
}
