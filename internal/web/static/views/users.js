// Users (admin only): local accounts and their roles.

import { api } from '../api.js';
import {
  h, replaceContent, pageHeader, pill, fmtTime, dataTable, listItems, act, confirmDialog, toast,
  errorBox, loading, field, inputText, inputSelect, inputCheck, formModal, mutBtn, notice,
} from '../ui.js';

const ROLES = [
  { value: 'viewer', label: 'viewer - read only' },
  { value: 'approver', label: 'approver - can approve / reject / retry changes' },
  { value: 'admin', label: 'admin - full control' },
];

export async function render(root, ctx) {
  const listEl = h('div', null, loading());
  root.append(
    pageHeader('Users', mutBtn('New user', { class: 'btn-primary', 'data-test': 'new-user', onclick: () => openEditor(null) })),
    listEl);

  async function load() {
    let items;
    try {
      items = listItems(await api.get('/users', { limit: 500 }));
    }
    catch (e) {
      replaceContent(listEl, errorBox(e));
      return;
    }
    replaceContent(listEl, dataTable([
      { label: 'Username', render: (u) => [h('strong', null, u.username), u.username === ctx.user.username ? h('span', { class: 'muted small' }, ' (you)') : null] },
      { label: 'Role', render: (u) => pill(u.role) },
      { label: 'Status', render: (u) => pill(u.disabled ? 'disabled' : 'active') },
      { label: 'Created', render: (u) => fmtTime(u.created_at) },
      { label: 'Last login', render: (u) => fmtTime(u.last_login_at) },
      {
        label: 'Actions',
        className: 'col-actions',
        render: (u) => {
          const self = u.username === ctx.user.username;
          return h('div', { class: 'row-actions' },
            mutBtn('Edit', { class: 'btn-sm', 'data-test': 'edit-user', onclick: () => openEditor(u) }),
            mutBtn('Reset password', { class: 'btn-sm', 'data-test': 'reset-user', onclick: () => openReset(u) }),
            self ? null : mutBtn(u.disabled ? 'Enable' : 'Disable', { class: 'btn-sm', 'data-test': 'toggle-user', onclick: (e) => toggle(u, e.currentTarget) }),
            self ? null : mutBtn('Delete', { class: 'btn-sm btn-danger', 'data-test': 'delete-user', onclick: (e) => remove(u, e.currentTarget) }));
        },
      },
    ], items, { caption: 'Users', empty: 'No users.' }));
  }

  const base = (u) => ({ username: u.username, role: u.role, disabled: Boolean(u.disabled) });

  async function toggle(u, button) {
    const disabling = !u.disabled;
    if (disabling) {
      const ok = await confirmDialog({
        title: 'Disable user',
        message: `Disable ${u.username}? They will be signed out and unable to log in until re-enabled.`,
        confirmLabel: 'Disable',
        danger: true,
      });
      if (!ok) {
        return;
      }
    }
    await act(button, async () => {
      await api.put(`/users/${encodeURIComponent(u.id)}`, { ...base(u), disabled: disabling });
      toast(`User ${disabling ? 'disabled' : 'enabled'}.`, 'ok');
      await load();
    });
  }

  async function remove(u, button) {
    const ok = await confirmDialog({
      title: 'Delete user',
      message: `Permanently delete ${u.username}? This cannot be undone. (Prefer Disable to keep the audit trail tidy.)`,
      confirmLabel: 'Delete',
      danger: true,
    });
    if (!ok) {
      return;
    }
    await act(button, async () => {
      await api.del(`/users/${encodeURIComponent(u.id)}`);
      toast('User deleted.', 'ok');
      await load();
    });
  }

  function openReset(u) {
    const model = { password: '' };
    formModal({
      title: `Reset password: ${u.username}`,
      submitLabel: 'Reset password',
      content: [
        notice('info', 'Sets a new password immediately. Share it with the user over a secure channel.'),
        field('New password', inputText(model, 'password', { type: 'password', autocomplete: 'new-password', required: true, 'data-test': 'reset-password' }), { required: true }),
      ],
      onSubmit: async () => {
        await api.put(`/users/${encodeURIComponent(u.id)}`, { ...base(u), password: model.password });
        toast('Password reset.', 'ok');
        await load();
        return true;
      },
    });
  }

  function openEditor(existing) {
    const model = existing
      ? { username: existing.username, role: existing.role, disabled: Boolean(existing.disabled), password: '' }
      : { username: '', role: 'viewer', disabled: false, password: '' };
    const self = existing && existing.username === ctx.user.username;
    formModal({
      title: existing ? `Edit user: ${existing.username}` : 'New user',
      submitLabel: existing ? 'Save user' : 'Create user',
      content: [
        field('Username', inputText(model, 'username', { required: true, 'data-test': 'user-name', autocomplete: 'off' }), { required: true }),
        field('Role', inputSelect(model, 'role', ROLES, { 'data-test': 'user-role', disabled: Boolean(self) }), { help: self ? 'You cannot change your own role.' : '' }),
        existing ? (self ? null : inputCheck(model, 'disabled', 'Disabled (cannot sign in)')) : null,
        existing ? null : field('Initial password', inputText(model, 'password', { type: 'password', autocomplete: 'new-password', required: true, 'data-test': 'user-password' }), { required: true, help: 'The server enforces the password policy.' }),
      ],
      onSubmit: async () => {
        const body = { username: model.username.trim(), role: model.role, disabled: Boolean(model.disabled) };
        if (existing) {
          await api.put(`/users/${encodeURIComponent(existing.id)}`, body);
        }
        else {
          await api.post('/users', { ...body, password: model.password });
        }
        toast(existing ? 'User saved.' : 'User created.', 'ok');
        await load();
        return true;
      },
    });
  }

  await load();
}
