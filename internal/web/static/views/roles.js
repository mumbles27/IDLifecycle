// Role rules: who gets which AD groups / OU / attribute overrides.

import { api } from '../api.js';
import {
  h, clear, replaceContent, pageHeader, card, pill, dataTable, listItems, act, confirmDialog, toast,
  errorBox, loading, field, inputText, inputNumber, inputArea, inputCheck, inputSelect, formModal, mutBtn,
  chip, isObject, cellText, reportError,
  historyLink,
} from '../ui.js';
import {
  attributeGrantFields, defaultAttrModel, previewPanel, sampleRecordPicker,
} from '../components.js';
import { groupPicker, groupLabel, isCloudRef } from '../group-picker.js';
import { openImpactModal } from './impact.js';

export const OPS = [
  { value: 'eq', label: 'equals' },
  { value: 'ne', label: 'does not equal' },
  { value: 'in', label: 'is one of (comma list)' },
  { value: 'not_in', label: 'is none of (comma list)' },
  { value: 'contains', label: 'contains' },
  { value: 'not_contains', label: 'does not contain' },
  { value: 'starts_with', label: 'starts with' },
  { value: 'ends_with', label: 'ends with' },
  { value: 'regex', label: 'matches regex' },
  { value: 'not_regex', label: 'does not match regex' },
  { value: 'exists', label: 'has a value' },
  { value: 'empty', label: 'is empty or missing' },
  { value: 'gt', label: 'is greater than / after' },
  { value: 'gte', label: 'is at least / on or after' },
  { value: 'lt', label: 'is less than / before' },
  { value: 'lte', label: 'is at most / on or before' },
  { value: 'older_than_days', label: 'is a date more than N days ago' },
  { value: 'newer_than_days', label: 'is a date less than N days ago' },
  { value: 'template', label: 'template returns true' },
  { value: 'all_of', label: 'ALL of a group of conditions' },
  { value: 'any_of', label: 'ANY of a group of conditions' },
];

const NO_VALUE = new Set(['exists', 'empty']);
const GROUP_OPS = new Set(['all_of', 'any_of']);
const DAY_OPS = new Set(['older_than_days', 'newer_than_days']);
const LIST_OPS = new Set(['in', 'not_in']);
const COMPARE_OPS = new Set(['gt', 'gte', 'lt', 'lte']);
const MAX_DEPTH = 3;
const MAX_CONDITIONS = 50;

const OP_HELP = {
  gt: 'Numbers are compared as numbers; otherwise dates (YYYY-MM-DD, RFC 3339 or M/D/YYYY).',
  gte: 'Numbers are compared as numbers; otherwise dates (YYYY-MM-DD, RFC 3339 or M/D/YYYY).',
  lt: 'Numbers are compared as numbers; otherwise dates (YYYY-MM-DD, RFC 3339 or M/D/YYYY).',
  lte: 'Numbers are compared as numbers; otherwise dates (YYYY-MM-DD, RFC 3339 or M/D/YYYY).',
  older_than_days: 'The field holds a date; true when it is more than this many days before today.',
  newer_than_days: 'The field holds a date; true when it is less than this many days before today (e.g. hired in the last 30 days).',
  template: 'A Go template over the whole record (the mapping functions work here); it matches when it outputs true, yes or 1, e.g. {{ if and (eq .Department "IT") .Manager }}true{{ end }}.',
  regex: 'RE2 syntax, case-sensitive unless the pattern starts with (?i).',
  not_regex: 'RE2 syntax, case-sensitive unless the pattern starts with (?i).',
};

function opLabel(op) {
  const o = OPS.find((x) => x.value === op);
  return o ? o.label : op;
}

function valueText(c) {
  return Array.isArray(c.value) ? c.value.join(', ') : (c.value === undefined || c.value === null ? '' : String(c.value));
}

