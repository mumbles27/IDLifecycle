// api.js - thin fetch wrapper for the IDLifecycle JSON API (see docs/ARCHITECTURE.md section 7).

import { qs } from './ui.js';

export class ApiError extends Error {
  constructor(message, { status = 0, code = '', handled = false } = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.handled = handled;
  }
}

const state = {
  csrf: '',
  user: null,
  onUnauthorized: null,
};

export function getUser() {
  return state.user;
}

export function setSession(resp) {
  if (!resp || typeof resp !== 'object') {
    state.user = null;
    state.csrf = '';
    return null;
  }
  const user = resp.user && typeof resp.user === 'object' ? resp.user : (resp.username ? resp : null);
  state.user = user;
  state.csrf = resp.csrf_token || resp.csrf || state.csrf || '';
  return user;
}

export function clearSession() {
  state.user = null;
  state.csrf = '';
}

export function onUnauthorized(fn) {
  state.onUnauthorized = fn;
}

const ROLE_LEVEL = { viewer: 1, approver: 2, admin: 3 };

/** True when the signed-in user has at least the given role. */
export function hasRole(min) {
  const u = state.user;
  if (!u) {
    return false;
  }
  return (ROLE_LEVEL[u.role] || 0) >= (ROLE_LEVEL[min] || 99);
}

async function parseBody(res) {
  if (res.status === 204) {
    return null;
  }
  const text = await res.text();
  if (!text) {
    return null;
  }
  try {
    return JSON.parse(text);
  }
  catch (e) {
    return { raw: text };
  }
}

export async function request(method, path, { body, params, quiet401 = false } = {}) {
  const headers = { Accept: 'application/json' };
  const init = { method, credentials: 'same-origin', headers };
  if (method !== 'GET' && method !== 'HEAD') {
    headers['X-CSRF-Token'] = state.csrf;
  }
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }

  let res;
  try {
    res = await fetch(`/api${path}${params ? qs(params) : ''}`, init);
  }
  catch (e) {
    throw new ApiError(`Cannot reach the server (${e.message || e}).`, { code: 'network' });
  }

  const data = await parseBody(res);
  if (res.ok) {
    return data;
  }

  const errObj = data && typeof data === 'object' ? data.error : null;
  const message = (errObj && errObj.message)
    || (typeof errObj === 'string' ? errObj : '')
    || `Request failed (HTTP ${res.status}).`;
  const code = (errObj && errObj.code) || '';

  if (res.status === 401 && !quiet401) {
    clearSession();
    if (state.onUnauthorized) {
      state.onUnauthorized(message);
    }
    throw new ApiError(message, { status: 401, code, handled: true });
  }
  throw new ApiError(message, { status: res.status, code });
}

export const api = {
  get: (path, params) => request('GET', path, { params }),
  post: (path, body) => request('POST', path, { body: body === undefined ? {} : body }),
  put: (path, body) => request('PUT', path, { body: body === undefined ? {} : body }),
  del: (path) => request('DELETE', path),
};

// --- session endpoints -------------------------------------------------

export async function login(username, password) {
  const resp = await request('POST', '/auth/login', { body: { username, password }, quiet401: true });
  setSession(resp);
  return state.user;
}

export async function restoreSession() {
  try {
    const resp = await request('GET', '/auth/me', { quiet401: true });
    return setSession(resp);
  }
  catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      return null;
    }
    throw e;
  }
}

export async function logout() {
  try {
    await request('POST', '/auth/logout', { body: {}, quiet401: true });
  }
  finally {
    clearSession();
  }
}

export function changePassword(current, next) {
  return api.post('/auth/password', { current, new: next });
}
