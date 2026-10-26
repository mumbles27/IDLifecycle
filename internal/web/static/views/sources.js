// Sources: HR feeds (CSV, Paylocity, ...). The config form is generated from GET /api/source-types.

import { api } from '../api.js';
import {
  h, clear, replaceContent, pageHeader, pill, dataTable, listItems, act, confirmDialog, toast,
  errorBox, loading, field, inputText, inputNumber, inputArea, inputCheck, inputSelect, formModal, openModal,
  mutBtn, truncCell, chip, fmtTime, csvToList, listToCsv, isObject, notice,
} from '../ui.js';
import { invalidateSourceCache } from '../components.js';

const MASK = '********';

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const listEl = h('div', null, loading());
  let types = [];
  try {
    types = listItems(await api.get('/source-types'));
  }
  catch (e) {
    root.append(errorBox(e));
  }

  root.append(
    pageHeader('Sources', isAdmin ? mutBtn('New source', { class: 'btn-primary', 'data-test': 'new-source', onclick: () => openEditor(null) }) : null),
    listEl);

  async function load() {
    let items;
    try {
      items = listItems(await api.get('/sources', { limit: 500 }));
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    const columns = [
      { label: 'Name', render: (s) => h('strong', null, s.name) },
      { label: 'Type', render: (s) => (types.find((t) => t.type === s.type) || {}).label || s.type },
      { label: 'Enabled', render: (s) => pill(s.enabled ? 'enabled' : 'disabled') },
      { label: 'Last sync', render: (s) => fmtTime(s.last_sync_at) },
      { label: 'Last status', render: (s) => (s.last_status ? pill(s.last_status) : '') },
      { label: 'Last error', render: (s) => (s.last_error ? truncCell(s.last_error, 80) : '') },
    ];
    if (isAdmin) {
      columns.push({
        label: 'Actions',
        className: 'col-actions',
        render: (s) => h('div', { class: 'row-actions' },
          mutBtn('Edit', { class: 'btn-sm', 'data-test': 'edit-source', onclick: () => openEditor(s) }),
          mutBtn('Test', { class: 'btn-sm', 'data-test': 'test-source', onclick: (e) => testSource(s, e.currentTarget) }),
          mutBtn('Delete', { class: 'btn-sm btn-danger', 'data-test': 'delete-source', onclick: (e) => remove(s, e.currentTarget) })),
      });
    }
    replaceContent(listEl, dataTable(columns, items, {
      caption: 'Sources',
      empty: 'No sources yet. Add an HR source to start syncing people.',
    }));
  }

  async function remove(s, button) {
    const ok = await confirmDialog({
      title: 'Delete source',
      message: `Delete the source "${s.name}"? Identities that came from it are kept in the database, but no new data will be fetched from it.`,
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) {
      return;
    }
    await act(button, async () => {
      await api.del(`/sources/${encodeURIComponent(s.id)}`);
      invalidateSourceCache();
      toast('Source deleted.', 'ok');
      await load();
    });
  }

  async function testSource(s, button) {
    await act(button, async () => {
      const res = await api.post(`/sources/${encodeURIComponent(s.id)}/test`, {});
      showTestResult(s, res || {});
      await load();
    });
  }

  function showTestResult(s, res) {
    const sample = Array.isArray(res.sample) ? res.sample : [];
    const attrs = Array.isArray(res.attributes) ? res.attributes : [];
    const cols = [];
    for (const row of sample) {
      if (isObject(row)) {
        for (const k of Object.keys(row)) {
          if (!cols.includes(k)) {
            cols.push(k);
          }
        }
      }
    }
    const ok = res.ok !== false && !res.error;
    const modal = openModal({
      title: `Test result: ${s.name}`,
      wide: true,
      content: h('div', { 'data-test': 'test-result' },
        h('p', null, pill(ok ? 'ok' : 'error', ok ? 'Connection OK' : 'Failed'), ' ',
          ok ? h('span', null, h('strong', null, String(res.count === undefined ? sample.length : res.count)), ' record(s) fetched') : null),
        res.error ? notice('error', res.error) : null,
        attrs.length ? h('div', null, h('h3', null, 'Discovered attributes'), h('div', { class: 'chips' }, attrs.map((a) => chip(a)))) : null,
        sample.length
          ? h('div', null, h('h3', null, `Sample (first ${sample.length})`),
            dataTable(cols.map((c) => ({ label: c, render: (r) => truncCell(isObject(r) ? r[c] : '', 40) })), sample, { caption: 'Sample records' }))
          : null),
      footer: [h('span', { class: 'spacer' }), h('button', { type: 'button', class: 'btn btn-primary', 'data-test': 'test-close', onclick: () => modal.close() }, 'Close')],
    });
  }

  function configControl(def, config) {
    const label = def.label || def.name;
    const common = { 'data-field': def.name };
    let control;
    let help = def.help || '';
    switch (def.type) {
      case 'bool':
        return h('div', { class: 'field' }, inputCheck(config, def.name, label, common), help ? h('p', { class: 'help' }, help) : null);
      case 'int':
        control = inputNumber(config, def.name, { ...common, required: Boolean(def.required), step: '1' });
        break;
      case 'password':
        control = inputText(config, def.name, { ...common, type: 'password', autocomplete: 'new-password', required: Boolean(def.required) });
        help = `${help ? `${help} ` : ''}A stored secret is shown as ${MASK}; leave it unchanged to keep the stored value.`;
        break;
      case 'text':
        control = inputArea(config, def.name, { ...common, rows: '5', required: Boolean(def.required), class: 'mono' });
        break;
      default:
        control = inputText(config, def.name, { ...common, required: Boolean(def.required) });
    }
    return field(label, control, { help, required: Boolean(def.required) });
  }

  function openEditor(existing) {
    const model = {
      name: existing ? existing.name : '',
      type: existing ? existing.type : (types[0] ? types[0].type : ''),
      enabled: existing ? Boolean(existing.enabled) : true,
    };
    const opts = (existing && existing.options) || {};
    const optModel = {
      external_id_field: opts.external_id_field || '',
      status_field: opts.status_field || '',
      terminated_values: listToCsv(opts.terminated_values),
      missing_means_terminated: Boolean(opts.missing_means_terminated),
      missing_grace_runs: opts.missing_grace_runs === undefined ? 3 : opts.missing_grace_runs,
    };
    let config = existing ? { ...(existing.config || {}) } : {};
    const cfgBox = h('div', { class: 'stack-sm', 'data-test': 'source-config' });
    // A manual source fixes these options on the server (ExternalID / Status / Terminated), so they are hidden for it.
    const idOpts = h('fieldset', { class: 'group' },
      h('legend', null, 'Identity options'),
      field('External ID field', inputText(optModel, 'external_id_field', { placeholder: 'EmployeeId', 'data-test': 'opt-extid' }), { help: 'The source field that uniquely identifies a person. It is stored in the AD ID attribute (e.g. employeeID).' }),
      field('Status field', inputText(optModel, 'status_field', { placeholder: 'Status' }), { help: 'Optional. Source field that says whether the person is still employed.' }),
      field('Terminated values', inputText(optModel, 'terminated_values', { placeholder: 'T, Terminated' }), { help: 'Comma-separated status values that mean "has left". Matching people are disabled, then deleted after the grace period.' }),
      inputCheck(optModel, 'missing_means_terminated', 'Treat people missing from the feed as terminated'),
      field('Missing grace runs', inputNumber(optModel, 'missing_grace_runs', { min: '0', step: '1' }), { help: 'How many consecutive runs a person may be absent before they count as terminated. Protects against partial feeds.' }));

    function drawConfig() {
      clear(cfgBox);
      const def = types.find((t) => t.type === model.type);
      if (!def) {
        cfgBox.appendChild(h('p', { class: 'muted' }, 'Unknown source type.'));
        return;
      }
      const manual = model.type === 'manual';
      if (idOpts) {
        idOpts.hidden = manual;
      }
      if (manual) {
        cfgBox.appendChild(notice('info', 'People in a manual source are typed in under ', h('a', { href: '#/manual' }, 'Add person'), '. "form_fields" defines that form, one field per line: Name | Label | flags (required, date, email, select: A; B; C, default: value). The employee ID is generated from the prefix, and the person counts as left the day after the end date field.'));
      }
      for (const f of def.fields || []) {
        if (config[f.name] === undefined && f.default !== undefined && f.default !== null) {
          config[f.name] = f.default;
        }
        cfgBox.appendChild(configControl(f, config));
      }
    }

    const typeSel = inputSelect(model, 'type', types.map((t) => ({ value: t.type, label: t.label || t.type })), {
      disabled: Boolean(existing),
      'data-test': 'source-type',
      onchange: () => { config = {}; drawConfig(); },
    });
    drawConfig();

    const testBtn = h('button', {
      type: 'button',
      class: 'btn',
      disabled: !existing,
      title: existing ? 'Tests the saved configuration' : 'Save the source first, then test it',
      'data-test': 'source-test-saved',
      onclick: (e) => testSource(existing, e.currentTarget),
    }, 'Test saved connection');

    const content = [
      field('Name', inputText(model, 'name', { required: true, 'data-test': 'source-name' }), { required: true }),
      field('Type', typeSel, { required: true }),
      inputCheck(model, 'enabled', 'Enabled'),
      h('fieldset', { class: 'group' }, h('legend', null, 'Connection'), cfgBox),
      idOpts,
    ];

    formModal({
      title: existing ? `Edit source: ${existing.name}` : 'New source',
      content,
      wide: true,
      submitLabel: existing ? 'Save source' : 'Create source',
      extraFooter: testBtn,
      onSubmit: async () => {
        const cfg = {};
        for (const [k, v] of Object.entries(config)) {
          if (v === '' || v === undefined || v === null) {
            continue;
          }
          cfg[k] = v;
        }
        if (model.type === 'manual') {
          optModel.external_id_field = 'ExternalID';
          optModel.status_field = 'Status';
          optModel.terminated_values = 'Terminated';
          optModel.missing_means_terminated = false;
        }
        const body = {
          name: model.name.trim(),
          type: model.type,
          enabled: Boolean(model.enabled),
          options: {
            external_id_field: optModel.external_id_field.trim(),
            status_field: optModel.status_field.trim(),
            terminated_values: csvToList(optModel.terminated_values),
            missing_means_terminated: Boolean(optModel.missing_means_terminated),
            missing_grace_runs: Number(optModel.missing_grace_runs) || 0,
          },
          config: cfg,
        };
        if (existing) {
          await api.put(`/sources/${encodeURIComponent(existing.id)}`, { ...existing, ...body });
        }
        else {
          await api.post('/sources', body);
        }
        invalidateSourceCache();
        toast(existing ? 'Source saved.' : 'Source created. Use Test to check the connection.', 'ok');
        await load();
        return true;
      },
    });
  }

  await load();
}
