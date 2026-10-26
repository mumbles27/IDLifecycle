// Login view (rendered outside the app shell).

import { login, signInOptions } from '../api.js';
import { h, replaceContent, field, notice } from '../ui.js';

export function renderLogin(root, { message, onSuccess }) {
  const error = h('div', { class: 'login-error', 'aria-live': 'assertive' });
  const user = h('input', { type: 'text', name: 'username', autocomplete: 'username', required: true, autocapitalize: 'none', spellcheck: 'false', 'data-test': 'login-user' });
  const pass = h('input', { type: 'password', name: 'password', autocomplete: 'current-password', required: true, 'data-test': 'login-pass' });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-block', 'data-test': 'login-submit' }, 'Sign in');
  // Filled in once /auth/providers answers; the password form works without it.
  const sso = h('div', { class: 'login-sso', 'data-test': 'login-sso', hidden: true });
  const localNote = h('div', { 'data-test': 'login-local-note' });

  const form = h('form', {
    class: 'login-form',
    onsubmit: async (ev) => {
      ev.preventDefault();
      replaceContent(error);
      submit.disabled = true;
      try {
        await login(user.value.trim(), pass.value);
        pass.value = '';
        onSuccess();
      }
      catch (e) {
        replaceContent(error, notice('error', e.message || 'Sign-in failed.'));
        pass.value = '';
        pass.focus();
      }
      finally {
        submit.disabled = false;
      }
    },
  },
  localNote,
  field('Username', user),
  field('Password', pass),
  error,
  submit);

  replaceContent(root, h('div', { class: 'login-wrap', 'data-view': 'login' },
    h('div', { class: 'card login-card' },
      h('h1', null, 'IDLifecycle'),
      h('p', { class: 'muted' }, 'Active Directory identity lifecycle - sign in to continue.'),
      message ? notice('warn', message) : null,
      sso,
      form)));
  user.focus();

  signInOptions().then((opts) => {
    const oidc = (opts && opts.oidc) || {};
    if (!oidc.enabled) {
      return;
    }
    // A full-page navigation: the server redirects to Entra ID and back.
    replaceContent(sso,
      h('a', { class: 'btn btn-primary btn-block', href: '/api/auth/oidc/start', 'data-test': 'login-oidc' }, oidc.label || 'Sign in with Microsoft'),
      h('p', { class: 'login-divider muted small' }, 'or use an IDLifecycle account'));
    sso.hidden = false;
    submit.classList.remove('btn-primary');
    if (opts.local_login === 'admins_only') {
      replaceContent(localNote, h('p', { class: 'muted small' }, 'Password sign-in is for administrators only (break-glass). Everyone else signs in with Microsoft.'));
    }
  });
}
