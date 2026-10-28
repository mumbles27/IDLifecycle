# Web UI

Dependency-free SPA (hand-written ES modules, no build step) in `internal/web/static`, embedded by
`internal/web` (`web.Handler()`). Coded against `docs/ARCHITECTURE.md` section 7.

## Server requirements

* Mount `web.Handler()` for everything that is not `/api/...`. It serves files with the right MIME types
  (`text/javascript` for `.js`), `Cache-Control: no-cache` for HTML and `public, max-age=3600` otherwise,
  falls back to `index.html` for unknown paths (missing `.js/.css/...` files get a real 404) and never
  answers `/api` or `/api/...` (404).
* CSP: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:` (`web.ContentSecurityPolicy`,
  also set by the handler). The UI uses no inline script/style/handlers, no eval and no HTML-string injection
  (enforced by `TestStaticFilesAreCSPClean`).
* The API must send/accept `X-CSRF-Token` on non-GET requests (token from login or `GET /api/auth/me`) and
  return errors as `{"error":{"code","message"}}`. A `401` anywhere returns the user to the login screen.

## Routes (hash router)

| Route | Min role | Notes |
|---|---|---|
| `#/dashboard` | viewer | mode card (DRY RUN / LIVE), scheduler, pending count, last run, source health, "Run sync now" (admin) |
| `#/approvals?status=&type=&q=` | viewer | filter, expand (summary + payload), approve/reject/retry and bulk (approver) |
| `#/runs`, `#/runs/<id>` | viewer | history, counters, errors (auto-refresh while running) |
| `#/identities`, `#/identities/<id>` | viewer | search, attributes, AD link, recent changes |
| `#/manual` (Add person) | viewer (create/edit/end: approver; delete: admin) | manual-entry sources: form from the source's fields, preview (names, OU, conflicts), people list |
| `#/audit` (History) | viewer | search with query syntax and filters, CSV export, "Verify integrity" |
| `#/placement` (OU placement) | viewer (edit: admin) | department -> OU rules, OU picker from AD, CSV import/export, values in use |
| `#/matching` (Matching) | viewer (edit: admin) | correlation rules and settings, report of uncorrelated accounts (link to person, ignore) |
| `#/mappings` | viewer (edit: admin) | editor with mode explanations and live template preview, "Preview impact" |
| `#/roles` | viewer (edit: admin) | rule editor: condition builder (every operator, nested ALL/ANY groups, template), "Test this rule" with a per-condition explanation, groups via the live AD group picker (`group-picker.js`), "Preview impact" |
| `#/lifecycle` (Lifecycle groups) | viewer (edit: admin) | groups for active / disabled accounts, what a disable removes, groups always kept; "Preview impact" |
| `#/preview` (Preview changes) | admin | impact preview (`views/impact.js`, also opened as a dialog by the "Preview impact" buttons): summary, live search, filters, per-person +/- steps |
| `#/sources` | viewer (edit: admin) | form generated from `/api/source-types` (select fields and fields shown only for some values of another field), "Test these settings" (`POST /api/sources/test`, before saving) |
| `#/settings` | viewer (edit: admin) | whole document saved with `PUT /api/settings` |
| `#/tables`, `#/tables/<name>` | viewer | read-only row browser |
| `#/users` | admin | users CRUD, reset password |

Mutating controls carry `data-mutating`; they are not rendered for roles that cannot use them.

## Source layout

`app.js` (shell, router, status banner) · `brand.js` (product name, logo and accent from `/branding/brand.json`) ·
`branding/` (default `logo.svg`, `favicon.ico`) · `api.js` (fetch + CSRF + 401 handling) · `ui.js` (`h()` DOM helper,
modals, toasts, tables, form helpers) · `components.js` (mapping fields, sample-record picker, template preview) · `group-picker.js` (live AD group search combobox)
· `views/*.js` (one per route).

## Test harnesses (not embedded)

* `internal/web/testdata/selftest/selftest.html|js` - drives the real UI against a real server; writes `PASS` or
  `FAIL: <reason>` into `<pre id="result">` (`RUNNING` while in progress, progress in `<pre id="log">`).
  Serve it same-origin at `/selftest/selftest.html?user=&pass=` (optional `csvpath=`, `report=<url>`). The test
  server needs at least one `pending` change (or a source that yields one after "Run sync").
* `internal/web/testdata/mock/` - in-memory mock of the API (`mock-api.js`) plus `mock.html?autologin=admin#/route`
  and `mock-selftest.html?user=admin&pass=admin-pass` for developing the UI without the backend. Serve `static/`
  at `/`, `testdata/selftest` at `/selftest/` and `testdata/mock` at `/mock/`.
