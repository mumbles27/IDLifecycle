// Retention: how long disabled accounts of people who left are kept before they are deleted, per policy.

import { api } from '../api.js';
import {
  h, clear, replaceContent, pageHeader, card, pill, dataTable, listItems, act, confirmDialog, toast,
  errorBox, loading, field, inputText, inputNumber, inputArea, inputCheck, inputSelect, formModal, mutBtn,
  chip, notice, historyLink,
} from '../ui.js';
import { ouPicker, identityPicker } from '../components.js';
import { groupPicker, groupLabel } from '../group-picker.js';
import { openImpactModal } from './impact.js';
import {
  conditionBuilder, conditionText, explainTable, toBody, toModel,
} from './roles.js';

// Fields a retention condition can test besides the person's last source record.
export const PSEUDO_FIELDS = [
  ['ad.memberOf', 'the AD groups of the account (use contains or is one of)'],
  ['ad.ou', 'the OU the account is in (its parent DN)'],
  ['ad.sam', 'the sAMAccountName'],
  ['source', 'the name of the source the person came from'],
  ['origin', '"source" or "manual" (added in the console)'],
];

export function daysText(days) {
  const n = Number(days) || 0;
  if (n <= 0) {
    return 'never deleted automatically';
  }
  return `deleted ${n} day${n === 1 ? '' : 's'} after the disable`;
}

function daysCell(days) {
  const n = Number(days) || 0;
  return n <= 0
    ? h('span', { 'data-test': 'retention-days' }, pill('never', 'never'), ' ', h('span', { class: 'muted small' }, 'kept until deleted by hand'))
    : h('span', { 'data-test': 'retention-days' }, `${n} day${n === 1 ? '' : 's'}`);
}

