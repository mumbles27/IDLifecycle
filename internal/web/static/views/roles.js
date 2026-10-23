// Role rules: who gets which AD groups / OU / attribute overrides.

import { api } from '../api.js';
import {
  h, clear, replaceContent, pageHeader, card, pill, dataTable, listItems, act, confirmDialog, toast,
  errorBox, loading, field, inputText, inputNumber, inputArea, inputCheck, inputSelect, formModal, mutBtn,
  chip, linesToList, listToLines, csvToList, isObject, cellText, reportError,
} from '../ui.js';
import {
  attributeGrantFields, defaultAttrModel, previewPanel, sampleRecordPicker,
} from '../components.js';

const OPS = [
  { value: 'eq', label: 'equals' },
  { value: 'ne', label: 'does not equal' },
  { value: 'in', label: 'is one of (comma list)' },
  { value: 'contains', label: 'contains' },
  { value: 'starts_with', label: 'starts with' },
  { value: 'regex', label: 'matches regex' },
  { value: 'exists', label: 'has a value' },
];

function opLabel(op) {
  const o = OPS.find((x) => x.value === op);
  return o ? o.label : op;
}

function conditionSummary(rule) {
  const conds = Array.isArray(rule.conditions) ? rule.conditions : [];
  if (conds.length === 0) {
    return h('span', null, pill('active', 'everyone'), ' ', h('span', { class: 'muted small' }, 'baseline - no conditions'));
  }
  const joiner = rule.match === 'any' ? ' OR ' : ' AND ';
  const text = conds.map((c) => (c.op === 'exists' ? `${c.attr} ${opLabel(c.op)}` : `${c.attr} ${opLabel(c.op)} ${Array.isArray(c.value) ? c.value.join(', ') : c.value}`)).join(joiner);
  return h('span', { class: 'small' }, text);
}

