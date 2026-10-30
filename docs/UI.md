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
| `#/approvals?status=&type=&q=` | viewer | filter (types include `cloud_groups` and `undo`), expand (summary + payload; cloud changes list groups to add, remove, or "all except"), approve/reject/retry and bulk (approver); applied changes: **Undo…** (admin; `undo.js`: per-item tick boxes for updates and cloud changes (All / None, re-preview on change), preview with live dry run, drift, Manual items, reason, typed confirmation, double tick-box to overwrite drift); undo changes: red `undo` tag, *Approve undo…* with a fresh dry run and typed confirmation, *Approve batch…* for run undos (typed `UNDO <n>`, fresh dry runs, reject the batch with a note), no bulk checkbox; `undone` / `partly undone` / `undo pending` pills |
| `#/runs`, `#/runs/<id>` | viewer | history, counters (incl. `on_hold`, `manual_not_disabled`) with a warning linking to the Manual people when `manual_not_disabled` > 0, errors (auto-refresh while running); **Undo changes from this run…** (admin): changes by person with item tick boxes, Preview (one dry run for the selection; changing the selection needs a new preview), reason, "Mark every undone item as Manual", typed `UNDO <n>` |
| `#/identities`, `#/identities/<id>?tab=` (`overview`, `history`, `changes`) | viewer | search (deleted and merged people stay listed); tabs: **Overview** (origin, attributes, AD link, "Leaving and retention" card ("Leaving on …", "Disabled on … · deleted on …" with the policy and why, *Change retention…* for approvers), "Removed from AD" box with the last known account, a red warning when someone who left HR is Manual, **Managed fields** (attributes, groups and OU: in AD, what the rules would set, Auto / Manual, *Make Manual…* / *Set back to Auto…*, "Manage this person manually…" with an extra tick for an inactive person), **Manual items** (by, since, reason, "IDLifecycle would …") for approvers); a *Managed* filter and *Manual (n)* badges in the list, **History** (timeline from `/api/identities/{id}/history`, *Load older*), **Changes** (every change, undo pills) |
| `#/manual` (Add person) | viewer (create/edit/end: approver; delete: admin) | manual-entry sources: form from the source's fields, preview (names, OU, conflicts), people list |
| `#/audit?q=&preset=` (`console`, `signins`) (History) | viewer | search with query syntax and filters, *Console activity* and *Sign-ins* buttons, CSV export, "Verify integrity"; a `changes` detail renders as a Field / Before / After table (secrets: "changed (secret value not recorded)"). Sources, Mappings, Roles, Users and Add person have a **History** link (`ui.historyLink`) to `type:<t> id:"<id>"` |
| `#/placement` (OU placement) | viewer (edit: admin) | department -> OU rules, OU picker from AD, CSV import/export, values in use |
| `#/matching` (Matching) | viewer (edit: admin) | correlation rules and settings, report of uncorrelated accounts (link to person, ignore) |
| `#/mappings` | viewer (edit: admin) | editor with mode explanations and live template preview, "Preview impact" |
| `#/roles` | viewer (edit: admin) | rule editor: condition builder (every operator, nested ALL/ANY groups, template), "Test this rule" with a per-condition explanation, groups via the live group picker (`group-picker.js`: Active Directory, plus Entra ID / Microsoft 365 when the cloud connection is on), "Preview impact" |
| `#/lifecycle` (Lifecycle groups) | viewer (edit: admin) | groups for active / disabled accounts, what a disable removes, groups always kept; "Preview impact" |
| `#/retention` (Retention) | viewer (edit: admin) | `views/retention.js`: policies by priority (↑/↓ renumbers 10, 20, 30…), the built-in Default last (edits `engine.delete_after_days` and `ad.disabled_ou`); editor with the role condition builder (`roles.js` exports `conditionBuilder`, `toModel`, `toBody`, `explainTable`, …), delete after N days / "Never delete automatically", "Deletes always wait for a person", Disabled OU (OU picker), extra kept groups (group picker), "Test this policy" against a person, "Preview impact", people count, History link |
| `#/leavers` (Leavers) | viewer | `views/leavers.js`: upcoming deletions (overdue = "due now") and people leaving on their date, window 7/30/90/365 days, kind filter, policy and why, Manual badge, links to the person |
| `#/preview` (Preview changes) | admin | impact preview (`views/impact.js`, also opened as a dialog by the "Preview impact" buttons): summary, live search, filters, per-person +/- steps |
| `#/sources` | viewer (edit: admin) | form generated from `/api/source-types` (select fields and fields shown only for some values of another field), "Test these settings" (`POST /api/sources/test`, before saving); "Leaving date (optional)": termination date field, format (Automatic, common layouts or a Go layout), days after the date |
| `#/settings` | viewer (edit: admin) | whole document saved with `PUT /api/settings`; the "Microsoft 365 / Entra ID" card (connection, user matching, Exchange Online, "Test connection" = `POST /api/cloud/test` with the unsaved form, "Access required" panel); the "Undo" card (allow undo, require a second person) |
| `#/tables`, `#/tables/<name>` | viewer | read-only row browser |
| `#/users` | admin | users CRUD, reset password |

Mutating controls carry `data-mutating`; they are not rendered for roles that cannot use them.

## Source layout

`app.js` (shell, router, status banner) · `brand.js` (product name, logo and accent from `/branding/brand.json`) ·
`branding/` (default `logo.svg`, `favicon.ico`) · `api.js` (fetch + CSRF + 401 handling) · `ui.js` (`h()` DOM helper,
modals, toasts, tables, form helpers) · `components.js` (mapping fields, sample-record picker, template preview) · `group-picker.js` (live group search combobox: AD, or Entra ID / Microsoft 365 with kind badges, greyed-out groups that cannot be managed, `entra:<id>` chips resolved by `/api/cloud/groups/resolve`)
· `undo.js` (the undo request and approve dialogs) · `views/*.js` (one per route).

## Test harnesses (not embedded)

* `internal/web/testdata/selftest/selftest.html|js` - drives the real UI against a real server; writes `PASS` or
  `FAIL: <reason>` into `<pre id="result">` (`RUNNING` while in progress, progress in `<pre id="log">`).
  Serve it same-origin at `/selftest/selftest.html?user=&pass=` (optional `csvpath=`, `report=<url>`). The test
  server needs at least one `pending` change (or a source that yields one after "Run sync").
* `internal/web/testdata/mock/` - in-memory mock of the API (`mock-api.js`) plus `mock.html?autologin=admin#/route`
  and `mock-selftest.html?user=admin&pass=admin-pass` for developing the UI without the backend. Serve `static/`
  at `/`, `testdata/selftest` at `/selftest/` and `testdata/mock` at `/mock/`; `go run ./cmd/idl-mockui` does exactly
  that on `127.0.0.1:8099` and prints both URLs. See [TESTING.md](TESTING.md#5-the-web-console-without-a-server).
