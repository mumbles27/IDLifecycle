# IDLifecycle

A Windows service that keeps Active Directory in step with your HR system.

* **Sources**: Paylocity (API), a CSV file, or a SQL database table/view (SQL Server, PostgreSQL, MySQL/MariaDB,
  SQLite) filled by your own HR export job; more can be added by implementing one small Go interface.
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

# 2. create config + data dir (restricted to SYSTEM, Administrators and you), point it at the scripts folder
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
* **Delete limits**: more than `max_deletes_per_run` (default 5) deletes planned in one run → every delete of that run
  is held for manual approval. At most `max_deletes_per_day` (default 20) deletes are *completed* in any rolling
  24 hours, however they were approved; the rest stay approved and run when the window allows (a `threshold`
  notification says so). `0` switches a limit off.
* **Stale decisions never run**: when the next run no longer plans a change (rehire, data corrected, AD fixed by hand),
  open proposals *including approved-but-not-yet-applied ones* are superseded.
* Approved changes go to PowerShell in chunks of 50 per invocation, so a large first load cannot hit the script
  timeout as one all-or-nothing call.
* The scripts refuse to delete an enabled account (or one with a user/computer/group below it; leaf children such as
  ActiveSync device containers are deleted with it, so the gMSA needs "Delete subtree" on user objects) and verify the account's ID attribute equals the change's
  external id before touching it. New accounts get a random password (must change at logon) that is never stored
  or logged; hand-over is out of scope (use your normal reset process).
* Every action is in an append-only, hash-chained **audit log** (`Audit → Verify integrity`).

## Operating notes

* **Permissions**: run the service as a gMSA that is delegated only what it needs (create/delete user objects and
  write the mapped attributes in the target OUs, manage membership of the managed groups). Nothing is stored
  for AD authentication. The account needs read access to the scripts folder and write access to
  `scripts\Logs` and the data dir.
* **CSV sources** can never read the service data directory; set `csv_allowed_dirs` in config.json to restrict them to specific folders.
* **SQL sources** run one `SELECT`/`WITH` query inside a read-only (or always-rolled-back) transaction; queries containing
  `;` or write keywords (INSERT, UPDATE, DELETE, INTO, EXEC, ...) are refused. That check is defence in depth: give the source a
  **read-only DB login**. The connection string is stored encrypted (or use `env:VAR_NAME`); for SQL Server omit `user:pass@` to use
  Windows integrated auth as the service account. More rows than `max_rows` fails the fetch rather than truncating it.
* **Protected attributes**: mappings cannot target identity/security attributes (sAMAccountName, UPN, altSecurityIdentities, SPNs, home/profile/script paths, proxyAddresses, accountExpires, msDS-*/msExch*/msPKI-*, adminCount, ...). The scripts also refuse membership changes on privileged groups (adminCount=1, Domain/Enterprise/Schema Admins, Administrators, Operators) — except that a **disable** never fails on them: the account is disabled and the privileged membership is kept and reported ("kept privileged group(s): ...") for an admin to clean up.
* **Mark-of-the-Web**: with `execution_policy` RemoteSigned, copied/downloaded scripts carrying a Zone.Identifier stream will not load. The installer unblocks them in `scripts_dir` (and fails the step if any remain); after updating the scripts by hand, re-run it or `Unblock-File` them.
* **TLS**: a non-loopback `listen` address is refused unless `tls.cert_file`/`tls.key_file` are set.
* **Secrets** (HR API credentials, SQL connection strings, SMTP/Entra secrets) are encrypted with AES-256-GCM. The key is `secret.key` in the data dir wrapped
  with DPAPI (machine scope) or `IDL_MASTER_KEY` (base64, 32 bytes) when set — required if you move the DB to another host.
* **Data directory ACL**: machine-scope DPAPI means anyone who can *read* `secret.key` can unwrap it, so the service
  **refuses to start** while Everyone/Users/Authenticated Users/Domain Users can read the data directory or its files.
  `init` and the installer restrict it; to fix an existing one:
  `idlifecycled lockdown -config C:\ProgramData\IDLifecycle\config.json -account 'CORP\svc-idl$'`.
* **Postgres/SQL Server/MySQL**: set `database.driver` and `database.dsn` (or `"dsn": "env:IDL_DB_DSN"`). The
  schema is created automatically. See `docs/ARCHITECTURE.md` for the portability rules.
* **Logs**: `<data_dir>\logs\idlifecycle.log` (JSON, rotated), PowerShell logs and CSV reports in `scripts\Logs`.

## Email notifications

**Settings → Notifications** (admin). Events: run failed, approvals waiting, changes failed in AD, safety threshold
reached (bulk guard / delete limits), and an optional summary of every run. **Send test email** tries the settings in
the form before you save them. Secrets are stored encrypted, shown as `********`, and must be re-entered when the
provider, SMTP host, tenant or client id changes (a stored secret is never sent to a new endpoint).

| Provider | Use for |
|---|---|
| Generic SMTP | on-prem Exchange / relay / any SMTP service: STARTTLS (587), implicit TLS (465) or plain (loopback only for auth), AUTH LOGIN/PLAIN or anonymous |
| Microsoft 365 – SMTP AUTH | `smtp.office365.com:587` STARTTLS with **OAuth2 (XOAUTH2, client credentials)**; Basic auth still selectable but being retired by Microsoft |
| Microsoft 365 – Graph API | `POST /v1.0/users/{from}/sendMail` with an app-only token; works where SMTP AUTH is disabled tenant-wide (security defaults) |

**M365 SMTP AUTH (OAuth2)**
1. Entra ID → App registrations → New registration (single tenant). Note the client and tenant id; create a client secret.
2. API permissions → *APIs my organization uses* → **Office 365 Exchange Online** → Application → **SMTP.SendAsApp** → grant admin consent.
3. Exchange Online PowerShell (use the **Enterprise application** object id, not the app registration's):
   ```powershell
   New-ServicePrincipal -AppId <client-id> -ObjectId <enterprise-app-object-id>
   Add-MailboxPermission -Identity idlifecycle@contoso.com -User <service-principal-id> -AccessRights FullAccess
   Set-CASMailbox idlifecycle@contoso.com -SmtpClientAuthenticationDisabled $false
   ```
4. In the GUI pick *Microsoft 365 – SMTP AUTH* (host/port/STARTTLS fill in), auth **OAuth2**, From = that mailbox, enter tenant/client/secret, **Send test email**.

**M365 Graph**
1. App registration + secret as above; API permissions → **Microsoft Graph** → Application → **Mail.Send** → admin consent.
2. Scope it to the sender — Mail.Send otherwise allows sending as *any* mailbox:
   `New-ApplicationAccessPolicy -AppId <client-id> -PolicyScopeGroupId idl-senders@contoso.com -AccessRight RestrictAccess -Description "IDLifecycle"`
   (or RBAC for Applications).
3. In the GUI pick *Microsoft 365 – Graph API*, From = the sender mailbox, tenant/client/secret, **Send test email**.

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