function grantSummary(rule) {
  const g = rule.grants || {};
  const parts = [];
  const groups = Array.isArray(g.groups) ? g.groups : [];
  if (groups.length > 0) {
    parts.push(`${groups.length} group${groups.length === 1 ? '' : 's'}`);
  }
  if (g.ou) {
    parts.push('OU');
  }
  const attrs = Array.isArray(g.attributes) ? g.attributes : [];
  if (attrs.length > 0) {
    parts.push(`${attrs.length} attribute${attrs.length === 1 ? '' : 's'}`);
  }
  return parts.length ? parts.join(' + ') : h('span', { class: 'muted' }, 'nothing');
}

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const listEl = h('div', null, loading());

  root.append(
    pageHeader('Role rules', isAdmin ? mutBtn('New rule', { class: 'btn-primary', 'data-test': 'new-role', onclick: () => openEditor(null) }) : null),
    h('details', { class: 'card explain', open: true },
      h('summary', null, 'How role rules work'),
      h('ul', null,
        h('li', null, h('strong', null, 'Rules with no conditions apply to everyone'), ' - use them for baseline groups every employee should get (e.g. an "All staff" group).'),
        h('li', null, 'Rules are ', h('strong', null, 'additive'), ': a person gets the union of the groups from every matching rule.'),
        h('li', null, 'If several matching rules set an OU or the same attribute, the rule with the ', h('strong', null, 'highest priority'), ' wins; rule attribute grants override the global mappings.'),
        h('li', null, 'A condition compares a source attribute (using the source\'s field names) with a value. "Match all" means every condition must hold, "match any" means at least one.'))),
    listEl);

  async function load() {
    let items;
    try {
      items = listItems(await api.get('/roles', { limit: 500 }));
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    items.sort((a, b) => (b.priority - a.priority) || String(a.name).localeCompare(String(b.name)));
    const columns = [
      { label: 'Priority', key: 'priority', className: 'num' },
      { label: 'Name', render: (r) => h('div', null, h('strong', null, r.name), r.description ? h('div', { class: 'muted small' }, r.description) : null) },
      { label: 'Applies when', render: (r) => h('div', null, conditionSummary(r), Array.isArray(r.conditions) && r.conditions.length > 1 ? h('span', { class: 'muted small' }, ` (match ${r.match || 'all'})`) : null) },
      { label: 'Grants', render: grantSummary },
      { label: 'Enabled', render: (r) => pill(r.enabled ? 'enabled' : 'disabled') },
    ];
    if (isAdmin) {
      columns.push({
        label: 'Actions',
        className: 'col-actions',
        render: (r) => h('div', { class: 'row-actions' },
          mutBtn('Edit', { class: 'btn-sm', 'data-test': 'edit-role', onclick: () => openEditor(r) }),
          mutBtn('Delete', { class: 'btn-sm btn-danger', 'data-test': 'delete-role', onclick: (e) => remove(r, e.currentTarget) })),
      });
    }
    replaceContent(listEl, dataTable(columns, items, {
      caption: 'Role rules',
      empty: 'No role rules yet. People will only get what the mappings and AD defaults give them.',
    }));
  }

  async function remove(r, button) {
    const ok = await confirmDialog({
      title: 'Delete role rule',
      message: `Delete "${r.name}"? People matching it lose these grants on the next run (groups are only removed when "remove stale groups" is enabled).`,
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) {
      return;
    }
    await act(button, async () => {
      await api.del(`/roles/${encodeURIComponent(r.id)}`);
      toast('Role rule deleted.', 'ok');
      await load();
    });
  }

  function openEditor(existing) {
    const grants = (existing && existing.grants) || {};
    const model = {
      name: existing ? existing.name : '',
      description: existing ? existing.description || '' : '',
      enabled: existing ? Boolean(existing.enabled) : true,
      priority: existing ? existing.priority : 100,
      match: existing ? existing.match || 'all' : 'all',
      groupsText: listToLines(grants.groups),
      ou: grants.ou || '',
    };
    const conditions = ((existing && existing.conditions) || []).map((c) => ({
      attr: c.attr || '',
      op: c.op || 'eq',
      value: Array.isArray(c.value) ? c.value.join(', ') : (c.value === undefined || c.value === null ? '' : String(c.value)),
      wasArray: Array.isArray(c.value),
    }));
    const attributes = (grants.attributes || []).map((a) => ({ ...defaultAttrModel(), ...a }));

    const preview = previewPanel({
      title: 'Preview attribute grant templates',
      getTemplates: () => attributes.map((a) => ({ label: a.target_attr || '(unnamed)', template: a.template })),
    });

    const condBox = h('div', { class: 'rows', 'data-test': 'conditions' });
    const attrBox = h('div', { class: 'rows', 'data-test': 'attr-grants' });

    function drawConditions() {
      clear(condBox);
      if (conditions.length === 0) {
        condBox.appendChild(h('p', { class: 'notice notice-info', 'data-test': 'no-conditions' }, 'No conditions: this rule applies to everyone (baseline grants).'));
      }
      conditions.forEach((c, i) => {
        const valueIn = inputText(c, 'value', { 'aria-label': `Condition ${i + 1} value`, placeholder: c.op === 'in' ? 'a, b, c' : 'value', hidden: c.op === 'exists' });
        condBox.appendChild(h('div', { class: 'cond-row' },
          inputText(c, 'attr', { 'aria-label': `Condition ${i + 1} attribute`, placeholder: 'Department', required: true, 'data-test': 'cond-attr' }),
          inputSelect(c, 'op', OPS, {
            'aria-label': `Condition ${i + 1} operator`,
            onchange: () => { valueIn.hidden = c.op === 'exists'; valueIn.placeholder = c.op === 'in' ? 'a, b, c' : 'value'; },
          }),
          valueIn,
          h('button', { type: 'button', class: 'btn btn-sm', 'aria-label': `Remove condition ${i + 1}`, onclick: () => { conditions.splice(i, 1); drawConditions(); } }, 'Remove')));
      });
    }

    function drawAttributes() {
      clear(attrBox);
      if (attributes.length === 0) {
        attrBox.appendChild(h('p', { class: 'muted small' }, 'No attribute overrides.'));
      }
      attributes.forEach((a, i) => {
        attrBox.appendChild(h('div', { class: 'grant-card' },
          attributeGrantFields(a, () => preview.run()),
          h('button', { type: 'button', class: 'btn btn-sm btn-danger', onclick: () => { attributes.splice(i, 1); drawAttributes(); preview.run(); } }, 'Remove attribute grant')));
      });
    }
    drawConditions();
    drawAttributes();

    const content = h('div', { class: 'editor-cols' },
      h('div', { class: 'editor-main' },
        field('Name', inputText(model, 'name', { required: true, 'data-test': 'role-name' }), { required: true }),
        field('Description', inputArea(model, 'description', { rows: '2' })),
        h('div', { class: 'field-row' },
          field('Priority', inputNumber(model, 'priority', { step: '1', required: true }), { help: 'Higher wins when rules conflict on OU or an attribute.' }),
          field('Match', inputSelect(model, 'match', [{ value: 'all', label: 'all conditions' }, { value: 'any', label: 'any condition' }]), { help: 'Ignored when there are no conditions.' }),
          inputCheck(model, 'enabled', 'Enabled')),
        h('fieldset', { class: 'group' },
          h('legend', null, 'Conditions'),
          condBox,
          h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'add-condition', onclick: () => { conditions.push({ attr: '', op: 'eq', value: '', wasArray: false }); drawConditions(); } }, 'Add condition')),
        h('fieldset', { class: 'group' },
          h('legend', null, 'Grants'),
          field('AD groups (one per line)', inputArea(model, 'groupsText', { rows: '4', placeholder: 'GG-AllStaff\nCN=Marketing,OU=Groups,DC=corp,DC=example,DC=com', 'data-test': 'role-groups' }), { help: 'Group names (sAMAccountName) or distinguished names. Added to every matching person.' }),
          field('Target OU (optional)', inputText(model, 'ou', { placeholder: 'OU=Finance,OU=Staff,DC=corp,DC=example,DC=com' }), { help: 'New accounts are created here; with "enforce OU" on, existing accounts are moved here.' }),
          h('h3', null, 'Attribute grants'),
          attrBox,
          h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'add-attr-grant', onclick: () => { attributes.push(defaultAttrModel()); drawAttributes(); } }, 'Add attribute grant'))),
      h('div', { class: 'editor-side' }, preview.el));

    formModal({
      title: existing ? `Edit rule: ${existing.name}` : 'New role rule',
      content,
      wide: true,
      submitLabel: existing ? 'Save rule' : 'Create rule',
      onSubmit: async () => {
        const body = {
          name: model.name.trim(),
          description: model.description || '',
          enabled: Boolean(model.enabled),
          priority: Number(model.priority) || 0,
          match: model.match,
          conditions: conditions.map((c) => {
            let value = c.op === 'exists' ? '' : c.value;
            if (c.wasArray && c.op === 'in') {
              value = csvToList(c.value);
            }
            return { attr: c.attr.trim(), op: c.op, value };
          }),
          grants: {
            groups: linesToList(model.groupsText),
            ou: model.ou.trim(),
            attributes: attributes.map((a) => ({
              target_attr: a.target_attr.trim(),
              template: a.template,
              mode: a.mode,
              on_empty: a.on_empty,
              value_type: a.value_type,
            })),
          },
        };
        if (existing) {
          await api.put(`/roles/${encodeURIComponent(existing.id)}`, { ...existing, ...body });
        }
        else {
          await api.post('/roles', body);
        }
        toast(existing ? 'Role rule saved.' : 'Role rule created.', 'ok');
        await load();
        return true;
      },
    });
    preview.run();
  }

  if (isAdmin) {
    root.append(testPanel());
  }
  await load();
}

