# IDLifecycle – Architecture & Interface Contracts

A Windows service (Go) that reads people from an HR source (Paylocity API, CSV, ...), computes the
desired Active Directory state from DB-held mapping + role rules, routes changes through an approval
queue (or auto-applies), and executes them through PowerShell scripts. Admins operate it through an
embedded web GUI.

```
 Sources (CSV, Paylocity, SQL)   +------------------ idlifecycled.exe ------------------+
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
| `cmd/idlifecycled` | Entry point: `run` (console), `service` (SCM), `install`/`uninstall`, `user add/passwd`, `version` |
| `internal/config` | Bootstrap config file (DB connection, listen addr, TLS, PowerShell paths) |
| `internal/store` | DB access, dialect layer (sqlite/postgres/mssql/mysql), migrations, all queries |
| `internal/secrets` | AES-GCM encryption for source secrets; key wrapped by DPAPI on Windows |
| `internal/datadir` | Data-directory ACL check (service refuses to start when broad principals can read it) and `lockdown` |
| `internal/notify` | E-mail notifications: generic SMTP, M365 SMTP AUTH (OAuth2 XOAUTH2), M365 Graph sendMail |
| `internal/model` | Plain data types shared by store/engine/api (JSON = snake_case) |
| `internal/sources` | `Source` interface + CSV + Paylocity + SQL (table/view) implementations + registry |
| `internal/engine` | Template rendering, role resolution, planner (diff), approval policy, executor, run orchestration |
| `internal/ad` | `Client` interface; `PowerShellClient` (real) and `FakeClient` (tests) |
| `internal/scheduler` | Interval / daily / cron scheduling, hot-reloaded from settings |
| `internal/api` | HTTP handlers, auth, sessions, CSRF, audit hooks |
| `internal/web` | `go:embed` of `static/` SPA |
| `scripts/` | PowerShell: `IDLifecycle.Common.psm1`, `Get-IDLSnapshot.ps1`, `New/Set/Disable/Enable/Remove-IDLUser.ps1`, `Install-IDLifecycleService.ps1` |
| `tests/powershell` | Stub ActiveDirectory module + Pester tests for the scripts |

## 2. Key design decisions (ADR summary)

1. **DB portability**: stdlib `database/sql` + hand-written SQL using `?` placeholders rebound per dialect.
   IDs are UUID strings generated in Go (no autoincrement differences). Timestamps are stored as UTC
   RFC3339 strings (`VARCHAR(40)`) which sort correctly everywhere. Booleans are `INT 0/1`. Long JSON/text
   uses `LongText` (TEXT / NVARCHAR(MAX) / LONGTEXT). No upserts (update-then-insert in a tx).
   Rejected: ORM (opaque SQL, dialect surprises), per-DB schemas by hand.
2. **Pure Go build** (CGO off): `modernc.org/sqlite`, `pgx` stdlib, `go-mssqldb`, `go-sql-driver/mysql`.
3. **AD changes go through PowerShell** run as the service account (gMSA recommended; delegated rights
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

## 3. Data model (portable DDL in `internal/store/migrations`)

`schema_migrations(version, applied_at)` ·
`settings(key PK, value LongText JSON, updated_at, updated_by)` ·
`users(id, username UNIQUE, password_hash, role, disabled, failed_logins, locked_until, created_at, last_login_at)` ·
`sessions(token_hash PK, user_id, csrf_token, created_at, expires_at, last_seen_at)` ·
`sources(id, name UNIQUE, type, enabled, options_json, config_json(secrets encrypted), created_at, updated_at, last_sync_at, last_status, last_error)` ·
`identities(id, source_id, external_id, UNIQUE(source_id, external_id), attrs_json, attrs_hash, active, missing_runs, first_seen, last_seen, ad_guid, ad_dn, ad_sam, applied_json, disabled_at, state)` ·
`mappings(id, name, target_attr, template, mode, on_empty, value_type, enabled, sort_order, description, created_at, updated_at)` ·
`role_rules(id, name, description, enabled, priority, match, conditions_json, grants_json, created_at, updated_at)` ·
`sync_runs(id, trigger, triggered_by, started_at, finished_at, status, counters_json, error)` ·
`changes(id, run_id, identity_id, external_id, display, type, status, payload_json, summary_json, payload_hash, created_at, decided_by, decided_at, decision_note, applied_at, error, attempts)` ·
`audit_log(id INT/seq as string-sortable `seq` BIGINT, ts, actor, action, entity_type, entity_id, detail_json, prev_hash, hash)`.

`identities.state`: `new` (seen, no AD account yet) · `linked` (AD account exists & matched) · `disabled` ·
`deleted` · `error`.

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
Change types: `create`, `update` (attrs, group add/remove, OU move), `disable`, `enable`, `delete`.
Each transition writes an `audit_log` row. Approval policy is per type: `settings.approval.{create,update,disable,enable,delete}` = `manual|auto`.

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
   role-rule groups to DNs. An empty snapshot while accounts are linked aborts the run.
5. For each identity compute desired state: attributes (global mappings overlaid by matching role-rule
   attribute grants, higher `priority` wins), groups (union of matched rules), OU (highest-priority rule
   with an OU, else `ad.default_ou`), then diff vs AD (§1 decision 5) → `create|update|disable|enable|delete`.
6. Dedup against open changes (same identity+type+payload_hash ⇒ keep; different ⇒ supersede old pending).
7. Apply approval policy / guard / dry-run to set initial status; write audit rows.
8. Executor processes `approved` changes, grouped by type, in order create → update → enable → disable →
   delete, one PowerShell invocation per type-batch. Results update changes + identities.
9. Finish run with counters (`total, created, updated, disabled, enabled, deleted, pending, failed,
   skipped, unchanged, source_errors`).

Naming (new accounts): templates `naming.sam_template`, `naming.upn_template` (local part), `naming.cn_template`,
rendered with the source attributes. Collision candidates `n=""`,2,3.. are generated in Go (sAMAccountName
truncated to 20 chars *before* appending n); the script picks the first candidate free in the whole directory.

Template language: Go `text/template` (no I/O). Data = source attribute map, so `{{ .FirstName }}` or
`{{ index . "currentStatus.statusType" }}`. Missing keys render empty. Functions: `lower upper title trim
first(s,n) last(s,n) substr(s,start,len) replace(s,old,new) default(fallback,v) coalesce(a,b,...) join(sep,
a,b..) ascii(s) digits(s) alnum(s) split(s,sep) index-able`. Pipe style works: `{{ .First | lower }}`.

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

* **`New-IDLUser.ps1`** item: `{"changeId","externalId","ou":"OU=...","candidates":[{"sam","upn","cn"}],"attributes":{"givenName":"Jane","sn":"Doe","displayName":"Jane Doe","title":"..."},"groups":["GG-AllStaff"],"password":"<generated>","enabled":true,"changePasswordAtLogon":true}`.
  Idempotent: if an account with `idAttribute == externalId` already exists ⇒ `status:"skipped"`, `message:"exists"`, returns its guid/dn/sam/upn (no changes). Otherwise picks the first candidate whose sam AND upn are free
  directory-wide and whose cn is free in the OU; creates the user (attributes applied via `-OtherAttributes`, the `idAttribute` set to `externalId`), then adds groups (an unresolvable group ⇒ item `failed` after creation with message listing it, account kept; result still includes guid/dn).
* **`Set-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid","set":{"title":"New"},"clear":["telephoneNumber"],"addGroups":["..."],"removeGroups":["..."],"moveTo":"OU=...|"" }`. Uses `-Replace`/`-Clear` with LDAP names. No-ops are `skipped`. Returns the new dn.
* **`Disable-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid","moveTo":"","removeGroups":[],"description":"Disabled by IDLifecycle 2026-10-16"}`. Idempotent (already disabled ⇒ still applies move/groups/description, status `applied` or `skipped` if nothing to do). The account is **disabled first**, before anything else can fail: an invalid/nonexistent `moveTo` ⇒ `failed` with `message:"failed after: disabled; ..."`. Privileged (adminCount=1 / well-known RID) and ambiguous groups in `removeGroups` are **kept**, never a failure: WARN log, `Skipped` report row, and `message` ends with `kept privileged group(s): <names>` (or `kept ambiguous group(s): ...`).
* **`Enable-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid","moveTo":"","addGroups":[]}`.
* **`Remove-IDLUser.ps1`** item: `{"changeId","externalId","objectGuid"}`. **Refuses** (`failed`, "account is enabled") if the account is enabled; refuses if object is not a user. Already gone ⇒ `skipped`. Deletes with `Remove-ADObject -Recursive` so leaf children (e.g. `CN=ExchangeActiveSyncDevices`) go with the account; refuses (`failed`) if any child is a user/computer/group/inetOrgPerson.

