// app.js - application shell: session handling, hash router, navigation, global status banner.

import {
  api, getUser, hasRole, onUnauthorized, restoreSession, logout, changePassword,
} from './api.js';
import {
  h, clear, replaceContent, toast, reportError, formModal, field, inputText, errorBox, loading, pill,
} from './ui.js';
import { renderLogin } from './views/login.js';
import { loadBrand, productName, brandLogo, pageTitle } from './brand.js';

const ROUTES = [
  { name: 'dashboard', label: 'Dashboard', group: 'Operate', min: 'viewer', load: () => import('./views/dashboard.js') },
  { name: 'approvals', label: 'Approvals', group: 'Operate', min: 'viewer', load: () => import('./views/approvals.js'), badge: true },
  { name: 'runs', label: 'Runs', group: 'Operate', min: 'viewer', load: () => import('./views/runs.js') },
  { name: 'preview', label: 'Preview changes', group: 'Operate', min: 'admin', load: () => import('./views/impact.js') },
  { name: 'identities', label: 'Identities', group: 'Operate', min: 'viewer', load: () => import('./views/identities.js') },
  { name: 'manual', label: 'Add person', group: 'Operate', min: 'viewer', load: () => import('./views/manual.js') },
  { name: 'leavers', label: 'Leavers', group: 'Operate', min: 'viewer', load: () => import('./views/leavers.js') },
  { name: 'audit', label: 'History', group: 'Operate', min: 'viewer', load: () => import('./views/audit.js') },
  { name: 'mappings', label: 'Attribute mappings', group: 'Configure', min: 'viewer', load: () => import('./views/mappings.js') },
  { name: 'roles', label: 'Role rules', group: 'Configure', min: 'viewer', load: () => import('./views/roles.js') },
  { name: 'lifecycle', label: 'Lifecycle groups', group: 'Configure', min: 'viewer', load: () => import('./views/lifecycle.js') },
  { name: 'retention', label: 'Retention', group: 'Configure', min: 'viewer', load: () => import('./views/retention.js') },
  { name: 'placement', label: 'OU placement', group: 'Configure', min: 'viewer', load: () => import('./views/placement.js') },
  { name: 'matching', label: 'Matching', group: 'Configure', min: 'viewer', load: () => import('./views/matching.js') },
  { name: 'sources', label: 'Sources', group: 'Configure', min: 'viewer', load: () => import('./views/sources.js') },
  { name: 'settings', label: 'Settings', group: 'Configure', min: 'viewer', load: () => import('./views/settings.js') },
  { name: 'tables', label: 'Data browser', group: 'Admin', min: 'viewer', load: () => import('./views/tables.js') },
  { name: 'users', label: 'Users', group: 'Admin', min: 'admin', load: () => import('./views/users.js') },
];

const app = {
  root: null,
  mode: 'init', // init | login | shell
  shell: null,
  status: null,
  cleanup: null,
  routeToken: 0,
  statusTimer: null,
};

// ---------------------------------------------------------------- status / banner

export function currentStatus() {
  return app.status;
}

export async function refreshStatus() {
  if (app.mode !== 'shell') {
    return null;
  }
  try {
    app.status = await api.get('/status');
  }
  catch (e) {
    return app.status;
  }
  updateStatusUi();
  return app.status;
}

function updateStatusUi() {
  const shell = app.shell;
  if (!shell || !app.status) {
    return;
  }
  const dry = app.status.mode === 'dry_run';
  clear(shell.banner);
  shell.banner.hidden = !dry;
  if (dry) {
    shell.banner.append(
      h('strong', null, 'DRY RUN MODE'),
      ' - changes are planned and recorded as "simulated", but nothing is written to Active Directory.',
      hasRole('admin') ? h('a', { href: '#/settings', class: 'banner-link' }, 'Go live in Settings') : null,
    );
  }
  replaceContent(shell.modeBadge, dry
    ? h('span', { class: 'mode-badge mode-dry', 'data-test': 'mode-badge' }, 'DRY RUN')
    : h('span', { class: 'mode-badge mode-live', 'data-test': 'mode-badge' }, 'LIVE'));

  const pending = Number(app.status.pending_changes) || 0;
  for (const badge of shell.nav.querySelectorAll('[data-badge="approvals"]')) {
    badge.textContent = pending > 0 ? String(pending) : '';
    badge.hidden = pending === 0;
    badge.setAttribute('aria-label', `${pending} pending`);
  }
}

