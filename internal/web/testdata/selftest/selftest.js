// selftest.js - drives the real IDLifecycle UI in a (headless) browser and reports into <pre id="result">.
//
//   Served same-origin by the test server at /selftest/selftest.html?user=<admin>&pass=<password>
//   Optional query parameters:
//     csvpath=<path>   value used for the CSV source's file-path field (default "selftest.csv")
//     report=<url>     also POST "<result>\n<log>" (text/plain) to this same-origin URL when finished
//
// Writes "PASS" or "FAIL: <reason>" into <pre id="result"> (initially "RUNNING"); step progress goes to
// <pre id="log">. No inline script: this file is loaded as an external module, so it works under the
// product's strict CSP.

import { api, login as apiLogin } from '../api.js';
import '../app.js';

const params = new URLSearchParams(location.search);
const USER = params.get('user') || 'admin';
const PASS = params.get('pass') || '';
const CSV_PATH = params.get('csvpath') || 'selftest.csv';
const REPORT_URL = params.get('report') || '';
const resultEl = document.getElementById('result');
const logEl = document.getElementById('log');

let finished = false;
function finish(text) {
  if (finished) {
    return;
  }
  finished = true;
  resultEl.textContent = text;
  if (REPORT_URL) {
    // Optional: also POST "<result>\n<log>" so a test server can collect it without parsing the DOM.
    fetch(REPORT_URL, { method: 'POST', credentials: 'same-origin', body: `${text}\n${logEl.textContent}` }).catch(() => {});
  }
}
function fail(reason) {
  finish(`FAIL: ${reason}`);
}
function log(line) {
  logEl.textContent += `${line}\n`;
}

window.addEventListener('error', (ev) => fail(`uncaught error: ${ev.message}`));
window.addEventListener('unhandledrejection', (ev) => fail(`unhandled rejection: ${ev.reason && ev.reason.message ? ev.reason.message : ev.reason}`));
document.addEventListener('securitypolicyviolation', (ev) => fail(`CSP violation: ${ev.violatedDirective} blocked ${ev.blockedURI}`));

// ---------------------------------------------------------------- helpers

class Fatal extends Error {}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
const text = (el) => (el ? el.textContent.replace(/\s+/g, ' ').trim() : '');

function byText(sel, needle, root = document) {
  return $$(sel, root).find((el) => text(el).includes(needle));
}

async function waitFor(fn, what, timeout = 10000) {
  const end = Date.now() + timeout;
  let lastErr = null;
  for (;;) {
    try {
      const v = await fn();
      if (v) {
        return v;
      }
    }
    catch (e) {
      if (e instanceof Fatal) {
        throw e;
      }
      lastErr = e;
    }
    if (Date.now() > end) {
      throw new Error(`timed out waiting for ${what}${lastErr ? ` (${lastErr.message})` : ''}`);
    }
    await sleep(40);
  }
}