## 7. HTTP API (JSON, snake_case)

Auth: cookie `idl_session` (HttpOnly, SameSite=Strict, Secure when TLS). Every non-GET request must send
`X-CSRF-Token` (value from `/api/auth/me` or login response). Errors: HTTP status + `{"error":{"code":"...","message":"..."}}`.
Lists: `{"items":[...],"total":N}` with `?limit=&offset=` (default 50, max 500). Roles: `viewer` read; `approver` +approve/reject/retry; `admin` everything.

| Method & path | Role | Notes |
|---|---|---|
| `POST /api/auth/login` `{username,password}` | none | → `{user:{id,username,role},csrf_token}` |
| `POST /api/auth/logout` · `GET /api/auth/me` · `POST /api/auth/password {current,new}` | any | |
| `GET /api/status` | viewer | `{version,db_driver,mode:"live|dry_run",scheduler:{enabled,next_run},running_run,pending_changes,last_run}` |
| `GET /api/source-types` | viewer | `[{type,label,fields:[{name,label,type:"string|password|int|bool|text",required,default,help}]}]` |
| `GET/POST /api/sources`, `GET/PUT/DELETE /api/sources/{id}` | admin (GET viewer) | source: `{id,name,type,enabled,options,config,last_sync_at,last_status,last_error}`; `options:{external_id_field,status_field,terminated_values:[],missing_means_terminated,missing_grace_runs}`; secrets returned as `"********"`, sending `"********"` back keeps stored value |
| `POST /api/sources/{id}/test` | admin | fetches; → `{ok,count,attributes:[names],sample:[{...}] (first 5),error}` |
| `GET/POST /api/mappings`, `GET/PUT/DELETE /api/mappings/{id}` | admin (GET viewer) | `{id,name,target_attr,template,mode:"create_only|sync|immutable",on_empty:"skip|clear",value_type:"string|user_ref",enabled,sort_order,description}` |
| `POST /api/templates/preview` | admin | `{template, record:{...}}` → `{ok,output,error}` |
| `GET/POST /api/roles`, `GET/PUT/DELETE /api/roles/{id}` | admin (GET viewer) | `{id,name,description,enabled,priority,match:"all|any",conditions:[{attr,op,value}],grants:{groups:[],ou,attributes:[{target_attr,template,mode,on_empty,value_type}]}}`, ops: `eq ne in contains starts_with regex exists` |
| `POST /api/roles/preview` | admin | `{record:{...}}` → `{matched:[{id,name}],groups:[],ou,attributes:{attr:value}}` |
| `GET/PUT /api/settings` | admin (GET viewer) | see §8; notification secrets are returned as `"********"` (sending it back keeps the stored value) |
| `POST /api/notifications/test` `{notifications:{...}}` | admin | sends a test message with the unsaved form values (masked secrets fall back to stored ones) → `{ok,error}` |
| `GET /api/changes?status=&type=&q=` · `GET /api/changes/{id}` | viewer | change: `{id,run_id,identity_id,external_id,display,type,status,payload,summary:[strings],created_at,decided_by,decided_at,decision_note,applied_at,error,attempts}` |
| `POST /api/changes/{id}/approve|reject|retry` `{note}` | approver | reject requires note |
| `POST /api/changes/bulk` `{ids:[],action:"approve|reject",note}` | approver | → `{results:[{id,ok,error}]}` |
| `GET /api/runs` · `GET /api/runs/{id}` · `POST /api/runs` | viewer / admin | run: `{id,trigger,triggered_by,started_at,finished_at,status:"running|ok|error|aborted",counters:{...},error}`; POST → 202 `{id}` (409 if running) |
| `GET /api/identities?q=&state=` · `GET /api/identities/{id}` | viewer | `{id,source_id,source_name,external_id,display,active,state,ad_guid,ad_dn,ad_sam,first_seen,last_seen,disabled_at,attrs:{...}}` |
| `GET /api/audit?actor=&action=&entity_type=` · `GET /api/audit/verify` | viewer | audit: `{seq,ts,actor,action,entity_type,entity_id,detail,hash}`; verify → `{ok,checked,first_bad_seq}` |
| `GET /api/tables` · `GET /api/tables/{name}?q=` | viewer | allow-listed read-only row browser (`sync_runs,changes,identities,audit_log`): `{name,columns:[..],rows:[[..]],total}` |
| `GET/POST /api/users`, `PUT/DELETE /api/users/{id}` | admin | `{id,username,role,disabled,created_at,last_login_at}`; create/PUT may include `password` |

