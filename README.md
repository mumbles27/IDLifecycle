# IDLifecycle

A Windows service that keeps Active Directory in step with your HR system.

* **Sources**: Paylocity (API) or a CSV file (more can be added by implementing one small Go interface).
* **Rules in a database**: attribute mappings, role rules (groups / OU / extra attributes), naming policy,
  schedule and approval policy all live in the DB and are edited in a web GUI.
* **Approvals**: every create / update / disable / enable / delete is proposed first. Each change type can be
  `manual` (a human approves it in the GUI) or `auto`. A fresh install starts in **dry-run** (plans are recorded,
  nothing is written) with everything manual.
* **AD changes** are made by PowerShell scripts that the Go service runs as its own (ideally gMSA) identity.
* **Databases**: SQLite (default), PostgreSQL, SQL Server, MySQL/MariaDB.

```
HR source -> ingest -> plan (diff vs AD snapshot) -> changes table -> approve (human | auto) -> PowerShell -> AD
                                  ^ mappings, roles, naming from DB        ^ GUI: approvals, history, settings
```

## Quick start

```powershell
# 1. build (Go 1.22+; no CGO needed)
go build -ldflags "-X idlifecycle/internal/app.Version=1.0.0" -o bin\idlifecycled.exe .\cmd\idlifecycled

# 2. create config + data dir, point it at the scripts folder
.\bin\idlifecycled.exe init -dir C:\ProgramData\IDLifecycle -scripts "C:\Program Files\IDLifecycle\scripts"
Copy-Item .\scripts -Destination "C:\Program Files\IDLifecycle\" -Recurse

# 3. first administrator (password from prompt, or IDL_PASSWORD, or one line on stdin; min 12 chars)
.\bin\idlifecycled.exe user add -config C:\ProgramData\IDLifecycle\config.json -username admin -role admin

# 4. run in the foreground to try it, then browse to http://127.0.0.1:8443/
.\bin\idlifecycled.exe run -config C:\ProgramData\IDLifecycle\config.json

# 5. install as a service (dry-run supported)
.\scripts\Install-IDLifecycleService.ps1 -BinaryPath ... -ConfigPath ... -ServiceAccount 'CORP\svc-idl$' -DryRun
```

Then in the GUI: **Settings → AD** (search bases, default OU, UPN suffix) → **Sources** (add + Test) →
**Mappings** → **Roles** → run a sync, review the *simulated* changes, and only then turn dry-run off.

## Concepts

### Attribute modes (per mapping / per role attribute grant)

| Mode | Behaviour |
|---|---|
| `create_only` | Set when the account is created; never touched again. |
| `sync` | Pushed when the **source value changes** (compared with the last value this service wrote). A manual edit in AD survives until HR data changes again. |
| `immutable` | **Forced** to equal the source-derived value on every run; any drift in AD is corrected (and shown as a proposed change). |

`on_empty: skip|clear` decides what an empty source value does. `value_type: user_ref` turns an employee id into the
user's DN (e.g. `manager`). The ID attribute (default `employeeID`) is the link key between HR and AD.

### Roles

A role rule has optional conditions (`eq ne in contains starts_with regex exists`, match `all`/`any`) and grants
(groups, OU, extra attributes). A rule **without conditions applies to everyone** (your baseline groups). Matching
rules are additive for groups; for OU and for attributes on the same target, the higher `priority` wins. With
`remove_stale_groups` on, the service removes people from *groups it manages* (any group named in a rule) when no
rule grants them any more; it never touches other groups.

### Templates

Go `text/template` over the source record: `{{ .FirstName }}`, `{{ index . "currentStatus.statusType" }}`.
Functions: `lower upper title trim first last substr replace default coalesce join ascii digits alnum split`.
Example: `{{ first .FirstName 1 | lower }}{{ .LastName | lower | alnum }}`. `range`, `define` and `template` are rejected.

### Lifecycle & safety rails

* Terminated (status field matches `terminated_values`, or absent from the feed for more than `missing_grace_runs`
  runs when `missing_means_terminated`) → **disable** (optionally move to the Disabled OU, strip groups). After
  `delete_after_days` → **delete**. Active again → **enable**.
* An empty or failed source fetch never means "everyone left": the run is flagged and nothing is proposed for it.
* **Bulk-destructive guard**: more than `max_destructive_per_run` disables/deletes in one run are held for manual
  approval even when the policy is `auto`.
* The scripts refuse to delete an enabled account and verify the account's ID attribute equals the change's
  external id before touching it. New accounts get a random password (must change at logon) that is never stored
  or logged; hand-over is out of scope (use your normal reset process).
* Every action is in an append-only, hash-chained **audit log** (`Audit → Verify integrity`).

## Operating notes

* **Permissions**: run the service as a gMSA that is delegated only what it needs (create/delete user objects and
  write the mapped attributes in the target OUs, manage membership of the managed groups). Nothing is stored
  for AD authentication. The account needs read access to the scripts folder and write access to
  `scripts\Logs` and the data dir.
* **CSV sources** can never read the service data directory; set `csv_allowed_dirs` in config.json to restrict them to specific folders.
* **Protected attributes**: mappings cannot target identity/security attributes (sAMAccountName, UPN, altSecurityIdentities, SPNs, msDS-*, adminCount, ...). The scripts also refuse membership changes on privileged groups (adminCount=1, Domain/Enterprise/Schema Admins, Administrators, Operators).
* **TLS**: a non-loopback `listen` address is refused unless `tls.cert_file`/`tls.key_file` are set.
* **Secrets** (HR API credentials) are encrypted with AES-256-GCM. The key is `secret.key` in the data dir wrapped
  with DPAPI (machine scope) or `IDL_MASTER_KEY` (base64, 32 bytes) when set — required if you move the DB to another host.
* **Postgres/SQL Server/MySQL**: set `database.driver` and `database.dsn` (or `"dsn": "env:IDL_DB_DSN"`). The
  schema is created automatically. See `docs/ARCHITECTURE.md` for the portability rules.
* **Logs**: `<data_dir>\logs\idlifecycle.log` (JSON, rotated), PowerShell logs and CSV reports in `scripts\Logs`.

## Tests

```powershell
go test ./...                                  # unit + integration (SQLite, fake AD, httptest, headless-Edge GUI test if Edge is present)
pwsh -NoProfile -File tests\powershell\Run-Tests.ps1   # Pester tests for the scripts against a stub ActiveDirectory module
$env:IDL_TEST_POSTGRES_DSN = '...'; go test ./internal/store/   # same store suite on PostgreSQL (also IDL_TEST_MSSQL_DSN, IDL_TEST_MYSQL_DSN)
```

The PowerShell tests run the real scripts against a **stub** `ActiveDirectory` module with `PSModulePath` restricted
to the stub, so they cannot reach a real domain.

## Known limits

* Only the SQLite path was executed in development; the PostgreSQL/SQL Server/MySQL code paths are covered by the
  shared test suite but need a run against real servers (set the `IDL_TEST_*_DSN` variables).
* The scripts were verified against a stub AD module, not a live domain. Do a supervised first run in a lab OU.
* Paylocity field names/endpoints come from public documentation and are configurable; validate against your tenant.
* Not yet done: audit rows are written next to (not inside) the state-change transaction; an auto-approved `enable` will re-enable an account that someone disabled deliberately for another reason (keep `approval.enable` manual); MSSQL string parameters are sent as nvarchar against VARCHAR key columns (correct, but can cost an index seek on very large tables); migrations are not transactional.
* Single-valued string attributes only; matching HR→AD is by the ID attribute only (no fuzzy adoption of
  pre-existing accounts); single service instance.