function setValue(el, value) {
  if (!el) {
    throw new Error('setValue: element not found');
  }
  el.value = value;
  el.dispatchEvent(new Event('input', { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
}

function topModal() {
  const all = $$('.modal');
  return all.length ? all[all.length - 1] : null;
}

function errorToast() {
  const t = $('.toast-error');
  return t ? text(t) : '';
}

function failOnErrorToast() {
  const msg = errorToast();
  if (msg) {
    throw new Fatal(`error toast shown: ${msg}`);
  }
}

async function go(hash, view) {
  location.hash = hash;
  await waitFor(() => $(`[data-view="${view}"]`) && !$('main .loading'), `view "${view}" to load`, 15000);
  failOnErrorToast();
}

async function submitModal(what) {
  const modal = topModal();
  if (!modal) {
    throw new Error(`${what}: no dialog is open`);
  }
  const submit = $('button[type="submit"]', modal);
  submit.click();
  await waitFor(() => {
    failOnErrorToast();
    return !topModal() || !modal.isConnected;
  }, `${what}: dialog to close after saving`, 15000);
}

async function uiLogin(user, pass) {
  await waitFor(() => $('[data-test="login-user"]'), 'the login form');
  setValue($('[data-test="login-user"]'), user);
  setValue($('[data-test="login-pass"]'), pass);
  $('[data-test="login-submit"]').click();
  await waitFor(() => {
    const err = $('.login-error .notice');
    if (err) {
      throw new Fatal(`login rejected: ${text(err)}`);
    }
    return $('[data-test="whoami"]');
  }, 'the app shell after login');
  if (!text($('[data-test="whoami"]')).includes(user)) {
    throw new Error(`shell does not show the signed-in user "${user}"`);
  }
}

async function uiLogout() {
  $('[data-test="logout"]').click();
  await waitFor(() => $('[data-test="login-user"]'), 'the login form after sign-out');
}

function rowWith(needle) {
  return $$('table.data tbody tr').find((tr) => text(tr).includes(needle));
}

async function confirmDanger(titleNeedle) {
  const modal = await waitFor(() => {
    const m = topModal();
    return m && text(m).includes(titleNeedle) ? m : null;
  }, `confirm dialog "${titleNeedle}"`);
  $('.btn-danger', modal).click();
}

const rand = Math.random().toString(36).slice(2, 8);

// ---------------------------------------------------------------- scenarios

async function scenarioLogin() {
  await uiLogin(USER, PASS);
  if ($$('.sidebar a[data-route]').length < 8) {
    throw new Error('navigation looks incomplete for an admin');
  }
}

async function scenarioDashboard() {
  await go('#/dashboard', 'dashboard');
  const card = await waitFor(() => $('[data-test="mode-card"]'), 'the mode card');
  const mode = text($('[data-test="mode-text"]', card));
  if (mode !== 'DRY RUN' && mode !== 'LIVE') {
    throw new Error(`unexpected mode text "${mode}"`);
  }
  if (!$('[data-test="scheduler-card"]')) {
    throw new Error('scheduler card missing');
  }
  const pending = text($('[data-test="pending-count"]'));
  if (!/^\d+$/.test(pending)) {
    throw new Error(`pending count is not a number: "${pending}"`);
  }
  if (!$('[data-test="run-now"]')) {
    throw new Error('"Run sync now" button missing for admin');
  }
  const banner = $('[data-test="dry-run-banner"]');
  await waitFor(() => (mode === 'DRY RUN' ? banner && !banner.hidden : banner && banner.hidden), 'dry-run banner to match the mode');
}

async function scenarioMapping() {
  const name = `selftest-map-${rand}`;
  await go('#/mappings', 'mappings');
  $('[data-test="new-mapping"]').click();
  await waitFor(() => topModal(), 'the mapping editor');
  setValue($('[data-test="map-name"]'), name);
  setValue($('[data-test="map-target"]'), 'description');
  setValue($('[data-test="map-template"]'), '{{ .FirstName }} selftest');
  setValue($('[data-test="map-mode"]'), 'immutable');
  await waitFor(() => {
    const out = $('[data-test="preview-out"] .preview-value');
    return out && text(out).includes('selftest');
  }, 'the live template preview to render', 8000);
  const help = text($('[data-test="map-mode"]').closest('.field'));
  if (!help.toLowerCase().includes('every run')) {
    throw new Error('mode explanation did not update for "immutable"');
  }
  await submitModal('mapping editor');
  const row = await waitFor(() => rowWith(name), 'the new mapping in the table');
  if (!text(row).includes('description') || !text(row).includes('immutable')) {
    throw new Error('mapping row does not show target attribute / mode');
  }
  // cleanup through the UI (also exercises delete + confirm)
  $('[data-test="delete-mapping"]', row).click();
  await confirmDanger('Delete mapping');
  await waitFor(() => !rowWith(name), 'the mapping to disappear after delete');
}

async function scenarioSource() {
  const name = `selftest-csv-${rand}`;
  await go('#/sources', 'sources');
  $('[data-test="new-source"]').click();
  await waitFor(() => topModal(), 'the source editor');
  const typeSel = $('[data-test="source-type"]');
  if (!$$('option', typeSel).some((o) => o.value === 'csv')) {
    throw new Error('source type "csv" is not offered by /api/source-types');
  }
  setValue(typeSel, 'csv');
  setValue($('[data-test="source-name"]'), name);
  await waitFor(() => $$('[data-test="source-config"] [data-field]').length > 0, 'generated config fields');
  for (const el of $$('[data-test="source-config"] [data-field]')) {
    if (!el.required || el.value !== '') {
      continue;
    }
    if (el.type === 'number') {
      setValue(el, '1');
    }
    else if (/path|file/i.test(el.dataset.field)) {
      setValue(el, CSV_PATH);
    }
    else {
      setValue(el, 'selftest');
    }
  }
  setValue($('[data-test="opt-extid"]'), 'EmployeeId');
  await submitModal('source editor');
  const row = await waitFor(() => rowWith(name), 'the new source in the table');
  if (!text(row).toLowerCase().includes('csv')) {
    throw new Error('source row does not show its type');
  }
  $('[data-test="delete-source"]', row).click();
  await confirmDanger('Delete source');
  await waitFor(() => !rowWith(name), 'the source to disappear after delete');
}

async function scenarioApprovals() {
  await go('#/approvals', 'approvals');
  const findApprove = () => $('[data-test="approve"]');
  if (!findApprove()) {
    // Nothing pending: try to create work by running a sync, then refresh a few times.
    try {
      await api.post('/runs', {});
    }
    catch (e) {
      log(`note: could not start a run (${e.message})`);
    }
    await waitFor(async () => {
      const refresh = byText('button', 'Refresh');
      if (refresh) {
        refresh.click();
      }
      await sleep(400);
      return findApprove();
    }, 'a pending change to approve (the test server must seed at least one pending change)', 30000);
  }
  const btn = findApprove();
  const id = btn.dataset.id;
  if (!id) {
    throw new Error('approve button carries no change id');
  }
  // expand the row and check the detail view
  const row = btn.closest('tr');
  const toggle = $('[data-test="row-toggle"]', row);
  toggle.click();
  const detail = row.nextElementSibling;
  await waitFor(() => detail && !detail.hidden && $('.summary-list, .change-detail .muted', detail) && $('pre.code', detail), 'the change detail (summary + payload)');
  // approve it
  btn.click();
  await waitFor(() => {
    failOnErrorToast();
    return !$(`[data-test="approve"][data-id="${id}"]`);
  }, 'the approved change to leave the pending list');
  const after = await waitFor(async () => {
    const c = await api.get(`/changes/${encodeURIComponent(id)}`);
    return c.status !== 'pending' ? c : null;
  }, 'the change status to flip away from pending', 10000);
  if (!['approved', 'applying', 'applied', 'failed', 'simulated'].includes(after.status)) {
    throw new Error(`unexpected status after approve: ${after.status}`);
  }
  log(`approved change ${id} -> ${after.status}`);
}

async function scenarioSettings() {
  await go('#/settings', 'settings');
  const dry = await waitFor(() => $('[data-test="set-dry-run"]'), 'the dry-run switch');
  if (dry.checked) {
    // Turning it off must ask for confirmation; cancel and verify nothing changed.
    dry.click();
    const modal = await waitFor(() => topModal(), 'the dry-run confirmation');
    if (!text(modal).toLowerCase().includes('dry run')) {
      throw new Error('confirmation does not mention dry run');
    }
    byText('button', 'Cancel', modal).click();
    await waitFor(() => !topModal(), 'the confirmation to close');
    if (!dry.checked) {
      throw new Error('dry run was switched off even though the confirmation was cancelled');
    }
  }
  const input = $('[data-test="set-max-destructive"]');
  const original = Number(input.value);
  const next = original === 37 ? 38 : 37;
  setValue(input, String(next));
  const save = async () => {
    $('[data-test="set-save"]').click();
    try {
      await waitFor(() => {
        failOnErrorToast();
        return byText('.toast', 'Settings saved');
      }, 'the "Settings saved" toast');
    }
    catch (e) {
      const invalid = $$('main :invalid').map((el) => el.dataset.test || el.id || el.name).join(', ');
      throw new Error(`${e.message}; invalid fields: ${invalid || 'none'}`);
    }
    $$('.toast').forEach((t) => t.remove());
  };
  await save();
  const stored = await api.get('/settings');
  if (stored.engine.max_destructive_per_run !== next) {
    throw new Error(`server stored ${stored.engine.max_destructive_per_run}, expected ${next}`);
  }
  await go('#/dashboard', 'dashboard');
  await go('#/settings', 'settings');
  const again = await waitFor(() => $('[data-test="set-max-destructive"]'), 'the settings form again');
  if (Number(again.value) !== next) {
    throw new Error(`form shows ${again.value} after reload, expected ${next}`);
  }
  // restore
  setValue(again, String(original));
  await save();
  const restored = await api.get('/settings');
  if (restored.engine.max_destructive_per_run !== original) {
    throw new Error('could not restore the original setting');
  }
}

async function scenarioViewer() {
  const username = `selftest_viewer_${rand}`;
  const password = `Selftest-${rand}-Xy9!pw`;
  const created = await api.post('/users', { username, role: 'viewer', password });
  let id = created && created.id;
  if (!id) {
    const list = await api.get('/users', { limit: 500 });
    const found = ((list && list.items) || []).find((u) => u.username === username);
    id = found && found.id;
  }
  try {
    await uiLogout();
    await uiLogin(username, password);
    if ($('.sidebar a[data-route="users"]')) {
      throw new Error('viewer can see the Users navigation entry');
    }
    const views = ['dashboard', 'approvals', 'runs', 'identities', 'audit', 'mappings', 'roles', 'sources', 'settings', 'tables'];
    for (const v of views) {
      await go(`#/${v}`, v);
      const mutating = $$('[data-mutating]');
      if (mutating.length > 0) {
        throw new Error(`viewer sees ${mutating.length} mutating control(s) on #/${v}: ${mutating.map((m) => text(m)).join(', ')}`);
      }
      if ($$('main .notice-error').length > 0) {
        throw new Error(`viewer gets an error on #/${v}: ${text($('main .notice-error'))}`);
      }
      if (v === 'settings') {
        if ($('[data-test="set-save"]')) {
          throw new Error('viewer sees the settings Save button');
        }
        if ($$('main fieldset[disabled]').length === 0) {
          throw new Error('settings form is not read-only for a viewer');
        }
      }
    }
    location.hash = '#/users';
    await waitFor(() => $('[data-test="forbidden"]'), 'the "not allowed" page for #/users');
  }
  finally {
    try {
      if ($('[data-test="logout"]')) {
        await uiLogout();
      }
      await apiLogin(USER, PASS);
      if (id) {
        await api.del(`/users/${encodeURIComponent(id)}`);
      }
    }
    catch (e) {
      log(`cleanup problem: ${e.message}`);
    }
  }
}

// ---------------------------------------------------------------- runner

const STEPS = [
  ['login', scenarioLogin],
  ['dashboard renders status', scenarioDashboard],
  ['create mapping via modal', scenarioMapping],
  ['create csv source', scenarioSource],
  ['approve a pending change', scenarioApprovals],
  ['settings save round-trip', scenarioSettings],
  ['viewer has no mutating controls', scenarioViewer],
];

async function main() {
  const watchdog = setTimeout(() => fail('selftest timed out (90s)'), 90000);
  for (const [name, fn] of STEPS) {
    if (finished) {
      return;
    }
    log(`> ${name}`);
    try {
      await fn();
    }
    catch (e) {
      fail(`${name}: ${e.message}`);
      return;
    }
    log(`  ok: ${name}`);
  }
  clearTimeout(watchdog);
  finish('PASS');
}

main();