Static SPA served at `/` (any non-`/api` path ⇒ `index.html`).

## 8. Settings document (`GET/PUT /api/settings`)

```json
{
 "schedule":{"enabled":true,"kind":"interval|daily|cron","interval_minutes":60,"daily_time":"02:00","cron_expr":"0 */4 * * *","timezone":"America/Chicago"},
 "approval":{"create":"manual","update":"auto","disable":"manual","enable":"manual","delete":"manual"},
 "engine":{"dry_run":true,"max_destructive_per_run":10,"delete_after_days":30,"remove_stale_groups":true,"enforce_ou":false,"max_deletes_per_run":5,"max_deletes_per_day":20},
 "ad":{"server":"","search_bases":["OU=Staff,DC=corp,DC=example,DC=com"],"default_ou":"OU=Staff,DC=...","disabled_ou":"","id_attribute":"employeeID","upn_suffix":"corp.example.com","strip_groups_on_disable":true},
 "naming":{"sam_template":"{{ first .FirstName 1 | lower }}{{ .LastName | lower }}","upn_template":"{{ .FirstName | lower }}.{{ .LastName | lower }}","cn_template":"{{ .FirstName }} {{ .LastName }}","max_candidates":9},
 "new_user":{"password_length":20,"change_password_at_logon":true,"enabled":true},
 "notifications":{"enabled":false,"provider":"smtp|m365_smtp|m365_graph","from":"idl@corp.example.com","to":["ops@corp.example.com"],"subject_prefix":"[IDLifecycle]","gui_base_url":"",
   "smtp":{"host":"","port":587,"security":"starttls|tls|none","auth":"none|login|oauth2","username":"","password":"(secret)"},
   "oauth":{"tenant_id":"","client_id":"","client_secret":"(secret)"},
   "events":{"run_failed":true,"approvals_pending":true,"changes_failed":true,"threshold":true,"run_summary":false}}
}
```
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

