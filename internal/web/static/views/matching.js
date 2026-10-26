// Matching (correlation): how people from the sources are matched to AD accounts that already exist - contractors who
// become employees, rehires, and the first rollout into an existing directory. Settings live in settings.correlation;
// the report is written by every sync.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, card, errorBox, loading, field, inputText, inputNumber, inputCheck, inputSelect,
  act, toast, notice, isObject, dataTable, pill, fmtTime, relTime, linesToList, listToLines, inputArea,
} from '../ui.js';
import { COMPARE_OPTIONS, identityPicker } from '../components.js';

const RENAME_OPTIONS = [
  { value: 'transitions', label: 'When the account moves to another person record (contractor to employee) - recommended' },
  { value: 'always', label: 'Always, when the naming templates give a different name' },
  { value: 'never', label: 'Never: keep the existing names' },
];

const AD_ATTRS = ['givenName', 'sn', 'displayName', 'department', 'title', 'company', 'mail', 'employeeID', 'employeeNumber',
  'telephoneNumber', 'mobile', 'physicalDeliveryOfficeName', 'manager', 'userPrincipalName', 'sAMAccountName'];

const SOURCE_FIELDS = ['FirstName', 'LastName', 'PreferredName', 'Department', 'Title', 'Company', 'Email', 'WorkEmail', 'Phone', 'Location', 'EmployeeID'];

function defaultRules() {
  return [
    { field: 'FirstName', ad_attr: 'givenName', compare: 'name', required: true },
    { field: 'LastName', ad_attr: 'sn', compare: 'name', required: true },
    { field: 'Department', ad_attr: 'department', compare: 'ci', required: false },
  ];
}