// ---------------------------------------------------------------- login / shell

function stopTimers() {
  if (app.statusTimer) {
    clearInterval(app.statusTimer);
    app.statusTimer = null;
  }
  if (typeof app.cleanup === 'function') {
    try {
      app.cleanup();
    }
    catch (e) {
      // ignore view cleanup problems
    }
  }
  app.cleanup = null;
}

function showLogin(message) {
  if (app.mode === 'login' && !message) {
    return;
  }
  stopTimers();
  app.mode = 'login';
  app.shell = null;
  app.status = null;
  app.routeToken += 1;
  renderLogin(app.root, {
    message,
    onSuccess: () => {
      showShell();
    },
  });
}

function buildShell() {
  const user = getUser();
  const nav = h('nav', { class: 'sidebar', id: 'sidebar', 'aria-label': 'Main' });
  let lastGroup = '';
  let list = null;
  for (const r of ROUTES) {
    if (!hasRole(r.min)) {
      continue;
    }
    if (r.group !== lastGroup) {
      lastGroup = r.group;
      nav.appendChild(h('h2', { class: 'nav-group' }, r.group));
      list = h('ul');
      nav.appendChild(list);
    }
    list.appendChild(h('li', null,
      h('a', { href: `#/${r.name}`, 'data-route': r.name },
        r.label,
        r.badge ? h('span', { class: 'nav-badge', 'data-badge': r.name, hidden: true }) : null)));
  }

  const main = h('main', { id: 'main', class: 'main', tabindex: '-1' });
  const banner = h('div', { class: 'banner banner-dry', role: 'status', hidden: true, 'data-test': 'dry-run-banner' });
  const modeBadge = h('span', { class: 'mode-slot' });
  const menuBtn = h('button', {
    type: 'button',
    class: 'icon-btn menu-toggle',
    'aria-controls': 'sidebar',
    'aria-expanded': 'false',
    'aria-label': 'Toggle navigation',
    onclick: () => {
      const open = !shell.classList.contains('nav-open');
      shell.classList.toggle('nav-open', open);
      menuBtn.setAttribute('aria-expanded', String(open));
    },
  }, '☰');

  const topbar = h('header', { class: 'topbar' },
    menuBtn,
    h('a', { href: '#/dashboard', class: 'brand', 'data-test': 'brand' }, brandLogo('brand-logo', { decorative: true }), h('span', { class: 'brand-name' }, productName())),
    modeBadge,
    h('span', { class: 'spacer' }),
    h('span', { class: 'whoami', 'data-test': 'whoami' },
      h('span', { class: 'who-name' }, user ? user.username : ''),
      user ? pill(user.role) : null),
    // Entra ID accounts have no password here.
    user && user.auth_source === 'oidc' ? null : h('button', { type: 'button', class: 'btn btn-sm', 'data-test': 'change-password', onclick: openChangePassword }, 'Change password'),
    h('button', {
      type: 'button',
      class: 'btn btn-sm',
      'data-test': 'logout',
      onclick: async () => {
        try {
          await logout();
        }
        catch (e) {
          // session may already be gone; go to login regardless
        }
        location.hash = '#/dashboard';
        showLogin();
      },
    }, 'Sign out'));

  const skip = h('button', { type: 'button', class: 'skip-link', onclick: () => main.focus() }, 'Skip to content');
  const shell = h('div', { class: 'shell' }, skip, topbar, banner, h('div', { class: 'body' }, nav, main));
  shell.main = main;
  shell.banner = banner;
  shell.modeBadge = modeBadge;
  shell.nav = nav;

  nav.addEventListener('click', (ev) => {
    if (ev.target.closest('a')) {
      shell.classList.remove('nav-open');
      menuBtn.setAttribute('aria-expanded', 'false');
    }
  });
  return shell;
}

function showShell() {
  stopTimers();
  app.mode = 'shell';
  app.shell = buildShell();
  replaceContent(app.root, app.shell);
  if (!location.hash || location.hash === '#/' || location.hash === '#/login') {
    location.hash = '#/dashboard';
  }
  refreshStatus();
  app.statusTimer = setInterval(refreshStatus, 30000);
  route();
}

// ---------------------------------------------------------------- router

