// Add person: out-of-band people (contractors, early starters ...) typed into the console. They live in a "manual"
// source; a sync treats them like everyone else (mappings, roles, OU placement, naming, approvals).

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, errorBox, loading, field, act, toast, notice, dataTable, pill, fmtTime,
  relTime, confirmDialog, noteDialog, formModal, dl,
  historyLink,
} from '../ui.js';
import { conflictList } from '../components.js';

/** One input for a form field definition, bound to model[f.name]. */
function fieldControl(f, model) {
  const common = {
    required: !!f.required,
    'data-field': f.name,
    'data-test': `mp-${f.name}`,
  };
  if (model[f.name] === undefined) {
    model[f.name] = f.default || '';
  }
  if (f.kind === 'select') {
    const opts = Array.isArray(f.options) ? f.options : [];
    const known = opts.includes(model[f.name]) || model[f.name] === '';
    return h('select', { ...common, value: model[f.name], onchange: (e) => { model[f.name] = e.target.value; } },
      h('option', { value: '' }, f.required ? 'Choose…' : '(none)'),
      known ? null : h('option', { value: model[f.name] }, model[f.name]),
      opts.map((o) => h('option', { value: o }, o)));
  }
  const type = f.kind === 'date' ? 'date' : (f.kind === 'email' ? 'email' : 'text');
  return h('input', { ...common, type, autocomplete: 'off', value: model[f.name], oninput: (e) => { model[f.name] = e.target.value; } });
}

function personForm(fields, model) {
  return h('div', { class: 'grid grid-2' }, fields.map((f) => field(f.label || f.name, fieldControl(f, model), { required: !!f.required })));
}

/** Only the form's fields (trimmed), plus any other stored attributes when editing. */
function collect(fields, model, base) {
  const out = { ...(base || {}) };
  for (const f of fields) {
    out[f.name] = String(model[f.name] === undefined ? '' : model[f.name]).trim();
  }
  return out;
}

