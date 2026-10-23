// Login view (rendered outside the app shell).

import { login } from '../api.js';
import { h, replaceContent, field, notice } from '../ui.js';

export function renderLogin(root, { message, onSuccess }) {
  const error = h('div', { class: 'login-error', 'aria-live': 'assertive' });
  const user = h('input', { type: 'text', name: 'username', autocomplete: 'username', required: true, autocapitalize: 'none', spellcheck: 'false', 'data-test': 'login-user' });
  const pass = h('input', { type: 'password', name: 'password', autocomplete: 'current-password', required: true, 'data-test': 'login-pass' });
  const submit = h('button', { type: 'submit', class: 'btn btn-primary btn-block', 'data-test': 'login-submit' }, 'Sign in');

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
  field('Username', user),
  field('Password', pass),
  error,
  submit);

  replaceContent(root, h('div', { class: 'login-wrap', 'data-view': 'login' },
    h('div', { class: 'card login-card' },
      h('h1', null, 'IDLifecycle'),
      h('p', { class: 'muted' }, 'Active Directory identity lifecycle - sign in to continue.'),
      message ? notice('warn', message) : null,
      form)));
  user.focus();
}
