// group-picker.js - group picker with live search (combobox pattern), used wherever a list of groups is edited:
// role rule grants and the lifecycle group lists. Searches Active Directory and - when the Microsoft 365 connection
// is on (settings.cloud.enabled) - Entra ID / Microsoft 365 / Exchange Online groups.

import { api } from './api.js';
import { h, uid, clear, field, debounce } from './ui.js';

export const CLOUD_PREFIX = 'entra:';

const groupMeta = new Map(); // lower(sAMAccountName, DN or entra:<id>) -> { dn, name, privileged, category, scope, cloud, kind, mail, manageable, reason }

/** Plain names of the cloud group kinds (design: security | m365 | mail_security | distribution). */
export const CLOUD_KINDS = {
  security: 'Security',
  m365: 'Microsoft 365',
  mail_security: 'Mail-enabled security',
  distribution: 'Distribution list',
};
const EXCHANGE_KINDS = new Set(['mail_security', 'distribution']);

/** Why a cloud group cannot be chosen (the server sends a code or a sentence). */
const CLOUD_REASONS = {
  role_assignable: 'Role-assignable group: IDLifecycle never changes its members.',
  dynamic: 'Dynamic membership: Entra ID sets the members from a rule.',
  synced: 'Synced from Active Directory: pick it under Active Directory instead.',
  exchange_off: 'Needs Exchange Online, which is not set up (Settings → Microsoft 365 / Entra ID).',
  unverified: 'Could not verify that this group is not role-assignable (grant Group.Read.All).',
  unsupported: 'This kind of group cannot be managed by IDLifecycle.',
  not_found: 'Group not found in Entra ID.',
};

export function cloudKindLabel(kind) {
  return CLOUD_KINDS[kind] || kind || 'cloud group';
}

export function cloudReasonText(reason) {
  return CLOUD_REASONS[reason] || reason || 'This group cannot be managed.';
}

export function isCloudRef(g) {
  return String(g || '').toLowerCase().startsWith(CLOUD_PREFIX);
}

const GUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function rememberGroup(g) {
  const meta = { dn: g.dn || '', name: g.name || g.sam || '', privileged: Boolean(g.privileged), category: g.category || '', scope: g.scope || '' };
  for (const k of [g.sam, g.dn]) {
    if (k) {
      groupMeta.set(String(k).toLowerCase(), meta);
    }
  }
}

function cloudRef(g) {
  return g.ref || `${CLOUD_PREFIX}${String(g.id || '').toLowerCase()}`;
}

function rememberCloudGroup(g) {
  groupMeta.set(cloudRef(g).toLowerCase(), {
    cloud: true, name: g.name || '', kind: g.kind || '', mail: g.mail || '', description: g.description || '',
    manageable: g.manageable !== false, reason: g.reason || '',
  });
}

/** Cached look-up of whether the Microsoft 365 connection is on; reset after the settings are saved. */
let cloudStatusPromise = null;
export function cloudStatus() {
  if (!cloudStatusPromise) {
    cloudStatusPromise = api.get('/settings').then((doc) => {
      const c = doc && doc.cloud ? doc.cloud : {};
      return { enabled: Boolean(c.enabled), exchange: Boolean(c.enabled && c.exchange && c.exchange.enabled) };
    }, () => ({ enabled: false, exchange: false }));
  }
  return cloudStatusPromise;
}
export function resetCloudStatus() {
  cloudStatusPromise = null;
}

/** Fills the names of cloud group references not seen yet (GET /api/cloud/groups/resolve). Resolves true when any was learned. */
export async function resolveCloudGroups(refs) {
  const ids = Array.from(new Set((refs || []).filter(isCloudRef).filter((r) => !groupMeta.has(String(r).toLowerCase()))
    .map((r) => String(r).slice(CLOUD_PREFIX.length).toLowerCase()))).slice(0, 50);
  if (ids.length === 0) {
    return false;
  }
  try {
    const res = await api.get('/cloud/groups/resolve', { ids: ids.join(',') });
    const groups = res && Array.isArray(res.groups) ? res.groups : [];
    groups.forEach(rememberCloudGroup);
    return groups.length > 0;
  }
  catch (e) {
    return false;
  }
}

/** Display name of a group reference: the cloud group's name (or "Entra group <id>") or the AD identity as typed. */
export function groupLabel(g) {
  const meta = groupMeta.get(String(g).toLowerCase());
  if (isCloudRef(g)) {
    return meta && meta.name ? meta.name : `Entra group ${String(g).slice(CLOUD_PREFIX.length)}`;
  }
  return String(g);
}

