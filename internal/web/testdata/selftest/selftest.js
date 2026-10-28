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

// The brand comes from /branding/brand.json: the default in the real E2E, a custom one from the mock API.
async function scenarioBranding() {
  const brand = await (await fetch('/branding/brand.json', { cache: 'no-cache' })).json();
  const name = brand.product_name;
  if (!name) {
    throw new Error('brand.json has no product_name');
  }
  if (window.__IDL_MOCK && (name !== 'Acme Identity (mock)' || !brand.custom)) {
    throw new Error(`the mock serves a custom brand, got ${JSON.stringify(brand)}`);
  }
  if (!window.__IDL_MOCK && (name !== 'IDLifecycle' || brand.custom)) {
    throw new Error(`the real server serves the default brand, got ${JSON.stringify(brand)}`);
  }
  const title = await waitFor(() => $('[data-test="login-title"]'), 'the login heading');
  if (text(title) !== name) {
    throw new Error(`login heading "${text(title)}", want "${name}"`);
  }
  const logo = $('.login-card [data-test="brand-logo"]');
  if (!logo || !logo.getAttribute('src').endsWith('/branding/logo')) {
    throw new Error('login logo missing');
  }
  if (!window.__IDL_MOCK) {
    await waitFor(() => logo.complete && logo.naturalWidth > 0, 'the logo image to load');
    const r = await fetch('/branding/logo');
    if (r.headers.get('Content-Type') !== 'image/svg+xml' || !(r.headers.get('Content-Security-Policy') || '').includes('sandbox')) {
      throw new Error(`logo headers: ${r.headers.get('Content-Type')} / ${r.headers.get('Content-Security-Policy')}`);
    }
    const ico = await fetch('/branding/favicon.ico');
    if (!ico.ok || ico.headers.get('Content-Type') !== 'image/x-icon') {
      throw new Error('favicon not served');
    }
  }
  const icon = $('link[rel="icon"]');
  if (icon && !icon.getAttribute('href').startsWith('/branding/favicon.ico')) {
    throw new Error('favicon link');
  }
  if (!document.title.includes(name)) {
    throw new Error(`document.title "${document.title}" lacks "${name}"`);
  }
  if (brand.accent) {
    const accent = getComputedStyle(document.documentElement).getPropertyValue('--accent').trim().toLowerCase();
    if (accent !== brand.accent.toLowerCase()) {
      throw new Error(`accent ${accent}, want ${brand.accent}`);
    }
  }
}