export async function render(root, ctx) {
  const isAdmin = ctx.can('admin');
  const canLink = ctx.can('approver');
  const settingsHost = h('div', null, loading());
  const reportHost = h('div', null, loading());
  root.append(pageHeader('Matching', h('button', { type: 'button', class: 'btn', onclick: () => { loadSettings(); loadReport(); } }, 'Refresh')),
    h('p', { class: 'help' }, 'Before creating a new account, the sync looks for an existing AD account that is the same person: someone who was a contractor (or entered under Add person) and now appears in HR, a rehire, or - on the first rollout - every account that has no employee ID yet. A match on every rule below links the account (and can rename it); a match on only the required rules waits in Approvals for a person to confirm.'),
    h('datalist', { id: 'm-ad-attrs' }, AD_ATTRS.map((a) => h('option', { value: a }))),
    h('datalist', { id: 'm-src-fields' }, SOURCE_FIELDS.map((a) => h('option', { value: a }))),
    settingsHost, reportHost);

  let doc = null;

  async function loadSettings() {
    try {
      doc = await api.get('/settings');
    }
    catch (e) {
      replaceContent(settingsHost, errorBox(e));
      return;
    }
    drawSettings(isObject(doc) ? doc : {});
  }

  function drawSettings(d) {
    if (!isObject(d.correlation)) {
      d.correlation = {};
    }
    const c = d.correlation;
    if (!Array.isArray(c.rules) || c.rules.length === 0) {
      c.rules = defaultRules();
    }
    if (!Array.isArray(c.ignore_accounts)) {
      c.ignore_accounts = [];
    }
    c.rename = c.rename || 'transitions';
    if (c.max_auto_links_per_run === undefined) {
      c.max_auto_links_per_run = 25;
    }
    if (c.report_uncorrelated === undefined) {
      c.report_uncorrelated = true;
    }
    const view = { ignore: listToLines(c.ignore_accounts) };
    const rulesEl = h('div', { class: 'stack', 'data-test': 'm-rules' });
    const drawRules = () => {
      replaceContent(rulesEl, c.rules.map((r, i) => h('div', { class: 'rule-row', 'data-test': 'm-rule' },
        field('Source field', inputText(r, 'field', { required: true, list: 'm-src-fields', 'data-test': 'm-rule-field' })),
        field('AD attribute', inputText(r, 'ad_attr', { required: true, list: 'm-ad-attrs', 'data-test': 'm-rule-attr' })),
        field('Compare', inputSelect(r, 'compare', COMPARE_OPTIONS, { 'data-test': 'm-rule-compare' })),
        inputCheck(r, 'required', 'Required', { 'data-test': 'm-rule-required' }),
        isAdmin ? h('button', {
          type: 'button', class: 'btn btn-sm btn-danger', 'aria-label': `Remove the rule for ${r.field || 'this field'}`,
          onclick: () => { c.rules.splice(i, 1); drawRules(); },
        }, 'Remove') : null)));
    };
    const saveBtn = h('button', { type: 'submit', class: 'btn btn-primary', 'data-mutating': '', 'data-test': 'm-save' }, 'Save');
    const form = h('form', {
      class: 'stack',
      onsubmit: (ev) => {
        ev.preventDefault();
        act(saveBtn, async () => {
          c.ignore_accounts = linesToList(view.ignore);
          if (c.max_auto_links_per_run === '') {
            delete c.max_auto_links_per_run;
          }
          if (!c.rules.some((r) => r.required)) {
            toast('Mark at least one rule as required, so a single matching attribute can never link an account.', 'error');
            return;
          }
          const saved = await api.put('/settings', d);
          toast('Matching settings saved.', 'ok');
          doc = isObject(saved) ? saved : d;
          drawSettings(doc);
          loadReport();
        });
      },
    },
    isAdmin ? null : notice('info', 'Read-only: only administrators can change the matching rules.'),
    h('fieldset', { class: 'plain', disabled: !isAdmin }, h('div', { class: 'stack' },
      card('Matching',
        inputCheck(c, 'enabled', 'Look for an existing AD account before creating a new one', { 'data-test': 'm-enabled' }),
        h('div', { class: 'grid grid-2' },
          field('Rename a linked account', inputSelect(c, 'rename', RENAME_OPTIONS, { 'data-test': 'm-rename' }),
            { help: 'The new name comes from the naming templates and must be free. "Never" is the safe choice for the first rollout into an existing directory.' }),
          field('Most automatic links per run', inputNumber(c, 'max_auto_links_per_run', { min: '0', step: '1', 'data-test': 'm-max-links' }),
            { help: 'When one sync would link more accounts than this, every link waits for approval (a safety net against a loose rule). 0 = no limit.' })),
        inputCheck(c, 'report_uncorrelated', 'Report AD accounts that match no person (and notify when new ones appear)', { 'data-test': 'm-report' }),
        h('p', { class: 'help' }, 'Full matches follow the "Link" approval policy (Settings); partial matches and choices between several accounts always wait for a person.')),
      card('Rules',
        h('p', { class: 'help' }, 'Each rule compares a source field with an AD attribute. A rule only counts when both values are filled in. An account is a candidate when every required rule matches; it is a full match when every rule matches.'),
        h('ul', { class: 'help' }, COMPARE_OPTIONS.map((o) => h('li', null, o.label))),
        rulesEl,
        isAdmin ? h('div', { class: 'row-gap' },
          h('button', { type: 'button', class: 'btn', 'data-test': 'm-rule-add', onclick: () => { c.rules.push({ field: '', ad_attr: '', compare: 'ci', required: false }); drawRules(); } }, 'Add rule'),
          h('button', { type: 'button', class: 'btn', onclick: () => { c.rules = defaultRules(); drawRules(); } }, 'Reset to first name + last name + department')) : null),
      card('Accounts never matched',
        field('Ignore these accounts (one per line)', inputArea(view, 'ignore', { rows: '4', class: 'mono', placeholder: 'svc_*\nadmin*\nOU=Service Accounts,DC=corp,DC=example,DC=com', 'data-test': 'm-ignore' }),
          { help: 'sAMAccountName patterns (* = anything) or an OU distinguished name: service, admin and shared accounts that no HR record will ever match. They are left out of matching and of the report.' })))),
    isAdmin ? h('div', { class: 'savebar' }, h('span', { class: 'spacer' }), saveBtn) : null);
    replaceContent(settingsHost, form);
    drawRules();
  }

  async function ignoreAccount(acct, button) {
    await act(button, async () => {
      const fresh = await api.get('/settings');
      if (!isObject(fresh.correlation)) {
        fresh.correlation = {};
      }
      const list = Array.isArray(fresh.correlation.ignore_accounts) ? fresh.correlation.ignore_accounts : [];
      if (!list.some((x) => String(x).toLowerCase() === String(acct.sam).toLowerCase())) {
        list.push(acct.sam);
      }
      fresh.correlation.ignore_accounts = list;
      doc = await api.put('/settings', fresh);
      toast(`${acct.sam} is ignored from now on.`, 'ok');
      drawSettings(isObject(doc) ? doc : fresh);
      loadReport(acct.object_guid);
    });
  }

  async function linkAccount(acct) {
    const who = await identityPicker({
      title: `Link ${acct.sam || 'this account'} to a person`,
      note: 'Choose the person this account belongs to. The next sync proposes the link (it sets the employee ID on the account); approve it under Approvals.',
      state: 'new',
    });
    if (!who) {
      return;
    }
    await act(null, async () => {
      await api.post(`/identities/${encodeURIComponent(who.id)}/link`, { object_guid: acct.object_guid });
      toast(`The next sync proposes linking ${acct.sam} to ${who.display || who.external_id}.`, 'ok');
    });
  }

  async function loadReport(hideGuid) {
    let rep;
    try {
      rep = await api.get('/correlation/report');
    }
    catch (e) {
      replaceContent(reportHost, card('Report', errorBox(e)));
      return;
    }
    rep = isObject(rep) ? rep : {};
    const unc = (Array.isArray(rep.uncorrelated) ? rep.uncorrelated : []).filter((a) => a.object_guid !== hideGuid);
    const verify = Array.isArray(rep.verify) ? rep.verify : [];
    const needsName = Array.isArray(rep.needs_name) ? rep.needs_name : [];
    const head = rep.generated_at
      ? h('p', { class: 'help' }, `From the sync ${relTime(rep.generated_at) || fmtTime(rep.generated_at)}`, rep.run_id ? [' (', h('a', { href: `#/runs/${encodeURIComponent(rep.run_id)}` }, 'run'), ')'] : null,
        `. ${Number(rep.unmatched_people) || 0} person(s) had no account and no match.`)
      : h('p', { class: 'muted' }, 'No sync has written a report yet.');
    replaceContent(reportHost, h('div', { class: 'stack', 'data-test': 'm-report-view' },
      rep.enabled === false ? notice('info', 'Matching is off: turn it on above to look for existing accounts.') : null,
      card('Matches to check',
        head,
        verify.length === 0 ? h('p', { class: 'muted' }, 'No match is waiting for a person.')
          : dataTable([
            { label: 'Person', render: (v) => h('div', null, v.display || v.external_id, h('div', { class: 'muted small' }, v.external_id)) },
            { label: 'Candidates', render: (v) => String(v.candidates || 0) },
            { label: '', render: (v) => h('a', { href: `#/approvals?type=link&q=${encodeURIComponent(v.external_id || '')}`, 'data-test': 'm-verify-link' }, 'Open in Approvals') },
          ], verify, { caption: 'Link changes waiting for a person' }),
        needsName.length === 0 ? null : h('div', { class: 'stack' },
          h('h3', null, 'Usernames to choose'),
          dataTable([
            { label: 'Person', render: (v) => h('div', null, v.display || v.external_id, h('div', { class: 'muted small' }, v.external_id)) },
            { label: '', render: (v) => h('a', { href: `#/approvals?q=${encodeURIComponent(v.external_id || '')}` }, 'Set the username in Approvals') },
          ], needsName, { caption: 'New accounts waiting for a username' }))),
      card(`AD accounts that match no person (${unc.length})`,
        h('p', { class: 'help' }, 'Accounts under the search bases that no source person is linked to or matched with. Link the ones that belong to someone; ignore service and shared accounts.'),
        dataTable([
          { label: 'Account', render: (a) => h('div', null, h('strong', null, a.sam || ''), a.display ? h('div', null, a.display) : null, h('div', { class: 'mono small wrap' }, a.dn || '')) },
          { label: 'ID attribute', render: (a) => (a.external_id ? h('code', null, a.external_id) : h('span', { class: 'muted' }, 'empty')) },
          { label: 'Enabled', render: (a) => pill(a.enabled ? 'enabled' : 'disabled') },
          {
            label: 'Actions',
            className: 'col-actions',
            render: (a) => {
              const cell = h('div', { class: 'row-actions' });
              if (canLink) {
                cell.append(h('button', { type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'm-link', onclick: () => linkAccount(a) }, 'Link to person…'));
              }
              if (isAdmin && a.sam) {
                cell.append(h('button', { type: 'button', class: 'btn btn-sm', 'data-mutating': '', 'data-test': 'm-ignore-btn', onclick: (e) => ignoreAccount(a, e.currentTarget) }, 'Ignore'));
              }
              return cell;
            },
          },
        ], unc, { caption: 'Uncorrelated AD accounts', wrapClass: 'unc-table', empty: rep.generated_at ? 'Every account matches a person (or is ignored).' : 'Run a sync with matching on to fill this list.' }))));
  }

  await Promise.all([loadSettings(), loadReport()]);
}