/** One condition (or group) as readable text: nested groups in brackets. */
export function conditionText(c) {
  if (GROUP_OPS.has(c.op)) {
    const kids = Array.isArray(c.conditions) ? c.conditions : [];
    return `(${kids.map(conditionText).join(c.op === 'any_of' ? ' OR ' : ' AND ') || 'empty group'})`;
  }
  if (c.op === 'template') {
    return `template ${valueText(c)}`;
  }
  if (NO_VALUE.has(c.op)) {
    return `${c.attr} ${opLabel(c.op)}`;
  }
  return `${c.attr} ${opLabel(c.op)} ${valueText(c)}`;
}

export function conditionSummary(rule) {
  const conds = Array.isArray(rule.conditions) ? rule.conditions : [];
  if (conds.length === 0) {
    return h('span', null, pill('active', 'everyone'), ' ', h('span', { class: 'muted small' }, 'baseline - no conditions'));
  }
  const joiner = rule.match === 'any' ? ' OR ' : ' AND ';
  return h('span', { class: 'small' }, conds.map(conditionText).join(joiner));
}

function grantSummary(rule) {
  const g = rule.grants || {};
  const parts = [];
  const groups = Array.isArray(g.groups) ? g.groups : [];
  if (groups.length > 0) {
    const cloud = groups.filter(isCloudRef).length;
    parts.push(`${groups.length} group${groups.length === 1 ? '' : 's'}${cloud ? ` (${cloud} cloud)` : ''}`);
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

// ---------------------------------------------------------------- condition model (editor)

export function toModel(c) {
  return {
    attr: c.attr || '',
    op: c.op || 'eq',
    value: valueText(c),
    conditions: Array.isArray(c.conditions) ? c.conditions.map(toModel) : [],
  };
}

export function toBody(c) {
  if (GROUP_OPS.has(c.op)) {
    return { attr: '', op: c.op, value: '', conditions: c.conditions.map(toBody) };
  }
  if (c.op === 'template') {
    return { attr: '*', op: 'template', value: c.value };
  }
  if (NO_VALUE.has(c.op)) {
    return { attr: c.attr.trim(), op: c.op, value: '' };
  }
  // The value is always a string; "in" / "not_in" take a comma list.
  return { attr: c.attr.trim(), op: c.op, value: String(c.value).trim() };
}

function countConditions(list) {
  return list.reduce((n, c) => n + 1 + (GROUP_OPS.has(c.op) ? countConditions(c.conditions) : 0), 0);
}

export function templateConditions(list, prefix = 'conditions') {
  const out = [];
  list.forEach((c, i) => {
    const path = `${prefix}[${i}]`;
    if (c.op === 'template') {
      out.push({ label: `Condition ${path}`, template: c.value });
    }
    if (GROUP_OPS.has(c.op)) {
      out.push(...templateConditions(c.conditions, `${path}.conditions`));
    }
  });
  return out;
}

/**
 * conditionBuilder(conditions, { onChange }) -> { el, redraw }
 * Edits the array in place. Groups (all_of / any_of) nest up to MAX_DEPTH levels; at most MAX_CONDITIONS in total.
 */
export function conditionBuilder(conditions, { onChange } = {}) {
  const root = h('div', { class: 'cond-builder', 'data-test': 'conditions' });
  const counter = h('p', { class: 'help', 'aria-live': 'polite', 'data-test': 'cond-count' });

  const changed = () => {
    if (onChange) {
      onChange();
    }
  };

  function drawList(container, list, depth, path) {
    clear(container);
    if (list.length === 0) {
      container.appendChild(depth === 1
        ? h('p', { class: 'notice notice-info', 'data-test': 'no-conditions' }, 'No conditions: this rule applies to everyone (baseline grants).')
        : h('p', { class: 'muted small' }, 'This group has no conditions yet - add one, or the rule cannot be saved.'));
    }
    list.forEach((c, i) => {
      container.appendChild(drawCondition(list, c, i, depth, `${path}[${i}]`));
    });
    const total = countConditions(conditions);
    const full = total >= MAX_CONDITIONS;
    const label = depth === 1 ? '' : ' to this group';
    container.appendChild(h('div', { class: 'row-gap cond-add' },
      h('button', {
        type: 'button', class: 'btn btn-sm', disabled: full, 'data-test': depth === 1 ? 'add-condition' : 'add-condition-nested',
        onclick: () => { list.push(toModel({ op: 'eq' })); redraw(); changed(); },
      }, `Add condition${label}`),
      depth < MAX_DEPTH ? h('button', {
        type: 'button', class: 'btn btn-sm', disabled: full, 'data-test': depth === 1 ? 'add-group' : 'add-group-nested',
        onclick: () => { list.push(toModel({ op: 'any_of', conditions: [{ op: 'eq' }] })); redraw(); changed(); },
      }, `Add group${label}`) : null));
  }

  function drawCondition(list, c, i, depth, path) {
    const remove = h('button', {
      type: 'button', class: 'btn btn-sm', 'aria-label': `Remove condition ${path}`,
      onclick: () => { list.splice(i, 1); redraw(); changed(); },
    }, 'Remove');

    if (GROUP_OPS.has(c.op)) {
      const inner = h('div', { class: 'cond-group-body' });
      drawList(inner, c.conditions, depth + 1, `${path}.conditions`);
      return h('fieldset', { class: `cond-group cond-depth-${depth}`, 'data-test': 'cond-group' },
        h('legend', null,
          inputSelect(c, 'op', [
            { value: 'all_of', label: 'ALL of these' },
            { value: 'any_of', label: 'ANY of these' },
          ], { 'aria-label': `Group ${path}: how its conditions combine`, 'data-test': 'cond-group-op', onchange: () => { redraw(); changed(); } })),
        inner,
        h('div', { class: 'cond-group-foot' }, remove));
    }

    const isTemplate = c.op === 'template';
    const valueAttrs = { 'aria-label': `Condition ${path} value`, 'data-test': 'cond-value', oninput: changed };
    let valueIn;
    if (isTemplate) {
      valueIn = inputArea(c, 'value', { ...valueAttrs, rows: '2', class: 'mono', placeholder: '{{ if eq .Department "IT" }}true{{ end }}' });
    }
    else if (DAY_OPS.has(c.op)) {
      valueIn = inputText(c, 'value', { ...valueAttrs, type: 'number', min: '0', step: '1', placeholder: 'days' });
    }
    else {
      let ph = 'value';
      if (LIST_OPS.has(c.op)) {
        ph = 'a, b, c';
      }
      else if (COMPARE_OPS.has(c.op)) {
        ph = 'number or date (YYYY-MM-DD)';
      }
      valueIn = inputText(c, 'value', { ...valueAttrs, placeholder: ph, hidden: NO_VALUE.has(c.op) });
    }
    const attrIn = inputText(c, 'attr', {
      'aria-label': `Condition ${path} field`, placeholder: 'Department', required: !isTemplate, hidden: isTemplate, 'data-test': 'cond-attr', oninput: changed,
    });
    const opSel = inputSelect(c, 'op', OPS.filter((o) => depth < MAX_DEPTH || !GROUP_OPS.has(o.value)), {
      'aria-label': `Condition ${path} test`,
      'data-test': 'cond-op',
      onchange: () => {
        if (GROUP_OPS.has(c.op) && c.conditions.length === 0) {
          c.conditions.push(toModel({ op: 'eq', attr: c.attr }));
        }
        if (c.op === 'template') {
          c.attr = '*';
        }
        else if (c.attr === '*') {
          c.attr = '';
        }
        redraw();
        changed();
      },
    });
    const help = OP_HELP[c.op];
    return h('div', { class: 'cond-item' },
      h('div', { class: `cond-row${isTemplate ? ' cond-row-template' : ''}`, 'data-test': 'cond-row' },
        isTemplate ? h('span', { class: 'cond-whole-record small muted' }, 'whole record') : attrIn,
        opSel,
        valueIn,
        remove),
      help ? h('p', { class: 'help cond-help' }, help) : null);
  }

  function redraw() {
    drawList(root, conditions, 1, 'conditions');
    const total = countConditions(conditions);
    counter.textContent = total >= MAX_CONDITIONS
      ? `${total} of ${MAX_CONDITIONS} conditions - the maximum for one rule.`
      : `${total} of at most ${MAX_CONDITIONS} conditions; groups nest up to ${MAX_DEPTH} levels.`;
  }
  redraw();
  return { el: h('div', null, root, counter), redraw };
}

// ---------------------------------------------------------------- page

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const listEl = h('div', null, loading());

  root.append(
    pageHeader('Role rules', isAdmin ? mutBtn('New rule', { class: 'btn-primary', 'data-test': 'new-role', onclick: () => openEditor(null) }) : null),
    h('details', { class: 'card explain', open: true },
      h('summary', null, 'How role rules work'),
      h('ul', null,
        h('li', null, h('strong', null, 'Rules with no conditions apply to everyone'), ' - use them for baseline groups every employee should get (or use Lifecycle groups).'),
        h('li', null, 'Rules are ', h('strong', null, 'additive'), ': a person gets the union of the groups from every matching rule.'),
        h('li', null, 'If several matching rules set an OU or the same attribute, the rule with the ', h('strong', null, 'highest priority'), ' wins; rule attribute grants override the global mappings.'),
        h('li', null, 'A condition compares a source field with a value: text, lists, regular expressions, numbers, dates, "days ago", or a template over the whole record. Groups combine conditions with ALL / ANY and can be nested.'),
        h('li', null, h('strong', null, 'Preview impact'), ' in the editor shows what the changed rule would do to every person before you save it.'))),
    listEl);

  let savedRoles = [];

  async function load() {
    let items;
    try {
      items = listItems(await api.get('/roles', { limit: 500 }));
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    savedRoles = items;
    items.sort((a, b) => (b.priority - a.priority) || String(a.name).localeCompare(String(b.name)));
    const columns = [
      { label: 'Priority', key: 'priority', className: 'num' },
      { label: 'Name', render: (r) => h('div', null, h('strong', null, r.name), r.description ? h('div', { class: 'muted small' }, r.description) : null) },
      { label: 'Applies when', render: (r) => h('div', null, conditionSummary(r), Array.isArray(r.conditions) && r.conditions.length > 1 ? h('span', { class: 'muted small' }, ` (match ${r.match || 'all'})`) : null) },
      { label: 'Grants', render: grantSummary },
      { label: 'Enabled', render: (r) => pill(r.enabled ? 'enabled' : 'disabled') },
    ];
    columns.push({
      label: 'Actions',
      className: 'col-actions',
      render: (r) => h('div', { class: 'row-actions' },
        isAdmin ? mutBtn('Edit', { class: 'btn-sm', 'data-test': 'edit-role', onclick: () => openEditor(r) }) : null,
        historyLink('role', r.id),
        isAdmin ? mutBtn('Delete', { class: 'btn-sm btn-danger', 'data-test': 'delete-role', onclick: (e) => remove(r, e.currentTarget) }) : null),
    });
    replaceContent(listEl, dataTable(columns, items, {
      caption: 'Role rules',
      empty: 'No role rules yet. People will only get what the mappings, lifecycle groups and AD defaults give them.',
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
      groups: Array.isArray(grants.groups) ? [...grants.groups] : [],
      ou: grants.ou || '',
    };
    const conditions = ((existing && existing.conditions) || []).map(toModel);
    const attributes = (grants.attributes || []).map((a) => ({ ...defaultAttrModel(), ...a }));

    const preview = previewPanel({
      title: 'Preview templates',
      getTemplates: () => [
        ...templateConditions(conditions),
        ...attributes.map((a) => ({ label: a.target_attr || '(unnamed)', template: a.template })),
      ],
    });

    const body = () => ({
      ...(existing || {}),
      name: model.name.trim(),
      description: model.description || '',
      enabled: Boolean(model.enabled),
      priority: Number(model.priority) || 0,
      match: model.match,
      conditions: conditions.map(toBody),
      grants: {
        groups: [...model.groups],
        ou: model.ou.trim(),
        attributes: attributes.map((a) => ({
          target_attr: a.target_attr.trim(),
          template: a.template,
          mode: a.mode,
          on_empty: a.on_empty,
          value_type: a.value_type,
        })),
      },
    });

    const builder = conditionBuilder(conditions, { onChange: () => preview.run() });
    const attrBox = h('div', { class: 'rows', 'data-test': 'attr-grants' });

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
    drawAttributes();

    const tester = ruleTester(body);
    const content = h('div', { class: 'editor-cols' },
      h('div', { class: 'editor-main' },
        field('Name', inputText(model, 'name', { required: true, 'data-test': 'role-name' }), { required: true }),
        field('Description', inputArea(model, 'description', { rows: '2' })),
        h('div', { class: 'field-row' },
          field('Priority', inputNumber(model, 'priority', { step: '1', required: true }), { help: 'Higher wins when rules conflict on OU or an attribute.' }),
          field('Match', inputSelect(model, 'match', [{ value: 'all', label: 'all conditions' }, { value: 'any', label: 'any condition' }], { 'data-test': 'role-match' }), { help: 'How the top-level conditions combine. Ignored when there are none.' }),
          inputCheck(model, 'enabled', 'Enabled')),
        h('fieldset', { class: 'group' },
          h('legend', null, 'Conditions'),
          builder.el),
        h('fieldset', { class: 'group' },
          h('legend', null, 'Grants'),
          groupPicker({ model, key: 'groups', label: 'Groups', dataTest: 'role-groups', help: 'Search AD by name or description (or Entra ID / Microsoft 365 when the Microsoft 365 connection is on); every matching person is added to these groups.' }),
          field('Target OU (optional)', inputText(model, 'ou', { placeholder: 'OU=Finance,OU=Staff,DC=corp,DC=example,DC=com' }), { help: 'New accounts are created here; with "enforce OU" on, existing accounts are moved here.' }),
          h('h3', null, 'Attribute grants'),
          attrBox,
          h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'add-attr-grant', onclick: () => { attributes.push(defaultAttrModel()); drawAttributes(); } }, 'Add attribute grant'))),
      h('div', { class: 'editor-side' }, tester.el, preview.el));

    const impactBtn = h('button', {
      type: 'button',
      class: 'btn',
      'data-test': 'role-impact',
      onclick: () => act(impactBtn, async () => {
        const proposed = body();
        const roles = savedRoles.filter((r) => !existing || r.id !== existing.id);
        roles.push(proposed);
        openImpactModal({ title: `Preview impact: ${proposed.name || 'new rule'}`, proposal: { roles } });
      }),
    }, 'Preview impact');

    formModal({
      title: existing ? `Edit rule: ${existing.name}` : 'New role rule',
      content,
      wide: true,
      submitLabel: existing ? 'Save rule' : 'Create rule',
      extraFooter: impactBtn,
      onSubmit: async () => {
        const b = body();
        if (existing) {
          await api.put(`/roles/${encodeURIComponent(existing.id)}`, b);
        }
        else {
          await api.post('/roles', b);
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

// ---------------------------------------------------------------- testing

export function explainTable(explain) {
  return dataTable([
    { label: 'Condition', render: (e) => h('code', { class: 'small' }, e.path || '') },
    { label: 'Field', render: (e) => (e.attr && e.attr !== '*' ? h('code', null, e.attr) : h('span', { class: 'muted' }, e.op === 'template' ? 'whole record' : '-')) },
    { label: 'Test', render: (e) => `${opLabel(e.op)}${NO_VALUE.has(e.op) || GROUP_OPS.has(e.op) ? '' : ` ${cellText(e.value)}`}` },
    { label: 'Value in the record', render: (e) => (e.actual === undefined || e.actual === null || e.actual === '' ? h('span', { class: 'muted' }, '(empty)') : h('code', null, cellText(e.actual))) },
    {
      label: 'Result',
      render: (e) => h('span', { class: e.matched ? 'match-yes' : 'match-no', 'data-test': 'explain-result' },
        h('span', { class: 'match-mark', 'aria-hidden': 'true' }, e.matched ? '✓' : '✗'), e.matched ? 'matches' : 'does not match'),
    },
  ], explain, { caption: 'Why each condition matched or not', wrapClass: 'compact', rowClass: (e) => `explain-depth-${String(e.path || '').split('.').length}` });
}

function resultBlock(res) {
  const matched = Array.isArray(res.matched) ? res.matched : [];
  const attrs = isObject(res.attributes) ? Object.entries(res.attributes) : [];
  return [
    h('p', null, h('strong', null, 'Matched rules: '), matched.length ? matched.map((m) => chip(m.name || m.id)) : h('span', { class: 'muted' }, 'none')),
    h('p', null, h('strong', null, 'Groups: '), Array.isArray(res.groups) && res.groups.length ? res.groups.map((g) => chip(groupLabel(g))) : h('span', { class: 'muted' }, 'none')),
    h('p', null, h('strong', null, 'OU: '), res.ou ? h('code', null, res.ou) : h('span', { class: 'muted' }, 'default OU')),
    attrs.length
      ? dataTable([{ label: 'Attribute', render: (r) => h('code', null, r[0]) }, { label: 'Value', render: (r) => cellText(r[1]) }], attrs, { caption: 'Attributes from role rules' })
      : h('p', { class: 'muted' }, 'No attribute overrides.'),
  ];
}

/** The editor's "Test this rule": the unsaved rule against a sample record, with the reason for each condition. */
function ruleTester(getRule) {
  const out = h('div', { class: 'test-out', 'aria-live': 'polite', 'data-test': 'rule-test-out' });
  const picker = sampleRecordPicker();
  const run = h('button', {
    type: 'button',
    class: 'btn btn-sm btn-primary',
    'data-test': 'rule-test-run',
    onclick: () => act(run, async () => {
      let record;
      try {
        record = picker.getRecord();
      }
      catch (e) {
        replaceContent(out, errorBox(e));
        return;
      }
      const rule = getRule();
      const res = await api.post('/roles/preview', { record, rule });
      const explain = Array.isArray(res.explain) ? res.explain : [];
      const top = explain.filter((e) => /^conditions\[\d+\]$/.test(String(e.path || '')));
      let verdict;
      if (!rule.enabled) {
        verdict = 'This rule is disabled, so it applies to nobody.';
      }
      else if ((rule.conditions || []).length === 0) {
        verdict = 'This rule has no conditions: it applies to everyone.';
      }
      else if (typeof res.rule_matched === 'boolean' || top.length) {
        const ok = typeof res.rule_matched === 'boolean'
          ? res.rule_matched
          : (rule.match === 'any' ? top.some((e) => e.matched) : top.every((e) => e.matched));
        verdict = ok ? 'This rule MATCHES the record.' : 'This rule does NOT match the record.';
      }
      replaceContent(out,
        verdict ? h('p', { class: 'rule-verdict', 'data-test': 'rule-verdict' }, h('strong', null, verdict)) : null,
        explain.length ? explainTable(explain) : null,
        h('details', null, h('summary', null, 'All rules together (the saved ones, with this rule as edited)'), resultBlock(res)));
    }),
  }, 'Test this rule');
  const el = h('section', { class: 'preview-panel' },
    h('h3', null, 'Test this rule'),
    h('p', { class: 'help' }, 'Runs the rule as edited (not yet saved) against a sample record and explains every condition.'),
    picker.el,
    h('div', { class: 'row-gap' }, run),
    out);
  return { el };
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
        replaceContent(out, h('h3', null, 'Result'), resultBlock(res));
      }
      catch (e) {
        reportError(e);
      }
    }),
  }, 'Test record');
  return card('Test a record',
    h('p', { class: 'help' }, 'Runs the ', h('strong', null, 'saved'), ' rules against a sample record and shows which rules match and what they would grant. To see why a single rule matches, use "Test this rule" in its editor.'),
    picker.el,
    h('div', { class: 'row-gap' }, run),
    out);
}
