# IDLifecycle – Architecture & Interface Contracts

A Windows service (Go) that reads people from an HR source (any REST/JSON HR API, CSV, SQL, people entered by hand), computes the
desired Active Directory state from DB-held mapping + role rules, routes changes through an approval
queue (or auto-applies), and executes them through PowerShell scripts. Admins operate it through an
embedded web GUI.

```
 Sources (CSV, REST API, SQL)    +------------------ idlifecycled.exe ------------------+
        |  fetch                 |  scheduler -> engine(plan) -> approvals -> executor  |
        v                        |        |           |                      |          |
   normalized records  --------> |      store (SQLite | Postgres | MSSQL | MySQL)       |
                                 |        |                                  |          |
   Browser <--- HTTPS/JSON ----> |   HTTP API + embedded SPA          PowerShell runner |
                                 +-----------------------------------------|-----------+
                                                                           v  stdin JSON / stdout JSON
                                                              scripts\*.ps1  -> ActiveDirectory module -> AD
```

## 1. Layout

| Path | Purpose |
|---|---|
| `cmd/idlifecycled` | Entry point: `run` (console), `service` (SCM), `init`, `lockdown`, `check` (`-cloud` adds the Microsoft 365 connection test; skipped when the connection is off; not one of the installer's required checks), `user add/passwd`, `backup`/`restore`, `config get/set` (basic `config.json` fields for the tray; never prints the DSN), `version` |
| `cmd/idl-mockui` | Developer tool, not in the release: serves the web UI against the in-browser mock API on `127.0.0.1:8099` (see [TESTING.md](TESTING.md)) |
| `internal/config` | Bootstrap config file (DB connection, listen addr, TLS, PowerShell paths) |
| `internal/store` | DB access, dialect layer (sqlite/postgres/mssql/mysql/db2), migrations, all queries |
| `internal/secrets` | AES-GCM encryption for source secrets; key wrapped by DPAPI on Windows |
| `internal/backup` | `.idlbak` backups: every backup table as JSON lines + `config.json` + the master key in a tar, encrypted with a passphrase (scrypt, chunked AES-256-GCM bound to the header); restore into any driver in one transaction; scheduled backups and pruning |
| `internal/datadir` | Data-directory ACL check (service refuses to start when broad principals can read it) and `lockdown` |
| `internal/notify` | E-mail notifications: generic SMTP, M365 SMTP AUTH (OAuth2 XOAUTH2), M365 Graph sendMail |
| `internal/model` | Plain data types shared by store/engine/api (JSON = snake_case) |
| `internal/sources` | `Source` interface + registry; CSV, SQL (table/view), manual entry and the generic `rest` HR API connector (auth modes, pagination modes, `Flatten`/`RecordsAt`, `NewHTTPClient` with retries/size cap/same-host redirects); `example_connector_test.go` shows a dedicated connector |
| `internal/engine` | Template rendering, role resolution, planner (diff), approval policy, executor, run orchestration |
| `internal/ad` | `Client` interface; `PowerShellClient` (real) and `FakeClient` (tests) |
| `internal/cloud` | Entra ID / Microsoft 365 / Exchange Online groups: `Client` interface (Test, SearchGroups, ResolveGroups, Snapshot, SetGroups) over the cloud scripts (same stdin/JSON runner as `ad`) and `Fake` (tests) |
| `internal/scheduler` | Interval / daily / cron scheduling, hot-reloaded from settings |
| `internal/api` | HTTP handlers, auth, sessions, CSRF, audit hooks |
| `internal/web` | `go:embed` of `static/` SPA; `branding.go` serves `/branding/brand.json`, `/branding/logo`, `/branding/favicon.ico` (no auth) from the trusted override folder `branding_dir` (default `%ProgramData%IDLifecycle-Branding`, checked with `datadir.CheckTrustedFolder`, re-read at most every 60 s, files ≤ 1 MiB and format-checked) or the embedded defaults in `static/branding/`; SVG gets a sandboxing CSP |
| `scripts/` | PowerShell: `IDLifecycle.Common.psm1`, `Get-IDLSnapshot.ps1`, `Get-IDLSchema.ps1` (field mapper only), `Get-IDLOUs.ps1` (OU picker only), `Search-IDLGroups.ps1` (group search only), cloud groups (optional): `IDLifecycle.Cloud.psm1`, `Test-IDLCloudConnection.ps1`, `Search-IDLCloudGroups.ps1`, `Get-IDLCloudSnapshot.ps1`, `Set-IDLCloudGroups.ps1` and the admin tool `New-IDLCloudCertificate.ps1`, `Test-IDLNames.ps1` (name checks), `Set-IDLLink.ps1` (account links), `New/Set/Disable/Enable/Remove-IDLUser.ps1`, `Install-IDLifecycleService.ps1`; setup: `New-IDLifecycleRelease.ps1`, `Install-IDLifecycle.ps1` (checks the release against `SHA256SUMS`; `-SkipIntegrityCheck` = least safe, no manifest), `Uninstall-IDLifecycle.ps1` and the setup window `Start-IDLifecycleSetup.ps1` (WPF; only collects input and runs the two scripts in a child `pwsh`) |
| `Setup.cmd` | The release folder's double-click launcher for the setup window (at the repo and release root; listed in `SHA256SUMS`) |
| `scripts/branding/` | Default brand files (`logo.svg`, `idlifecycle.ico`, `logo-256.png`, `brand.json`) and `New-IDLBrandAssets.ps1`, which draws them (and the web copies) with System.Drawing |
| `scripts/Start-IDLifecycleTray.ps1`, `Start-IDLifecycleTrayLauncher.ps1`, `Invoke-IDLTrayAction.ps1` | Tray icon (WinForms NotifyIcon + WPF settings window, runs as the signed-in user, reads `<InstallDir>	ray.json`; promotes its own HKCU NotifyIconSettings entry into the taskbar once per user; the installer starts it at once through Explorer, unelevated), its Windows PowerShell 5.1 launcher (used by the shortcuts when PowerShell 7 is not installed machine-wide: finds the user's pwsh.exe at sign-in) and its elevated helper (Start/Stop/Restart/SetConfig; re-checks elevation and that the service is this install's `idlifecycled.exe service`; `config set`; rewrites `tray.json`) |
| `tests/powershell` | Stub ActiveDirectory module + Pester tests for the scripts |

## 2. Key design decisions (ADR summary)

1. **DB portability**: stdlib `database/sql` + hand-written SQL using `?` placeholders rebound per dialect.
   IDs are UUID strings generated in Go (no autoincrement differences). Timestamps are stored as UTC
   RFC3339 strings (`VARCHAR(40)`) which sort correctly everywhere. Booleans are `INT 0/1`. Long JSON/text
   uses `LongText` (TEXT / NVARCHAR(MAX) / LONGTEXT). No upserts (update-then-insert in a tx).
   Rejected: ORM (opaque SQL, dialect surprises), per-DB schemas by hand.
   **Db2 (LUW 11.5+, driver `db2`, via `github.com/alexbrainman/odbc`, Windows only)**: `?` is native (no rebind);
   `{TEXT}` = `CLOB(16M)`, `{STR}` = `VARCHAR(255)` (bytes, not characters). CLOB columns may not be used in `=`/`<>`,
   `IN`, `ORDER BY`, `GROUP BY`, `DISTINCT` or indexes, so no query does that on any dialect (`TestDB2StatementAudit` enforces
   it by recording every statement each `*Store` method issues through a fake driver). Search (`where.like`) on db2 emits
   `LOWER(CAST(clobcol AS VARCHAR(32672))) LIKE CAST(? AS VARCHAR(32672)) ESCAPE '!'` and never escapes `[` (Db2 rejects an escape char
   followed by anything but `%`, `_` or itself, SQLSTATE 22025). Paging is `OFFSET n ROWS FETCH FIRST m ROWS ONLY` with integer literals.
   `schema_migrations` is created after a `SYSCAT.TABLES` check (`TABSCHEMA = CURRENT SCHEMA`, `TABNAME = 'SCHEMA_MIGRATIONS'`) instead of
   `IF NOT EXISTS`. Identifiers are never quoted, so Db2 folds them to upper case; `BrowseTable` lower-cases result column names.
   None of the table/column names is enforced as reserved by Db2 LUW (MODE, HASH, ROLE, VERSION are only on IBM's portability list).
   `Open` appends `LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;` to the ODBC string unless set (the driver package does not
   know `SQL_CLOB`; wide types avoid ANSI code-page conversion, so `MapCharToWChar=0` is refused) and bounds the connect itself
   (`connstr.BoundedPing`: the ODBC driver ignores the context while connecting). Open errors are scrubbed of the DSN and password. Unique violations: SQL0803N / SQLSTATE 23505. Default isolation is CS; the audit append is
   serialised by a Go mutex and the `seq` primary key, as on the other dialects. Not yet run against a live Db2.
2. **Pure Go build** (CGO off): `modernc.org/sqlite`, `pgx` stdlib, `go-mssqldb`, `go-sql-driver/mysql`; `alexbrainman/odbc`
   (Db2) syscalls into `odbc32.dll` on Windows and is linked only in the Windows build (`odbc_windows.go`).
3. **AD changes go through PowerShell** run as the service account (gMSA recommended; an MSA, a normal user account whose password the
   Service Control Manager keeps, or a built-in account also work; delegated rights
   only, no stored AD credentials). Go passes JSON on **stdin** (never the command line, so passwords and
   PII do not appear in process listings) and reads one JSON document from **stdout**.
4. **Plan/Apply split.** The planner computes diffs from (source record, AD snapshot, rules) and writes
   `changes` rows. Nothing touches AD until a change is `approved` (by a human, or by auto policy). Plans
   are derived from actual AD state each run, so they are idempotent and self-healing after a crash.
5. **Attribute modes**: `create_only` (set at creation only), `sync` (push when the *source* value changes;
   manual AD edits survive), `immutable` (every run forces AD to equal the source-derived value).
   `employeeID` (the link key) is implicitly immutable.
6. **Safety rails** (identity-lifecycle best practice): empty/failed source fetch aborts the run (never
   interpreted as "everyone left"); bulk-destructive guard (`max_destructive_per_run`) forces manual
   approval; disable-before-delete with a grace period; script refuses to delete an enabled account;
   every script verifies the AD account's ID attribute equals the change's `external_id`; global DryRun.
7. **Audit log is append-only with a hash chain** (`hash = SHA256(prev_hash | canonical record)`);
   `GET /api/audit/verify` recomputes it.
8. **Auth**: local accounts (bcrypt), roles `admin|approver|viewer`, server-side sessions (token stored as
   SHA-256), CSRF header on all non-GET, lockout after repeated failures, strict CSP, no CDN assets.
   **Optional Entra ID sign-in** (`internal/api/oidc.go`, `github.com/coreos/go-oidc/v3` + `golang.org/x/oauth2`): OpenID Connect
   authorization code flow with PKCE (S256), a nonce, and a `state` kept server-side (single use, 10 min) and bound to the browser by
   the `idl_oidc` cookie (HttpOnly, SameSite=Lax because the return from Entra ID is a cross-site top-level GET, Path `/api/auth/oidc`).
   go-oidc verifies issuer (`<authority>/<tenant GUID>/v2.0`), audience (client ID), expiry and the signature against the tenant's
   JWKS; `tid` must equal the configured tenant and `oid` must be present. Account key = `<tid>:<oid>` (`users.external_id`), created on
   first sign-in with `auth_source='oidc'` and no password; name (`preferred_username`) and role follow the token on every sign-in (a
   role change revokes the account's other sessions). Role = highest of admin/approver/viewer whose configured list contains one of the
   token's `roles` (app roles) or `groups` (object IDs); none ⇒ refused, nothing created. Refused too: a name already used by a local
   (or other Entra) account — no implicit linking — and accounts disabled in IDLifecycle. The callback answers with a page that
   meta-refreshes to `/`, a same-site navigation, so the SameSite=Strict session cookie set on the cross-site response is sent.
   Local password sign-in stays available; `auth.local_login=admins_only` limits it to local admins (break-glass) and needs OIDC on.
   Turning OIDC off (or changing tenant/client) revokes Entra sessions; switching to admins_only revokes local non-admin sessions.
   Logout ends the IDLifecycle session only (not the Entra ID session). MFA is enforced by Entra ID Conditional Access.

## 3. Data model (portable DDL in `internal/store/migrations`)

`schema_migrations(version, applied_at)` ·
`settings(key PK, value LongText JSON, updated_at, updated_by)` ·
`users(id, username UNIQUE, password_hash, role, disabled, failed_logins, locked_until, created_at, last_login_at, auth_source 'local'|'oidc', external_id '<tid>:<oid>' (migration 2, indexed))` ·
`sessions(token_hash PK, user_id, csrf_token, created_at, expires_at, last_seen_at)` ·
`sources(id, name UNIQUE, type, enabled, options_json, config_json(secrets encrypted), created_at, updated_at, last_sync_at, last_status, last_error)` ·
`identities(id, source_id, external_id, UNIQUE(source_id, external_id), attrs_json, attrs_hash, active, missing_runs, first_seen, last_seen, ad_guid, ad_dn, ad_sam, applied_json, disabled_at, state, ou_override, ou_override_value (migration 3: an OU a person chose and the placement value it applies to), merged_into, link_decision (migration 4: the record that took over this record's account; a person's link decision `""|new|guid:<objectGUID>`))` ·
`identity_choices(identity_id PK, name_override JSON Candidate, link_rejects JSON [objectGUID], updated_at)` (migration 4; written only by `SetIdentityNameOverride` / `AddIdentityLinkRejects`, never by a sync) ·
`manual_people(id, source_id, external_id, UNIQUE(source_id, external_id), attrs_json, ended, created_by, created_at, updated_by, updated_at)` (migration 4; people typed into the console for `manual` sources) ·
`mappings(id, name, target_attr, template, mode, on_empty, value_type, enabled, sort_order, description, created_at, updated_at)` ·
`role_rules(id, name, description, enabled, priority, match, conditions_json, grants_json, created_at, updated_at)` ·
`sync_runs(id, trigger, triggered_by, started_at, finished_at, status, counters_json, error)` ·
`changes(id, run_id, identity_id, external_id, display, type, status, payload_json, summary_json, payload_hash, created_at, decided_by, decided_at, decision_note, applied_at, error, attempts)` ·
`audit_log(id INT/seq as string-sortable `seq` BIGINT, ts, actor, action, entity_type, entity_id, detail_json, prev_hash, hash)`, index
`ix_audit_entity(entity_type, entity_id)` (migration 6, for one object's / one person's history) ·
`change_undo(change_id PK, applied_state JSON, undone_by, updated_at)` (migration 5: what an applied change found before it acted, and the
undo that rolled it back) ·
`identity_holds(id PK, identity_id (indexed), kind, hold_key, until_at (zero = no end), reason, created_by, created_at, change_id)` (migration 5). Migration 7: `change_undo_items(change_id, item_key, undo_change_id)` (per-item undo) · `identity_hold_state(hold_id PK, suppressed_json, updated_at)` (what a Manual pin stopped).
Migration 8: `retention_policies(id, name UNIQUE, description, enabled, priority, match_mode, conditions_json, delete_after_days,
disabled_ou, extra_keep_groups, approval, created_at, updated_at)` · `identities.retention_policy_id`, `identities.retention_assigned_by`
(a person's assignment; `default` = the Default) · `identity_retention(identity_id PK, policy_id, policy_name, reason, delete_due, ...)`
(the resolved policy and delete date, written only when they change).
Nothing deletes identities, changes or audit rows: a person stays in the database after leaving HR and after their account is deleted from AD.

`identities.state`: `new` (seen, no AD account yet) · `linked` (AD account exists & matched) · `disabled` ·
`deleted` · `error` · `merged` (its account was handed to `merged_into` by a `link`; the planner skips it).

## 4. Change lifecycle (state machine)

| From | To | Trigger | Who |
|---|---|---|---|
| (none) | `pending` | planner, approval policy for that type = `manual` (or destructive guard tripped) | system |
| (none) | `approved` | planner, policy = `auto` | system (`system:auto`) |
| (none) | `simulated` | planner while `engine.dry_run=true` (never applied) | system |
| `pending` | `approved` | approve | admin/approver |
| `pending` | `rejected` | reject (note required) | admin/approver |
| `pending` | `superseded` | next run produced a different plan for the same identity+type, or none at all | system |
| `approved` | `superseded` | not yet applied, and the next run planned something different or nothing (rehire, data corrected) | system |
| `approved` | `applying` | executor picks it up | system |
| `applying` | `applied` / `failed` | PowerShell result | system |
| `applying` | `failed` | service restart found it interrupted ("interrupted – verify in AD") | system |
| `failed` | `approved` | retry | admin/approver |

Illegal: anything out of `applied`, `rejected`, `superseded`, `simulated`; `rejected→approved`.
A change whose payload has `needs_ou` (OU placement found no OU) is always created `pending` and cannot be approved until
an OU is set (`POST /api/changes/{id}/ou`), which rewrites the payload/summary/hash while the change stays `pending` and records the
choice on the identity so the next run plans the same payload (no supersede).
The same holds for `needs_name` (every generated name is taken; `POST /api/changes/{id}/name`, remembered in `identity_choices.name_override`) and for a
`link` whose `payload.link.chosen` is empty (`POST /api/changes/{id}/link`, remembered as `link_decision = guid:...`). A `link` with `verify` (partial match,
or several candidates) is always created `pending`. Rejecting a `link` adds its chosen account (all candidates when none was chosen) to `link_rejects`.
Change types: `link` (hand an existing account to a record), `create`, `update` (attrs, group add/remove, OU move), `disable`, `enable`, `delete`, `cloud_groups` (Entra ID / Microsoft 365 / Exchange Online group membership of the account's cloud copy; separate from the AD change of the same person), `undo` (rolls back one applied change; see §4a).
Each transition writes an `audit_log` row. Approval policy is per type: `settings.approval.{create,update,disable,enable,delete,link,cloud_groups}` = `manual|auto` (`link` default `auto`; `cloud_groups` defaults to the value of `update`). `undo` has no policy: always `pending`, never `simulated`, never auto-approved or bulk-approved, never superseded by a sync.

### 4a. Undo (`internal/engine/undo.go`, `holds.go`)

* **Applied state.** When an action script applies an item it returns `before` (§6.2); the executor stores it with the after-DN/sam/upn and
  what the change wrote (`wrote`) in `change_undo.applied_state`. Changes without it are not undoable.
* **`engine.PlanUndo`** builds the inverse from the original change and its applied state:

  | original (or an undo with this script) | script | inverse | `expect` (checked by the script before any write) | default holds |
  |---|---|---|---|---|
  | `update` (`set`) | Set-IDLUser | `set`/`clear` the before values, remove `groupsAdded`, add `groupsRemoved`, `moveTo` the previous parent OU | the values the change wrote, `memberOf` / `notMemberOf`, after-DN | `attribute:<name>`, `group:<as planned>`, `ou` |
  | `disable` | Enable-IDLUser | move back, re-add / remove groups, `set`/`clear` description | `enabled:false`, DN, groups, description | `state` |
  | `enable` | Disable-IDLUser | move back, groups back, attributes back | `enabled:true`, DN, groups | `state` |
  | `create` | Disable-IDLUser | disable the created account | `enabled:true` | `state` |
  | `link` | Set-IDLLink | `externalId` = the previous id value (`""` = clear), `rename` + `proxyAddresses` = the previous names (`link_from` / `link_to`) | id value, names after the rename | `state` |
  | `cloud_groups` | Set-IDLCloudGroups | add `cloudRemoved`, remove `cloudAdded` | — | `cloud_group:<id>` |

  Not undoable (409, reason in the preview): status ≠ applied, no applied state, `delete` (AD Recycle Bin only), already undone, an undo
  already open, nothing actually changed, a link that took the account over from another record (merge), identity merged / deleted,
  `undo.enabled` false, `engine.dry_run` true. A new request replaces an earlier *failed* undo of the same change.
* **Request** (`POST /api/changes/{id}/undo`): the server recomputes the plan and its `preview_hash`, runs it with `-DryRun`, and refuses a
  stale hash, drift without `force`, a wrong typed confirmation (`confirm_text` = the account's sAMAccountName), a reason under 10
  characters or a hold that wasn't offered. Creates a `pending` change of type `undo` with `payload.undo = {of, of_type, script, expect,
  link_from, link_to, rename, proxy_addresses, force, reason, holds, requested_by, confirm_text}` and the inverse in the normal payload fields.
* **Approve** needs `{confirm}` = `confirm_text`; with `undo.second_person` the requester gets 403. Plain `Approve` and bulk refuse undo changes.
* **Apply**: undo changes run last. Drift ⇒ `failed` with `drift: ...`, nothing written; a retry repeats the same check. On success the
  original gets `undone_by`, the holds are created (`change_id` = the undo), and the identity follows: disable undone ⇒ `linked`; enable or
  create undone ⇒ `disabled`; link undone ⇒ `new`, AD fields cleared, the account added to `link_rejects`. Undo changes are themselves undoable (redo).
* **Holds** (`identity_holds`): kinds `attribute` (LDAP name), `group` (as the planner writes it), `cloud_group` (object id), `ou`, `state`.
  The planner and the impact preview drop held items for that identity (`ou` drops the move; `state` drops create, link, disable, enable and
  delete; an inactive identity under a state hold gets nothing, and cloud-group changes wait until the person is active with an enabled
  account). Expired holds (`until_at` past) are ignored. Run counter `on_hold`. Released with `DELETE /api/identities/{id}/holds/{hid}`.
* Audit: `change.undo_requested`, `change.approve`, `change.applied` / `change.failed` (the undo), `change.undone` (the original),
  `identity.state` with `change_type:"undo"`, `identity.hold_added`, `identity.hold_released`.

### 4b. Per-item undo, run undo, Manual (`undo_items.go`, `undo_run.go`, `manual.go`)

* **Items** of `update` (and undo-of-update) and `cloud_groups` changes: keys `attribute:<name>`, `group_add:<DN>`,
  `group_remove:<DN>` (the DN the script reported), `ou`, `cloud_add:<id>`, `cloud_remove:<id>` (lower-case object id);
  item JSON `{key,kind,label,from,to,selectable,undone_by?,pending_undo?,manual,hold_id?,group?,attribute?}`. Other types
  are `whole_only`. The inverse, `expect` (the DN only when `ou` is selected), steps, holds and `preview_hash` cover the
  selected items; selecting all = a whole undo (`payload.undo.items` empty). Per item: `change_undo_items(change_id,
  item_key, undo_change_id)` (migration 7) → change JSON `undone_items:{key:undoId}`, `undo_pending_items:{key:undoId}`;
  `undone_by` only when every item is undone. An open undo blocks only its items (409); an undone item is a 400.
* **Run undo**: one undo change per selected change, sharing `payload.undo.batch`; ≤ 200 per batch; `confirm` =
  `UNDO <count>`; `manual` (default true) pins every offered hold. Batch approve re-runs every dry run and refuses the
  whole batch (409) if any would overwrite a later change it was not forced to, or fails; second-person rule applies.
  Audit `undo.batch_requested|approved|rejected` (entity `undo_batch`).
* **Manual** = `identity_holds` (UI wording). Kind `all` = nothing is planned for the person (incl. disable and delete);
  `state` = no create, link, disable, enable, delete. Pins without `until` last until released ("Set back to Auto").
  A real sync (never the impact preview) records what a pin stopped in `identity_hold_state` (migration 7) as
  `suppressed:{summary:"would set title to 'Lead'",at}`, written only when the text changes. An inactive person under
  `all`/`state` is not disabled: run counter `manual_not_disabled`, `identity_issues` notification, warning in `/managed`.

### 4c. Leavers: termination dates and retention (`sources/termination.go`, `engine/retention.go`)

* **Termination date** (source options `termination_date_field`, `termination_date_format` `""|auto|<Go layout>`,
  `termination_after_days` 0-3650): terminated from the day after date + N days, in `schedule.timezone`. `auto` reads
  YYYY-MM-DD, ISO date-times, MM/DD/YYYY, DD.MM.YYYY and Excel serial numbers. Unreadable ⇒ ignored, warning (identity
  issues notes). A future date only sets `leave_date`. Terminated if status, date or missing says so; `identity.inactive`
  reason `termination_date` {field, date}.
* **Resolution** each sync: the person's assignment (`retention_policy_id`; an existing, enabled policy, or `default`),
  else the first enabled policy by `priority` whose conditions match (a policy with no conditions never matches by
  itself), else **Default** = `engine.delete_after_days` + `ad.disabled_ou`. Conditions use the role-rule model and
  evaluator over the last source record plus `ad.memberOf` (any group, by DN or CN), `ad.ou`, `ad.sam`, `source`,
  `origin`. Changes of the resolved policy ⇒ `identity.retention_changed` (not on the first resolution).
* **Planning** (`planDeactivate`): move to `policy.disabled_ou || ad.disabled_ou`; keep `lifecycle.disabled_keep ∪
  policy.extra_keep_groups`; delete when now ≥ `disabled_at + delete_after_days` (0 = never); `approval:"manual"` ⇒ the
  delete is created pending ("HELD for manual approval: retention policy …"). Manual pins `all`/`state` still suppress
  disable and delete. The AD snapshot reads every policy's disabled OU (sync, `/managed`, impact preview), so moved
  accounts stay tracked. The impact preview accepts a proposed `retention_policies` list.

## 5. Sync algorithm (engine.Run)

1. Single-flight lock (mutex + `sync_runs.status='running'` check). Create run.
2. Fetch every enabled source. **A failing source is skipped (its identities are left untouched and never marked
   missing) and the run finishes with status `error`**; other sources are still planned. Zero records from a
   source that previously had records ⇒ that source fails. Duplicate `external_id` in a feed ⇒ those records
   are skipped (counter `skipped_duplicate_id`), existing identities left as they are.
3. Upsert `identities` (attrs, hash, last_seen). Determine `active`: `status_field` ∈ `terminated_values` ⇒
   inactive; absent from feed for ≥ `missing_grace_runs` runs and `missing_means_terminated` ⇒ inactive.
4. Take AD snapshot (`Get-IDLSnapshot.ps1`): managed users by `id_attribute` within `search_bases` **plus
   `disabled_ou`** (so disabled accounts stay visible), with needed properties and `memberOf`; resolve all
   role-rule groups to DNs. An empty snapshot while accounts are linked aborts the run. With `correlation.enabled` the snapshot also returns
   accounts with an empty `id_attribute` (`includeUnlinked`) and the attributes the correlation rules read plus `adminCount`.
4a. Correlation (`engine.correlate`): for every active record without an account by ID (and `link_decision != new`) the candidate accounts are
   those not ignored (`ignore_accounts`), not privileged (`adminCount=1`), not in `link_rejects`, and not linked to an *active* record of the
   *same* source (another source = contractor -> employee, an inactive record = rehire). A candidate needs every `required` rule; `full` =
   every rule. One full candidate alone ⇒ chosen; one full among others or one partial ⇒ chosen + `verify`; else nothing chosen. An account
   chosen for two records is chosen for neither. `link_decision = guid:X` ⇒ that account only, chosen. Records owning an account under
   discussion get no plan this run (`held_for_link`). Merged records are skipped (`merged`).
5. For each identity compute desired state: attributes (global mappings overlaid by matching role-rule
   attribute grants, higher `priority` wins), groups (union of matched rules plus `lifecycle_groups.active_add`; a disable removes groups by `lifecycle_groups.disabled_remove` - `all_except` the `disabled_keep` and `disabled_add` groups, only `managed` groups, or `none` - and adds `disabled_add`, which an enable removes again), OU (`engine.ResolvePlacement`: the
   identity's `ou_override` while its placement value is unchanged → highest-priority role rule with an OU → `placement.rules`
   for the placement field (exact, then `*` wildcards) → `placement.unmapped`: `hold` = `needs_ou` or `default` = `ad.default_ou`;
   with placement off: role rule OU, else `ad.default_ou`), then diff vs AD (§1 decision 5) → `create|update|disable|enable|delete`.
   With `placement.move_on_change` an update moves the account only when the placement value differs from the one recorded
   in `applied_json` (key `~placement`, set when a create/move is applied; first seen = recorded without moving).
4b/5b. Cloud groups (`engine/cloud.go`, only with `cloud.enabled`): group references `entra:<object id>` in role grants and lifecycle lists never
   reach the AD snapshot or the AD scripts. After the AD snapshot, `Get-IDLCloudSnapshot.ps1` reads the synced Entra users and the members and
   kind of every referenced cloud group. The AD account is matched to its Entra user by `cloud.user_match`: `sid` = `onPremisesSecurityIdentifier`
   equals the snapshot's `sid`, `immutable_id` = base64 of the objectGUID bytes, `upn`. Not found yet ⇒ nothing planned (counter
   `cloud_waiting`). Per person ONE `cloud_groups` change:
   - active: add = cloud refs of matched rules + `active_add` not held; remove the managed cloud groups no longer granted
     (`remove_stale_groups`);
   - disable: `all_except` ⇒ `cloud_remove_all` with `cloud_keep` = keep + disabled_add (planned once; the identity carries
     `~cloud_removed_all` until a rehire), `managed` ⇒ remove the managed cloud groups held, `none` ⇒ nothing; plus add the `disabled_add`
     cloud refs;
   - enable: remove the `disabled_add` cloud refs held.

   Missing or non-manageable groups (dynamic, role-assignable, synced from AD, Exchange Online off) ⇒ one warning per run and never planned.
   A failed cloud snapshot ⇒ counter `cloud_errors` and a run error, no cloud changes planned (open ones are not superseded), and the AD
   work continues. The bulk-destructive guard also counts cloud removals of people who are no longer active. The executor runs
   `cloud_groups` after the disables.
5a. Name check (`engine.checkNames`): one `Test-IDLNames.ps1` call for every candidate of every create (sam, upn, mail, cn in the OU); names
   claimed by earlier creates of the run (ordered by external id) count as taken; taken candidates are dropped, none left ⇒ `needs_name` +
   `name_conflicts`. Link renames are checked the same way with the account itself excluded (none free ⇒ no rename, `rename_note`).
   When AD cannot be asked the plans are left as they are (warning).
6. Dedup against open changes (same identity+type+payload_hash ⇒ keep; different ⇒ supersede old pending).
7. Apply approval policy / guard / dry-run to set initial status; write audit rows.
8. Executor processes `approved` changes, grouped by type, in order link → create → update → enable → disable →
   delete, one PowerShell invocation per type-batch. Results update changes + identities.
9. Finish run with counters (`total, created, updated, disabled, enabled, deleted, pending, failed,
   skipped, unchanged, source_errors`, `on_hold` = items a Manual pin kept out of the plan, `manual_not_disabled` = people who left but are Manual).

Identity events (`internal/engine/history.go`; audit rows with entity `identity`, actor `system`, detail always has `external_id` and
`source_id`), written only when something actually changed, never on an unchanged sync:
`identity.first_seen` {source, source_type, origin `source|manual`, created_by (manual), active, fields} ·
`identity.source_changed` {source, changes:{field:{from,to}}} (a field that disappeared ⇒ `to:null`) ·
`identity.inactive` {reason `terminated` + status_field/status, or `missing` + missing_runs} · `identity.reactivated` ·
`identity.state` {from, to, ad_guid/ad_dn/ad_sam, previous_* when unlinked, change + change_type, or `observed:true`} ·
`identity.account_missing` {ad_guid, ad_dn, ad_sam, previous_state} (once per disappearance of a linked account from the snapshot) ·
`identity.account_deleted` {last known ad_guid, ad_dn, ad_sam, ad_upn, attributes = the last applied values}.
`change.create` and `change.applied|failed` rows carry `identity_id`.

Naming (new accounts): templates `naming.sam_template`, `naming.upn_template` (local part), `naming.cn_template`,
and optional `naming.mail_template` (local part; `@naming.mail_domain`, default `ad.upn_suffix`; skipped when a mapping writes `mail`),
rendered with the source attributes. Candidates by `naming.conflict_mode` (`engine.modeCandidates`): `manual` = the base names only; `number` =
base, 2..`max_candidates`; `random` = base + `conflict_random_digits` digits from sha256(identity id, i) (stable per person); `template` = base +
`conflict_suffix_template` (letters/digits only). An identity's `name_override` replaces them in every mode. The sAMAccountName is truncated to 20 chars
*before* the suffix; UPN, mail and CN get the same suffix. The script still picks the first candidate free in the whole directory (race safety).
After a link, the correlation report (`engine.CorrelationReport`, settings row `state:correlation_report`) lists enabled, non-privileged, not ignored
accounts that no record accounts for and nobody was proposed; accounts not in the previous report are sent with `identity_issues`.
Manual sources (`sources.TypeManual`) read `manual_people` through `sources.BuildFor`; each row = the form fields + `ExternalID`, `Status`
(`Terminated` when ended or `end_date_field` < today) and `Source=manual`; the feed guards (empty / shrunk feed) do not apply to them.

Template language: Go `text/template` (no I/O). Data = source attribute map, so `{{ .FirstName }}` or
`{{ index . "currentStatus.statusType" }}`. Missing keys render empty. Functions: `lower upper title trim
first(s,n) last(s,n) substr(s,start,len) replace(s,old,new) default(fallback,v) coalesce(a,b,...) join(sep,
a,b..) ascii(s) digits(s) alnum(s) split(s,sep) index-able regexReplace(pattern,repl,s) regexFind(pattern,s) regexMatch(pattern,s)`.
Pipe style works: `{{ .First | lower }}`. Regexes are RE2 (linear time), ≤ 256 characters, compiled once (bounded cache).
The initial-password template (`new_user.password_mode=template`) also has `random(n) randomDigits(n) randomSymbols(n)` (crypto/rand,
1-64); it is rendered in the executor from the identity's current attributes, never stored, checked by
`engine.CheckInitialPassword` (8-128 chars, 3 of 4 classes, not containing the sAMAccountName) and forces change at logon.

## 6. PowerShell contract (Go <-> scripts)

Invocation: `pwsh.exe -NoProfile -NonInteractive -ExecutionPolicy <policy> -File <scripts_dir>\<name>.ps1 [-DryRun]`
(DryRun only on action scripts). Request JSON (UTF-8, no BOM) on stdin; **exactly one JSON document on
stdout**; human logs go to the information stream / log files, never stdout. Exit codes follow the style
guide: 0 ok, 1 partial (some items failed), 2 invalid input, 3 dependency missing, 4 auth/connection, 5 fatal.
On exit codes 0/1 stdout holds the full result; on others it holds `{"ok":false,"error":"..."}` where possible.
Scripts log to `$PSScriptRoot\Logs\<Script>_<ts>.log` and write the standard CSV report
(`<Script>_Report_<ts>.csv`) for action scripts. **Never log passwords.**
Optional `server` (domain controller) in every request ⇒ pass `-Server` to AD cmdlets when non-empty.

### 6.1 `Get-IDLSnapshot.ps1` (report-only, no DryRun)
Request:
```json
{"server":"", "searchBases":["OU=Staff,DC=corp,DC=example,DC=com"], "idAttribute":"employeeID",
 "properties":["givenName","sn","title","department","manager"], "groups":["GG-AllStaff","CN=Marketing,OU=Groups,DC=corp,DC=example,DC=com"]}
```
Response (`ok:true`): users = every user under searchBases whose `idAttribute` is non-empty (enabled or not):
```json
{"ok":true,
 "users":[{"objectGuid":"...","dn":"CN=...","sam":"jdoe","upn":"jdoe@corp.example.com","enabled":true,
           "parentOu":"OU=Staff,DC=...","externalId":"E1001",
           "attributes":{"givenName":"Jane","sn":"Doe","title":null},
           "memberOf":["CN=GG-AllStaff,OU=Groups,DC=..."]}],
 "groups":[{"identity":"GG-AllStaff","dn":"CN=GG-AllStaff,OU=Groups,DC=...","found":true}]}
```
Attribute values are strings (null when unset). Non-string AD values are rendered as invariant strings.
Optional `"includeUnlinked":true` (sent when correlation is on) also returns users under searchBases whose `idAttribute` is empty (`externalId:""`).

### 6.1a `Get-IDLSchema.ps1` (report-only, optional)
Used only by the field mapper (`GET /api/mapper/ad-attributes`); syncs never call it, and when it is missing the GUI uses
`ad.BuiltinUserAttributes`. Request `{"server":"", "className":"user"}`. Two one-level queries of the schema partition
(all `classSchema`, all `attributeSchema`); the class's `(system)mustContain`/`(system)mayContain` are followed through
`subClassOf` and `(system)auxiliaryClass`. Response:
```json
{"ok":true,"className":"user","attributes":[{"name":"manager","description":"<adminDescription>","syntax":"dn",
  "singleValued":true,"systemOnly":false,"constructed":false,"linkId":42,"rangeUpper":0}]}
```
`syntax` ∈ `string dn integer boolean time binary oid other` (from `attributeSyntax`); `constructed` = `systemFlags & 4`;
odd `linkId` = back-link. Mappability is decided in Go (`engine.ClassifyAttribute`: protected list, constructed, back-link,
system-only, multi-valued, non-text syntax ⇒ not mappable; `dn` ⇒ hint to use `user_ref`).

### 6.1b `Get-IDLOUs.ps1` (report-only, optional)
Used only by the OU picker (`GET /api/ad/ous`). Request `{"server":"", "searchBases":[]}` (empty = `defaultNamingContext`); one subtree
query per base for `objectClass=organizationalUnit`. Response `{"ok":true,"ous":[{"dn":"OU=Finance,...","name":"Finance"}]}`; Go sorts it as a tree.

### 6.1c `Test-IDLNames.ps1` (report-only)
Used by the name check, *Set username* and the manual-entry preview. Request `{"server":"","names":[{"field":"sam|upn|mail|cn","value":"...","ou":"OU=... (cn only)"}],
"exclude":["<objectGUID>"]}`; sam and upn are looked up directory-wide on every object class, mail as `mail` or `proxyAddresses=smtp:<value>` (also
contacts and groups), cn one level below `ou`; objects in `exclude` never count. Response `{"ok":true,"taken":[{"field","value","dn","objectGuid"}]}`.

### 6.1d `Search-IDLGroups.ps1` (report-only)
Used only by the group picker (`GET /api/ad/groups`). Request `{"server":"","query":"fin","max":25}`; query 2-64 characters, `*` is the only
wildcard (every other LDAP special character is escaped and matched literally), matched as `*query*` against cn, sAMAccountName,
displayName and description across the whole domain. Response `{"ok":true,"groups":[{"dn","sam","name","description",
"category":"security|distribution","scope":"global|universal|domainlocal","privileged":bool}],"truncated":bool}` sorted by name;
`privileged` = adminCount=1 or a protected well-known RID (512/518/519/544/548/549/550/551). Never changes anything.

### 6.2 Action scripts. Common request envelope
```json
{"server":"", "idAttribute":"employeeID", "items":[ {...} ]}
```
Common response:
```json
{"ok":true, "dryRun":false, "results":[{"changeId":"...","status":"applied|would|skipped|failed",
  "message":"...", "objectGuid":"...", "dn":"...", "sam":"...", "upn":"...", "error":null}],
 "summary":{"total":1,"applied":1,"would":0,"skipped":0,"failed":0}}
```
In DryRun, successful items return `status:"would"` and nothing is modified. `ok` is false only for
script-level failure. Per-item failure ⇒ `status:"failed"` + `error`, exit code 1 if any failed.
Every item also carries `externalId`; for existing accounts the script **verifies the account's idAttribute
equals externalId** and fails the item otherwise.

**Undo support** (§4a). Results of `applied` and `would` items carry `before`, the account as the item found it (read live; not on
`skipped` items or failed AD items):
* Set / Disable / Enable-IDLUser: `{"enabled":bool,"dn":"...","attributes":{"<name>":"<old>"|null},"groupsAdded":[DN],"groupsRemoved":[DN]}` —
  only attributes and groups that actually changed (no-ops, kept privileged groups and unknown groups are left out).
* Set-IDLLink: `{"idValue":"<old or ''>","dn":"...","names":{"sam","upn","cn","mail","proxyAddresses":[..]}}` (`names` only when renamed).
* New-IDLUser: `{"created":true}`. Set-IDLCloudGroups: `{"cloudAdded":[ids],"cloudRemoved":[ids]}` (also on a partly failed item).

Optional item fields: **`expect`** on Set, Disable, Enable and Set-IDLLink, every key optional:
`{"enabled":bool,"dn":"...","attributes":{"name":"value"|null},"memberOf":[DN|sam],"notMemberOf":[DN|sam],"idValue":"...","names":{"sam","upn","cn","mail"}}`,
checked before any change (DryRun too); a mismatch fails the item with nothing written and
`error:"drift: title is 'Lead', expected 'Senior'; no longer a member of GG-X; account is disabled, expected enabled"` (empty values as `empty`).
**`force:true`** skips the check (WARN logged). Disable and Enable accept **`set` / `clear`** like Set-IDLUser (Disable checks them before
disabling; `description` together with `description` in set/clear is refused). Set-IDLLink accepts `externalId:""` with a non-empty
`expectedExternalId` (clears the ID attribute = unlink) and **`proxyAddresses`** (exact list, set with the rename).

* **`New-IDLUser.ps1`** item: `{"changeId","externalId","ou":"OU=...","candidates":[{"sam","upn","cn","mail"?}],"attributes":{"givenName":"Jane","sn":"Doe","displayName":"Jane Doe","title":"..."},"groups":["GG-AllStaff"],"password":"<generated>","enabled":true,"changePasswordAtLogon":true}`.
  Idempotent: if an account with `idAttribute == externalId` already exists ⇒ `status:"skipped"`, `message:"exists"`, returns its guid/dn/sam/upn (no changes). Otherwise picks the first candidate whose sam AND upn are free (and whose `mail`, when given, is not used as `mail` or `proxyAddresses` by any object)
  directory-wide (and whose `mail`, when given, is not used as `mail` or `proxyAddresses` by any object) and whose cn is free in the OU; creates the user (attributes applied via `-OtherAttributes`, the `idAttribute` set to `externalId`), then adds groups (an unresolvable group ⇒ item `failed` after creation with message listing it, account kept; result still includes guid/dn). A privileged or ambiguous group in `groups` fails the item **before** the account is created (`refusing to add <group>: it is a protected administrative group`).
* **`Set-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid","set":{"title":"New"},"clear":["telephoneNumber"],"addGroups":["..."],"removeGroups":["..."],"moveTo":"OU=...|"" }`. Uses `-Replace`/`-Clear` with LDAP names. No-ops are `skipped`. Returns the new dn. A privileged or ambiguous group in `addGroups` or `removeGroups` fails the item before any change.
* **`Disable-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid","moveTo":"","removeGroups":[],"addGroups":[] (applied after the account is disabled),"description":"Disabled by IDLifecycle 2026-10-16"}`. Idempotent (already disabled ⇒ still applies move/groups/description, status `applied` or `skipped` if nothing to do). The account is **disabled first**, before anything else can fail: an invalid/nonexistent `moveTo` ⇒ `failed` with `message:"failed after: disabled; ..."`. Privileged (adminCount=1 / well-known RID) and ambiguous groups in `removeGroups` are **kept**, never a failure: WARN log, `Skipped` report row, and `message` ends with `kept privileged group(s): <names>` (or `kept ambiguous group(s): ...`).
* **`Enable-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid","moveTo":"","addGroups":[],"removeGroups":[]}` (`removeGroups` = the `disabled_add` groups of a rehire). A privileged or ambiguous `addGroups` entry fails the item before any change. A privileged or ambiguous `removeGroups` entry is kept (WARN log, `Skipped` row, "kept ... group(s)" note), and unknown ones are ignored, so removals never block the enable. Disable `addGroups` likewise reports privileged, ambiguous and unknown groups but never fails.
* **`Set-IDLLink.ps1`** item: `{"changeId","objectGuid","externalId","expectedExternalId","rename":{"sam","upn","cn","mail"}|null}`. Finds the account by GUID;
  refuses (`failed`) when its `idAttribute` is not `expectedExternalId` (`""` = must be empty), when another object already has `externalId`, when
  the account is privileged (adminCount=1 or a protected group), or when a new name is taken by another object. Sets `idAttribute`, then renames:
  sAMAccountName, UPN, mail (+ `proxyAddresses`: new primary `SMTP:`, the old primary kept as `smtp:`), CN via `Rename-ADObject`.
* **`Remove-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid"}`. **Refuses** (`failed`, "account is enabled") if the account is enabled; refuses if object is not a user. Already gone ⇒ `skipped`. Deletes with `Remove-ADObject -Recursive` so leaf children (e.g. `CN=ExchangeActiveSyncDevices`) go with the account; refuses (`failed`) if any child is a user/computer/group/inetOrgPerson.

### 6.3 Cloud scripts (Entra ID / Microsoft 365 / Exchange Online; optional)
Every request carries the connection, on stdin only: `{"cloud":{"environment":"global|usgov|usgovdod|china","tenantId","clientId",
"auth":"certificate|secret","certThumbprint","clientSecret" (only with auth=secret),"exchange":{"enabled","organization"}}, ...}`.
`IDLifecycle.Cloud.psm1` has no Graph SDK dependency. It gets a client-credentials token: with a certificate, a JWT client assertion signed
RS256 with the key from `Cert:\LocalMachine\My` (falls back to `CurrentUser\My`); the header carries `x5t` and `x5t#S256`, the claims
`aud`, `iss`=`sub`=client id, `jti`, `nbf` and `exp` (10 minutes). Graph is called with `Invoke-RestMethod`: paging by `@odata.nextLink`,
and 429/503/504 are retried up to 5 times, honouring `Retry-After` (capped at 60 s). The Graph base URL can be overridden only with
`IDL_CLOUD_TEST=1` (tests). Exchange Online: `Connect-ExchangeOnline` with the certificate object (a machine-store key works for a service
account), limited to the four group cmdlets, disconnected in `finally`. The token and the secret are never logged.
Exit codes: 0 ok, 1 item failures, 2 bad input, 3 dependency (certificate unusable, ExchangeOnlineManagement missing), 4 sign-in failed /
401 / 403 / 5xx after retries.

Group kinds: `security`, `m365` (Unified), `mail_security`, `distribution`. Not manageable (`manageable:false`, `reason`): `dynamic`,
`role_assignable`, `synced` (onPremisesSyncEnabled - managed in AD), `exchange_off` (mail kinds while Exchange Online is off), `unverified`
(`isAssignableToRole` not readable - grant Group.Read.All), `unsupported`, and `not_found` (id lookups / snapshot).
* **`Test-IDLCloudConnection.ps1`** `{cloud}` → `{"ok":true,"healthy":bool,"checks":[{"name","ok","warning","detail"}]}`. The checks run
  in this order: `certificate` (private key, CSP when Exchange is on, warning < 30 days to expiry), `token`, `graph_permissions` (the
  token's `roles` must hold GroupMember.ReadWrite.All, Group.Read.All and User.Read.All; broader ones ⇒ warning), `graph_read` (/groups and
  /users), `exchange_module`, `exchange_connect`, `exchange_permissions`.
* **`Search-IDLCloudGroups.ps1`** `{cloud,query,max}` (query 2-64, max 1-100, default 25; `$search` on displayName and mail with
  ConsistencyLevel eventual) or `{cloud,ids:[1-50]}` (each `GET /groups/{id}`, in the requested order, with `found`); both at once ⇒ exit 2 →
  `{"ok":true,"groups":[{"id","name","description","mail","kind","manageable","reason"}],"truncated"}`.
* **`Get-IDLCloudSnapshot.ps1`** `{cloud,groups:[ids, ≤1000],match,includeUsers}` → `{"ok":true,"users":[{"id","upn","sid","immutableId",
  "enabled"}] (users with onPremisesSyncEnabled), "groups":[{"id","found","name","kind","manageable","reason","members":[user ids]}]}`.
  Members are direct user members only. Read-only.
* **`Set-IDLCloudGroups.ps1`** (`-DryRun`) items `{"changeId","externalId","userId","add":[],"remove":[],"removeAll":false,"keep":[]}`:
  - every group is re-read live and non-manageable ones are refused, so that group fails and the rest of the item continues; the item is
    `failed` when any group failed;
  - `security`/`m365` go through Graph (`members/$ref`), `mail_security`/`distribution` through Exchange Online
    (`Add-/Remove-DistributionGroupMember -BypassSecurityGroupManagerCheck`, the group found by `ExternalDirectoryObjectId`);
  - "already a member" and "not a member" are fine (`skipped` when nothing changed);
  - `removeAll`: every manageable group the user is a DIRECT member of (`transitiveMemberOf` filtered by a direct-membership check), except
    `keep`; role-assignable, dynamic and synced groups stay and are named in the message;
  - an Exchange Online failure fails only the Exchange groups.

  Results use the action-script shape, with `upn` set (`objectGuid`/`dn`/`sam` null).
* **`New-IDLCloudCertificate.ps1`** (admin tool, elevated; not a stdin script): self-signed RSA-2048 CSP certificate ("Microsoft Enhanced RSA
  and AES Cryptographic Provider", non-exportable key) in `LocalMachine\My`. It grants the service account (`-ServiceAccount`, default the
  `-ServiceName` service's log-on account) Read on the key and writes only the public `.cer` to `-OutDir`. Other parameters: `-Subject`,
  `-Years` 1-5, `-StoreLocation`, `-DryRun` and `-Json`.
`Get-IDLSnapshot.ps1` returns `sid` (objectSid, `S-1-5-...`) for every user, so `cloud.user_match = sid` can be matched.

## 7. HTTP API (JSON, snake_case)

Auth: cookie `idl_session` (HttpOnly, SameSite=Strict, Secure when TLS). Every non-GET request must send
`X-CSRF-Token` (value from `/api/auth/me` or login response). Errors: HTTP status + `{"error":{"code":"...","message":"..."}}`.
Lists: `{"items":[...],"total":N}` with `?limit=&offset=` (default 50, max 500). Roles: `viewer` read; `approver` +approve/reject/retry; `admin` everything.

| Method & path | Role | Notes |
|---|---|---|
| `POST /api/auth/login` `{username,password}` | none | → `{user:{id,username,role,auth_source},csrf_token}`; Entra ID accounts are refused like a bad password; 403 `local_login_restricted` for a correct password of a local non-admin when `local_login=admins_only` |
| `POST /api/auth/logout` · `GET /api/auth/me` · `POST /api/auth/password {current,new}` | any | password change is refused (400) for Entra ID accounts |
| `GET /api/auth/providers` | none | `{local_login,oidc:{enabled,label}}` for the login page |
| `GET /api/auth/oidc/start` · `GET /api/auth/oidc/callback` | none (browser navigation) | 302 to Entra ID / HTML page; errors are HTML pages, refusals are audited as `auth.login_failed` with `method:"oidc"` |
| `POST /api/auth/oidc/test` `{auth:{...}}` | admin | unsaved settings: discovery for the tenant, then a client-credentials token request (validates client ID + secret, needs no API permission) → `{ok,error}`; stores nothing |
| `GET /api/status` | viewer | `{version,db_driver,mode:"live|dry_run",scheduler:{enabled,next_run},running_run,pending_changes,last_run}` |
| `GET /api/source-types` | viewer | `[{type,label,fields:[{name,label,type:"string|password|int|bool|text|select",required,default,help,options:[..] (select),show_if:{field,values} (shown only when that field has one of the values)}]}]`; `rest` = the generic HR API connector, `manual` = people entered in the console |
| `GET/POST /api/sources`, `GET/PUT/DELETE /api/sources/{id}` | admin (GET viewer) | source: `{id,name,type,enabled,options,config,last_sync_at,last_status,last_error}`; `options:{external_id_field,status_field,terminated_values:[],missing_means_terminated,missing_grace_runs,termination_date_field,termination_date_format,termination_after_days}`; secrets returned as `"********"`, sending `"********"` back keeps stored value |
| `POST /api/sources/{id}/test` · `POST /api/sources/test` | admin | the second tests unsaved settings (same body as a save, optional `id` whose stored secrets fill masked fields; nothing stored; audited `source.test`);  fetches; → `{ok,count,attributes:[names],sample:[{...}] (first 5),error}` |
| `GET/POST /api/mappings`, `GET/PUT/DELETE /api/mappings/{id}` | admin (GET viewer) | `{id,name,target_attr,template,mode:"create_only|sync|immutable",on_empty:"skip|clear",value_type:"string|user_ref",enabled,sort_order,description}` |
| `POST /api/templates/preview` | admin | `{template, record:{...}, kind?:"password"}` → `{ok,output,error}` (+ `check`: why AD would refuse a password, `"` = fine); the record is typed by the admin, never a real identity |
| `POST /api/mapper/sources/{id}/fields` | admin | fetches the source once → `{ok,error,count,id_field,status_field,fields:[{name,filled,samples:[≤3, ≤60 chars],used_by:[ref],role:"external_id|status"}],references:[{kind:"mapping|role",id,name,target_attr,enabled,fields,dynamic,error,missing:[],suggestions:{missing:"OtherCase"}}]}` (audited as `source.fields`) |
| `GET /api/mapper/references` | admin | `{references:[{kind,id,name,target_attr,enabled,fields,dynamic,error}]}` — the source fields each mapping / role rule reads (`engine.TemplateFields`: `.X`, `$.X`, `index . "k"`; `dynamic` when a key is computed), no source fetch |
| `GET /api/mapper/ad-attributes?refresh=1` | admin | `{source:"live|builtin",live_error,fetched_at,attributes:[{name,description,syntax,single_valued,mappable,reason,hint}]}`; live schema cached 30 min per `ad.server` |
| `GET/POST /api/roles`, `GET/PUT/DELETE /api/roles/{id}` | admin (GET viewer) | `{id,name,description,enabled,priority,match:"all|any",conditions:[{attr,op,value,conditions?}],grants:{groups:[],ou,attributes:[{target_attr,template,mode,on_empty,value_type}]}}`; ops: `eq ne in not_in contains not_contains starts_with ends_with regex not_regex exists empty gt gte lt lte older_than_days newer_than_days template` (attr `*`, value = a template that renders true/yes/1) and the groups `all_of` / `any_of` (children in `conditions`, depth <= 3, <= 50 conditions per rule); validation errors name the path (`conditions[1].conditions[0].value`) |
| `POST /api/roles/preview` | admin | `{record:{...}, rule?:{...unsaved rule...}}` → also `explain:[{path,attr,op,value,actual,matched}]` and `rule_matched` for the unsaved rule; otherwise → `{matched:[{id,name}],groups:[],ou,attributes:{attr:value}}` |
| `GET/PUT /api/settings` | admin (GET viewer) | see §8; notification secrets are returned as `"********"` (sending it back keeps the stored value) |
| `POST /api/notifications/test` `{notifications:{...}}` | admin | sends a test message with the unsaved form values (masked secrets fall back to stored ones) → `{ok,error}` |
| `GET /api/changes?status=&type=&q=` · `GET /api/changes/{id}` | viewer | change: `{id,run_id,identity_id,external_id,display,type,status,payload,summary:[strings],created_at,decided_by,decided_at,decision_note,applied_at,error,attempts}` |
| `POST /api/changes/{id}/approve|reject|retry` `{note}` | approver | reject requires note |
| `POST /api/changes/bulk` `{ids:[],action:"approve|reject",note}` | approver | → `{results:[{id,ok,error}]}` |
| `POST /api/changes/{id}/ou` `{ou,add_rule}` | approver (`add_rule`: admin) | pending/simulated create, update or enable: sets the OU / move target, clears `needs_ou`, remembers it for the person (`SetIdentityOU`); `add_rule` also adds the placement rule for the person's value; audited `change.set_ou` |
| `GET /api/ad/groups?q=&max=` | admin | live group search (`Search-IDLGroups.ps1`): q 2-64 characters, max 1-100 (default 25) → `{groups:[{dn,sam,name,description,category,scope,privileged}],truncated}`; when AD cannot be asked: 200 with `error` and no groups |
| `GET /api/cloud/groups?q=&max=` | admin | live Entra ID / Microsoft 365 group search (`Search-IDLCloudGroups.ps1`): q 2-64, max 1-100 → `{groups:[{id,ref:"entra:<id>",name,description,mail,kind:"security|m365|mail_security|distribution",manageable,reason:"dynamic|role_assignable|synced|exchange_off"}],truncated}`; connection off or Graph error: 200 with `error` and no groups |
| `GET /api/cloud/groups/resolve?ids=a,entra:b` | admin | names and kinds for 1-50 cloud group ids (last cloud snapshot first, then a lookup); unknown ids are left out |
| `POST /api/cloud/test` | admin | `Test-IDLCloudConnection.ps1` with the saved connection, or with an unsaved `{"cloud":{...}}` body (a masked secret = the saved one); nothing saved → `{ok,checks:[{name,ok,detail}]}`; audited `cloud.test` without secrets |
| `POST /api/preview/impact` | admin | impact preview: body `{roles?,mappings?,settings? (partial, read over the saved settings; AD scope fields are kept as saved),refresh,q,only_changed (default true),type,offset,limit (<= 500)}` → `{generated_at,snapshot_at,people,names_checked:false,notes,summary:{changed_people,baseline,proposed,groups_added:[{group,people}],groups_removed,attributes:[{attr,people}]},items:[{identity_id,external_id,display,ad_sam,state,difference:"new|changed|gone|same",baseline,proposed,lines:[{op:"+|-",text}]}],total}`; a side may carry `also:["cloud_groups"]` when the person has an AD change and a cloud change (lines cover both, `type` matches either; cloud groups are labelled `Entra: <name>`); the connection itself is always the saved one (switching it on/off can be previewed, other edits get a note); plans everyone with the saved and the proposed configuration on one AD (and cloud) snapshot (reused <= 15 min, `refresh` forces one), writes nothing, never checks names; 409 `busy` (another preview) or `running` (a sync); audited `preview.impact` |
| `GET /api/ad/ous?refresh=1` | approver | `{ous:[{dn,name}],read_at,error}` below `ad.search_bases`; cached 30 min |
| `GET /api/placement/values?field=` | viewer | `{field,values:[{value,people,ou}]}` over active identities, unmapped first |
| `GET /api/changes/{id}/names` | approver | pending create: `{current:Candidate,conflicts:[{field,value,dn,with}],suggestions:[{mode:"number|random|template",candidate}]}` (≤ 6, checked free) |
| `POST /api/changes/{id}/name` `{sam,upn,cn,mail?}` | approver | format + AD + other open creates checked; 409 `{"error":{"code":"name_taken",...},"conflicts":[...]}`; clears `needs_name`, remembers the names (`identity_choices`); audited `change.set_name` |
| `POST /api/ad/check-names` `{names:[{field,value,ou?}]}` | approver | ≤ 20 names → `{taken:[NameConflict]}` (AD + open creates) |
| `POST /api/changes/{id}/link` `{object_guid}` | `{create_new:true}` | approver | pending link: choose a candidate (planned like the next sync, remembered as `link_decision`), or reject it and set `link_decision=new`; audited `change.set_link` |
| `POST /api/identities/{id}/link` `{object_guid}` | `{clear:true}` | approver | sets/clears `link_decision` of a record without an account; audited `identity.link_decision` |
| `GET /branding/brand.json` · `GET /branding/logo` · `GET /branding/favicon.ico` | none | `{product_name,accent,custom}`; the logo (svg or png) and icon from the override folder or the defaults; `no-cache`, `nosniff`; SVG with `default-src 'none'; style-src 'unsafe-inline'; sandbox` |
| `GET /api/correlation/report` | viewer | `{run_id,generated_at,enabled,uncorrelated:[{object_guid,dn,sam,upn,display,enabled,external_id}],verify:[{change_id,identity_id,display,external_id,candidates}],needs_name:[...],unmatched_people}`; accounts ignored since the run are filtered out |
| `GET /api/manual/sources` · `GET /api/manual/sources/{sid}/people` | viewer | `[{id,name,enabled,id_prefix,fields:[{name,label,required,kind:"text|date|email|select",options,default}]}]`; people `[{id,source_id,external_id,attrs,ended,created_by,created_at,updated_by,updated_at,state,ad_sam}]` |
| `POST /api/manual/sources/{sid}/preview` `{attrs}` | approver | `{names,needs_name,ou,ou_reason,needs_ou,conflicts,warnings}`; nothing stored |
| `POST /api/manual/sources/{sid}/people` `{attrs,sync}` · `PUT .../people/{pid}` `{attrs}` · `POST .../people/{pid}/end` `{note}` | approver | values checked against the form (400 names the field); id = `id_prefix` + 6 random `[A-Z2-9]`; `sync:true` starts a run (`run_id`); audited `manual.create|update|end` |
| `DELETE /api/manual/sources/{sid}/people/{pid}` | admin | 409 once the person has an AD account (end them instead); audited `manual.delete` |
| `GET /api/backups` · `POST /api/backups` · `GET /api/backups/{name}` | admin | list `{folder,files:[{name,size,created}],enabled,interval_hours,keep,passphrase_set,next_due}`; back up now (201 file; 400 without a passphrase); download (audited) |
| `GET /api/runs` · `GET /api/runs/{id}` · `POST /api/runs` | viewer / admin | run: `{id,trigger,triggered_by,started_at,finished_at,status:"running|ok|error|aborted",counters:{...},error}`; POST → 202 `{id}` (409 if running) |
| `GET /api/identities?q=&state=` · `GET /api/identities/{id}` | viewer | `{id,source_id,source_name,external_id,display,active,state,ad_guid,ad_dn,ad_sam,first_seen,last_seen,disabled_at,attrs:{...},origin:"source|manual",created_by (manual),holds:[{id,identity_id,kind,key,until|null,reason,created_by,created_at,change_id}],ad_last_known:{guid,dn,sam,upn,reason:"deleted|missing",deleted_at} (only once the account is gone)}` |
| `GET /api/identities/{id}/history?limit=&before_seq=` | viewer | one person's timeline, newest first: the identity's own rows, the rows of its changes (joined on `changes.identity_id`, so older rows too) and of its `manual_people` record → `{items:[{seq,ts,actor,action,entity_type,entity_id,kind:"identity|change|console",title,lines:[..],change_id?,change_type?,detail}],next_before_seq (0 = no more)}`; `limit` default 50, max 200; titles and lines are plain English built server-side |
| `DELETE /api/identities/{id}/holds/{hid}` (`?note=` or body `{note}`) | approver | "Set back to Auto": releases a Manual pin → the hold; audited `identity.hold_released` |
| `POST /api/identities/{id}/holds` `{kind:"attribute|group|cloud_group|ou|state|all",key,reason (≥10),until?}` | approver | "Make Manual" without an undo → 201 hold `{id,identity_id,kind,key,until,reason,created_by,created_at,change_id,suppressed?:{summary,at}}`; audited `identity.hold_added` |
| `GET /api/identities/{id}/managed?refresh=1` | viewer | `{identity_id,snapshot_at,account,attributes:[{name,ad_value,desired,mode:"auto|manual",sync,hold?}],groups:[{group,name,member,desired,mode,hold?}],ou:{current,desired,mode,hold?},person:{mode:"auto|manual|state",hold?,active},holds,warnings}` from the cached AD snapshot (≤ 15 min; `refresh` forces one). `GET /api/identities?manual=1` = people with a pin in force; list items carry `manual_count` |
| `GET/POST /api/retention-policies`, `GET/PUT/DELETE /api/retention-policies/{id}` | admin (GET viewer) | list `{items:[policy],total,default_people}`; policy `{id,name,description,enabled,priority,match_mode,conditions,delete_after_days (0 = never),disabled_ou,extra_keep_groups,approval:""|"manual",created_at,updated_at,people}`; "Default" is a reserved name; deleting a policy clears assignments to it; audited `retention.create|update|delete` (entity `retention_policy`, before/after detail) |
| `POST /api/retention-policies/test` `{policy,identity_id}` or `{policy,record}` | admin | `{matched,explain:[{path,attr,op,value,actual,matched}],note?}` (AD groups from the cached snapshot) |
| `GET /api/retention/upcoming?days=&kind=delete|leaving&limit=&offset=` | viewer | `{items:[{kind,identity_id,display,external_id,ad_sam,disabled_at?,delete_due?,leave_date?,policy_id,policy_name,reason,assigned_by?,manual_hold}],total}`, soonest first, overdue deletes included |
| `PUT /api/identities/{id}/retention` `{policy_id:""|"default"|id,reason (≥10)}` | approver | `{identity_id,retention_policy_id,retention}`; audited `identity.retention_set`. Identity detail carries `retention:{policy_id,policy_name,reason:"assigned|rule|default",assigned_by?,delete_due?,delete_after_days,leave_date?,resolved_at}` and `leave_date` |
| `GET /api/runs/{id}/undo` | admin | `{run,changes:[{change,undoable,reason,whole_only,items}]}` |
| `POST /api/runs/{id}/undo/preview` `{selection:{changeId:["*"]|[keys]}}` | admin | `{run_id,people,count,changes:[{change_id,type,display,external_id,identity_id,confirm_text,items,steps,dry_run,drift,holds,warnings}],drift_count,failed_count,warnings,confirm_text:"UNDO <count>",approval,preview_hash}` |
| `POST /api/runs/{id}/undo` `{selection,reason,confirm,force,manual,preview_hash}` | admin | 201 `{batch,changes}` |
| `GET /api/undo-batches/{b}` · `POST .../approve {confirm,note}` · `POST .../reject {note}` | approver | `{batch,run_id,requested_by,reason,created_at,force,total,pending,changes:[{change_id,change,display,external_id,identity_id,status,confirm_text,items,steps,dry_run,drift,forced?}],drift_count,failed_count,confirm_text:"UNDO <pending>",approval}` → `{batch,approved}` / `{batch,rejected}` |
| `GET /api/changes/{id}/undo?items=<JSON array>` (or repeated `?item=`) | admin | undo preview `{items,selected,whole_only,undoable,reason,change,script,steps:[..],dry_run:{status,message,error}|null,drift:[..],holds:[{kind,key,label,default}],confirm_text,approval:"manual|second_person",warnings:[..],preview_hash}`; runs the inverse with `-DryRun` (live read, no writes) |
| `POST /api/changes/{id}/undo` `{reason,confirm,force,holds:[{kind,key,until?}],preview_hash}` | admin | 201 → the `undo` change (pending); 400 wrong confirm / reason < 10 / hold not offered / `until` in the past; 409 not undoable / stale `preview_hash` / drift without `force` / dry run failed / undo already open; audited `change.undo_requested` |
| `GET /api/changes/{id}/undo-check` | approver | for an undo change: a fresh dry run `{dry_run,drift,steps,confirm_text}` |
| `POST /api/changes/{id}/approve` `{note,confirm}` | approver | `confirm` (= `confirm_text`) required for undo changes (400); 403 when `undo.second_person` and the approver requested it. `POST /api/changes/bulk` reports undo changes as skipped ("approved one at a time") |
| `GET /api/audit?q=&actor=&action=&entity_type=&entity_id=&format=csv` · `GET /api/audit/verify` | viewer | `q` = search query (`store.ParseAuditQuery`: words, `-word`, `actor:`, `action:x.*`, `type:`, `id:`, `-actor:` / `-action:x.*` / `-type:` (leave out), `level:error`, `since:`/`before:`; 400 with the reason when invalid); `format=csv` → up to 10,000 rows (formula-safe cells, audited `audit.export`); audit: `{seq,ts,actor,action,entity_type,entity_id,detail,hash}`; verify → `{ok,checked,first_bad_seq}`. Entity types: `source, mapping, role, user, manual_person, identity, change, settings, run, ...` |
| `GET /api/tables` · `GET /api/tables/{name}?q=` | viewer | allow-listed read-only row browser (`sync_runs,changes,identities,audit_log`): `{name,columns:[..],rows:[[..]],total}` |
| `GET/POST /api/users`, `PUT/DELETE /api/users/{id}` | admin | `{id,username,role,disabled,created_at,last_login_at}`; create/PUT may include `password` |

Static SPA served at `/` (any non-`/api` path ⇒ `index.html`).

## 8. Settings document (`GET/PUT /api/settings`)

```json
{
 "schedule":{"enabled":true,"kind":"interval|daily|cron","interval_minutes":60,"daily_time":"02:00","cron_expr":"0 */4 * * *","timezone":"America/Chicago"},
 "approval":{"create":"manual","update":"auto","disable":"manual","enable":"manual","delete":"manual","link":"auto","cloud_groups":"auto"},
 "engine":{"dry_run":true,"max_destructive_per_run":10,"delete_after_days":30,"remove_stale_groups":true,"enforce_ou":false,"max_deletes_per_run":5,"max_deletes_per_day":20},
 "ad":{"server":"","search_bases":["OU=Staff,DC=corp,DC=example,DC=com"],"default_ou":"OU=Staff,DC=...","disabled_ou":"","id_attribute":"employeeID","upn_suffix":"corp.example.com","strip_groups_on_disable":true},
 "naming":{"sam_template":"{{ first .FirstName 1 | lower }}{{ .LastName | lower }}","upn_template":"{{ .FirstName | lower }}.{{ .LastName | lower }}","cn_template":"{{ .FirstName }} {{ .LastName }}","max_candidates":9,"mail_template":"","mail_domain":"",
   "conflict_mode":"manual|number|random|template","conflict_random_digits":3,"conflict_suffix_template":"{{ last .EmployeeID 4 }}"},
 "new_user":{"password_mode":"random|template","password_length":20,"password_template":"{{ upper (first .LastName 1) }}{{ last .EmployeeID 4 }}!{{ random 6 }}","change_password_at_logon":true,"enabled":true},
 "lifecycle_groups":{"active_add":["ALLUSERS"],"disabled_add":["Disabled Users"],"disabled_remove":"all_except|managed|none","disabled_keep":["Mailbox-Retention"]},
 "cloud":{"enabled":false,"environment":"global|usgov|usgovdod|china","tenant_id":"","client_id":"","auth":"certificate|secret","cert_thumbprint":"","client_secret":"(secret)",
   "user_match":"sid|immutable_id|upn","exchange":{"enabled":false,"organization":"contoso.onmicrosoft.com"}},
 "placement":{"enabled":false,"field":"Department","unmapped":"hold|default","move_on_change":true,"rules":[{"match":"Finance","ou":"OU=Finance,OU=Staff,DC=..."},{"match":"Sales*","ou":"..."}]},
 "backup":{"enabled":false,"interval_hours":24,"keep":14,"folder":"","passphrase":"(secret)"},
 "undo":{"enabled":true,"second_person":false},
 "correlation":{"enabled":false,"rules":[{"field":"FirstName","ad_attr":"givenName","compare":"exact|ci|name","required":true},{"field":"LastName","ad_attr":"sn","compare":"name","required":true},{"field":"Department","ad_attr":"department","compare":"ci","required":false}],
   "rename":"never|transitions|always","max_auto_links_per_run":25,"report_uncorrelated":true,"ignore_accounts":["svc_*","OU=Service Accounts,DC=..."]},
 "notifications":{"enabled":false,"provider":"smtp|m365_smtp|m365_graph","from":"idl@corp.example.com","to":["ops@corp.example.com"],"subject_prefix":"[IDLifecycle]","gui_base_url":"",
   "smtp":{"host":"","port":587,"security":"starttls|tls|none","auth":"none|login|oauth2","username":"","password":"(secret)"},
   "oauth":{"tenant_id":"","client_id":"","client_secret":"(secret)"},
   "events":{"run_failed":true,"approvals_pending":true,"changes_failed":true,"threshold":true,"run_summary":false,"identity_issues":true}},
 "auth":{"local_login":"all|admins_only",
   "oidc":{"enabled":false,"tenant_id":"<GUID>","client_id":"<GUID>","client_secret":"(secret)","redirect_url":"https://idl.corp.example.com:8443/api/auth/oidc/callback",
     "button_label":"Sign in with Microsoft","admin_values":["IDLifecycle.Admin"],"approver_values":["IDLifecycle.Approver"],"viewer_values":["IDLifecycle.Viewer"]}}
}
```
`undo` is optional in a PUT (absent = kept); `second_person` makes the approver of an undo differ from its requester.
Console audit detail: `*.update` rows carry `changes:{"<dotted.path>":{from,to}}` (plus the older `changed:[paths]`), `*.create` / `*.delete`
rows `fields:{path:value}`; secrets (any field whose stored and masked forms differ) only as `{changed:true}` / `"(hidden)"`. Every console
row has `remote` (client IP).
`cloud` is optional in a PUT (absent = kept). Its client secret is encrypted at rest, masked, and kept when `********` or `""` is sent, unless the
tenant or client ID changed. The thumbprint is normalized (spaces and colons removed, upper case). Exchange Online requires `auth:"certificate"` and an
organization. Lifecycle lists and role grants accept `entra:<object id>` next to AD group names (a malformed reference ⇒ 400).
`auth` is optional in a PUT: when absent, the stored sign-in settings are kept. The client secret is encrypted at rest and masked; it must be
re-entered when the tenant or client ID changes. `redirect_url` must be https (http only for localhost) with path `/api/auth/oidc/callback`.
Defaults: `dry_run=true` (fresh installs plan but never write until an admin flips it), all approvals `manual`.
`max_deletes_per_run`: more deletes than this in one run ⇒ all of that run's deletes are held for manual approval. `max_deletes_per_day`:
the executor completes at most this many deletes per rolling 24 h; further approved deletes wait (status stays `approved`). 0 = no limit.
The executor sends at most 50 changes to one PowerShell invocation.

## 9. Bootstrap config (`config.json`)
```json
{"listen":"127.0.0.1:8443","tls":{"cert_file":"","key_file":""},"data_dir":"C:\\ProgramData\\IDLifecycle",
 "database":{"driver":"sqlite","dsn":"file:C:/ProgramData/IDLifecycle/idl.db"},
 "powershell":{"exe":"pwsh.exe","scripts_dir":"C:\\Program Files\\IDLifecycle\\scripts","execution_policy":"RemoteSigned","timeout_seconds":600},
 "log_level":"info"}
```
`dsn` may be `env:VAR_NAME`. Non-loopback `listen` without TLS is refused at startup.
Optional `"branding_dir"` (default `%ProgramData%IDLifecycle-Branding` on Windows) points at the branding override folder.
`idlifecycled config get -config <path>` prints `{listen, tls_cert_file, tls_key_file, log_level, data_dir, db_driver, scripts_dir,
console_url}`; `config set -config <path> [-listen] [-tls-cert] [-tls-key] [-log-level]` changes only those fields with the start-up
validation (exit 2, nothing written, when invalid; empty `-tls-cert`/`-tls-key` = HTTPS off) and writes through a temp file + rename.

## 10. Testing strategy

| Layer | How |
|---|---|
| store | table tests on in-memory SQLite; Postgres/MSSQL/MySQL/Db2 suites run when `IDL_TEST_POSTGRES_DSN` / `IDL_TEST_MSSQL_DSN` / `IDL_TEST_MYSQL_DSN` / `IDL_TEST_DB2_DSN` are set (same shared suite); server-less Db2 dialect tests and a statement-recording fake driver audit every query for Db2 rules |
| templates/roles/mappings/planner | pure unit tests |
| sources | CSV fixtures; the REST connector against `httptest` servers (every auth and pagination mode, retries, refused cross-host links); SQL against a temp SQLite DB (plus query-validation table) |
| engine | end-to-end with SQLite + `ad.FakeClient` (create/update/disable/delete flows, approvals, dry-run, guards, crash recovery) |
| api | `httptest` server: auth, CSRF, RBAC, CRUD, approval flow |
| PowerShell | Pester tests run the real scripts in a child `pwsh` with `PSModulePath` pointing **only** at `tests/powershell/StubAD` (in-memory-JSON fake `ActiveDirectory` module), so tests can never reach a real domain; the cloud scripts run against `StubEXO` (fake ExchangeOnlineManagement) and `FakeGraph.ps1` (a local fake Graph and token endpoint that verifies every client assertion), never a real tenant; Go integration test drives `PowerShellClient` through the same stub |
| web | headless Edge loads `selftest` pages against a real test server (skipped if Edge missing); `go run ./cmd/idl-mockui` serves the UI against the in-browser mock API (`testdata/mock`) |

How to run each suite, write a test, and check a real environment: [TESTING.md](TESTING.md). Which suite covers which code: [TESTMAP.html](TESTMAP.html).

## 11. Non-goals / known gaps
* No password delivery: initial passwords are random, set must-change-at-logon, never persisted or logged. Admins use their normal reset process.
* Single-valued string attributes only (`proxyAddresses` is only touched by a link rename). Manager via `value_type:"user_ref"`.
* Correlation compares whole normalized values (no similarity scores, nicknames or initials); a merged record cannot be un-merged in the console.
* Single service instance (no HA/leader election).
* The generic REST connector was tested against simulated APIs only; check a real HR API with "Test these settings" first.

## SQL source (`internal/sources/sql.go`)
For customers whose own job writes HR data into a database table/view. Config: `driver` (`sqlserver|postgres|mysql|sqlite|db2|odbc`;
aliases `mssql`, `postgresql`/`pgx`, `mariadb`, `ibmdb2` → `db2`, and `db2i`/`as400`/`ibmi` → `odbc`, since IBM i Access does not take the Db2 LUW CLI keywords), `dsn` (a `password` field, so encrypted at rest and masked; `env:VAR` is
resolved at fetch time, for every driver), `query`, `timeout_seconds` (120), `max_rows` (200000).
* **db2 / odbc** (`sql_odbc_windows.go`): both open `sql.Open("odbc", dsn)` via `github.com/alexbrainman/odbc`, which calls
  `odbc32.dll` through syscall — no cgo, the exe stays pure Go. Its non-Windows build needs cgo + unixODBC, so it is only linked on
  Windows; elsewhere these drivers fail with a clear error. The DSN is an ODBC connection string. Install a 64-bit driver matching the
  service: IBM Data Server Driver for ODBC and CLI (Db2 LUW, z/OS — z/OS usually needs DB2 Connect licensing; the `Driver={...}` name depends
  on the install, a system DSN `DSN=HRDB2;` is simplest) or IBM i Access Client Solutions (`Driver={IBM i Access ODBC Driver};System=...;DefaultLibraries=...`).
  Windows integrated auth is not offered (only whatever a driver supports through its own options). `db2` and `odbc` behave the same today; they differ only in help text.
  Driver behaviour found in the source: no `ConnBeginTx`, so database/sql refuses `TxOptions.ReadOnly` and the always-rolled-back plain
  transaction (autocommit off) is used — nothing Db2-specific is issued, so the **read-only login is the real guard**. `SQLDriverConnect`
  ignores the context, so `connect` bounds it with `timeout_seconds` (the abandoned attempt is closed when the driver returns). Value
  types: SMALLINT/INTEGER → int32, BIGINT → int64, DECIMAL/NUMERIC/FLOAT → **float64** (exact to ~15 digits; `CAST` long keys to CHAR),
  DATE/TIMESTAMP → local `time.Time`, CHAR/VARCHAR → ANSI-code-page bytes (decoded via `MultiByteToWideChar(CP_ACP)` unless already
  valid UTF-8; Db2 CHAR blank padding is trimmed), NCHAR/wide types → string. Types the driver does not know (e.g. Db2 GRAPHIC/CLOB/DECFLOAT/XML
  when described with Db2-specific SQL types) fail with "unsupported column type" and a hint to CAST them; Db2 CLI keywords such as
  `LongDataCompat=1` / `MapGraphicDescribe=1` may also help. Tested here only against the ODBC driver manager and the in-box
  "SQL Server" driver failing to connect — **not against a live Db2**.
* **Query check** (`ValidateSQLQuery`, on save and on build): comments removed (`-- ` needs trailing whitespace; MySQL `/*! */`
  kept as code; done under both `''` and backslash string-escape rules, both must pass), then it must start with
  `SELECT`/`WITH`, have no `;` except one trailing, and no whole-word INSERT/UPDATE/DELETE/MERGE/DROP/ALTER/CREATE/TRUNCATE/
  EXEC/EXECUTE/GRANT/REVOKE/INTO/CALL. String literals are *not* exempt (conservative). Defence in depth only: the login must be read-only.
* **Fetch**: one connection, `PingContext`, `BeginTx(ReadOnly)` (Postgres/MySQL enforce it; SQLite additionally gets
  `PRAGMA query_only`; go-mssqldb rejects ReadOnly so SQL Server gets a plain transaction) — always rolled back, never committed.
  Columns → keys (trimmed; unnamed/duplicate case-insensitive ⇒ error); NULL → "", time → RFC3339, floats without exponent.
  More than `max_rows` ⇒ error (never truncated). Errors are prefixed `sql:` and scrubbed of the DSN and its password
  (URL `user:pass@`, and `password=`/`pwd=`/`newpwd=` key/values case-insensitively, quoted, bare or ODBC-braced `{..;..}}..}`).
