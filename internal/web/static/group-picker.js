// group-picker.js - AD group picker with live search (combobox pattern), used wherever a list of groups is edited:
// role rule grants and the lifecycle group lists.

import { api } from './api.js';
import { h, uid, clear, field, debounce } from './ui.js';

const groupMeta = new Map(); // lower(sAMAccountName or DN) -> { dn, name, privileged, category, scope }

function rememberGroup(g) {
  const meta = { dn: g.dn || '', name: g.name || g.sam || '', privileged: Boolean(g.privileged), category: g.category || '', scope: g.scope || '' };
  for (const k of [g.sam, g.dn]) {
    if (k) {
      groupMeta.set(String(k).toLowerCase(), meta);
    }
  }
}

/**
 * groupPicker({ model, key, label, help, dataTest }) -> element
 * Edits model[key] (an array of group identities as typed: sAMAccountName or DN). Typing searches
 * GET /api/ad/groups?q= after 300 ms; arrow keys move through the results, Enter adds the active result - or the typed
 * text when no result is active, so a group can be added when AD cannot be reached - and Escape closes the list.
 * Chosen groups are chips with a remove button; the DN is the chip's tooltip and protected administrative groups carry
 * a "privileged" badge (the scripts refuse to add accounts to them).
 */
export function groupPicker({ model, key, label, help = '', dataTest = 'group-picker', readOnly = false }) {
  if (!Array.isArray(model[key])) {
    model[key] = [];
  }
  const listId = uid('gp-list');
  const statusId = uid('gp-status');
  const chips = h('ul', { class: 'gp-chips', 'aria-label': `${label}: chosen groups`, 'data-test': `${dataTest}-chips` });
  const input = h('input', {
    type: 'text',
    role: 'combobox',
    autocomplete: 'off',
    spellcheck: 'false',
    'aria-autocomplete': 'list',
    'aria-expanded': 'false',
    'aria-controls': listId,
    placeholder: 'Type 2+ letters to search AD, or a group name and Enter',
    'data-test': `${dataTest}-input`,
  });
  const list = h('ul', { class: 'gp-list', id: listId, role: 'listbox', 'aria-label': `${label}: search results`, hidden: true });
  const status = h('p', { class: 'help gp-status', id: statusId, 'aria-live': 'polite', 'data-test': `${dataTest}-status` });
  let results = [];
  let active = -1;
  let seq = 0;

  const drawChips = () => {
    clear(chips);
    if (model[key].length === 0) {
      chips.appendChild(h('li', { class: 'muted small gp-none' }, 'No groups.'));
    }
    model[key].forEach((g, i) => {
      const meta = groupMeta.get(String(g).toLowerCase());
      chips.appendChild(h('li', { class: 'gp-chip', title: meta && meta.dn ? meta.dn : String(g), 'data-test': `${dataTest}-chip` },
        h('span', { class: 'gp-chip-name' }, String(g)),
        meta && meta.privileged ? h('span', { class: 'gp-priv', title: 'Protected administrative group: the scripts refuse to add accounts to it' }, 'privileged') : null,
        readOnly ? null : h('button', {
          type: 'button',
          class: 'gp-remove',
          'aria-label': `Remove ${g}`,
          'data-mutating': '',
          onclick: () => {
            model[key].splice(i, 1);
            drawChips();
            status.textContent = `Removed ${g}.`;
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

  const drawResults = (truncated) => {
    clear(list);
    results.forEach((g, i) => {
      list.appendChild(h('li', {
        id: `${listId}-${i}`,
        role: 'option',
        class: 'gp-option',
        'aria-selected': 'false',
        'data-test': `${dataTest}-option`,
        // mousedown (not click) so the input does not lose focus and close the list first
        onmousedown: (ev) => { ev.preventDefault(); add(g.sam || g.dn, g); },
      },
      h('span', { class: 'gp-opt-name' }, g.name || g.sam),
      g.sam && g.sam !== g.name ? h('span', { class: 'gp-opt-sam mono small' }, g.sam) : null,
      h('span', { class: 'gp-opt-kind small muted' }, [g.scope, g.category].filter(Boolean).join(' ')),
      g.privileged ? h('span', { class: 'gp-priv' }, 'privileged') : null,
      g.description ? h('span', { class: 'gp-opt-desc small muted' }, g.description) : null));
    });
    const open = results.length > 0;
    list.hidden = !open;
    input.setAttribute('aria-expanded', String(open));
    active = -1;
    status.textContent = open
      ? `${results.length} group${results.length === 1 ? '' : 's'} found${truncated ? ' (more exist - type more letters)' : ''}. Use the arrow keys and Enter to add one.`
      : 'No group matches. Press Enter to add the name as typed.';
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
    status.textContent = 'Searching AD…';
    try {
      const res = await api.get('/ad/groups', { q, max: 25 });
      if (mine !== seq) {
        return;
      }
      if (res && res.error) {
        // The server answers 200 with an error when AD itself cannot be searched.
        throw new Error(res.error);
      }
      results = res && Array.isArray(res.groups) ? res.groups : [];
      results.forEach(rememberGroup);
      drawResults(res && res.truncated);
    }
    catch (e) {
      if (mine !== seq) {
        return;
      }
      results = [];
      close();
      status.textContent = `AD search is not available (${e.message}). Type the group name and press Enter to add it anyway.`;
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
        add(results[active].sam || results[active].dn, results[active]);
      }
      else if (input.value.trim()) {
        search.cancel();
        seq += 1;
        add(input.value);
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
    return h('div', { class: 'field gp gp-readonly' }, h('span', { class: 'field-title' }, label), chips);
  }
  const wrap = field(label, input, { help });
  // Screen readers get the status (result count, errors) as part of the description.
  input.setAttribute('aria-describedby', `${input.getAttribute('aria-describedby') || ''} ${statusId}`.trim());
  wrap.classList.add('gp');
  wrap.append(h('div', { class: 'gp-pop' }, list), status, chips);
  return wrap;
}
