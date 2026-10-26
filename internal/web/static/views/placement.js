// OU placement: which OU a new account goes to, by department (or another source field), with an OU picker fed
// from AD. Stored in settings.placement; admins edit, everyone else reads.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, errorBox, loading, field, inputText, inputCheck, inputSelect, act, toast,
  notice, isObject, clear,
} from '../ui.js';
import { ouPicker } from '../components.js';

function csvField(v) {
  const s = String(v === undefined || v === null ? '' : v);
  return /[",\r\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

/** Parses "value,ou" lines (a header row "department,ou" or "match,ou" is skipped; quotes as in CSV). */
export function parseRulesCSV(text) {
  const rows = [];
  let row = [];
  let cur = '';
  let quoted = false;
  const s = String(text || '');
  for (let i = 0; i < s.length; i += 1) {
    const c = s[i];
    if (quoted) {
      if (c === '"' && s[i + 1] === '"') {
        cur += '"';
        i += 1;
      }
      else if (c === '"') {
        quoted = false;
      }
      else {
        cur += c;
      }
    }
    else if (c === '"') {
      quoted = true;
    }
    else if (c === ',') {
      row.push(cur);
      cur = '';
    }
    else if (c === '\n' || c === '\r') {
      if (c === '\r' && s[i + 1] === '\n') {
        i += 1;
      }
      row.push(cur);
      rows.push(row);
      row = [];
      cur = '';
    }
    else {
      cur += c;
    }
  }
  if (cur !== '' || row.length > 0) {
    row.push(cur);
    rows.push(row);
  }
  const out = [];
  for (const r of rows) {
    const match = (r[0] || '').trim();
    const ou = (r.slice(1).join(',') || '').trim();
    if (!match && !ou) {
      continue;
    }
    if (out.length === 0 && /^(department|dept|match|value)$/i.test(match) && /^ou$/i.test(ou)) {
      continue;
    }
    out.push({ match, ou });
  }
  return out;
}

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const body = h('div', null, loading());
  root.append(pageHeader('OU placement'), body);

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
    if (!isObject(doc.placement)) {
      doc.placement = {};
    }
    const pl = doc.placement;
    if (!Array.isArray(pl.rules)) {
      pl.rules = [];
    }
    pl.field = pl.field || 'Department';
    pl.unmapped = pl.unmapped || 'hold';
    const rulesEl = h('div', { 'data-test': 'placement-rules' });
    const valuesEl = h('div', { 'data-test': 'placement-values' }, loading());
    const status = h('div', { 'aria-live': 'polite' });

    const pick = async (rule, input) => {
      const res = await ouPicker({ title: `OU for "${rule.match || 'new value'}"`, current: rule.ou });
      if (res) {
        rule.ou = res.ou;
        input.value = res.ou;
      }
    };

    const drawRules = () => {
      if (pl.rules.length === 0) {
        replaceContent(rulesEl, h('p', { class: 'muted' }, 'No rules yet. Add one below, import a CSV, or map the departments listed further down.'));
        return;
      }
      replaceContent(rulesEl, h('table', { class: 'data' },
        h('thead', null, h('tr', null, h('th', null, `${pl.field} value`), h('th', null, 'OU'), h('th', null, ''))),
        h('tbody', null, pl.rules.map((rule, i) => {
          const ouInput = inputText(rule, 'ou', { class: 'mono', placeholder: 'OU=Finance,OU=Staff,DC=corp,DC=example,DC=com', required: true, 'data-test': 'rule-ou' });
          return h('tr', null,
            h('td', null, inputText(rule, 'match', { required: true, placeholder: 'Finance or Sales*', 'data-test': 'rule-match' })),
            h('td', null, h('div', { class: 'row-gap' }, ouInput, isAdmin ? h('button', { type: 'button', class: 'btn btn-sm', onclick: () => pick(rule, ouInput) }, 'Pick…') : null)),
            h('td', null, isAdmin ? h('button', { type: 'button', class: 'btn btn-sm btn-danger', 'aria-label': `Remove the rule for ${rule.match}`, onclick: () => { pl.rules.splice(i, 1); drawRules(); } }, 'Remove') : null));
        }))));
    };

    const addRule = (match, ou) => {
      const existing = pl.rules.find((r) => (r.match || '').trim().toLowerCase() === String(match).trim().toLowerCase());
      if (existing) {
        existing.ou = ou;
      }
      else {
        pl.rules.push({ match, ou });
      }
      drawRules();
    };

    const loadValues = async () => {
      let res;
      try {
        res = await api.get('/placement/values', { field: pl.field });
      }
      catch (e) {
        replaceContent(valuesEl, errorBox(e));
        return;
      }
      const vals = (res && Array.isArray(res.values)) ? res.values : [];
      if (vals.length === 0) {
        replaceContent(valuesEl, h('p', { class: 'muted' }, `No active people have a ${pl.field} yet (run a sync first).`));
        return;
      }
      const unmapped = vals.filter((v) => !v.ou).length;
      replaceContent(valuesEl,
        h('p', { class: 'help' }, `${vals.length} value(s) among the active people; ${unmapped} without an OU. Values are compared without regard to case. Map adds a rule - save to keep it.`),
        h('table', { class: 'data' },
          h('thead', null, h('tr', null, h('th', null, pl.field), h('th', { class: 'num' }, 'People'), h('th', null, 'OU from the rules'), h('th', null, ''))),
          h('tbody', null, vals.map((v) => h('tr', { class: v.ou ? '' : 'row-error' },
            h('td', null, v.value || h('span', { class: 'muted' }, '(empty)')),
            h('td', { class: 'num' }, String(v.people)),
            h('td', { class: 'mono small' }, v.ou || h('span', { class: 'muted' }, 'none - waits for a person to choose')),
            h('td', null, isAdmin && v.value ? h('button', {
              type: 'button', class: 'btn btn-sm', 'data-test': 'map-value',
              onclick: async () => {
                const res2 = await ouPicker({ title: `OU for "${v.value}"`, current: v.ou });
                if (res2) {
                  addRule(v.value, res2.ou);
                  toast(`Rule for "${v.value}" added - save to keep it.`, 'ok');
                }
              },
            }, v.ou ? 'Change…' : 'Map…') : null))))));
    };

    const importInput = h('input', {
      type: 'file', accept: '.csv,text/csv', class: 'sr-only', 'data-test': 'rules-import',
      onchange: async () => {
        const f = importInput.files && importInput.files[0];
        if (!f) {
          return;
        }
        const rows = parseRulesCSV(await f.text());
        for (const r of rows) {
          addRule(r.match, r.ou);
        }
        importInput.value = '';
        toast(`${rows.length} rule(s) imported - check them and save.`, 'ok');
      },
    });
    const exportRules = () => {
      const text = ['department,ou', ...pl.rules.map((r) => `${csvField(r.match)},${csvField(r.ou)}`)].join('\r\n') + '\r\n';
      const a = h('a', { href: URL.createObjectURL(new Blob([text], { type: 'text/csv' })), download: 'ou-placement.csv' });
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    };

    const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary', 'data-mutating': '', 'data-test': 'placement-save' }, 'Save');
    const form = h('form', {
      class: 'stack',
      onsubmit: (ev) => {
        ev.preventDefault();
        act(saveBtn, async () => {
          clear(status);
          const saved = await api.put('/settings', doc);
          toast('OU placement saved.', 'ok');
          draw(isObject(saved) ? saved : doc);
        });
      },
    },
    isAdmin ? null : notice('info', 'Read-only: only administrators can change OU placement.'),
    h('fieldset', { class: 'plain', disabled: !isAdmin }, h('div', { class: 'stack' },
      card('How new accounts get their OU',
        h('p', { class: 'help' }, 'The OU of a new account, in this order: an OU someone chose for that person on an approval (kept until their department changes), a role rule that sets an OU (for exceptions such as contractors), the rules below, then the setting for values without a rule.'),
        inputCheck(pl, 'enabled', 'Place accounts by the field below', { 'data-test': 'placement-enabled' }),
        h('div', { class: 'grid grid-2' },
          field('Source field', inputText(pl, 'field', { required: true, placeholder: 'Department', 'data-test': 'placement-field' }), { help: 'The HR field that decides the OU, e.g. Department or Division.' }),
          field('When a value has no rule', inputSelect(pl, 'unmapped', [
            { value: 'hold', label: 'Wait in Approvals until someone chooses the OU (recommended)' },
            { value: 'default', label: 'Use the default OU (Settings -> Active Directory)' },
          ], { 'data-test': 'placement-unmapped' }))),
        inputCheck(pl, 'move_on_change', 'Move existing accounts when their value changes (a department move)', { 'data-test': 'placement-move' }),
        h('p', { class: 'help' }, 'Only a change of the value moves an account, so an OU someone sets by hand in AD is otherwise left alone. A title change alone never moves anyone.')),
      card('Rules',
        h('p', { class: 'help' }, 'One OU per value. Values are compared without regard to case; * matches anything (Sales* = every Sales department). Exact values win over * rules; the first matching * rule in the list is used.'),
        rulesEl,
        isAdmin ? h('div', { class: 'row-gap' },
          h('button', { type: 'button', class: 'btn', 'data-test': 'rule-add', onclick: () => { pl.rules.push({ match: '', ou: '' }); drawRules(); } }, 'Add rule'),
          h('button', { type: 'button', class: 'btn', onclick: () => importInput.click() }, 'Import CSV…'),
          importInput,
          h('button', { type: 'button', class: 'btn', onclick: exportRules }, 'Export CSV'),
          h('span', { class: 'help' }, ' CSV columns: department,ou')) : null),
      card(`${pl.field} values in use`, valuesEl))),
    status,
    isAdmin ? h('div', { class: 'savebar' }, h('span', { class: 'spacer' }), saveBtn) : null);

    replaceContent(body, form);
    drawRules();
    loadValues();
  }

  await load();
}
