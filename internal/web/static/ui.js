// ui.js - tiny DOM toolkit shared by every view.
//
// Rules this file enforces for the whole app (the server sends a strict CSP):
//   * DOM is built with createElement/textContent only. There is deliberately no
//     HTML-string injection anywhere in the code base (a Go test greps for it).
//   * No inline style="" attributes; styling is class based (app.css).
//   * Event handlers are attached with addEventListener, never as attributes.

const PROP_ATTRS = new Set([
  'value', 'checked', 'selected', 'disabled', 'hidden', 'readOnly', 'multiple',
  'required', 'open', 'indeterminate', 'autofocus',
]);

let uidCounter = 0;

export function uid(prefix = 'id') {
  uidCounter += 1;
  return `${prefix}-${uidCounter}`;
}

/**
 * h('div.card.wide', { id: 'x', onclick: fn, 'aria-label': 'y' }, child, 'text', [more])
 * - "tag.class1.class2" shorthand for classes
 * - attrs: class, text, dataset, on* (function => addEventListener), boolean props
 * - children: strings/numbers become text nodes; null/undefined/false are skipped; arrays are flattened
 */
export function h(tag, attrs, ...children) {
  const parts = String(tag).split('.');
  const el = document.createElement(parts[0] || 'div');
  for (let i = 1; i < parts.length; i += 1) {
    if (parts[i]) {
      el.classList.add(parts[i]);
    }
  }

  let deferredValue;
  let hasDeferredValue = false;

  if (attrs && typeof attrs === 'object' && !(attrs instanceof Node) && !Array.isArray(attrs)) {
    for (const [key, val] of Object.entries(attrs)) {
      if (val === undefined || val === null) {
        continue;
      }
      if (key === 'class') {
        for (const c of String(Array.isArray(val) ? val.join(' ') : val).split(/\s+/)) {
          if (c) {
            el.classList.add(c);
          }
        }
      }
      else if (key === 'text') {
        el.textContent = String(val);
      }
      else if (key === 'style') {
        // Inline styles are blocked by CSP; ignore on purpose.
        continue;
      }
      else if (key === 'dataset') {
        for (const [dk, dv] of Object.entries(val)) {
          el.dataset[dk] = String(dv);
        }
      }
      else if (key.startsWith('on') && typeof val === 'function') {
        el.addEventListener(key.slice(2).toLowerCase(), val);
      }
      else if (key === 'value') {
        deferredValue = val;
        hasDeferredValue = true;
      }
      else if (PROP_ATTRS.has(key)) {
        el[key] = Boolean(val);
      }
      else if (val === false) {
        continue;
      }
      else if (val === true) {
        el.setAttribute(key, '');
      }
      else {
        el.setAttribute(key, String(val));
      }
    }
  }
  else if (attrs !== undefined && attrs !== null) {
    children.unshift(attrs);
  }

  appendChildren(el, children);

  if (hasDeferredValue) {
    el.value = deferredValue;
  }
  return el;
}

function appendChildren(el, children) {
  for (const child of children) {
    if (child === undefined || child === null || child === false || child === true) {
      continue;
    }
    if (Array.isArray(child)) {
      appendChildren(el, child);
    }
    else if (child instanceof Node) {
      el.appendChild(child);
    }
    else {
      el.appendChild(document.createTextNode(String(child)));
    }
  }
}

export function clear(el) {
  while (el.firstChild) {
    el.removeChild(el.firstChild);
  }
  return el;
}

export function replaceContent(el, ...children) {
  clear(el);
  appendChildren(el, children);
  return el;
}

// ---------------------------------------------------------------- formatting

export function isObject(v) {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

export function listItems(resp) {
  if (Array.isArray(resp)) {
    return resp;
  }
  if (resp && Array.isArray(resp.items)) {
    return resp.items;
  }
  return [];
}

export function fmtTime(value) {
  if (!value) {
    return '';
  }
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) {
    return String(value);
  }
  return d.toLocaleString();
}