/**
 * groupPicker({ model, key, label, help, dataTest }) -> element
 * Edits model[key] (an array of group identities: sAMAccountName or DN for AD, entra:<objectId> for cloud groups).
 * Typing searches GET /api/ad/groups?q= (or /api/cloud/groups?q= when "Entra ID / Microsoft 365" is chosen) after
 * 300 ms; arrow keys move through the results, Enter adds the active result - or, for AD, the typed text when no
 * result is active, so a group can be added when AD cannot be reached - and Escape closes the list.
 * Chosen groups are chips with a remove button. AD chips: the DN is the tooltip and protected administrative groups
 * carry a "privileged" badge. Cloud chips carry a "cloud" marker and the kind (Exchange Online kinds say so).
 */
export function groupPicker({ model, key, label, help = '', dataTest = 'group-picker', readOnly = false }) {
  if (!Array.isArray(model[key])) {
    model[key] = [];
  }
  const listId = uid('gp-list');
  const statusId = uid('gp-status');
  const chips = h('ul', { class: 'gp-chips', 'aria-label': `${label}: chosen groups`, 'data-test': `${dataTest}-chips` });
  const AD_PLACEHOLDER = 'Type 2+ letters to search AD, or a group name and Enter';
  const CLOUD_PLACEHOLDER = 'Type 2+ letters to search Entra ID / Microsoft 365 groups';
  const input = h('input', {
    type: 'text',
    role: 'combobox',
    autocomplete: 'off',
    spellcheck: 'false',
    'aria-autocomplete': 'list',
    'aria-expanded': 'false',
    'aria-controls': listId,
    placeholder: AD_PLACEHOLDER,
    'data-test': `${dataTest}-input`,
  });
  const list = h('ul', { class: 'gp-list', id: listId, role: 'listbox', 'aria-label': `${label}: search results`, hidden: true });
  const status = h('p', { class: 'help gp-status', id: statusId, 'aria-live': 'polite', 'data-test': `${dataTest}-status` });
  let results = [];
  let active = -1;
  let seq = 0;
  let source = 'ad'; // ad | cloud
  let exchangeOn = false;

  const drawChips = () => {
    clear(chips);
    if (model[key].length === 0) {
      chips.appendChild(h('li', { class: 'muted small gp-none' }, 'No groups.'));
    }
    model[key].forEach((g, i) => {
      const meta = groupMeta.get(String(g).toLowerCase());
      const cloud = isCloudRef(g);
      const name = groupLabel(g);
      let title = meta && meta.dn ? meta.dn : String(g);
      if (cloud) {
        title = [meta && meta.kind ? `${cloudKindLabel(meta.kind)} group in Entra ID` : 'Entra ID group', meta && meta.mail, String(g)].filter(Boolean).join(' · ');
      }
      chips.appendChild(h('li', { class: `gp-chip${cloud ? ' gp-chip-cloud' : ''}`, title, 'data-test': `${dataTest}-chip` },
        cloud ? h('span', { class: 'gp-cloud', 'data-test': `${dataTest}-chip-cloud` }, 'cloud') : null,
        h('span', { class: 'gp-chip-name' }, name),
        cloud && meta && meta.kind ? h('span', { class: 'gp-kind' }, cloudKindLabel(meta.kind), EXCHANGE_KINDS.has(meta.kind) ? ' · Exchange Online' : '') : null,
        cloud && meta && !meta.manageable ? h('span', { class: 'gp-priv', title: cloudReasonText(meta.reason) }, 'not manageable') : null,
        !cloud && meta && meta.privileged ? h('span', { class: 'gp-priv', title: 'Protected administrative group: the scripts refuse to add accounts to it' }, 'privileged') : null,
        readOnly ? null : h('button', {
          type: 'button',
          class: 'gp-remove',
          'aria-label': `Remove ${name}`,
          'data-mutating': '',
          onclick: () => {
            model[key].splice(i, 1);
            drawChips();
            status.textContent = `Removed ${name}.`;
            input.focus();
          },
        }, '×')));
    });
  };

  const close = () => {
    list.hidden = true;
    input.setAttribute('aria-expanded', 'false');
    input.removeAttribute('aria-activedescendant');
    active = -1;
  };

  const setActive = (i) => {
    const opts = list.querySelectorAll('[role="option"]');
    if (opts.length === 0) {
      return;
    }
    active = (i + opts.length) % opts.length;
    opts.forEach((o, n) => {
      o.setAttribute('aria-selected', String(n === active));
      o.classList.toggle('gp-active', n === active);
    });
    input.setAttribute('aria-activedescendant', opts[active].id);
    if (typeof opts[active].scrollIntoView === 'function') {
      opts[active].scrollIntoView({ block: 'nearest' });
    }
  };

  const add = (identity, meta) => {
    const v = String(identity || '').trim();
    if (!v) {
      return;
    }
    if (meta) {
      rememberGroup(meta);
    }
    if (!model[key].some((g) => String(g).toLowerCase() === v.toLowerCase())) {
      model[key].push(v);
    }
    input.value = '';
    results = [];
    close();
    drawChips();
    status.textContent = meta && meta.privileged ? `Added ${v} - a protected administrative group: accounts will NOT be added to it.` : `Added ${v}.`;
  };

  const addCloud = (g) => {
    if (g.manageable === false) {
      status.textContent = `${g.name} cannot be added. ${cloudReasonText(g.reason)}`;
      return;
    }
    rememberCloudGroup(g);
    const ref = cloudRef(g);
    if (!model[key].some((x) => String(x).toLowerCase() === ref.toLowerCase())) {
      model[key].push(ref);
    }
    input.value = '';
    results = [];
    close();
    drawChips();
    status.textContent = `Added ${g.name} (${cloudKindLabel(g.kind)}${EXCHANGE_KINDS.has(g.kind) ? ', changed through Exchange Online' : ''}).`;
  };

  const choose = (g) => (source === 'cloud' ? addCloud(g) : add(g.sam || g.dn, g));

  const adOption = (g) => [
    h('span', { class: 'gp-opt-name' }, g.name || g.sam),
    g.sam && g.sam !== g.name ? h('span', { class: 'gp-opt-sam mono small' }, g.sam) : null,
    h('span', { class: 'gp-opt-kind small muted' }, [g.scope, g.category].filter(Boolean).join(' ')),
    g.privileged ? h('span', { class: 'gp-priv' }, 'privileged') : null,
    g.description ? h('span', { class: 'gp-opt-desc small muted' }, g.description) : null,
  ];

  const cloudOption = (g) => [
    h('span', { class: 'gp-opt-name' }, g.name),
    g.mail ? h('span', { class: 'gp-opt-sam mono small' }, g.mail) : null,
    h('span', { class: 'gp-kind' }, cloudKindLabel(g.kind)),
    EXCHANGE_KINDS.has(g.kind) ? h('span', { class: 'gp-opt-kind small muted' }, 'via Exchange Online') : null,
    g.manageable === false ? h('span', { class: 'gp-priv' }, 'cannot be used') : null,
    g.manageable === false ? h('span', { class: 'gp-opt-desc small' }, cloudReasonText(g.reason)) : null,
    g.description ? h('span', { class: 'gp-opt-desc small muted' }, g.description) : null,
  ];

  const drawResults = (truncated) => {
    clear(list);
    results.forEach((g, i) => {
      const disabled = source === 'cloud' && g.manageable === false;
      list.appendChild(h('li', {
        id: `${listId}-${i}`,
        role: 'option',
        class: `gp-option${disabled ? ' gp-disabled' : ''}`,
        'aria-selected': 'false',
        'aria-disabled': disabled ? 'true' : null,
        'data-test': `${dataTest}-option`,
        // mousedown (not click) so the input does not lose focus and close the list first
        onmousedown: (ev) => { ev.preventDefault(); choose(g); },
      }, source === 'cloud' ? cloudOption(g) : adOption(g)));
    });
    const open = results.length > 0;
    list.hidden = !open;
    input.setAttribute('aria-expanded', String(open));
    active = -1;
    if (open) {
      const blocked = source === 'cloud' ? results.filter((g) => g.manageable === false).length : 0;
      status.textContent = `${results.length} group${results.length === 1 ? '' : 's'} found${blocked ? ` (${blocked} cannot be used)` : ''}${truncated ? ' (more exist - type more letters)' : ''}. Use the arrow keys and Enter to add one.`;
    }
    else {
      status.textContent = source === 'cloud'
        ? 'No cloud group matches.'
        : 'No group matches. Press Enter to add the name as typed.';
    }
  };

  const search = debounce(async () => {
    const q = input.value.trim();
    seq += 1;
    const mine = seq;
    if (q.length < 2) {
      results = [];
      close();
      status.textContent = q.length === 1 ? 'Type at least 2 letters to search.' : '';
      return;
    }
    const cloud = source === 'cloud';
    status.textContent = cloud ? 'Searching Entra ID…' : 'Searching AD…';
    try {
      const res = await api.get(cloud ? '/cloud/groups' : '/ad/groups', { q, max: 25 });
      if (mine !== seq) {
        return;
      }
      if (res && res.error) {
        // The server answers 200 with an error when the directory itself cannot be searched.
        throw new Error(res.error);
      }
      results = res && Array.isArray(res.groups) ? res.groups : [];
      results.forEach(cloud ? rememberCloudGroup : rememberGroup);
      drawResults(res && res.truncated);
    }
    catch (e) {
      if (mine !== seq) {
        return;
      }
      results = [];
      close();
      status.textContent = cloud
        ? `Entra ID search is not available (${e.message}).`
        : `AD search is not available (${e.message}). Type the group name and press Enter to add it anyway.`;
    }
  }, 300);

  input.addEventListener('input', search);
  input.addEventListener('keydown', (ev) => {
    if (ev.key === 'ArrowDown') {
      ev.preventDefault();
      if (list.hidden && results.length) {
        drawResults(false);
      }
      setActive(active + 1);
    }
    else if (ev.key === 'ArrowUp') {
      ev.preventDefault();
      if (!list.hidden) {
        setActive(active - 1);
      }
    }
    else if (ev.key === 'Enter') {
      // Never submits the surrounding form: Enter means "add this group".
      ev.preventDefault();
      if (!list.hidden && active >= 0 && results[active]) {
        choose(results[active]);
      }
      else if (input.value.trim()) {
        const typed = input.value.trim();
        search.cancel();
        seq += 1;
        if (source === 'cloud') {
          // Cloud groups are stored by object ID; a typed ID (or entra:<id>) is accepted as is.
          const id = typed.toLowerCase().startsWith(CLOUD_PREFIX) ? typed.slice(CLOUD_PREFIX.length) : typed;
          if (GUID_RE.test(id)) {
            add(`${CLOUD_PREFIX}${id.toLowerCase()}`);
            resolveCloudGroups(model[key]).then((got) => got && drawChips());
          }
          else {
            status.textContent = 'Pick a cloud group from the search results, or type its object ID.';
          }
        }
        else {
          add(typed);
        }
      }
    }
    else if (ev.key === 'Escape' && !list.hidden) {
      ev.preventDefault();
      close();
    }
  });
  input.addEventListener('blur', () => setTimeout(close, 150));

  drawChips();
  if (readOnly) {
    // Name look-ups are admin-only; read-only viewers see the object ID.
    return h('div', { class: 'field gp gp-readonly' }, h('span', { class: 'field-title' }, label), chips);
  }
  const wrap = field(label, input, { help });
  // Screen readers get the status (result count, errors) as part of the description.
  input.setAttribute('aria-describedby', `${input.getAttribute('aria-describedby') || ''} ${statusId}`.trim());
  wrap.classList.add('gp');
  // Names of cloud groups chosen earlier.
  resolveCloudGroups(model[key]).then((got) => got && drawChips());

  // Source switch: shown only when the Microsoft 365 connection is on.
  const radioName = uid('gp-src');
  const setSource = (s) => {
    if (s === source) {
      return;
    }
    source = s;
    input.placeholder = s === 'cloud' ? CLOUD_PLACEHOLDER : AD_PLACEHOLDER;
    results = [];
    close();
    status.textContent = s === 'cloud'
      ? `Searching Entra ID / Microsoft 365 groups${exchangeOn ? '' : ' (mail-enabled security groups and distribution lists need Exchange Online, which is not set up)'}.`
      : 'Searching Active Directory groups.';
    if (input.value.trim().length >= 2) {
      search();
    }
  };
  const sourceBox = h('fieldset', { class: 'gp-source', hidden: true, 'data-test': `${dataTest}-source` },
    h('legend', { class: 'small' }, 'Search in'),
    [['ad', 'Active Directory'], ['cloud', 'Entra ID / Microsoft 365']].map(([value, text]) => {
      const id = uid('gp-src');
      return h('span', { class: 'gp-source-opt' },
        h('input', { type: 'radio', id, name: radioName, value, checked: value === 'ad', 'data-test': `${dataTest}-source-${value}`, onchange: () => setSource(value) }),
        h('label', { for: id }, text));
    }));
  cloudStatus().then((s) => {
    exchangeOn = s.exchange;
    sourceBox.hidden = !s.enabled;
  });
  wrap.insertBefore(sourceBox, wrap.firstChild.nextSibling);
  wrap.append(h('div', { class: 'gp-pop' }, list), status, chips);
  return wrap;
}