## 10. Testing strategy

| Layer | How |
|---|---|
| store | table tests on in-memory SQLite; Postgres/MSSQL/MySQL suites run when `IDL_TEST_POSTGRES_DSN` / `IDL_TEST_MSSQL_DSN` / `IDL_TEST_MYSQL_DSN` are set (same shared suite) |
| templates/roles/mappings/planner | pure unit tests |
| sources | CSV fixtures; Paylocity against `httptest` server; SQL against a temp SQLite DB (plus query-validation table) |
| engine | end-to-end with SQLite + `ad.FakeClient` (create/update/disable/delete flows, approvals, dry-run, guards, crash recovery) |
| api | `httptest` server: auth, CSRF, RBAC, CRUD, approval flow |
| PowerShell | Pester tests run the real scripts in a child `pwsh` with `PSModulePath` pointing **only** at `tests/powershell/StubAD` (in-memory-JSON fake `ActiveDirectory` module), so tests can never reach a real domain; Go integration test drives `PowerShellClient` through the same stub |
| web | headless Edge loads `selftest` pages against a real test server (skipped if Edge missing) |

## 11. Non-goals / known gaps
* No password delivery: initial passwords are random, set must-change-at-logon, never persisted or logged. Admins use their normal reset process.
* Single-valued string attributes only (no proxyAddresses, etc.). Manager via `value_type:"user_ref"`.
* Matching source→AD is by the ID attribute only; no fuzzy matching/adoption wizard.
* Single service instance (no HA/leader election).
* Paylocity field names were written from public API docs and are configurable; validate against your tenant.

## SQL source (`internal/sources/sql.go`)
For customers whose own job writes HR data into a database table/view. Config: `driver` (`sqlserver|postgres|mysql|sqlite`;
aliases `mssql`, `postgresql`/`pgx`, `mariadb`), `dsn` (a `password` field, so encrypted at rest and masked; `env:VAR` is
resolved at fetch time), `query`, `timeout_seconds` (120), `max_rows` (200000).
* **Query check** (`ValidateSQLQuery`, on save and on build): comments removed (`-- ` needs trailing whitespace; MySQL `/*! */`
  kept as code; done under both `''` and backslash string-escape rules, both must pass), then it must start with
  `SELECT`/`WITH`, have no `;` except one trailing, and no whole-word INSERT/UPDATE/DELETE/MERGE/DROP/ALTER/CREATE/TRUNCATE/
  EXEC/EXECUTE/GRANT/REVOKE/INTO/CALL. String literals are *not* exempt (conservative). Defence in depth only: the login must be read-only.
* **Fetch**: one connection, `PingContext`, `BeginTx(ReadOnly)` (Postgres/MySQL enforce it; SQLite additionally gets
  `PRAGMA query_only`; go-mssqldb rejects ReadOnly so SQL Server gets a plain transaction) — always rolled back, never committed.
  Columns → keys (trimmed; unnamed/duplicate case-insensitive ⇒ error); NULL → "", time → RFC3339, floats without exponent.
  More than `max_rows` ⇒ error (never truncated). Errors are prefixed `sql:` and scrubbed of the DSN and its password.