export async function render(root, ctx) {
  const canAdd = ctx.can('approver');
  const isAdmin = ctx.can('admin');
  const body = h('div', { class: 'stack' }, loading());
  root.append(pageHeader('Add person'), body);

  let sources;
  try {
    const res = await api.get('/manual/sources');
    sources = Array.isArray(res) ? res : ((res && Array.isArray(res.items)) ? res.items : []);
  }
  catch (e) {
    replaceContent(body, errorBox(e));
    return;
  }
  if (sources.length === 0) {
    replaceContent(body, notice('info',
      'There is no manual source yet. ',
      isAdmin ? ['Add one under ', h('a', { href: '#/sources' }, 'Sources'), ' (type "manual"): it defines the form people fill in here.']
        : 'Ask an administrator to add a source of type "manual" under Sources.'));
    return;
  }

  const state = { sid: ctx.query.get('source') || sources[0].id };
  if (!sources.some((s) => s.id === state.sid)) {
    state.sid = sources[0].id;
  }
  const picker = sources.length > 1
    ? field('Source', h('select', {
      'data-test': 'mp-source', value: state.sid,
      onchange: (e) => { state.sid = e.target.value; draw(); },
    }, sources.map((s) => h('option', { value: s.id }, `${s.name}${s.enabled ? '' : ' (disabled)'}`))))
    : null;
  const warnHost = h('div');
  const formHost = h('div');
  const listHost = h('div');
  replaceContent(body,
    h('p', { class: 'help' }, 'For people who are not in an HR feed yet: contractors, consultants, early starters. They are created and kept up to date like everyone else; if the person later shows up in the HR source, Matching links the existing account to them instead of creating a second one.'),
    picker, warnHost, formHost, listHost);

  const current = () => sources.find((s) => s.id === state.sid) || sources[0];
  const base = () => `/manual/sources/${encodeURIComponent(current().id)}`;

  function draw() {
    const src = current();
    const fields = Array.isArray(src.fields) ? src.fields : [];
    replaceContent(warnHost, src.enabled ? null : notice('warn', `The source "${src.name}" is disabled: people entered here are not synced until it is enabled.`));
    if (canAdd) {
      drawForm(src, fields);
    }
    else {
      replaceContent(formHost);
    }
    loadPeople(fields);
  }

  function drawForm(src, fields) {
    const model = {};
    const opts = { sync: true };
    const previewBox = h('div', { 'aria-live': 'polite', 'data-test': 'mp-preview' });
    const syncBox = h('div', { class: 'check' },
      h('input', { type: 'checkbox', id: 'mp-sync', checked: true, 'data-test': 'mp-sync', onchange: (e) => { opts.sync = e.target.checked; } }),
      h('label', { for: 'mp-sync' }, 'Run a sync now (the account is proposed under Approvals right away)'));
    const previewBtn = h('button', { type: 'button', class: 'btn', 'data-test': 'mp-preview-btn' }, 'Preview');
    const createBtn = h('button', { type: 'submit', class: 'btn btn-primary', 'data-mutating': '', 'data-test': 'mp-create' }, 'Create');
    const form = h('form', {
      class: 'stack',
      'data-test': 'mp-form',
      onsubmit: (ev) => {
        ev.preventDefault();
        if (!form.reportValidity()) {
          return;
        }
        act(createBtn, async () => {
          const res = await api.post(`${base()}/people`, { attrs: collect(fields, model), sync: opts.sync });
          const who = [model.FirstName, model.LastName].filter(Boolean).join(' ') || (res && res.external_id) || 'Person';
          toast(`${who} added as ${(res && res.external_id) || 'a new person'}${res && res.run_id ? '; a sync is running' : ''}.`, 'ok');
          drawForm(src, fields);
          loadPeople(fields);
          ctx.refreshStatus();
        });
      },
    },
    personForm(fields, model),
    syncBox,
    h('div', { class: 'row-gap' }, previewBtn, createBtn),
    previewBox);
    previewBtn.addEventListener('click', () => {
      if (!form.reportValidity()) {
        return;
      }
      act(previewBtn, async () => {
        const res = await api.post(`${base()}/preview`, { attrs: collect(fields, model) });
        showPreview(previewBox, res || {});
      });
    });
    replaceContent(formHost, card(`New person (${src.name})`,
      h('p', { class: 'help' }, `Fields marked * are required. The employee ID is generated (${src.id_prefix || ''}…). Preview shows the account the sync will propose.`),
      form));
  }

  function showPreview(box, res) {
    const n = res.names || {};
    const conflicts = Array.isArray(res.conflicts) ? res.conflicts : [];
    const warnings = Array.isArray(res.warnings) ? res.warnings : [];
    replaceContent(box, h('div', { class: 'preview-box stack' },
      h('h3', null, 'The sync will propose'),
      dl([
        ['sAMAccountName', n.sam ? h('code', null, n.sam) : ''],
        ['UPN', n.upn ? h('code', null, n.upn) : ''],
        ['CN', n.cn || ''],
        ['Mail', n.mail || ''],
        ['OU', res.ou ? h('span', { class: 'mono small wrap' }, res.ou) : h('span', { class: 'muted' }, res.ou_reason ? 'chosen at approval' : '—')],
        ['OU note', res.ou_reason || ''],
      ]),
      conflicts.length > 0
        ? h('div', { class: 'notice notice-warn' }, h('p', null, 'Some names are taken; the account will wait for a username (or get a suffix, depending on the naming settings):'), conflictList(conflicts))
        : notice('ok', 'The names are free.'),
      warnings.length > 0 ? h('ul', { class: 'summary-list' }, warnings.map((w) => h('li', null, w))) : null));
  }

  async function loadPeople(fields) {
    replaceContent(listHost, card('People', loading()));
    let people;
    try {
      const res = await api.get(`${base()}/people`);
      people = Array.isArray(res) ? res : ((res && Array.isArray(res.items)) ? res.items : []);
    }
    catch (e) {
      replaceContent(listHost, card('People', errorBox(e)));
      return;
    }
    const name = (p) => {
      const a = p.attrs || {};
      return [a.FirstName, a.LastName].filter(Boolean).join(' ') || a.DisplayName || p.external_id;
    };
    const actions = (p) => {
      const cell = h('div', { class: 'row-actions' });
      if (!canAdd) {
        cell.append(historyLink('manual_person', p.id));
        return cell;
      }
      cell.append(h('button', { type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'mp-edit', onclick: () => edit(p, fields) }, 'Edit…'));
      if (!p.ended) {
        cell.append(h('button', { type: 'button', class: 'btn btn-sm btn-danger', 'data-mutating': '', 'data-test': 'mp-end', onclick: (e) => end(p, e.currentTarget) }, 'End…'));
      }
      cell.append(historyLink('manual_person', p.id));
      if (isAdmin && !p.ad_sam) {
        cell.append(h('button', { type: 'button', class: 'btn btn-sm btn-danger', 'data-mutating': '', 'data-test': 'mp-delete', onclick: (e) => remove(p, e.currentTarget) }, 'Delete'));
      }
      return cell;
    };
    replaceContent(listHost, card('People',
      dataTable([
        { label: 'Name', render: (p) => h('div', null, h('div', null, name(p)), h('div', { class: 'muted small' }, p.external_id)) },
        { label: 'Department', render: (p) => (p.attrs && p.attrs.Department) || '' },
        { label: 'Status', render: (p) => (p.ended ? pill('inactive', 'ended') : pill(p.state || 'new')) },
        { label: 'AD account', render: (p) => p.ad_sam || h('span', { class: 'muted' }, 'none yet') },
        { label: 'Created', render: (p) => h('span', { title: fmtTime(p.created_at) }, `${relTime(p.created_at) || fmtTime(p.created_at)}${p.created_by ? ` by ${p.created_by}` : ''}`) },
        { label: 'Actions', className: 'col-actions', render: actions },
      ], people, { caption: 'Manually entered people', wrapClass: 'mp-people', empty: 'Nobody has been entered in this source yet.' })));
  }

  function edit(p, fields) {
    const model = { ...(p.attrs || {}) };
    formModal({
      title: `Edit ${p.external_id}`,
      wide: true,
      content: [
        h('p', { class: 'help' }, 'Changes reach AD through the next sync (and its approvals), like an HR change.'),
        personForm(fields, model),
      ],
      onSubmit: async () => {
        await api.put(`${base()}/people/${encodeURIComponent(p.id)}`, { attrs: collect(fields, model, p.attrs) });
        toast('Saved.', 'ok');
        loadPeople(fields);
        return true;
      },
    });
  }

  async function end(p, button) {
    const note = await noteDialog({
      title: `End ${p.external_id}`,
      message: 'The person is marked as left. The next sync disables the account through the normal flow (approval policy, disabled OU, later deletion).',
      label: 'Note (optional)',
      confirmLabel: 'End',
      danger: true,
    });
    if (note === null) {
      return;
    }
    await act(button, async () => {
      await api.post(`${base()}/people/${encodeURIComponent(p.id)}/end`, { note });
      toast(`${p.external_id} ended.`, 'ok');
      loadPeople(current().fields || []);
    });
  }

  async function remove(p, button) {
    const ok = await confirmDialog({
      title: `Delete ${p.external_id}?`,
      message: 'The entry is removed. This is only possible while no AD account is linked; use End for people who had an account.',
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) {
      return;
    }
    await act(button, async () => {
      await api.del(`${base()}/people/${encodeURIComponent(p.id)}`);
      toast(`${p.external_id} deleted.`, 'ok');
      loadPeople(current().fields || []);
    });
  }

  draw();
}
