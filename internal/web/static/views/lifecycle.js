// Lifecycle groups: groups tied to an account's state rather than to a role - every active account, every disabled
// account, and what a disable removes. Stored in settings.lifecycle_groups; admins edit, everyone else reads.

import { api } from '../api.js';
import {
  h, uid, replaceContent, pageHeader, card, errorBox, loading, act, toast, notice, isObject, btn,
} from '../ui.js';
import { groupPicker } from '../group-picker.js';
import { openImpactModal } from './impact.js';

const REMOVE_MODES = [
  {
    value: 'all_except',
    label: 'Remove every group except the ones listed below',
    help: 'The account keeps only the "keep" groups and the groups for disabled accounts. Protected administrative groups are never touched by the scripts.',
  },
  {
    value: 'managed',
    label: 'Remove only the groups this service manages',
    help: 'Groups from role rules and the "every active account" list are removed; groups someone added by hand in AD stay.',
  },
  {
    value: 'none',
    label: 'Remove no groups',
    help: 'The account is disabled with all its memberships intact (the "add" list below still applies).',
  },
];

function lifecycleModel(doc) {
  if (!isObject(doc.lifecycle_groups)) {
    doc.lifecycle_groups = {};
  }
  const lg = doc.lifecycle_groups;
  for (const k of ['active_add', 'disabled_add', 'disabled_keep']) {
    if (!Array.isArray(lg[k])) {
      lg[k] = [];
    }
  }
  if (!lg.disabled_remove) {
    // Older settings documents only have the AD switch.
    lg.disabled_remove = doc.ad && doc.ad.strip_groups_on_disable ? 'all_except' : 'none';
  }
  return lg;
}

function removeModeRadios(lg, readOnly, onChange) {
  const name = uid('remove-mode');
  return h('fieldset', { class: 'group radio-group', 'data-test': 'lifecycle-remove' },
    h('legend', null, 'When an account is disabled'),
    REMOVE_MODES.map((m) => {
      const id = uid('rm');
      const helpId = `${id}-help`;
      return h('div', { class: 'radio-option' },
        h('input', {
          type: 'radio', id, name, value: m.value, checked: lg.disabled_remove === m.value, disabled: readOnly,
          'aria-describedby': helpId, 'data-test': `lifecycle-remove-${m.value}`,
          onchange: () => { lg.disabled_remove = m.value; onChange(); },
        }),
        h('label', { for: id }, m.label),
        h('p', { class: 'help', id: helpId }, m.help));
    }));
}

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const body = h('div', null, loading());
  root.append(pageHeader('Lifecycle groups'), body);

  async function load() {
    let doc;
    try {
      doc = await api.get('/settings');
    }
    catch (e) {
      replaceContent(body, errorBox(e));
      return;
    }
    draw(isObject(doc) ? doc : {});
  }

  function draw(doc) {
    const lg = lifecycleModel(doc);
    const keepBox = h('div');
    const drawKeep = () => {
      replaceContent(keepBox, lg.disabled_remove === 'all_except'
        ? groupPicker({ model: lg, key: 'disabled_keep', label: 'Groups a disabled account keeps', dataTest: 'lifecycle-keep', readOnly: !isAdmin, help: 'For example a mail-retention or legal-hold group.' })
        : h('p', { class: 'muted small' }, 'A keep list only matters with "Remove every group except…".'));
    };
    drawKeep();

    const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary', 'data-mutating': '', 'data-test': 'lifecycle-save' }, 'Save');
    const impactBtn = btn('Preview impact', {
      'data-test': 'lifecycle-impact',
      onclick: () => openImpactModal({ title: 'Preview impact: lifecycle groups', proposal: { settings: doc } }),
    });
    const form = h('form', {
      class: 'stack',
      onsubmit: (ev) => {
        ev.preventDefault();
        act(saveBtn, async () => {
          const saved = await api.put('/settings', doc);
          toast('Lifecycle groups saved.', 'ok');
          draw(isObject(saved) ? saved : doc);
        });
      },
    },
    isAdmin ? null : notice('info', 'Read-only: only administrators can change lifecycle groups.'),
    card('How lifecycle groups work',
      h('p', { class: 'help' }, 'These groups follow the account\'s state, on top of the groups from role rules. Groups are given as sAMAccountName or distinguished name; search AD to pick them. Accounts are never added to protected administrative groups (Domain Admins and the like), even when one is listed.')),
    card('Active accounts',
      groupPicker({ model: lg, key: 'active_add', label: 'Every active account is a member of', dataTest: 'lifecycle-active', readOnly: !isAdmin, help: 'Added when an account is created or enabled, and kept on every sync - like a role rule that matches everyone.' })),
    card('Disabled accounts',
      removeModeRadios(lg, !isAdmin, drawKeep),
      keepBox,
      groupPicker({ model: lg, key: 'disabled_add', label: 'Add disabled accounts to', dataTest: 'lifecycle-disabled', readOnly: !isAdmin, help: 'Added after the account is disabled and removed again when it is enabled, e.g. a "Disabled Users" group.' })),
    isAdmin ? h('div', { class: 'row-gap' }, saveBtn, impactBtn) : null);
    replaceContent(body, form);
  }

  await load();
}
