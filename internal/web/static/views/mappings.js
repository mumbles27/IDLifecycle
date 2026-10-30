// Attribute mappings: global templates that compute AD attributes from source records.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, pill, dataTable, listItems, act, confirmDialog, toast,
  errorBox, loading, field, inputText, inputNumber, inputArea, inputCheck, formModal, mutBtn, truncCell, notice,
  historyLink,
} from '../ui.js';
import {
  selectWithHelp, previewPanel, MODE_OPTIONS, MODE_INFO, ON_EMPTY_OPTIONS, ON_EMPTY_INFO,
  VALUE_TYPE_OPTIONS, VALUE_TYPE_INFO,
} from '../components.js';
import { fieldMapper, templateFor } from './mapper.js';
import { openImpactModal } from './impact.js';

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const listEl = h('div', null, loading());
  const header = pageHeader('Attribute mappings',
    isAdmin ? mutBtn('New mapping', { class: 'btn-primary', 'data-test': 'new-mapping', onclick: () => openEditor(null) }) : null);

  // The field mapper fetches live HR data and reads the AD schema, so only admins get it.
  const mapper = isAdmin ? fieldMapper({
    getMappings: () => load.items || [],
    onMapField: (f) => openEditor(null, { name: f.name, template: templateFor(f.name) }),
    onMapAttribute: (a) => openEditor(null, {
      name: a.description || a.name,
      target_attr: a.name,
      value_type: a.syntax === 'dn' ? 'user_ref' : 'string',
    }),
  }) : null;

  root.append(header, explainer(), mapper ? mapper.el : null, listEl);

  function explainer() {
    return h('details', { class: 'card explain', open: true },
      h('summary', null, 'How mappings and modes work'),
      h('p', null, 'Each mapping renders a template from the HR source record and writes it to one AD attribute. Mappings apply to everyone; role rules can override an attribute for matching people. ', h('code', null, 'employeeID'), ' (the link key) is always treated as immutable.'),
      h('dl', { class: 'kv kv-wide' },
        Object.keys(MODE_INFO).flatMap((k) => [h('dt', null, pill(k)), h('dd', null, MODE_INFO[k])])));
  }

  let savedMappings = [];

  async function load() {
    let items;
    try {
      items = listItems(await api.get('/mappings', { limit: 500 }));
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    savedMappings = items;
    items.sort((a, b) => (a.sort_order - b.sort_order) || String(a.name).localeCompare(String(b.name)));
    const columns = [
      { label: 'Order', key: 'sort_order', className: 'num' },
      { label: 'Name', render: (m) => h('strong', null, m.name) },
      { label: 'Target attribute', render: (m) => h('code', null, m.target_attr) },
      { label: 'Template', render: (m) => h('span', { class: 'mono' }, truncCell(m.template, 60)) },
      { label: 'Mode', render: (m) => pill(m.mode) },
      { label: 'On empty', key: 'on_empty' },
      { label: 'Type', key: 'value_type' },
      { label: 'Enabled', render: (m) => pill(m.enabled ? 'enabled' : 'disabled') },
    ];
    columns.push({
      label: 'Actions',
      className: 'col-actions',
      render: (m) => h('div', { class: 'row-actions' },
        isAdmin ? mutBtn('Edit', { class: 'btn-sm', 'data-test': 'edit-mapping', onclick: () => openEditor(m) }) : null,
        historyLink('mapping', m.id),
        isAdmin ? mutBtn('Delete', { class: 'btn-sm btn-danger', 'data-test': 'delete-mapping', onclick: (e) => remove(m, e.currentTarget) }) : null),
    });
    replaceContent(listEl, dataTable(columns, items, {
      caption: 'Attribute mappings',
      empty: 'No mappings yet. Add one to start populating AD attributes.',
    }));
    load.items = items;
    if (mapper) {
      mapper.refresh();
    }
  }

  async function remove(m, button) {
    const ok = await confirmDialog({
      title: 'Delete mapping',
      message: `Delete the mapping "${m.name}" (${m.target_attr})? Future runs will no longer manage this attribute. Existing AD values are not changed.`,
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) {
      return;
    }
    await act(button, async () => {
      await api.del(`/mappings/${encodeURIComponent(m.id)}`);
      toast('Mapping deleted.', 'ok');
      await load();
    });
  }

  /** preset pre-fills a new mapping (from the field mapper's Map… buttons). */
  function openEditor(existing, preset) {
    const items = load.items || [];
    const nextOrder = items.reduce((mx, m) => Math.max(mx, Number(m.sort_order) || 0), 0) + 10;
    const model = {
      name: '',
      target_attr: '',
      template: '',
      mode: 'sync',
      on_empty: 'skip',
      value_type: 'string',
      enabled: true,
      sort_order: nextOrder,
      description: '',
      ...(preset || {}),
      ...(existing || {}),
    };
    const adAttrs = mapper ? mapper.attributes() : [];
    const srcFields = mapper ? mapper.fields() : [];
    const preview = previewPanel({
      getTemplates: () => [{ label: model.target_attr || 'Result', template: model.template }],
    });

    const tpl = inputArea(model, 'template', {
      rows: '3',
      class: 'mono',
      required: true,
      placeholder: '{{ .FirstName }} {{ .LastName }}',
      'data-test': 'map-template',
      oninput: () => preview.run(),
    });
    const listId = adAttrs.length > 0 ? `ad-attrs-${Date.now()}` : null;
    const targetWarn = h('p', { class: 'field-warn small', 'data-test': 'map-target-warn', hidden: true });
    const checkTarget = () => {
      const want = String(model.target_attr || '').trim().toLowerCase();
      const a = adAttrs.find((x) => x.name.toLowerCase() === want);
      let msg = '';
      if (want && a && !a.mappable) {
        msg = `${a.name} cannot be mapped: ${a.reason}.`;
      }
      else if (want && a && a.hint) {
        msg = `${a.name}: ${a.hint}.`;
      }
      else if (want && !a && mapper && mapper.adLive()) {
        msg = `The AD schema has no user attribute named "${model.target_attr.trim()}".`;
      }
      targetWarn.textContent = msg;
      targetWarn.hidden = msg === '';
    };
    const target = inputText(model, 'target_attr', {
      required: true,
      placeholder: 'displayName',
      list: listId,
      autocomplete: 'off',
      'data-test': 'map-target',
      oninput: () => { checkTarget(); preview.run(); },
    });
    const datalist = listId
      ? h('datalist', { id: listId }, adAttrs.filter((a) => a.mappable).map((a) => h('option', { value: a.name }, a.description || a.name)))
      : null;
    const name = inputText(model, 'name', { required: true, placeholder: 'Display name', 'data-test': 'map-name' });

    // Insert a source field at the cursor (fields come from the field mapper's last load).
    const insertField = (f) => {
      const text = templateFor(f.name);
      const start = tpl.selectionStart ?? tpl.value.length;
      const end = tpl.selectionEnd ?? tpl.value.length;
      tpl.setRangeText(text, start, end, 'end');
      tpl.dispatchEvent(new Event('input', { bubbles: true }));
      tpl.focus();
    };
    const fieldButtons = srcFields.length > 0
      ? h('div', { class: 'field' },
        h('span', { class: 'field-title' }, 'Insert a source field'),
        h('div', { class: 'chips', 'data-test': 'insert-fields' },
          srcFields.map((f) => h('button', { type: 'button', class: 'chip chip-btn', 'data-test': 'insert-field', 'data-name': f.name, onclick: () => insertField(f) }, f.name))),
        h('p', { class: 'help' }, 'Adds the field to the template at the cursor.'))
      : null;

    const targetField = field('Target attribute (LDAP name)', target, {
      required: true,
      help: adAttrs.length > 0
        ? 'Start typing to pick from the AD user attributes that can be mapped.'
        : 'The AD attribute to write, e.g. displayName, title, department, manager.',
    });
    targetField.append(...[datalist, targetWarn].filter(Boolean));

    const form = h('div', { class: 'editor-cols' },
      h('div', { class: 'editor-main' },
        field('Name', name, { required: true }),
        targetField,
        field('Template', tpl, { required: true, help: 'Go template evaluated against the source record.' }),
        fieldButtons,
        selectWithHelp('Mode', model, 'mode', MODE_OPTIONS, MODE_INFO, { 'data-test': 'map-mode' }),
        selectWithHelp('When the result is empty', model, 'on_empty', ON_EMPTY_OPTIONS, ON_EMPTY_INFO),
        selectWithHelp('Value type', model, 'value_type', VALUE_TYPE_OPTIONS, VALUE_TYPE_INFO),
        h('div', { class: 'field-row' },
          field('Order', inputNumber(model, 'sort_order', { step: '1' }), { help: 'Lower numbers are evaluated first.' }),
          inputCheck(model, 'enabled', 'Enabled')),
        field('Description', inputArea(model, 'description', { rows: '2' }))),
      h('div', { class: 'editor-side' }, preview.el));

    const mappingBody = () => ({
      name: model.name.trim(),
      target_attr: model.target_attr.trim(),
      template: model.template,
      mode: model.mode,
      on_empty: model.on_empty,
      value_type: model.value_type,
      enabled: Boolean(model.enabled),
      sort_order: Number(model.sort_order) || 0,
      description: model.description || '',
    });
    const impactBtn = h('button', {
      type: 'button',
      class: 'btn',
      'data-test': 'mapping-impact',
      onclick: () => {
        const proposed = existing ? { ...existing, ...mappingBody() } : mappingBody();
        const mappings = savedMappings.filter((m) => !existing || m.id !== existing.id);
        mappings.push(proposed);
        openImpactModal({ title: `Preview impact: ${proposed.name || 'new mapping'}`, proposal: { mappings } });
      },
    }, 'Preview impact');

    formModal({
      title: existing ? `Edit mapping: ${existing.name}` : 'New mapping',
      extraFooter: impactBtn,
      content: [existing && existing.target_attr && String(existing.target_attr).toLowerCase() === 'employeeid'
        ? notice('warn', 'employeeID is the link key between the source and AD and is always enforced as immutable.') : null, form],
      wide: true,
      submitLabel: existing ? 'Save mapping' : 'Create mapping',
      onSubmit: async () => {
        const body = mappingBody();
        if (existing) {
          await api.put(`/mappings/${encodeURIComponent(existing.id)}`, { ...existing, ...body });
        }
        else {
          await api.post('/mappings', body);
        }
        toast(existing ? 'Mapping saved.' : 'Mapping created.', 'ok');
        await load();
        return true;
      },
    });
    checkTarget();
    preview.run();
  }

  await load();
}