function appliesWhen(p) {
  const conds = Array.isArray(p.conditions) ? p.conditions : [];
  if (conds.length === 0) {
    return h('span', { class: 'muted small' }, 'only people assigned to it by hand');
  }
  const joiner = p.match_mode === 'any' ? ' OR ' : ' AND ';
  return h('span', { class: 'small' }, conds.map(conditionText).join(joiner));
}

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const listEl = h('div', null, loading());
  root.append(
    pageHeader('Retention',
      isAdmin ? mutBtn('New policy', { class: 'btn-primary', 'data-test': 'new-retention', onclick: () => openEditor(null) }) : null,
      h('a', { class: 'btn', href: '#/leavers', 'data-test': 'retention-to-leavers' }, 'Upcoming deletions')),
    h('details', { class: 'card explain', open: true },
      h('summary', null, 'How leavers and retention work'),
      h('ul', null,
        h('li', null, 'When someone leaves (a terminated status, a termination date that has passed, or missing from the source), their account is ',
          h('strong', null, 'disabled'), ', moved to the disabled OU and removed from its groups except the ones kept under ',
          h('a', { href: '#/lifecycle' }, 'Lifecycle groups'), '.'),
        h('li', null, 'A ', h('strong', null, 'retention policy'), ' decides how long the disabled account is kept before it is ',
          h('strong', null, 'deleted'), '. The first enabled policy (lowest priority number) whose conditions match the person wins; nobody matched gets the ',
          h('strong', null, 'Default'), '.'),
        h('li', null, 'A person can also be ', h('strong', null, 'assigned'), ' a policy by hand on their page (Identities); that wins over the conditions.'),
        h('li', null, 'Deletes still follow the approval policy, the delete limits and Manual items. A policy can make its deletes always wait for a person, or never delete at all (legal hold).'),
        h('li', null, 'Changing a policy changes the delete date of people already disabled. Use ', h('strong', null, 'Preview impact'), ' before saving.'))),
    listEl);

  let saved = [];
  let settings = null;

  async function load() {
    let resp;
    try {
      [resp, settings] = await Promise.all([api.get('/retention-policies', { limit: 500 }), api.get('/settings')]);
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    saved = listItems(resp).slice().sort((a, b) => (Number(a.priority) - Number(b.priority)) || String(a.name).localeCompare(String(b.name)));
    const engine = (settings && settings.engine) || {};
    const ad = (settings && settings.ad) || {};
    const defaultRow = {
      id: '', name: 'Default', builtin: true, description: 'Everyone no other policy applies to (Settings → Engine).', enabled: true,
      delete_after_days: engine.delete_after_days, disabled_ou: ad.disabled_ou || '', extra_keep_groups: [], approval: '',
      people: resp && Number.isFinite(resp.default_people) ? resp.default_people : null,
    };
    const rows = [...saved, defaultRow];
    const columns = [
      {
        label: 'Priority',
        render: (p) => (p.builtin
          ? h('span', { class: 'muted' }, 'last')
          : h('span', null, String(p.priority),
            isAdmin ? h('span', { class: 'retention-order' },
              mutBtn('↑', { class: 'btn-sm', title: `Move ${p.name} up`, 'aria-label': `Move ${p.name} up`, disabled: saved.indexOf(p) === 0, onclick: (e) => move(p, -1, e.currentTarget) }),
              mutBtn('↓', { class: 'btn-sm', title: `Move ${p.name} down`, 'aria-label': `Move ${p.name} down`, disabled: saved.indexOf(p) === saved.length - 1, onclick: (e) => move(p, 1, e.currentTarget) })) : null)),
      },
      { label: 'Name', render: (p) => h('div', null, h('strong', null, p.name), p.description ? h('div', { class: 'muted small' }, p.description) : null) },
      { label: 'Applies to', render: (p) => (p.builtin ? h('span', { class: 'muted small' }, 'everyone else') : appliesWhen(p)) },
      { label: 'Delete after', render: (p) => daysCell(p.delete_after_days) },
      { label: 'Disabled OU', render: (p) => (p.disabled_ou ? h('code', { class: 'small wrap' }, p.disabled_ou) : h('span', { class: 'muted small' }, p.builtin ? 'not moved' : 'the default')) },
      {
        label: 'Also kept',
        render: (p) => (Array.isArray(p.extra_keep_groups) && p.extra_keep_groups.length
          ? h('span', null, p.extra_keep_groups.map((g) => chip(groupLabel(g))))
          : h('span', { class: 'muted small' }, '-')),
      },
      { label: 'Deletes', render: (p) => (p.approval === 'manual' ? pill('manual', 'always need approval') : h('span', { class: 'muted small' }, 'approval policy')) },
      { label: 'People', render: (p) => (p.people === null || p.people === undefined ? '' : String(p.people)) },
      { label: 'Enabled', render: (p) => pill(p.enabled ? 'enabled' : 'disabled') },
      {
        label: 'Actions',
        className: 'col-actions',
        render: (p) => h('div', { class: 'row-actions' },
          isAdmin ? mutBtn('Edit', { class: 'btn-sm', 'data-test': p.builtin ? 'edit-retention-default' : 'edit-retention', onclick: () => (p.builtin ? editDefault() : openEditor(p)) }) : null,
          p.builtin ? null : historyLink('retention_policy', p.id),
          isAdmin && !p.builtin ? mutBtn('Delete', { class: 'btn-sm btn-danger', 'data-test': 'delete-retention', onclick: (e) => remove(p, e.currentTarget) }) : null),
      },
    ];
    replaceContent(listEl, dataTable(columns, rows, {
      caption: 'Retention policies',
      rowClass: (p) => (p.builtin ? 'retention-default' : ''),
    }));
  }

  // Up/down: renumber the policies 10, 20, 30 ... in the new order (only the ones whose number changes are saved).
  async function move(p, dir, button) {
    const order = saved.slice();
    const i = order.indexOf(p);
    const j = i + dir;
    if (j < 0 || j >= order.length) {
      return;
    }
    [order[i], order[j]] = [order[j], order[i]];
    await act(button, async () => {
      for (let k = 0; k < order.length; k += 1) {
        const want = (k + 1) * 10;
        if (Number(order[k].priority) !== want) {
          await api.put(`/retention-policies/${encodeURIComponent(order[k].id)}`, { ...order[k], priority: want });
        }
      }
      toast('Order saved.', 'ok');
      await load();
    });
  }

  async function remove(p, button) {
    const ok = await confirmDialog({
      title: 'Delete retention policy',
      message: `Delete "${p.name}"? People it applies to fall back to the next matching policy or the Default${Number(p.people) > 0 ? ` (${p.people} people now)` : ''}. Someone already disabled longer than that policy's period is then planned for deletion on the next sync.`,
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) {
      return;
    }
    await act(button, async () => {
      await api.del(`/retention-policies/${encodeURIComponent(p.id)}`);
      toast('Retention policy deleted.', 'ok');
      await load();
    });
  }

  function retentionFields(model) {
    const daysIn = inputNumber(model, 'delete_after_days', { min: '0', step: '1', 'data-test': 'retention-days-input', disabled: Boolean(model.never) });
    const never = inputCheck(model, 'never', 'Never delete automatically (keep until deleted by hand, e.g. a legal hold)', {
      'data-test': 'retention-never',
      onchange: (e) => { model.never = e.target.checked; daysIn.disabled = model.never; },
    });
    const ouIn = inputText(model, 'disabled_ou', { placeholder: 'OU=Disabled,DC=corp,DC=example,DC=com', 'data-test': 'retention-ou' });
    const pick = h('button', {
      type: 'button',
      class: 'btn btn-sm',
      onclick: async () => {
        const res = await ouPicker({ title: 'OU for disabled accounts', current: ouIn.value });
        if (res && res.ou) {
          model.disabled_ou = res.ou;
          ouIn.value = res.ou;
        }
      },
    }, 'Pick…');
    return { daysIn, never, ouIn, pick };
  }

  async function editDefault() {
    const engine = (settings && settings.engine) || {};
    const ad = (settings && settings.ad) || {};
    const days = Number(engine.delete_after_days) || 0;
    const model = { delete_after_days: days > 0 ? days : 30, never: days <= 0, disabled_ou: ad.disabled_ou || '' };
    const f = retentionFields(model);
    formModal({
      title: 'Default retention',
      content: [
        h('p', null, 'Applies to everyone no other policy matches or is assigned to. Stored in Settings (Engine: delete after days; Active Directory: disabled OU).'),
        field('Delete after (days)', f.daysIn, { help: 'Counted from the day the account was disabled.' }),
        f.never,
        field('Disabled OU', h('div', { class: 'row-gap' }, f.ouIn, f.pick), { help: 'Disabled accounts are moved here. Empty = they stay where they are.' }),
      ],
      submitLabel: 'Save default',
      onSubmit: async () => {
        const n = model.never ? 0 : Number(model.delete_after_days);
        if (!model.never && (!Number.isInteger(n) || n < 1)) {
          throw new Error('Delete after must be a whole number of days (1 or more), or tick "Never delete automatically".');
        }
        const fresh = await api.get('/settings');
        fresh.engine = { ...(fresh.engine || {}), delete_after_days: n };
        fresh.ad = { ...(fresh.ad || {}), disabled_ou: String(model.disabled_ou || '').trim() };
        await api.put('/settings', fresh);
        toast('Default retention saved.', 'ok');
        await load();
        return true;
      },
    });
  }

  function openEditor(existing) {
    const days = existing ? Number(existing.delete_after_days) || 0 : 90;
    const model = {
      name: existing ? existing.name : '',
      description: existing ? existing.description || '' : '',
      enabled: existing ? Boolean(existing.enabled) : true,
      priority: existing ? existing.priority : ((saved.length + 1) * 10),
      match_mode: existing ? existing.match_mode || 'all' : 'all',
      delete_after_days: days > 0 ? days : 365,
      never: Boolean(existing) && days <= 0,
      disabled_ou: existing ? existing.disabled_ou || '' : '',
      extra_keep_groups: existing && Array.isArray(existing.extra_keep_groups) ? [...existing.extra_keep_groups] : [],
      always_manual: existing ? existing.approval === 'manual' : false,
    };
    const conditions = ((existing && existing.conditions) || []).map(toModel);
    const body = () => ({
      ...(existing || {}),
      name: model.name.trim(),
      description: model.description || '',
      enabled: Boolean(model.enabled),
      priority: Number(model.priority) || 0,
      match_mode: model.match_mode,
      conditions: conditions.map(toBody),
      delete_after_days: model.never ? 0 : Number(model.delete_after_days),
      disabled_ou: String(model.disabled_ou || '').trim(),
      extra_keep_groups: [...model.extra_keep_groups],
      approval: model.always_manual ? 'manual' : '',
    });
    const f = retentionFields(model);
    const builder = conditionBuilder(conditions);
    const tester = policyTester(body);

    const content = h('div', { class: 'editor-cols' },
      h('div', { class: 'editor-main' },
        field('Name', inputText(model, 'name', { required: true, 'data-test': 'retention-name' }), { required: true }),
        field('Description', inputArea(model, 'description', { rows: '2' })),
        h('div', { class: 'field-row' },
          field('Priority', inputNumber(model, 'priority', { step: '1', required: true, 'data-test': 'retention-priority' }), { help: 'Lower numbers are checked first; the first match wins.' }),
          field('Match', inputSelect(model, 'match_mode', [{ value: 'all', label: 'all conditions' }, { value: 'any', label: 'any condition' }], { 'data-test': 'retention-match' }), { help: 'How the top-level conditions combine.' }),
          inputCheck(model, 'enabled', 'Enabled')),
        h('fieldset', { class: 'group' },
          h('legend', null, 'Applies to'),
          h('p', { class: 'help' }, 'Conditions test the person\'s last source record (it is kept after they leave) and these fields: ',
            PSEUDO_FIELDS.map(([name, what], i) => h('span', null, i ? '; ' : '', h('code', null, name), ` = ${what}`)),
            '. With no conditions the policy only applies to people assigned to it by hand.'),
          builder.el),
        h('fieldset', { class: 'group' },
          h('legend', null, 'Retention'),
          field('Delete after (days)', f.daysIn, { help: 'Counted from the day the account was disabled.' }),
          f.never,
          inputCheck(model, 'always_manual', 'Deletes under this policy always wait for a person (even when deletes are approved automatically)', { 'data-test': 'retention-approval' })),
        h('fieldset', { class: 'group' },
          h('legend', null, 'On disable'),
          field('Disabled OU (optional)', h('div', { class: 'row-gap' }, f.ouIn, f.pick), { help: 'Overrides the default disabled OU for people under this policy. Empty = the default.' }),
          groupPicker({ model, key: 'extra_keep_groups', label: 'Also keep these groups', dataTest: 'retention-keep', help: 'Kept on disable in addition to the groups kept under Lifecycle groups (for example a mailbox retention group).' }))),
      h('div', { class: 'editor-side' }, tester.el));

    const impactBtn = h('button', {
      type: 'button',
      class: 'btn',
      'data-test': 'retention-impact',
      onclick: () => act(impactBtn, async () => {
        const proposed = body();
        const list = saved.filter((p) => !existing || p.id !== existing.id);
        list.push(proposed);
        openImpactModal({ title: `Preview impact: ${proposed.name || 'new policy'}`, proposal: { retention_policies: list } });
      }),
    }, 'Preview impact');

    formModal({
      title: existing ? `Edit policy: ${existing.name}` : 'New retention policy',
      content,
      wide: true,
      submitLabel: existing ? 'Save policy' : 'Create policy',
      extraFooter: impactBtn,
      onSubmit: async () => {
        const b = body();
        if (!model.never && (!Number.isInteger(b.delete_after_days) || b.delete_after_days < 1)) {
          throw new Error('Delete after must be a whole number of days (1 or more), or tick "Never delete automatically".');
        }
        if (existing) {
          await api.put(`/retention-policies/${encodeURIComponent(existing.id)}`, b);
        }
        else {
          await api.post('/retention-policies', b);
        }
        toast(existing ? 'Retention policy saved.' : 'Retention policy created.', 'ok');
        await load();
        return true;
      },
    });
  }

  await load();
}

/** The editor's "Test this policy": the unsaved policy against one person, with the reason for each condition. */
function policyTester(getPolicy) {
  const out = h('div', { class: 'test-out', 'aria-live': 'polite', 'data-test': 'retention-test-out' });
  let person = null;
  const who = h('p', { class: 'small', 'data-test': 'retention-test-person' }, h('span', { class: 'muted' }, 'No person chosen.'));
  const choose = h('button', {
    type: 'button',
    class: 'btn btn-sm',
    'data-test': 'retention-test-pick',
    onclick: async () => {
      const p = await identityPicker({ title: 'Test against a person', note: 'Pick someone (a leaver is a good test: their last source record is kept).' });
      if (p) {
        person = p;
        replaceContent(who, 'Person: ', h('strong', null, p.display || p.external_id), ` (${p.external_id})`);
      }
    },
  }, 'Choose a person…');
  const run = h('button', {
    type: 'button',
    class: 'btn btn-sm btn-primary',
    'data-test': 'retention-test-run',
    onclick: () => act(run, async () => {
      if (!person) {
        replaceContent(out, notice('info', 'Choose a person first.'));
        return;
      }
      const policy = getPolicy();
      const res = await api.post('/retention-policies/test', { policy, identity_id: person.id });
      const explain = Array.isArray(res.explain) ? res.explain : [];
      let verdict = res.matched ? `This policy APPLIES to ${person.display || person.external_id}.` : `This policy does NOT apply to ${person.display || person.external_id}.`;
      if (!policy.enabled) {
        verdict = 'This policy is disabled, so it applies to nobody.';
      }
      replaceContent(out,
        h('p', { class: 'rule-verdict', 'data-test': 'retention-verdict' }, h('strong', null, verdict)),
        res.matched && policy.enabled ? h('p', { class: 'small' }, `If they leave: ${daysText(policy.delete_after_days)}.`) : null,
        explain.length ? explainTable(explain) : null);
    }),
  }, 'Test this policy');
  const el = h('section', { class: 'preview-panel' },
    h('h3', null, 'Test this policy'),
    h('p', { class: 'help' }, 'Runs the policy as edited (not yet saved) against one person and explains every condition. A person assigned a policy by hand gets that one whatever the conditions say.'),
    who,
    h('div', { class: 'row-gap' }, choose, run),
    out);
  return { el, clear: () => clear(out) };
}