function parseHash() {
  const raw = (location.hash || '').replace(/^#\/?/, '');
  const [pathPart, queryPart] = raw.split('?');
  const segs = pathPart.split('/').filter(Boolean).map((s) => decodeURIComponent(s));
  return {
    name: segs[0] || 'dashboard',
    params: segs.slice(1),
    query: new URLSearchParams(queryPart || ''),
  };
}

export function navigate(hash) {
  location.hash = hash.startsWith('#') ? hash : `#/${hash}`;
}

async function route() {
  if (app.mode !== 'shell' || !app.shell) {
    return;
  }
  app.routeToken += 1;
  const token = app.routeToken;
  if (typeof app.cleanup === 'function') {
    try {
      app.cleanup();
    }
    catch (e) {
      // ignore
    }
  }
  app.cleanup = null;

  const { name, params, query } = parseHash();
  const def = ROUTES.find((r) => r.name === name);
  const main = app.shell.main;

  for (const a of app.shell.nav.querySelectorAll('a[data-route]')) {
    if (a.dataset.route === name) {
      a.setAttribute('aria-current', 'page');
    }
    else {
      a.removeAttribute('aria-current');
    }
  }

  if (!def) {
    document.title = pageTitle('Not found');
    replaceContent(main, h('div', { class: 'view' }, h('h1', null, 'Page not found'), h('p', null, `There is no page called "${name}". `, h('a', { href: '#/dashboard' }, 'Back to the dashboard'), '.')));
    return;
  }
  document.title = pageTitle(def.label);
  if (!hasRole(def.min)) {
    replaceContent(main, h('div', { class: 'view', 'data-test': 'forbidden' }, h('h1', null, 'Not allowed'), h('p', null, `Your role (${getUser().role}) cannot open ${def.label}.`)));
    return;
  }

  replaceContent(main, loading());
  try {
    const mod = await def.load();
    if (token !== app.routeToken) {
      return;
    }
    const view = h('div', { class: 'view', 'data-view': name });
    replaceContent(main, view);
    const ctx = {
      user: getUser(),
      role: getUser().role,
      can: hasRole,
      params,
      query,
      navigate,
      refreshStatus,
      status: app.status,
    };
    const cleanup = await mod.render(view, ctx);
    if (token !== app.routeToken) {
      if (typeof cleanup === 'function') {
        cleanup();
      }
      return;
    }
    app.cleanup = typeof cleanup === 'function' ? cleanup : null;
  }
  catch (e) {
    if (token !== app.routeToken) {
      return;
    }
    // eslint-disable-next-line no-console
    console.error(e);
    replaceContent(main, h('div', { class: 'view' }, h('h1', null, def.label), errorBox(e)));
  }
}

// ---------------------------------------------------------------- change password

function openChangePassword() {
  const model = { current: '', next: '', confirm: '' };
  const cur = inputText(model, 'current', { type: 'password', autocomplete: 'current-password', required: true });
  const nxt = inputText(model, 'next', { type: 'password', autocomplete: 'new-password', required: true });
  const cnf = inputText(model, 'confirm', { type: 'password', autocomplete: 'new-password', required: true });
  formModal({
    title: 'Change password',
    submitLabel: 'Change password',
    content: [
      field('Current password', cur, { required: true }),
      field('New password', nxt, { required: true, help: 'The server enforces the password policy and will tell you if it is rejected.' }),
      field('Confirm new password', cnf, { required: true }),
    ],
    onSubmit: async () => {
      if (model.next !== model.confirm) {
        cnf.setCustomValidity('Passwords do not match.');
        cnf.reportValidity();
        cnf.setCustomValidity('');
        return false;
      }
      await changePassword(model.current, model.next);
      toast('Password changed.', 'ok');
      return true;
    },
  });
}

// ---------------------------------------------------------------- boot

export async function boot(root) {
  app.root = root;
  onUnauthorized(() => {
    showLogin(app.mode === 'shell' ? 'Your session has ended. Please sign in again.' : undefined);
  });
  window.addEventListener('hashchange', route);
  replaceContent(root, loading('Starting…'));
  await loadBrand();
  document.title = pageTitle();
  try {
    const user = await restoreSession();
    if (user) {
      showShell();
    }
    else {
      showLogin();
    }
  }
  catch (e) {
    replaceContent(root, h('div', { class: 'login-wrap' }, h('div', { class: 'card' }, h('h1', null, productName()), errorBox(e),
      h('button', { type: 'button', class: 'btn', onclick: () => location.reload() }, 'Retry'))));
    reportError(e);
  }
}

const rootEl = document.getElementById('app');
if (rootEl && !window.__IDL_NO_AUTOBOOT) {
  boot(rootEl);
}