async function scenarioLogin() {
  await uiLogin(USER, PASS);
  if ($$('.sidebar a[data-route]').length < 8) {
    throw new Error('navigation looks incomplete for an admin');
  }
  const brandName = text($('[data-test="brand"] .brand-name'));
  if (!brandName || !document.title.endsWith(brandName)) {
    throw new Error(`top bar product name "${brandName}" / title "${document.title}"`);
  }
  if (!$('[data-test="brand"] [data-test="brand-logo"]')) {
    throw new Error('top bar logo missing');
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

// Field mapper on the mappings page: needs the server's seeded csv source (EmployeeID, FirstName, LastName).
async function scenarioFieldMapper() {
  const missingName = `selftest-missing-${rand}`;
  const mapName = `selftest-ln-${rand}`;
  const created = [];
  const missing = await api.post('/mappings', { name: missingName, target_attr: 'department', template: '{{ .Department }}', mode: 'sync', on_empty: 'skip', value_type: 'string', enabled: false });
  created.push(missing.id);
  try {
    await go('#/mappings', 'mappings');
    const panel = await waitFor(() => $('[data-test="field-mapper"]'), 'the field mapper panel');
    await waitFor(() => $('[data-test="mapper-attr"][data-name="sn"] [data-test="map-attr"]'), 'the AD attribute list with a mappable "sn"');
    if ($('[data-test="mapper-attr"][data-name="sAMAccountName"]')) {
      throw new Error('unmappable attributes must be hidden until "show attributes that cannot be mapped" is ticked');
    }
    setValue($('[data-test="mapper-ad-filter"]'), 'surname');
    await waitFor(() => $$('[data-test="mapper-attr"]').length === 1 && $('[data-test="mapper-attr"][data-name="sn"]'), 'the filter to narrow the list to "sn"');
    setValue($('[data-test="mapper-ad-filter"]'), '');

    const sel = $('[data-test="mapper-source"]', panel);
    const opt = await waitFor(() => $$('option', sel).find((o) => text(o).startsWith('seed ')), 'the seeded source in the mapper');
    setValue(sel, opt.value);
    $('[data-test="mapper-load"]', panel).click();
    const first = await waitFor(() => $('[data-test="mapper-field"][data-name="FirstName"]'), 'the source fields');
    if (!text(first).includes('Jane') || !text(first).includes('100% filled')) {
      throw new Error(`FirstName row does not show samples / fill rate: ${text(first)}`);
    }
    if (!text($('[data-test="mapper-field"][data-name="EmployeeID"]')).includes('link key')) {
      throw new Error('EmployeeID is not marked as the link key');
    }
    const problem = await waitFor(() => $$('[data-test="mapper-problem"]').find((p) => text(p).includes(missingName)), 'a problem for the mapping that reads a missing field');
    if (!text(problem).includes('Department')) {
      throw new Error(`problem text does not name the missing field: ${text(problem)}`);
    }
    const last = $('[data-test="mapper-field"][data-name="LastName"]');
    if (!$('[data-test="not-mapped"]', last)) {
      throw new Error('LastName should show as not used by any mapping');
    }

    // Map… from the source side pre-fills the template.
    $('[data-test="map-field"]', last).click();
    const modal = await waitFor(() => topModal(), 'the mapping editor from Map…');
    const tpl = $('[data-test="map-template"]', modal);
    if (tpl.value !== '{{ .LastName }}') {
      throw new Error(`template not pre-filled: "${tpl.value}"`);
    }
    const target = $('[data-test="map-target"]', modal);
    const list = target.getAttribute('list') && document.getElementById(target.getAttribute('list'));
    if (!list || !$$('option', list).some((o) => o.value === 'sn')) {
      throw new Error('the target attribute has no AD attribute suggestions');
    }
    setValue(target, 'sAMAccountName');
    await waitFor(() => { const w = $('[data-test="map-target-warn"]', modal); return w && !w.hidden && text(w).includes('cannot be mapped'); }, 'a warning for an unmappable target');
    setValue(target, 'sn');
    await waitFor(() => $('[data-test="map-target-warn"]', modal).hidden, 'the warning to clear for "sn"');
    tpl.setSelectionRange(tpl.value.length, tpl.value.length);
    $('[data-test="insert-field"][data-name="FirstName"]', modal).click();
    if (!tpl.value.endsWith('{{ .FirstName }}')) {
      throw new Error(`insert field did not add to the template: "${tpl.value}"`);
    }
    setValue(tpl, '{{ .LastName }}');
    setValue($('[data-test="map-name"]', modal), mapName);
    await submitModal('mapping editor (from the field mapper)');
    await waitFor(() => text($('[data-test="mapper-field"][data-name="LastName"]')).includes(`${mapName} → sn`), 'LastName to show the new mapping');
    await waitFor(() => text($('[data-test="mapper-attr"][data-name="sn"]')).includes(mapName), 'the sn attribute to show the new mapping');

    // Map… from the AD side pre-fills the attribute.
    $('[data-test="mapper-attr"][data-name="title"] [data-test="map-attr"]').click();
    const modal2 = await waitFor(() => topModal(), 'the mapping editor from the AD side');
    if ($('[data-test="map-target"]', modal2).value !== 'title') {
      throw new Error('target attribute not pre-filled from the AD side');
    }
    byText('.modal-footer button, button', 'Cancel', modal2).click();
    await waitFor(() => !modal2.isConnected, 'the editor to close on cancel');
  }
  finally {
    const all = listItems(await api.get('/mappings', { limit: 500 }));
    for (const m of all) {
      if (m.name === mapName || created.includes(m.id)) {
        await api.del(`/mappings/${encodeURIComponent(m.id)}`);
      }
    }
  }
}

function listItems(resp) {
  return (resp && Array.isArray(resp.items)) ? resp.items : [];
}

// Sign-in options: the Settings card, then the login page with Entra ID on and password sign-in limited to
// admins. Nothing is redirected (no identity provider here); settings are restored afterwards.
async function scenarioSignInOptions() {
  const before = await api.get('/settings');
  try {
    await go('#/settings', 'settings');
    await waitFor(() => $('[data-test="set-oidc-enabled"]') && $('[data-test="set-local-login"]'), 'the Sign-in card');
    $('[data-test="set-oidc-redirect-here"]').click();
    if ($('[data-test="set-oidc-redirect"]').value !== `${location.origin}/api/auth/oidc/callback`) {
      throw new Error('"Use this server\'s address" did not fill the redirect URI');
    }
    const doc = await api.get('/settings');
    doc.auth = {
      local_login: 'admins_only',
      oidc: {
        enabled: true, tenant_id: '11111111-2222-3333-4444-555555555555', client_id: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
        client_secret: 'selftest-secret', redirect_url: 'http://localhost/api/auth/oidc/callback', button_label: 'Sign in with Contoso',
        admin_values: ['IDLifecycle.Admin'], approver_values: [], viewer_values: [],
      },
    };
    const saved = await api.put('/settings', doc);
    if (saved.auth.oidc.client_secret !== '********') {
      throw new Error('the saved settings must return the client secret masked');
    }
    await uiLogout();
    const btn = await waitFor(() => $('[data-test="login-oidc"]'), 'the Sign in with Microsoft button');
    if (text(btn) !== 'Sign in with Contoso' || !btn.getAttribute('href').endsWith('/api/auth/oidc/start')) {
      throw new Error(`unexpected SSO button: "${text(btn)}" -> ${btn.getAttribute('href')}`);
    }
    if (!text($('[data-test="login-local-note"]')).includes('administrators only')) {
      throw new Error('the break-glass note is missing when password sign-in is admins-only');
    }
    await uiLogin(USER, PASS); // a local admin may still sign in with a password
  }
  finally {
    if (!$('[data-test="whoami"]')) {
      await apiLogin(USER, PASS);
    }
    const cur = await api.get('/settings');
    cur.auth = { ...before.auth, oidc: { ...before.auth.oidc, client_secret: '' } };
    await api.put('/settings', cur);
    location.hash = '#/dashboard';
  }
  await uiLogout();
  await waitFor(() => $('[data-test="login-user"]') && $('[data-test="login-sso"]') && $('[data-test="login-sso"]').hidden && !$('[data-test="login-oidc"]'), 'the login page without the SSO button once Entra ID sign-in is off', 5000);
  await uiLogin(USER, PASS);
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
  // The HR system API form: select fields, and fields that only show for the chosen authentication.
  if ($$('option', typeSel).some((o) => o.value === 'rest')) {
    setValue(typeSel, 'rest');
    const authSel = await waitFor(() => $('[data-test="source-config"] select[data-field="auth"]'), 'the authentication select');
    const keyField = () => $('[data-test="source-config"] [data-field="api_key"]');
    await waitFor(() => keyField() && keyField().closest('.field').hidden, 'the API key field hidden while auth is none');
    setValue(authSel, 'api_key');
    await waitFor(() => !keyField().closest('.field').hidden, 'the API key field shown for api_key authentication');
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
  setValue($('[data-test="opt-extid"]'), 'EmployeeID');
  // Test the settings before saving them.
  $('[data-test="source-test-form"]').click();
  const result = await waitFor(() => $('[data-test="test-result"]'), 'the test result of the unsaved settings');
  if (!text(result).includes('Connection OK')) {
    throw new Error(`testing the unsaved settings failed: ${text(result)}`);
  }
  $('[data-test="test-close"]').click();
  await waitFor(() => !$('[data-test="test-result"]'), 'the test result to close');
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
  const findApprove = () => $('[data-test="approve"]:not([disabled])');
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

  // Initial password from a template: the template box and its complexity check appear, and "must change at next
  // logon" is forced on. Not saved; reloading the page discards it.
  const mode = await waitFor(() => $('[data-test="set-pw-mode"]'), 'the initial password mode');
  mode.value = 'template';
  mode.dispatchEvent(new Event('change', { bubbles: true }));
  const tpl = await waitFor(() => $('[data-test="set-pw-template"]'), 'the password template field');
  if (tpl.closest('[hidden]')) {
    throw new Error('the password template field stays hidden in template mode');
  }
  const must = $('[data-test="set-must-change"]');
  if (!must.checked || !must.disabled) {
    throw new Error('template mode must force "must change password at next logon"');
  }
  setValue(tpl, '{{ .FirstName }}');
  await waitFor(() => {
    const c = $('[data-test="preview-check"]');
    return c && text(c).includes('at least 8');
  }, 'the password preview to report a too-short password');
  await waitFor(() => $('[data-test="backup-list"]') && $('[data-test="backup-now"]'), 'the Backups card');
  if (!$('[data-test="set-mail-template"]') || !$('[data-test="set-mail-domain"]')) {
    throw new Error('the mail template fields are missing');
  }
  await go('#/dashboard', 'dashboard');
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
    const views = ['dashboard', 'approvals', 'runs', 'identities', 'manual', 'audit', 'mappings', 'roles', 'lifecycle', 'placement', 'matching', 'sources', 'settings', 'tables'];
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
    if ($('.sidebar a[data-route="preview"]')) {
      throw new Error('viewer can see the Preview changes navigation entry');
    }
    location.hash = '#/preview';
    await waitFor(() => $('[data-test="forbidden"]'), 'the "not allowed" page for #/preview');
    location.hash = '#/dashboard';
    await waitFor(() => !$('[data-test="forbidden"]'), 'leaving the "not allowed" page');
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

function key(el, k) {
  el.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, cancelable: true }));
}

// Role editor: live AD group search (GG-All is seeded in the fake AD of the real E2E), a nested condition group, the
// explain view of "Test this rule", the impact preview of the unsaved rule, and the nested rule saved and reloaded.
async function scenarioRoleRule() {
  const name = `selftest-role-${rand}`;
  await go('#/roles', 'roles');
  $('[data-test="new-role"]').click();
  const modal = await waitFor(() => topModal(), 'the role editor');
  setValue($('[data-test="role-name"]', modal), name);

  // Group picker: type, arrow down, Enter -> chip.
  const input = $('[data-test="role-groups-input"]', modal);
  input.focus();
  setValue(input, 'GG-Al');
  const option = await waitFor(() => $$('[data-test="role-groups-option"]', modal).find((o) => text($('.gp-opt-name', o)) === 'GG-All'), 'GG-All in the AD group search results');
  if (input.getAttribute('aria-expanded') !== 'true') {
    throw new Error('the group combobox does not report aria-expanded=true with results open');
  }
  const wanted = $$('[data-test="role-groups-option"]', modal).indexOf(option);
  for (let i = 0; i <= wanted; i += 1) {
    key(input, 'ArrowDown');
  }
  if (input.getAttribute('aria-activedescendant') !== option.id) {
    throw new Error('arrow keys did not move the active option to GG-All');
  }
  key(input, 'Enter');
  // The fake AD of the real E2E reports sAMAccountNames in lower case.
  await waitFor(() => $$('[data-test="role-groups-chip"]', modal).some((c) => text(c).toLowerCase().startsWith('gg-all')), 'the GG-All chip');
  if (!topModal() || topModal() !== modal) {
    throw new Error('Enter in the group picker closed or submitted the editor');
  }
  // Escape closes the result list, not the dialog.
  setValue(input, 'GG-Al');
  await waitFor(() => input.getAttribute('aria-expanded') === 'true', 'the result list to reopen');
  key(input, 'Escape');
  if (input.getAttribute('aria-expanded') !== 'false' || topModal() !== modal) {
    throw new Error('Escape did not just close the group result list');
  }
  // Free text: a group AD search does not return can still be added.
  setValue(input, `GG-Selftest-${rand}`);
  key(input, 'Enter');
  await waitFor(() => $$('[data-test="role-groups-chip"]', modal).length === 2, 'the typed group chip');

  // Conditions: EmployeeId has a value AND (Department = Finance OR (Status is not T)).
  $('[data-test="add-condition"]', modal).click();
  setValue($$('[data-test="cond-attr"]', modal)[0], 'EmployeeId');
  setValue($$('[data-test="cond-op"]', modal)[0], 'exists');
  $('[data-test="add-group"]', modal).click();
  // The builder redraws on structural changes, so the group is looked up again each time.
  const group = () => $('[data-test="cond-group"]', modal);
  await waitFor(group, 'a condition group');
  setValue($('[data-test="cond-attr"]', group()), 'Department');
  setValue($('[data-test="cond-value"]', group()), 'Finance');
  $('[data-test="add-condition-nested"]', group()).click();
  await waitFor(() => $$('[data-test="cond-attr"]', group()).length === 2, 'a second condition in the group');
  setValue($$('[data-test="cond-attr"]', group())[1], 'Status');
  setValue($$('[data-test="cond-op"]', group())[1], 'ne');
  setValue($$('[data-test="cond-value"]', group())[1], 'T');

  // Test this rule: explain rows with text results.
  setValue($$('[data-test="sample-json"]', modal)[0], JSON.stringify({ EmployeeId: 'E1', Department: 'Sales', Status: 'A' }));
  $('[data-test="rule-test-run"]', modal).click();
  await waitFor(() => {
    failOnErrorToast();
    return $$('[data-test="explain-result"]', modal).length >= 4;
  }, 'the explain table');
  const results = $$('[data-test="explain-result"]', modal).map(text);
  if (!results.some((r) => r.includes('does not match')) || !results.some((r) => /✓ ?matches/.test(r))) {
    throw new Error(`explain results do not say which conditions matched: ${results.join(' | ')}`);
  }
  if (!text($('[data-test="rule-verdict"]', modal)).includes('MATCHES')) {
    throw new Error(`the rule should match the sample record: ${text($('[data-test="rule-verdict"]', modal))}`);
  }

  // Preview impact of the unsaved rule (stacked dialog).
  $('[data-test="role-impact"]', modal).click();
  const impactModal = await waitFor(() => (topModal() !== modal ? topModal() : null), 'the impact preview dialog');
  await waitFor(() => text($('[data-test="impact-count"]', impactModal)) || $('.notice-warn', impactModal), 'the impact preview to load', 20000);
  failOnErrorToast();
  if ($('main .notice-error', impactModal) || $('.notice-error', impactModal)) {
    throw new Error(`impact preview failed: ${text($('.notice-error', impactModal))}`);
  }
  // Whoever the new rule changes gets its groups.
  const changed = Number(text($('[data-test="impact-changed"] strong', impactModal)) || 0);
  if (changed > 0 && text($('[data-test="impact-groups-added"]', impactModal)) === 'none') {
    throw new Error('the impact preview of the new rule changes people but adds no group');
  }
  byText('button', 'Close', impactModal).click();
  await waitFor(() => topModal() === modal, 'the impact dialog to close');

  await submitModal('role editor');
  const list = await api.get('/roles', { limit: 500 });
  const saved = listItems(list).find((r) => r.name === name);
  if (!saved) {
    throw new Error('the new role was not saved');
  }
  try {
    const g = (saved.conditions || [])[1];
    if (!g || g.op !== 'any_of' || !Array.isArray(g.conditions) || g.conditions.length !== 2 || g.conditions[1].attr !== 'Status') {
      throw new Error(`nested condition not stored as sent: ${JSON.stringify(saved.conditions)}`);
    }
    if (!(saved.grants.groups || []).some((g) => g.toLowerCase() === 'gg-all')) {
      throw new Error(`picked group not stored: ${JSON.stringify(saved.grants.groups)}`);
    }
    // Reload and reopen: the group and its conditions come back.
    await go('#/dashboard', 'dashboard');
    await go('#/roles', 'roles');
    const row = await waitFor(() => rowWith(name), 'the new role in the table');
    if (!text(row).includes('OR')) {
      throw new Error(`the list does not summarize the nested group: ${text(row)}`);
    }
    $('[data-test="edit-role"]', row).click();
    const again = await waitFor(() => topModal(), 'the role editor again');
    const g2 = await waitFor(() => $('[data-test="cond-group"]', again), 'the condition group after reload');
    if ($$('[data-test="cond-attr"]', g2).map((el) => el.value).join() !== 'Department,Status') {
      throw new Error('the nested conditions did not reload');
    }
    if ($$('[data-test="role-groups-chip"]', again).length !== 2) {
      throw new Error('the group chips did not reload');
    }
    byText('button', 'Cancel', again).click();
    await waitFor(() => !topModal(), 'the editor to close');
  }
  finally {
    await api.del(`/roles/${encodeURIComponent(saved.id)}`);
  }
}

// Lifecycle groups: add a group for disabled accounts (typed, no AD search needed), switch the removal mode, save,
// check what the server stored, then put the original block back.
async function scenarioLifecycle() {
  const before = await api.get('/settings');
  const original = JSON.parse(JSON.stringify(before.lifecycle_groups || {}));
  const group = `GG-Selftest-Disabled-${rand}`;
  await go('#/lifecycle', 'lifecycle');
  const input = await waitFor(() => $('[data-test="lifecycle-disabled-input"]'), 'the disabled-accounts group picker');
  setValue(input, group);
  key(input, 'Enter');
  await waitFor(() => $$('[data-test="lifecycle-disabled-chip"]').some((c) => text(c).startsWith(group)), 'the typed group chip');
  $('[data-test="lifecycle-remove-managed"]').click();
  await waitFor(() => !$('[data-test="lifecycle-keep-input"]'), 'the keep list to hide in "managed" mode');
  $('[data-test="lifecycle-save"]').click();
  await waitFor(() => {
    failOnErrorToast();
    return byText('.toast', 'Lifecycle groups saved');
  }, 'the "Lifecycle groups saved" toast');
  $$('.toast').forEach((t) => t.remove());
  try {
    const stored = (await api.get('/settings')).lifecycle_groups || {};
    if (!(stored.disabled_add || []).includes(group) || stored.disabled_remove !== 'managed') {
      throw new Error(`server stored ${JSON.stringify(stored)}`);
    }
    await go('#/dashboard', 'dashboard');
    await go('#/lifecycle', 'lifecycle');
    await waitFor(() => $$('[data-test="lifecycle-disabled-chip"]').some((c) => text(c).startsWith(group)), 'the saved group after reload');
    if (!$('[data-test="lifecycle-remove-managed"]').checked) {
      throw new Error('the removal mode did not reload');
    }
  }
  finally {
    const doc = await api.get('/settings');
    doc.lifecycle_groups = original;
    await api.put('/settings', doc);
  }
  await go('#/dashboard', 'dashboard');
}

// Preview changes page: search narrows the list to one person and a row expands to its +/- lines.
async function scenarioImpact() {
  await go('#/preview', 'preview');
  const count = await waitFor(() => {
    const c = $('[data-test="impact-count"]');
    return c && text(c) ? c : null;
  }, 'the impact preview to load', 20000);
  failOnErrorToast();
  const rows = () => $$('[data-test="impact-results"] tbody tr:not(.detail-row)');
  if (rows().length === 0) {
    log('  (nobody in the preview - search and expand not checked)');
    return;
  }
  const first = text($('strong', rows()[0]));
  const totalBefore = Number((text(count).match(/^\d+/) || ['0'])[0]);
  setValue($('[data-test="impact-search"]'), first);
  await waitFor(() => text(count).includes(`"${first}"`) && rows().length > 0 && rows().every((r) => text(r).includes(first)), 'the search to narrow the list');
  const totalAfter = Number((text(count).match(/^\d+/) || ['0'])[0]);
  if (totalAfter > totalBefore || totalAfter < 1) {
    throw new Error(`search did not narrow the list (${totalBefore} -> ${totalAfter})`);
  }
  $('[data-test="row-toggle"]', rows()[0]).click();
  const detail = await waitFor(() => {
    const d = rows()[0].nextElementSibling;
    return d && d.classList.contains('detail-row') && !d.hidden ? d : null;
  }, 'the row details');
  const lines = $$('[data-test="impact-diff"] li', detail);
  if (lines.length > 0 && !lines.every((l) => /^(Added|Removed): /.test(text($('.sr-only', l))) && /^[+−]$/.test(text($('.diff-mark', l))))) {
    throw new Error('diff lines lack their text markers');
  }
  setValue($('[data-test="impact-search"]'), `no-such-person-${rand}`);
  await waitFor(() => text(count).startsWith('0 '), 'an empty result for an unknown name');
  await go('#/dashboard', 'dashboard');
}

// ---------------------------------------------------------------- runner

const STEPS = [
  ['branding (logo, product name, favicon)', scenarioBranding],
  ['login', scenarioLogin],
  ['dashboard renders status', scenarioDashboard],
  ['create mapping via modal', scenarioMapping],
  ['field mapper shows fields, attributes and problems', scenarioFieldMapper],
  ['create csv source', scenarioSource],
  ['approve a pending change', scenarioApprovals],
  ['settings save round-trip', scenarioSettings],
  ['sign-in options (Entra ID button, break-glass)', scenarioSignInOptions],
  ['history search', scenarioHistory],
  ['OU placement page', scenarioPlacement],
  ['role rule: group search, nested conditions, explain, impact', scenarioRoleRule],
  ['lifecycle groups save', scenarioLifecycle],
  ['impact preview: search and expand', scenarioImpact],
  ['set a username on a name conflict', scenarioNeedsName],
  ['choose the account for a link', scenarioLinkChoose],
  ['add a person by hand (preview + create)', scenarioManualPerson],
  ['matching page (rules, report, ignore)', scenarioMatching],
  ['viewer has no mutating controls', scenarioViewer],
];

async function scenarioHistory() {
  await go('#/audit', 'audit');
  const q = await waitFor(() => $('[data-test="history-q"]'), 'the history search box');
  setValue(q, 'action:change.*');
  await waitFor(() => {
    const rows = $$('[data-test="history-table"] tbody tr');
    return rows.length > 0 && rows.every((tr) => text(tr.children[3]).startsWith('change.'));
  }, 'only change.* entries for action:change.*');
  // Clicking an actor adds it to the query.
  const actorBtn = $('[data-test="history-table"] tbody tr td:nth-child(3) button');
  const actor = text(actorBtn);
  actorBtn.click();
  await waitFor(() => q.value.includes(`actor:${actor}`), 'the clicked actor in the query');
  setValue(q, 'colour:red');
  await waitFor(() => byText('.notice', 'unknown filter'), 'an explanation for an unknown filter');
  setValue(q, '');
  const exp = $('[data-test="history-export"]');
  if (!exp || !exp.getAttribute('href').startsWith('/api/audit?format=csv')) {
    throw new Error('the CSV export link is missing');
  }
}

async function scenarioPlacement() {
  await go('#/placement', 'placement');
  await waitFor(() => $('[data-test="placement-enabled"]') && $('[data-test="placement-rules"]'), 'the placement settings and rules');
  await waitFor(() => {
    const v = $('[data-test="placement-values"]');
    return v && (v.querySelector('table') || text(v).includes('No active people'));
  }, 'the department values in use');
  // Add a rule and open the OU picker (the test directory has no OUs to list), then leave without saving.
  $('[data-test="rule-add"]').click();
  await waitFor(() => $$('[data-test="rule-match"]').length > 0, 'a new rule row');
  byText('button', 'Pick…').click();
  const modal = await waitFor(() => topModal(), 'the OU picker');
  await waitFor(() => $('[data-test="ou-list"]', modal) && !text($('[data-test="ou-list"]', modal)).includes('Reading OUs'), 'the OU list to load');
  byText('button', 'Cancel', modal).click();
  await waitFor(() => !topModal(), 'the picker to close');
  await go('#/dashboard', 'dashboard');
}

// Needs: a pending create with payload.needs_name (its sam is taken in AD) - see the fixtures in e2e_test.go.
async function scenarioNeedsName() {
  await go('#/approvals?status=pending', 'approvals');
  const btn = await waitFor(() => $('[data-test="set-name"]'), 'a "Set username" button (fixture: a pending create whose sAMAccountName is taken)');
  const id = btn.dataset.id;
  const approve = $(`[data-test="approve"][data-id="${id}"]`);
  if (!approve || !approve.disabled) {
    throw new Error('Approve must be disabled while the change waits for a username');
  }
  const before = await api.get(`/changes/${encodeURIComponent(id)}`);
  btn.click();
  const modal = await waitFor(() => {
    const m = topModal();
    return m && $('[data-test="name-sam"]', m) ? m : null;
  }, 'the username dialog');
  // The taken name is refused inline (409) and the dialog stays open.
  const takenSam = ((before.payload && before.payload.name_conflicts) || []).find((c) => c.field === 'sam');
  if (takenSam && !takenSam.with) {
    setValue($('[data-test="name-sam"]', modal), takenSam.value);
    $('button[type="submit"]', modal).click();
    await waitFor(() => $('[data-test="name-check"] .notice-error', modal), 'the inline "already taken" message');
    if (!topModal()) {
      throw new Error('the dialog closed although the name was taken');
    }
  }
  const sam = `st${rand}`;
  const suffix = (before.payload && before.payload.candidates && before.payload.candidates[0] && before.payload.candidates[0].upn.split('@')[1])
    || ($('[data-test="name-upn"]', modal).value.split('@')[1]) || 'corp.test';
  setValue($('[data-test="name-sam"]', modal), sam);
  setValue($('[data-test="name-upn"]', modal), `${sam}@${suffix}`);
  setValue($('[data-test="name-cn"]', modal), `Selftest ${rand}`);
  const mail = $('[data-test="name-mail"]', modal);
  if (mail && mail.closest('.field')) {
    setValue(mail, `${sam}@${suffix}`);
  }
  $('[data-test="name-check-btn"]', modal).click();
  await waitFor(() => text($('[data-test="name-check"]', modal)).includes('free'), 'the AD check to report the names as free');
  await submitModal('set username');
  const after = await waitFor(async () => {
    const c = await api.get(`/changes/${encodeURIComponent(id)}`);
    return c.payload && !c.payload.needs_name ? c : null;
  }, 'needs_name to be cleared on the change');
  const cand = (after.payload.candidates || [])[0] || {};
  if (cand.sam !== sam) {
    throw new Error(`the change got sam ${cand.sam}, expected ${sam}`);
  }
  await waitFor(() => {
    const a = $(`[data-test="approve"][data-id="${id}"]`);
    return a && !a.disabled;
  }, 'Approve to be enabled after the username was set');
}

// Needs: a pending link change with at least one candidate (fixture: matching on, a source person with no account by ID,
// and AD accounts without employeeID that match first + last name).
async function scenarioLinkChoose() {
  await go('#/approvals?status=pending&type=link', 'approvals');
  const btn = await waitFor(() => $('[data-test="review-link"]'), 'a link change to review (fixture: a partial / ambiguous match)');
  const id = btn.dataset.id;
  btn.click();
  const modal = await waitFor(() => {
    const m = topModal();
    return m && $$('[data-test="link-candidate"]', m).length > 0 ? m : null;
  }, 'the candidate accounts');
  if (!$('[data-test="link-create-new"]', modal)) {
    throw new Error('the "create a new account" choice is missing');
  }
  if (!$('[data-test="match-full"], [data-test="match-partial"]', modal)) {
    throw new Error('candidates show no full/partial badge');
  }
  const choose = $('[data-test="link-choose"]', modal);
  const guid = choose.closest('[data-test="link-candidate"]').dataset.guid;
  choose.click();
  await waitFor(() => {
    failOnErrorToast();
    return !modal.isConnected;
  }, 'the match dialog to close after choosing');
  await waitFor(async () => {
    const c = await api.get(`/changes/${encodeURIComponent(id)}`);
    return c.payload && c.payload.link && c.payload.link.chosen === guid;
  }, 'the chosen account to be stored on the change');
  await waitFor(() => {
    const a = $(`[data-test="approve"][data-id="${id}"]`);
    return a && !a.disabled;
  }, 'Approve to be enabled once an account is chosen');
}

// Self-contained: creates a manual source through the API, adds and deletes a person through the UI, removes the source.
async function scenarioManualPerson() {
  const name = `selftest-manual-${rand}`;
  const src = await api.post('/sources', {
    name, type: 'manual', enabled: true,
    options: { external_id_field: 'ExternalID', status_field: 'Status', terminated_values: ['Terminated'], missing_means_terminated: false, missing_grace_runs: 0 },
    config: {},
  });
  const sid = (src && src.id) || (listItems(await api.get('/sources')).find((s) => s.name === name) || {}).id;
  if (!sid) {
    throw new Error('could not create the manual source');
  }
  try {
    await go(`#/manual?source=${encodeURIComponent(sid)}`, 'manual');
    await waitFor(() => $('[data-test="mp-form"]') && $('[data-test="mp-FirstName"]'), 'the person form');
    setValue($('[data-test="mp-FirstName"]'), 'Pat');
    setValue($('[data-test="mp-LastName"]'), `Selftest${rand}`);
    const dept = $('[data-test="mp-Department"]');
    if (dept) {
      setValue(dept, dept.tagName === 'SELECT' ? (Array.from(dept.options).find((o) => o.value) || {}).value || '' : 'Finance');
    }
    for (const el of $$('[data-test="mp-form"] :invalid')) {
      if (el.dataset.field) {
        setValue(el, el.type === 'date' ? '2030-01-31' : (el.type === 'email' ? 'pat@example.com' : 'x'));
      }
    }
    $('[data-test="mp-preview-btn"]').click();
    await waitFor(() => {
      failOnErrorToast();
      return text($('[data-test="mp-preview"]')).includes('sAMAccountName');
    }, 'the preview of the account names');
    const sync = $('[data-test="mp-sync"]');
    if (sync.checked) {
      sync.click(); // keep the run queue quiet for the later scenarios
    }
    $('[data-test="mp-create"]').click();
    const row = await waitFor(() => {
      failOnErrorToast();
      return $$('.mp-people tbody tr').find((tr) => text(tr).includes(`Selftest${rand}`));
    }, 'the new person in the list');
    const del = $('[data-test="mp-delete"]', row);
    if (!del) {
      throw new Error('an admin cannot delete a person without an AD account');
    }
    del.click();
    await confirmDanger('Delete');
    await waitFor(() => !$$('.mp-people tbody tr').some((tr) => text(tr).includes(`Selftest${rand}`)), 'the person to disappear after delete');
  }
  finally {
    try {
      await api.del(`/sources/${encodeURIComponent(sid)}`);
    }
    catch (e) {
      log(`cleanup problem: ${e.message}`);
    }
  }
}

// Needs: correlation enabled and a sync that reported at least one uncorrelated AD account (fixture: an AD account under
// the search bases with no employeeID that matches nobody).
async function scenarioMatching() {
  await go('#/matching', 'matching');
  await waitFor(() => $$('[data-test="m-rule"]').length > 0 && $('[data-test="m-enabled"]'), 'the matching rules');
  await waitFor(() => $('[data-test="m-report-view"]'), 'the matching report');
  const row = await waitFor(() => $$('.unc-table tbody tr').find((tr) => $('[data-test="m-ignore-btn"]', tr)), 'an uncorrelated AD account (fixture)');
  const sam = text($('strong', row));
  const before = await api.get('/settings');
  $('[data-test="m-ignore-btn"]', row).click();
  await waitFor(() => {
    failOnErrorToast();
    return byText('.toast', 'ignored');
  }, 'the "ignored" toast');
  const after = await api.get('/settings');
  const list = (after.correlation && after.correlation.ignore_accounts) || [];
  if (!list.some((x) => String(x).toLowerCase() === sam.toLowerCase())) {
    throw new Error(`ignore_accounts does not contain ${sam}`);
  }
  await waitFor(() => !$$('.unc-table tbody tr').some((tr) => text($('strong', tr)) === sam), 'the ignored account to leave the report');
  // restore
  after.correlation.ignore_accounts = (before.correlation && before.correlation.ignore_accounts) || [];
  await api.put('/settings', after);
  $$('.toast').forEach((t) => t.remove());
  await go('#/dashboard', 'dashboard');
}

async function main() {
  const watchdog = setTimeout(() => fail('selftest timed out (170s)'), 170000);
  // ?only=login,username,... runs just the steps whose names contain one of the words (handy against the mock API).
  const only = (params.get('only') || '').split(',').map((s) => s.trim()).filter(Boolean);
  for (const [name, fn] of STEPS) {
    if (finished) {
      return;
    }
    if (only.length > 0 && !only.some((w) => name.includes(w))) {
      continue;
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