export function relTime(value) {
  if (!value) {
    return '';
  }
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) {
    return '';
  }
  const diff = d.getTime() - Date.now();
  const abs = Math.abs(diff);
  const mins = Math.round(abs / 60000);
  let text;
  if (abs < 45000) {
    text = 'moments';
  }
  else if (mins < 60) {
    text = `${mins} min`;
  }
  else if (mins < 60 * 36) {
    text = `${Math.round(mins / 60)} h`;
  }
  else {
    text = `${Math.round(mins / 1440)} d`;
  }
  if (text === 'moments') {
    return diff >= 0 ? 'in a moment' : 'just now';
  }
  return diff >= 0 ? `in ${text}` : `${text} ago`;
}

export function fmtDuration(start, end) {
  if (!start || !end) {
    return '';
  }
  const ms = new Date(end).getTime() - new Date(start).getTime();
  if (!Number.isFinite(ms) || ms < 0) {
    return '';
  }
  const s = Math.round(ms / 1000);
  if (s < 60) {
    return `${s}s`;
  }
  const m = Math.floor(s / 60);
  if (m < 60) {
    return `${m}m ${s % 60}s`;
  }
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

export function pretty(value) {
  try {
    return JSON.stringify(value, null, 2);
  }
  catch (e) {
    return String(value);
  }
}

export function cellText(value) {
  if (value === null || value === undefined) {
    return '';
  }
  if (typeof value === 'object') {
    try {
      return JSON.stringify(value);
    }
    catch (e) {
      return String(value);
    }
  }
  return String(value);
}

export function linesToList(text) {
  return String(text || '').split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
}

export function listToLines(list) {
  return Array.isArray(list) ? list.join('\n') : '';
}

export function csvToList(text) {
  return String(text || '').split(',').map((s) => s.trim()).filter(Boolean);
}

export function listToCsv(list) {
  return Array.isArray(list) ? list.join(', ') : '';
}

export function debounce(fn, ms = 350) {
  let t = null;
  const wrapped = (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
  wrapped.cancel = () => clearTimeout(t);
  return wrapped;
}

// ---------------------------------------------------------------- pills

const KNOWN_PILLS = new Set([
  'pending', 'approved', 'applying', 'applied', 'failed', 'rejected', 'superseded', 'simulated',
  'running', 'ok', 'error', 'aborted', 'new', 'linked', 'disabled', 'deleted', 'active', 'inactive',
  'enabled', 'admin', 'approver', 'viewer', 'live', 'dry_run', 'create_only', 'sync', 'immutable', 'undone', 'undo_pending', 'partly_undone', 'manual', 'auto', 'never', 'leaving', 'deletion',
]);

export function pill(status, label) {
  const raw = String(status === undefined || status === null || status === '' ? 'unknown' : status);
  const slug = raw.toLowerCase().replace(/[^a-z0-9_]+/g, '-');
  const cls = KNOWN_PILLS.has(slug) ? `pill pill-${slug}` : 'pill pill-neutral';
  return h('span', { class: cls }, label || raw.replace(/_/g, ' '));
}

// ---------------------------------------------------------------- toasts

let toastHost = null;

function ensureToastHost() {
  if (toastHost && document.body.contains(toastHost)) {
    return toastHost;
  }
  toastHost = h('div', { class: 'toasts', id: 'toasts', role: 'region', 'aria-label': 'Notifications' });
  document.body.appendChild(toastHost);
  return toastHost;
}

export function toast(message, kind = 'info') {
  const host = ensureToastHost();
  const el = h('div', { class: `toast toast-${kind}`, role: kind === 'error' ? 'alert' : 'status' });
  const close = h('button', {
    type: 'button',
    class: 'toast-close',
    'aria-label': 'Dismiss notification',
    onclick: () => el.remove(),
  }, '×');
  el.append(h('span', { class: 'toast-msg' }, String(message)), close);
  host.appendChild(el);
  while (host.children.length > 5) {
    host.firstChild.remove();
  }
  if (kind !== 'error') {
    setTimeout(() => el.remove(), 5000);
  }
  return el;
}

export function reportError(err) {
  if (err && err.handled) {
    return;
  }
  const msg = err && err.message ? err.message : String(err);
  // eslint-disable-next-line no-console
  console.error(err);
  toast(msg, 'error');
}

/** Run an async action; disables the button meanwhile and toasts failures. */
export async function act(button, fn) {
  if (button) {
    button.disabled = true;
  }
  try {
    return await fn();
  }
  catch (e) {
    reportError(e);
    return undefined;
  }
  finally {
    if (button && button.isConnected) {
      button.disabled = false;
    }
  }
}

// ---------------------------------------------------------------- modals

const modalStack = [];
const FOCUSABLE = 'a[href],button:not([disabled]),input:not([disabled]):not([type=hidden]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

function focusables(root) {
  return Array.from(root.querySelectorAll(FOCUSABLE)).filter((el) => !el.hidden && el.offsetParent !== null);
}

function updateInert() {
  const app = document.getElementById('app');
  if (app) {
    if (modalStack.length > 0) {
      app.setAttribute('inert', '');
    }
    else {
      app.removeAttribute('inert');
    }
  }
  document.body.classList.toggle('modal-open', modalStack.length > 0);
}

document.addEventListener('keydown', (ev) => {
  if (modalStack.length === 0) {
    return;
  }
  const top = modalStack[modalStack.length - 1];
  if (ev.key === 'Escape') {
    // An open combobox list (group picker) closes itself first; the dialog stays open.
    const t = ev.target;
    if (t && t.getAttribute && t.getAttribute('role') === 'combobox' && t.getAttribute('aria-expanded') === 'true') {
      return;
    }
    ev.preventDefault();
    ev.stopPropagation();
    top.close(undefined);
    return;
  }
  if (ev.key === 'Tab') {
    const items = focusables(top.dlg);
    if (items.length === 0) {
      ev.preventDefault();
      top.dlg.focus();
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    const active = document.activeElement;
    if (ev.shiftKey && (active === first || !top.dlg.contains(active))) {
      ev.preventDefault();
      last.focus();
    }
    else if (!ev.shiftKey && (active === last || !top.dlg.contains(active))) {
      ev.preventDefault();
      first.focus();
    }
  }
}, true);

/**
 * openModal({ title, content, footer, wide, bare, onClose })
 * Returns { el, close(result), body }. Escape closes, Tab is trapped, focus is restored.
 */
export function openModal({ title, content, footer, wide = false, bare = false, onClose, initialFocus } = {}) {
  const previous = document.activeElement;
  const titleId = uid('modal-title');
  let closed = false;

  const closeBtn = h('button', {
    type: 'button',
    class: 'icon-btn',
    'aria-label': 'Close dialog',
    onclick: () => entry.close(undefined),
  }, '×');

  const body = bare ? content : h('div', { class: 'modal-body' }, content);
  const dlg = h('div', {
    class: wide ? 'modal modal-wide' : 'modal',
    role: 'dialog',
    'aria-modal': 'true',
    'aria-labelledby': titleId,
    tabindex: '-1',
  },
  h('div', { class: 'modal-header' }, h('h2', { id: titleId }, title || ''), closeBtn),
  body,
  !bare && footer ? h('div', { class: 'modal-footer' }, footer) : null);

  const backdrop = h('div', { class: 'modal-backdrop' }, dlg);

  const entry = {
    dlg,
    body,
    el: dlg,
    close(result) {
      if (closed) {
        return;
      }
      closed = true;
      const idx = modalStack.indexOf(entry);
      if (idx >= 0) {
        modalStack.splice(idx, 1);
      }
      backdrop.remove();
      updateInert();
      if (previous && previous.isConnected && typeof previous.focus === 'function') {
        previous.focus();
      }
      if (onClose) {
        onClose(result);
      }
    },
  };

  modalStack.push(entry);
  document.body.appendChild(backdrop);
  updateInert();

  const target = (initialFocus && dlg.querySelector(initialFocus))
    || focusables(body)[0]
    || closeBtn;
  target.focus();
  return entry;
}

/**
 * formModal({ title, content, submitLabel, danger, wide, onSubmit })
 * onSubmit may be async; return false to keep the modal open. Errors are toasted and keep it open.
 */
export function formModal({ title, content, submitLabel = 'Save', cancelLabel = 'Cancel', danger = false, wide = false, onSubmit, onClose, extraFooter, initialFocus }) {
  let modal = null;
  const submit = h('button', { type: 'submit', class: danger ? 'btn btn-danger' : 'btn btn-primary' }, submitLabel);
  const cancel = h('button', { type: 'button', class: 'btn', onclick: () => modal.close(undefined) }, cancelLabel);
  const form = h('form', {
    class: 'modal-form',
    onsubmit: async (ev) => {
      ev.preventDefault();
      if (!form.reportValidity()) {
        return;
      }
      submit.disabled = true;
      try {
        const res = await onSubmit(modal);
        if (res !== false) {
          modal.close(res === undefined ? true : res);
        }
      }
      catch (e) {
        reportError(e);
      }
      finally {
        submit.disabled = false;
      }
    },
  },
  h('div', { class: 'modal-body' }, content),
  h('div', { class: 'modal-footer' }, extraFooter, h('span', { class: 'spacer' }), cancel, submit));
  modal = openModal({ title, content: form, bare: true, wide, initialFocus, onClose });
  return modal;
}

export function confirmDialog({ title = 'Please confirm', message, confirmLabel = 'Confirm', cancelLabel = 'Cancel', danger = false }) {
  return new Promise((resolve) => {
    let settled = false;
    const done = (v) => {
      if (!settled) {
        settled = true;
        resolve(v);
      }
    };
    let modal = null;
    const ok = h('button', { type: 'button', class: danger ? 'btn btn-danger' : 'btn btn-primary', onclick: () => { done(true); modal.close(true); } }, confirmLabel);
    const cancel = h('button', { type: 'button', class: 'btn', onclick: () => modal.close(false) }, cancelLabel);
    modal = openModal({
      title,
      content: h('div', { class: 'confirm-message' }, message),
      footer: [h('span', { class: 'spacer' }), cancel, ok],
      onClose: () => done(false),
    });
    (danger ? cancel : ok).focus();
  });
}

/** Ask for a free-text note. Resolves to the note string, or null when cancelled. */
export function noteDialog({ title, message, label = 'Note', required = false, confirmLabel = 'Submit', danger = false, placeholder = '' }) {
  return new Promise((resolve) => {
    const model = { note: '' };
    const area = h('textarea', {
      rows: '4',
      required,
      placeholder,
      oninput: (e) => { model.note = e.target.value; },
    });
    const content = [
      message ? h('p', null, message) : null,
      field(label, area, { required }),
    ];
    let result = null;
    formModal({
      title,
      content,
      submitLabel: confirmLabel,
      danger,
      initialFocus: 'textarea',
      onSubmit: () => {
        if (required && !model.note.trim()) {
          area.setCustomValidity('A note is required.');
          area.reportValidity();
          area.setCustomValidity('');
          return false;
        }
        result = model.note.trim();
        return true;
      },
      onClose: () => resolve(result),
    });
  });
}

// ---------------------------------------------------------------- form helpers

/** Wrap a control with a bound label and optional help text. */
export function field(label, control, { help, required = false, className = '' } = {}) {
  if (!control.id) {
    control.id = uid('f');
  }
  const kids = [h('label', { for: control.id }, label, required ? h('span', { class: 'req', 'aria-hidden': 'true' }, ' *') : null), control];
  if (help) {
    const helpId = `${control.id}-help`;
    control.setAttribute('aria-describedby', helpId);
    kids.push(h('p', { class: 'help', id: helpId }, help));
  }
  return h('div', { class: `field ${className}`.trim() }, kids);
}

function optionNodes(options) {
  return options.map((o) => {
    const opt = typeof o === 'string' ? { value: o, label: o } : o;
    return h('option', { value: opt.value }, opt.label === undefined ? opt.value : opt.label);
  });
}

function withHandler(attrs, evt, fn) {
  const extra = attrs[evt];
  const rest = { ...attrs };
  delete rest[evt];
  return [rest, (e) => { fn(e); if (typeof extra === 'function') { extra(e); } }];
}

export function inputText(model, key, attrs = {}) {
  const [rest, handler] = withHandler(attrs, 'oninput', (e) => { model[key] = e.target.value; });
  return h('input', { type: 'text', autocomplete: 'off', spellcheck: 'false', ...rest, value: model[key] === undefined || model[key] === null ? '' : model[key], oninput: handler });
}

export function inputNumber(model, key, attrs = {}) {
  const [rest, handler] = withHandler(attrs, 'oninput', (e) => {
    const v = e.target.value;
    model[key] = v === '' ? '' : Number(v);
  });
  return h('input', { type: 'number', ...rest, value: model[key] === undefined || model[key] === null ? '' : model[key], oninput: handler });
}

export function inputArea(model, key, attrs = {}) {
  const [rest, handler] = withHandler(attrs, 'oninput', (e) => { model[key] = e.target.value; });
  return h('textarea', { rows: '4', spellcheck: 'false', ...rest, value: model[key] === undefined || model[key] === null ? '' : model[key], oninput: handler });
}

export function inputSelect(model, key, options, attrs = {}) {
  const [rest, handler] = withHandler(attrs, 'onchange', (e) => { model[key] = e.target.value; });
  return h('select', { ...rest, value: model[key] === undefined || model[key] === null ? '' : model[key], onchange: handler }, optionNodes(options));
}

export function inputCheck(model, key, label, attrs = {}) {
  const [rest, handler] = withHandler(attrs, 'onchange', (e) => { model[key] = e.target.checked; });
  const input = h('input', { type: 'checkbox', ...rest, id: rest.id || uid('c'), checked: Boolean(model[key]), onchange: handler });
  return h('div', { class: 'check' }, input, h('label', { for: input.id }, label));
}

// ---------------------------------------------------------------- composite widgets

export function loading(text = 'Loading…') {
  return h('p', { class: 'muted loading', role: 'status' }, text);
}

export function emptyState(text) {
  return h('p', { class: 'empty' }, text);
}

export function errorBox(err) {
  const msg = err && err.message ? err.message : String(err);
  return h('div', { class: 'notice notice-error', role: 'alert' }, msg);
}

export function notice(kind, ...children) {
  return h('div', { class: `notice notice-${kind}` }, children);
}

export function pageHeader(title, ...actions) {
  return h('div', { class: 'page-header' }, h('h1', null, title), h('div', { class: 'page-actions' }, actions));
}

export function card(title, ...children) {
  return h('section', { class: 'card' }, title ? h('h2', null, title) : null, children);
}

/** Text that is clipped to `max` characters; clicking toggles the full text. */
export function truncCell(value, max = 80) {
  const text = cellText(value);
  if (text.length <= max) {
    return h('span', { class: 'cell-text' }, text);
  }
  let expanded = false;
  const btn = h('button', {
    type: 'button',
    class: 'trunc',
    'aria-expanded': 'false',
    title: 'Click to expand or collapse',
  }, `${text.slice(0, max)}…`);
  btn.addEventListener('click', () => {
    expanded = !expanded;
    btn.textContent = expanded ? text : `${text.slice(0, max)}…`;
    btn.classList.toggle('trunc-open', expanded);
    btn.setAttribute('aria-expanded', String(expanded));
  });
  return btn;
}

/**
 * dataTable(columns, rows, { caption, empty, expand(row) -> Node, rowClass(row) })
 * columns: [{ label, key, render(row, index), className }]
 * When `expand` is given a toggle column is added; the detail node is built lazily.
 */
export function dataTable(columns, rows, opts = {}) {
  const { caption, empty = 'Nothing to show.', expand, rowClass } = opts;
  if (!rows || rows.length === 0) {
    return emptyState(empty);
  }
  const headCells = [];
  if (expand) {
    headCells.push(h('th', { scope: 'col', class: 'col-toggle' }, h('span', { class: 'sr-only' }, 'Details')));
  }
  for (const c of columns) {
    headCells.push(h('th', { scope: 'col', class: c.className || '' }, c.label));
  }
  const span = columns.length + (expand ? 1 : 0);
  const body = h('tbody');
  rows.forEach((row, index) => {
    const cells = [];
    let detailRow = null;
    let detailBuilt = false;
    if (expand) {
      const toggle = h('button', {
        type: 'button',
        class: 'icon-btn toggle',
        'aria-expanded': 'false',
        'aria-label': 'Show details',
        'data-test': 'row-toggle',
        onclick: () => {
          const open = toggle.getAttribute('aria-expanded') !== 'true';
          toggle.setAttribute('aria-expanded', String(open));
          toggle.setAttribute('aria-label', open ? 'Hide details' : 'Show details');
          if (open && !detailBuilt) {
            detailBuilt = true;
            detailRow.firstChild.appendChild(expand(row));
          }
          detailRow.hidden = !open;
        },
      }, h('span', { class: 'chev', 'aria-hidden': 'true' }, '▸'));
      cells.push(h('td', { class: 'col-toggle' }, toggle));
    }
    for (const c of columns) {
      const content = c.render ? c.render(row, index) : cellText(row[c.key]);
      cells.push(h('td', { class: c.className || '', 'data-label': typeof c.label === 'string' ? c.label : null }, content));
    }
    const tr = h('tr', { class: rowClass ? rowClass(row) : '' }, cells);
    body.appendChild(tr);
    if (expand) {
      detailRow = h('tr', { class: 'detail-row', hidden: true }, h('td', { colspan: String(span) }));
      body.appendChild(detailRow);
    }
  });
  return h('div', { class: `table-wrap ${opts.wrapClass || ''}`.trim() },
    h('table', { class: 'data' },
      caption ? h('caption', { class: 'sr-only' }, caption) : null,
      h('thead', null, h('tr', null, headCells)),
      body));
}

/** Pager with "Showing x-y of N", Prev/Next and page size. */
export function pager({ total, limit, offset, onChange }) {
  const t = Number.isFinite(total) ? total : 0;
  const from = t === 0 ? 0 : offset + 1;
  const to = Math.min(offset + limit, t);
  const prev = h('button', { type: 'button', class: 'btn btn-sm', disabled: offset <= 0, onclick: () => onChange(Math.max(0, offset - limit), limit) }, 'Previous');
  const next = h('button', { type: 'button', class: 'btn btn-sm', disabled: offset + limit >= t, onclick: () => onChange(offset + limit, limit) }, 'Next');
  const sizeId = uid('ps');
  const size = h('select', {
    id: sizeId,
    value: String(limit),
    onchange: (e) => onChange(0, Number(e.target.value)),
  }, [10, 25, 50, 100, 200].map((n) => h('option', { value: String(n) }, String(n))));
  return h('nav', { class: 'pager', 'aria-label': 'Pagination' },
    h('span', { class: 'pager-info', 'data-test': 'pager-info' }, `Showing ${from}–${to} of ${t}`),
    h('span', { class: 'spacer' }),
    h('label', { for: sizeId, class: 'inline-label' }, 'Rows'),
    size,
    prev,
    next);
}

/** Definition list from [label, value] pairs. */
export function dl(pairs) {
  const list = h('dl', { class: 'kv' });
  for (const [k, v] of pairs) {
    list.append(h('dt', null, k), h('dd', null, v === undefined || v === null || v === '' ? h('span', { class: 'muted' }, '—') : v));
  }
  return list;
}

export function chip(text) {
  return h('span', { class: 'chip' }, text);
}

const CHANGE_TYPE_NAMES = { cloud_groups: 'cloud groups' };

/** The word shown for a change type ("cloud_groups" reads "cloud groups"; the rest are shown as they are). */
export function changeTypeName(type) {
  return CHANGE_TYPE_NAMES[type] || type || '';
}

/** The coloured tag of a change type in lists (the text carries the meaning; the colour only repeats it). */
export function typeTag(type) {
  return h('span', { class: `type-tag type-${type}` }, changeTypeName(type));
}

export function codeBlock(value) {
  return h('pre', { class: 'code' }, typeof value === 'string' ? value : pretty(value));
}

/** A button flagged as mutating so tests/viewers can verify role gating. */
export function mutBtn(label, attrs = {}) {
  return h('button', { type: 'button', ...attrs, class: `btn ${attrs.class || ''}`.trim(), 'data-mutating': '' }, label);
}

export function btn(label, attrs = {}) {
  return h('button', { type: 'button', ...attrs, class: `btn ${attrs.class || ''}`.trim() }, label);
}

/** The History page filtered to one object (everyone who can read History may follow it). */
export function historyHref(entityType, entityId) {
  return `#/audit${qs({ q: `type:${entityType} id:"${entityId}"` })}`;
}

export function historyLink(entityType, entityId, label = 'History') {
  return h('a', {
    class: 'btn btn-sm', href: historyHref(entityType, entityId), 'data-test': 'history-link', title: `Everything recorded about this ${entityType.replace('_', ' ')}`,
  }, label);
}

/** Query-string builder that skips empty values. */
export function qs(params) {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params || {})) {
    if (v === undefined || v === null || v === '') {
      continue;
    }
    p.set(k, String(v));
  }
  const s = p.toString();
  return s ? `?${s}` : '';
}