function testPanel() {
  const out = h('div', { class: 'test-out', 'aria-live': 'polite', 'data-test': 'role-test-out' });
  const picker = sampleRecordPicker();
  const run = h('button', {
    type: 'button',
    class: 'btn btn-primary',
    'data-test': 'role-test-run',
    onclick: () => act(run, async () => {
      let record;
      try {
        record = picker.getRecord();
      }
      catch (e) {
        replaceContent(out, errorBox(e));
        return;
      }
      try {
        const res = await api.post('/roles/preview', { record });
        const matched = Array.isArray(res.matched) ? res.matched : [];
        const attrs = isObject(res.attributes) ? Object.entries(res.attributes) : [];
        replaceContent(out,
          h('h3', null, 'Result'),
          h('p', null, h('strong', null, 'Matched rules: '), matched.length ? matched.map((m) => chip(m.name || m.id)) : h('span', { class: 'muted' }, 'none')),
          h('p', null, h('strong', null, 'Groups: '), Array.isArray(res.groups) && res.groups.length ? res.groups.map((g) => chip(g)) : h('span', { class: 'muted' }, 'none')),
          h('p', null, h('strong', null, 'OU: '), res.ou ? h('code', null, res.ou) : h('span', { class: 'muted' }, 'default OU')),
          attrs.length
            ? dataTable([{ label: 'Attribute', render: (r) => h('code', null, r[0]) }, { label: 'Value', render: (r) => cellText(r[1]) }], attrs, { caption: 'Attributes from role rules' })
            : h('p', { class: 'muted' }, 'No attribute overrides.'));
      }
      catch (e) {
        reportError(e);
      }
    }),
  }, 'Test record');
  return card('Test a record',
    h('p', { class: 'help' }, 'Runs the ', h('strong', null, 'saved'), ' rules against a sample record and shows which rules match and what they would grant.'),
    picker.el,
    h('div', { class: 'row-gap' }, run),
    out);
}
