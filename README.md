# IDLifecycle

<img src="scripts/branding/logo-256.png" alt="IDLifecycle" width="96" align="right">

A Windows service that keeps Active Directory in step with your HR system.

**Documentation:** [Install and initial setup guide](docs/SETUP-GUIDE.md) (step by step, for the people installing it) ·
[Architecture and interface contracts](docs/ARCHITECTURE.md) · [Web UI notes](docs/UI.md) · [Code map](https://mumbles27.github.io/IDLifecycle/CODEMAP.html)
(a diagram of how the packages, scripts and web UI connect; its source is `docs/CODEMAP.html`) ·
[Testing guide](docs/TESTING.md) (how the tests work and how to check your own environment) ·
[Test map](https://mumbles27.github.io/IDLifecycle/TESTMAP.html) (which test covers what; its source is `docs/TESTMAP.html`) · this README (reference).

* **Sources**: any HR system with a REST/JSON API (generic connector, no code), a CSV file, or a SQL database table/view (SQL Server, PostgreSQL, MySQL/MariaDB,
  SQLite) filled by your own HR export job; more can be added by implementing one small Go interface.
* **Rules in a database**: attribute mappings, role rules (groups / OU / extra attributes), naming policy,
  schedule and approval policy all live in the DB and are edited in a web GUI.
* **Approvals**: every create / update / disable / enable / delete is proposed first. Each change type can be
  `manual` (a human approves it in the GUI) or `auto`. A fresh install starts in **dry-run** (plans are recorded,
  nothing is written) with everything manual.
* **AD changes** are made by PowerShell scripts that the Go service runs as its own identity (ideally a gMSA; an MSA
  or a normal service user account also works).
* **Placement, backups, history**: new accounts land in the OU for their department (with a human choice when none is mapped);
  encrypted backups restore on any server or database type; every action is searchable in the History page.
* **Databases**: SQLite (default), PostgreSQL, SQL Server, MySQL/MariaDB, IBM Db2 LUW 11.5+ (via ODBC, Windows only; not yet run against a live Db2).

```
HR source -> ingest -> plan (diff vs AD snapshot) -> changes table -> approve (human | auto) -> PowerShell -> AD
                                  ^ mappings, roles, naming from DB        ^ GUI: approvals, history, settings
```

## Quick start

Easiest: on the server, get this repository (`git clone`, or GitHub's *Download ZIP*), double-click **`Setup.cmd`**
and choose **GitHub** as the source. The installer downloads the code, builds it with Go on the server (Go 1.22+ must
be installed), and installs it; for a private repository give it a read-only token. No Go on the server? Build a
release zip elsewhere (below) and choose **A release zip or folder**.

```powershell
# a release folder (and zip) for servers without Go: exe + scripts + Setup.cmd + SHA256SUMS, restricted to SYSTEM/Administrators/you
.\scripts\New-IDLifecycleRelease.ps1 -OutDir C:\Install\IDLifecycle-1.0.0 -Build -Version 1.0.0 -Zip
# then double-click C:\Install\IDLifecycle-1.0.0\Setup.cmd - the setup window walks you through it (see "Install & uninstall")
# or run the installer yourself - from the release folder, from GitHub, or from the zip:
.\scripts\Install-IDLifecycle.ps1 -FromGitHub -GitHubRef main -ServiceAccount 'CORP\svc-idl$' -DryRun
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -SourceDir D:\Downloads\IDLifecycle-1.0.0.zip -ServiceAccount 'CORP\svc-idl$'
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -DryRun   # preview
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$'           # elevated; prompts for the admin password
```

Or step by step by hand:

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

## Install & uninstall

### Setup window (recommended for most people)

Double-click **`Setup.cmd`** in the release folder. It needs PowerShell 7 and a Windows edition with the desktop
experience (on Server Core use the scripts below). It asks for administrator rights; if you decline, only test runs
are possible. The window shows what is already installed, then walks through:

| Install / upgrade | Uninstall |
|---|---|
| **Source**: this release folder, **GitHub** (repository, branch/tag, token for a private repository; built here with Go), or a **release zip or folder** |
| **Locations**: program and data folder (service name under *Advanced*) | **What to remove**: program files (default yes), data folder (off by default; then a backup folder or *No backup*, and the service name typed as confirmation), DSN variable |
| **Service account**: gMSA/MSA, user account + password, or built-in | **Review** with the equivalent command |
| **Database**: SQLite, SQL Server, PostgreSQL, MySQL, Db2 + connection string (examples shown) | **Run** |
| **Web console**: address, HTTPS certificate/key, first administrator | |
| **Options**: start the service, install the AD module, upgrade (`-Force`), add the tray icon (default on), require a signed exe, skip the SHA-256 check (least safe, asks to confirm), **restore from a backup** (`.idlbak` + passphrase, optionally its settings) | |
| **Review** with the equivalent PowerShell command, then **Test run (no changes)** or **Install** | |
| **Run**: the script's log live (WOULD blue, DONE green, warnings amber, errors red), the result, *Open the web console*, *Open log folder* | |

Each page is checked before *Next*, the same way the script will check it. When IDLifecycle is already installed,
the window fills in its folders and account and pre-ticks *Upgrade*; an existing `config.json` keeps its database and
users. After a successful test run the button changes to *Install now*.

The window only collects input. Everything is done by `scripts\Install-IDLifecycle.ps1` /
`Uninstall-IDLifecycle.ps1` run in a child `pwsh -NonInteractive`, so the checks, logs and exit codes below all apply.
The passwords and the connection string are read from password boxes and reach the script only through its
environment (`IDL_PASSWORD`, `IDL_SERVICE_PASSWORD`, `IDL_SETUP_DB_DSN`, `IDL_GITHUB_TOKEN`, `IDL_BACKUP_PASSPHRASE`), which the
script removes immediately.
They never appear on a command line, in a log or in the copyable command, where they are replaced by `Read-Host`.
Removing the data folder is sent with `-Force` only after the window has checked the typed service name.
`Setup.cmd` is also copied into the program folder, so the window can be started there later: to uninstall, or to
upgrade from GitHub or a newer release zip.

### Tray icon

The tray icon (`scripts\Start-IDLifecycleTray.ps1`) starts when someone signs in to the server (or from Start menu ->
IDLifecycle -> IDLifecycle Tray). Its tooltip shows the service state (checked every 10 seconds; a notification appears
when the service stops on its own). Right-click:

| Menu item | What it does |
|---|---|
| **Open admin console** (also double-click) | opens the web console's address in the browser |
| Service: *state* - *version* | status line |
| **Settings...** | a window with the service, state, version, console address and data folder, where you change the web console address, the HTTPS certificate and key (or switch HTTPS off for an address on this server) and the log level, optionally restarting the service to apply them |
| **Restart service**, **Stop service** / **Start service** | depending on the current state |
| **Open log folder** | the service's log folder when you can read it, else the setup log folder |
| **Exit** | closes the tray icon only - the service keeps running |

The tray runs as the signed-in user and changes nothing itself: Save, Restart, Stop and Start run
`scripts\Invoke-IDLTrayAction.ps1` elevated (one UAC prompt per action). That helper checks again that it is elevated and
that the service really is this installation's `idlifecycled.exe service`, validates the settings with the same rules as
the service (`idlifecycled config set`: a network address needs HTTPS, the certificate and key must load), changes only
those fields in `config.json`, updates `tray.json`, and logs to the setup log folder. It uses the execution policy from
`config.json` (never `Bypass`). Everything else is set in the web console. One tray runs per signed-in user; it closes
itself when `tray.json` disappears (uninstall). The installer starts the tray at once for whoever is signed in (through
Explorer, so never with administrator rights); everyone else gets it at their next sign-in. On its first start the tray
asks Windows to show it in the taskbar instead of the hidden-icons overflow (^), once per user, so hiding it again
sticks. Without PowerShell 7 installed machine-wide (`%ProgramFiles%PowerShell`, e.g. PowerShell from the Microsoft
Store) the shortcuts start `Start-IDLifecycleTrayLauncher.ps1` with Windows PowerShell, which finds each user's `pwsh.exe`
at sign-in, so a PowerShell update never breaks the shortcut. `-NoTray` (or unticking the option in the setup window) leaves it out.

The same settings from an elevated command line:

```powershell
$exe = 'C:\Program Files\IDLifecycle\idlifecycled.exe'; $cfg = 'C:\ProgramData\IDLifecycle\config.json'
& $exe config get -config $cfg
& $exe config set -config $cfg -listen 0.0.0.0:8443 -tls-cert C:\certs\idl.pem -tls-key C:\certs\idl.key -log-level info
Restart-Service IDLifecycle
```

`config get` prints the basic settings as JSON (never the database connection string); `config set` changes only the
fields given, checks them as the service does at start-up (exit 2 and nothing written when they are invalid) and keeps
every other setting.

### Scripts

`scripts\Install-IDLifecycle.ps1` is the guided, idempotent setup (elevated; `-DryRun` previews every step without
elevation). Re-running it changes only what is out of line, so it is also the upgrade and repair tool. Where it installs
from:

| Source | How | Notes |
|---|---|---|
| Release folder | default `-SourceDir` = the folder the script sits in (`New-IDLifecycleRelease.ps1`) | checked in place (release trust below) |
| GitHub | `-FromGitHub [-GitHubRepo owner/name] [-GitHubRef main\|v1.2.0\|<commit>] [-GitHubToken <SecureString>]` | the ref is resolved to a commit (logged), its source zip downloaded over HTTPS, `idlifecycled.exe` built with Go on this server (`go build -trimpath -mod=readonly`, CGO off; module downloads need internet access), and a release laid out (exe, `scripts\`, `Setup.cmd`, `SHA256SUMS`). Go missing → exit 3. A private repository needs a fine-grained read-only *Contents* token (`-GitHubToken` or `IDL_GITHUB_TOKEN`); it is used only in the download requests |
| Release zip | `-SourceDir <file>.zip` (`New-IDLifecycleRelease.ps1 -Zip`) | the zip must sit where only SYSTEM, Administrators, TrustedInstaller and you can change it; entries that would land outside the folder are refused |
| Any folder or zip, unchecked (**least safe**) | `-SourceDir <folder or zip> -SkipIntegrityCheck` | for testing, or a folder you put together yourself: no `SHA256SUMS` needed (see below) |

GitHub and zip sources are put into a **new folder below the protected staging root** (`%ProgramData%\IDLifecycle-Setup\Staging`;
a non-elevated `-DryRun` uses `%LOCALAPPDATA%`), and then **that version's own** `Install-IDLifecycle.ps1` is run with
the same parameters (secrets only in its environment) - so a new version is installed by its own installer, which checks
the release again exactly like a release folder. Its log is a separate file in the same log folder. The staging folder is
removed when that run succeeds and kept (for inspection) when it fails.

| Step | What it does |
|---|---|
| Release trust | before anything from `-SourceDir` runs: the folder, every file in it and its parent folders must not be changeable by anyone but SYSTEM, Administrators, TrustedInstaller and you (no untrusted owner either); every file must match `SHA256SUMS` (which may list only `idlifecycled.exe`, `Setup.cmd` and `scripts\*`), and `scripts\` may hold nothing the manifest does not list. Exit 2, no `-Force` bypass. A signed exe's signer is reported; `-RequireSignature` refuses an unsigned one |
| Preflight | Windows + PowerShell 7, elevation, ActiveDirectory module (`Install-WindowsFeature RSAT-AD-PowerShell`, or `-InstallPrerequisites`), `Test-ADServiceAccount` for an MSA/gMSA, the password of a user service account, IBM ODBC driver for db2; a pre-existing DataDir / `config.json` / `secret.key` / DB / InstallDir owned by another account is refused (delete it if you did not create it) |
| Program files | copies the manifest's files (+ `SHA256SUMS`) to `-InstallDir`; reports install / upgrade / same from `idlifecycled version`; replacing files that differ, and removing files in `scripts\` the release does not ship, needs `-Force`; only verified release files are unblocked; InstallDir gets a protected ACL (SYSTEM, Administrators, TrustedInstaller full; Users and the service account read; owner Administrators); a running service is stopped first and always started again |
| Config | `idlifecycled init` only when `<DataDir>\config.json` is missing - an existing config is **never** overwritten, but its `powershell.scripts_dir` / `data_dir` must point at this install (`-Force` corrects just those two fields) |
| Lockdown | `idlifecycled lockdown -account <ServiceAccount>`; then (server DB) writes the DSN into `config.json`; registers the `IDLifecycle` event log source |
| Restore (optional) | with `-RestoreFrom <file.idlbak>`: the backup's header is checked in the preflight; after the lockdown a running service is stopped and `idlifecycled restore` replaces the database contents and `secret.key` (`-RestoreConfig`: also the backup's `config.json` settings; a database that already has data needs `-Force`). The passphrase comes from `-RestorePassphrase`, `IDL_BACKUP_PASSPHRASE` or a prompt and reaches only that child. See *Backups and restore* |
| Database + admin | `check` confirms the data dir ACL and creates/migrates the schema; the first admin is created when none exists (`-AdminPassword`, `IDL_PASSWORD`, or a prompt; not needed when a restored backup brings administrators) |
| Service | `Install-IDLifecycleService.ps1` from the installed scripts |
| Verify + start | every required `check -json` entry (incl. `program_files_acl`) must have run and passed (a skipped `ad_module` is fine - the preflight covers it; else nothing is started); start (restart when this run changed `config.json`), wait for Running, probe `http(s)://<listen>/` for HTTP 200, print the GUI URL |
| Tray and shortcuts | writes `<InstallDir>\tray.json` (service name, console URL, listen address, log level - no secrets), creates `%ProgramData%\IDLifecycle-Branding` (protected, with a README; see *Branding*), a Start menu entry *IDLifecycle Setup*, and - unless `-NoTray` - *IDLifecycle Tray* in the Start menu and in the common Startup folder (the tray then starts for everyone who signs in) |

`SHA256SUMS` sits in the release folder it describes, so it gives **integrity** (nothing was swapped or added after
the release was built), not authenticity: the ACL check is the real guard, which is why a folder that users can write
to (e.g. a clone of this repository below `C:\`) is refused. Sign the exe and use `-RequireSignature` if you need to
prove who built it. The installer's own log and CSV report go to `%ProgramData%\IDLifecycle-Setup\Logs` (SYSTEM and
Administrators only; a non-elevated `-DryRun` uses `%LOCALAPPDATA%\IDLifecycle-Setup\Logs`).

**Installing without `SHA256SUMS` (`-SkipIntegrityCheck`, least safe).** For testing, or when you have an
`idlifecycled.exe` (e.g. `go build -trimpath -o idlifecycled.exe .\cmd\idlifecycled` in a checkout) next to the
`scripts\` folder but no release built with `New-IDLifecycleRelease.ps1`. The installer then reads no manifest: it takes
`idlifecycled.exe`, `Setup.cmd` (if present) and every file below `scripts\` (except `Logs\`), hashes them at that
moment and installs those hashes as `SHA256SUMS`, so upgrades and uninstall work as usual. A zip without `SHA256SUMS` is
accepted too (a single top folder, as in GitHub's *Download ZIP*, is used as the release). Nothing proves the files are
the ones that were built or reviewed - a changed or extra script is installed and later run by the service with its AD
rights - so every such run logs `UNVERIFIED INSTALL` warnings and reports `IntegrityCheck,Skipped`, and the setup window
asks for confirmation. Still enforced: the folder's permissions (copy it to e.g. `C:\Install` if it is refused),
`idlifecycled.exe` and every required script, and `-RequireSignature`. With `-FromGitHub` it is not needed and ignored
(that build has its own `SHA256SUMS`). For production, reinstall from a release built with `New-IDLifecycleRelease.ps1`.

```powershell
# least safe - testing only: a self-built exe next to the scripts of a checkout, no SHA256SUMS
C:\Install\IDLifecycle\scripts\Install-IDLifecycle.ps1 -SourceDir C:\Install\IDLifecycle -SkipIntegrityCheck -ServiceAccount 'CORP\svc-idl$'
```

```powershell
# SQL Server: the DSN is stored as database.dsn in config.json inside the locked-down data dir
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -DbDriver mssql -DbDsn (Read-Host -AsSecureString 'DSN') `
    -AdminPassword (Read-Host -AsSecureString 'Admin password')
# opt-in: keep the DSN in a machine environment variable instead (config.json gets "env:IDL_DB_DSN")
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -DbDriver mssql -DbDsnEnvVar IDL_DB_DSN -DbDsn (Read-Host -AsSecureString 'DSN')
# upgrade: build a new release folder and run its installer with -Force
C:\Install\IDLifecycle-1.2.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -Force
```

Other parameters: `-InstallDir` (default `C:\Program Files\IDLifecycle`), `-DataDir` (default `C:\ProgramData\IDLifecycle`),
`-ServiceName`, `-Listen` (non-loopback needs `-TlsCertFile`/`-TlsKeyFile`), `-AdminUser`, `-DbDsnEnvVar`, `-Start:$false`,
`-RequireSignature`, `-NonInteractive`. Exit codes 0/1/2/3/4/5 as for the other scripts. Secrets (and the password
part of a DSN) never reach the log, the CSV report or a command line. Besides the parameters, the secrets can come
from the process environment: `IDL_PASSWORD` (admin), `IDL_SERVICE_PASSWORD` (service account), `IDL_SETUP_DB_DSN`
(connection string), `IDL_GITHUB_TOKEN`, `IDL_BACKUP_PASSPHRASE`; each is removed from the installer's environment as soon
as it is read, and none of these names can be used as `-DbDsnEnvVar`.

* **Service account** (`-ServiceAccount`), any of:

  | Kind | Example | Password |
  |---|---|---|
  | gMSA or standalone MSA (recommended) | `CORP\svc-idl$` | none: Windows rotates it. The preflight runs `Test-ADServiceAccount` |
  | Normal domain or local user | `CORP\svc-idl`, `.\svc-idl`, `svc-idl@corp.com` | `-ServiceAccountPassword` (SecureString) or `IDL_SERVICE_PASSWORD`, else a prompt |
  | Built-in / virtual | `NT AUTHORITY\NetworkService`, `LocalSystem`, `NT SERVICE\IDLifecycle` | none |

  A user account's password is checked against AD (or the local SAM) before anything changes. It is passed to
  `Install-IDLifecycleService.ps1` in that child's environment only, and stored by the Service Control Manager
  through `Set-Service -Credential`. It never appears on a command line, in a log or in a report. It's needed
  when the service is created or moved to the account. On later runs it's optional; give it after the password has
  changed, and the stored one is updated. If the password expires, the service stops starting, so exclude the
  account from expiry or use a gMSA. MSA, gMSA and user accounts are granted **Log on as a service** in the local
  security policy when they lack it. If a domain GPO defines that right, add the account there too, or the next
  policy refresh removes it.

  ```powershell
  C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl' -ServiceAccountPassword (Read-Host -AsSecureString 'Service account password')
  ```
* **Server databases** (mssql, postgres, mysql, db2) need an existing empty database and a login that may create tables.
  **Default (recommended):** `-DbDsn` is stored in `config.json`. `init` only gets a placeholder; the real value is
  written atomically *after* `lockdown` has restricted the data dir to SYSTEM, Administrators, the installing user and
  the service account (the same protection as `secret.key`), and `check` confirms the ACL. A re-run with the same DSN
  is skipped; replacing a different stored DSN needs `-Force`. Windows integrated auth for SQL Server avoids a password
  altogether.
* **`-DbDsnEnvVar IDL_<NAME>`** (opt-in, for teams that inject the variable with their own secret tooling; the name
  must start with `IDL_`) keeps the DSN in a machine environment variable. That is **not** a secret store:
  `BUILTIN\Users` can read `HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment` and every new process
  gets the variable. The Service Control Manager only passes a new/changed machine variable to services after a
  **reboot**, so the installer does not start the service in the run that set it.
* **db2** needs the IBM Data Server Driver for ODBC and CLI (64-bit).

`scripts\Uninstall-IDLifecycle.ps1` (or *Uninstall* in the setup window) stops and deletes the service, removes the event log source (unless another
idlifecycled service still uses it) and the program folder - with the install manifest, only the files it lists plus
the generated logs/reports in `scripts\Logs`; anything else is left and reported. The data folder (audit log,
database, `secret.key`) is **kept** unless `-RemoveData` is given together with `-Force` (or the service name typed as
confirmation) **and** `-BackupTo <folder>` or an explicit `-NoBackup`. The backup goes into a new folder restricted to
SYSTEM, Administrators and you before the zip is written; it is verified, and on any failure the partial zip is
deleted and nothing is removed. `-RemoveDbDsnEnvVar` removes the DSN variable (only `IDL_*` names).

* InstallDir is taken from the service's binPath only when that folder holds the install manifest; a data folder is
  removed only when it holds `secret.key` and a `config.json` whose `data_dir` is that folder.
* A folder is never deleted while someone other than SYSTEM, Administrators, TrustedInstaller, you or the service
  account can change it; links are removed without being followed and every entry is re-checked right before it is
  deleted.
* The path guard refuses drive roots, Windows, Program Files/ProgramData/Users themselves and anything directly below
  `C:\Users`, `ProgramData\Microsoft`, system folders (`$Recycle.Bin`, `System Volume Information`, `Recovery`),
  PowerShell's own folder, alternate data streams and junctions.
* It does **not** touch Active Directory (gMSA, managed accounts) and does **not** drop server databases - it prints
  the driver and database (never credentials) so you can drop them yourself. Re-running it is harmless (all skipped).

```powershell
.\scripts\Uninstall-IDLifecycle.ps1 -DryRun
.\scripts\Uninstall-IDLifecycle.ps1 -RemoveData -BackupTo D:\Backup -Force -RemoveDbDsnEnvVar
```

## Concepts

### Attribute modes (per mapping / per role attribute grant)

| Mode | Behaviour |
|---|---|
| `create_only` | Set when the account is created; never touched again. |
| `sync` | Pushed when the **source value changes** (compared with the last value this service wrote). A manual edit in AD survives until HR data changes again. |
| `immutable` | **Forced** to equal the source-derived value on every run; any drift in AD is corrected (and shown as a proposed change). |

`on_empty: skip|clear` decides what an empty source value does. `value_type: user_ref` turns an employee id into the
user's DN (e.g. `manager`). The ID attribute (default `employeeID`) is the link key between HR and AD.

### Field mapper (Attribute mappings page, admins)

Pick a source and **Load fields** to see every field it sends, how often it is filled, a few sample values and
which mappings / role rules read it, next to the AD user attributes. **Map…** on a source field opens a new mapping
with the template filled in (`{{ .Field }}`, or `{{ index . "Field name" }}` for names with spaces or dots); **Map…** on
an AD attribute fills in the target. Attributes that cannot be mapped (multi-valued, system-only, computed,
binary, or protected) are hidden unless you tick *Show attributes that cannot be mapped*, and each says why.

When HR renames or drops a field, the panel lists every mapping and role rule that still reads the old name (with
a hint when only the case differs, since templates are case-sensitive) so you can fix it before the next sync.

The AD list comes from the live schema through `scripts\Get-IDLSchema.ps1` (read-only; it follows the user class's
parent and auxiliary classes, so custom schema extensions show up). If the script is missing or AD cannot be read,
the panel falls back to a built-in list of common attributes and says why. The schema is cached for 30 minutes;
**Refresh from AD** re-reads it.

### Roles

A role rule grants **groups, an OU and extra attributes** to everyone whose source record matches its conditions; a rule
**without conditions applies to everyone**. Matching rules are additive for groups; for the OU and for attributes on the
same target, the higher `priority` wins. With `remove_stale_groups` on, the service removes people from *groups it
manages* (any group named in a rule or in the lifecycle groups) when nothing grants them any more; it never touches
other groups.

**Conditions** compare a source field (case-insensitive, values trimmed):

| Operator | Matches when the field ... |
|---|---|
| `eq` `ne` | equals / differs from the value |
| `in` `not_in` | is / is not one of a comma-separated list |
| `contains` `not_contains` `starts_with` `ends_with` | as named |
| `regex` `not_regex` | matches / does not match a regular expression (RE2) |
| `exists` `empty` | has a value / is missing or blank |
| `gt` `gte` `lt` `lte` | compares as numbers when both sides are numbers, else as dates (YYYY-MM-DD, RFC 3339, M/D/YYYY) |
| `older_than_days` `newer_than_days` | is a date more / less than N days ago (e.g. hire date newer than 30 days) |
| `template` | a template (same functions as mappings) renders `true`, `yes` or `1` - for anything else, e.g. `{{ and (eq .Company "Acme") (regexMatch "^IT" .CostCenter) }}` |
| `all_of` / `any_of` | a group of conditions that must all / any match - nest them (up to 3 levels, 50 conditions per rule) for "A and (B or C)" |

The rule editor builds these with a form, and **Test this rule** shows for a sample record which condition matched and
why (✓/✗ with the actual value). Groups are added with a **live AD group search** (type 2+ letters: name, account name and
description, the whole domain; administrative groups are marked and can never be granted - the scripts refuse to add
anyone to Domain Admins, Administrators and the other protected groups). If AD cannot be searched, type the group name.

### Lifecycle groups

**Configure → Lifecycle groups** sets groups by account state, on top of the role rules:

| Setting | Effect |
|---|---|
| Active accounts get | every active account is a member (e.g. `ALLUSERS`), like a rule for everyone |
| Disabled accounts get | added when an account is disabled (e.g. `Disabled Users`); removed again when it is re-enabled |
| On disable remove | **all groups except the kept ones** (default), only **groups this service manages**, or **nothing** |
| Always keep | groups never removed on disable (e.g. a mailbox-retention group) |

Administrative groups are never removed by a disable either. Older installs keep their behaviour: the former
*strip groups on disable* setting becomes "all groups except the kept ones" (on) or "nothing" (off).

### Previewing the impact of a change

**Preview impact** (on a role rule, the lifecycle groups and the mappings) and **Operate → Preview changes** show what
the next sync would do to every account - before anything is saved. The service plans everyone twice against the same
AD snapshot - with the saved configuration and with your unsaved one - and lists who is affected: people changed per
change type, the groups added and removed and how many people each affects, attributes changed, and per person the
difference (+ added / − removed steps). A live search box finds one person (name, ID or account name); filters show
only changed people or one change type. Nothing is written and no names are checked in AD; the AD snapshot is reused
for 15 minutes (**Refresh from AD** takes a new one). Without changes, the page shows exactly what the next sync plans.

### Microsoft 365, Entra ID and Exchange Online groups

Role rules and lifecycle groups can also grant **cloud groups**. Once the Microsoft 365 connection is set up, the
group search has a second source, **Entra ID / Microsoft 365**, next to Active Directory. Cloud groups are stored as
`entra:<object id>`.

| Group type | Changed through | Notes |
|---|---|---|
| Entra ID security group (cloud) | Microsoft Graph | |
| Microsoft 365 group (Teams, SharePoint, shared mailbox) | Microsoft Graph | |
| Mail-enabled security group (cloud) | Exchange Online PowerShell | Graph cannot change these |
| Distribution list (cloud) | Exchange Online PowerShell | Graph cannot change these |
| Any group synced from AD, including on-prem Exchange distribution groups | Active Directory (as before) | pick it under *Active Directory*; the cloud copy follows by sync |
| Dynamic groups, role-assignable groups | never | shown greyed out with the reason |

How it works:
- The service changes the **cloud copy of each AD account** (users synced by Entra Connect or Cloud Sync). It
  matches the account by its SID (`onPremisesSecurityIdentifier`, the default), by `onPremisesImmutableId`, or by UPN.
- A new hire gets their cloud groups on the first sync after their account appears in Entra ID, normally within 30
  minutes. Until then the preview shows "waiting for Entra sync".
- Cloud group changes are a separate change type, **Cloud groups**. They have their own approval setting, so they
  can fail or wait for approval without holding up the AD change for the same person.
- They follow the same lifecycle rules as AD groups: active accounts are added to role and *active* groups, and
  stale managed groups are removed. On disable, the account is taken out of all cloud groups except the kept ones,
  or only the managed ones, or none. A rehire leaves the *disabled* groups again.
- Dynamic, role-assignable and AD-synced groups are always left alone.
- If Microsoft 365 cannot be reached, the run warns and plans no cloud changes; the AD work still runs.

#### Access required

You need one app registration in Entra ID (single tenant) with a certificate. A client secret also works if you only
use Graph.

1. **Certificate.** On the IDLifecycle server, as an administrator, run:
   ```powershell
   scripts\New-IDLCloudCertificate.ps1 -OutDir C:\Temp
   ```
   It creates a self-signed certificate in `LocalMachine\My`, valid for 2 years, with the key held by the older key
   provider (CSP) that Exchange Online requires. It gives the service account read access to the private key and
   exports only the public `.cer` file. Upload that file under the app's **Certificates & secrets**.
2. **Microsoft Graph application permissions**, with admin consent granted:
   | Permission | Why |
   |---|---|
   | `GroupMember.ReadWrite.All` | search groups, read members, add and remove members (security and Microsoft 365 groups) |
   | `Group.Read.All` | read every group's properties, including whether it is role-assignable (without it such groups are refused as *unverified*) |
   | `User.Read.All` | find the cloud copy of each AD account, and its group memberships on disable |

   Do **not** grant `RoleManagement.ReadWrite.Directory`. Role-assignable groups are refused by design.
3. **Exchange Online** (only for mail-enabled security groups and distribution lists):
   - Under **API permissions → APIs my organization uses → Office 365 Exchange Online → Application**, add
     **`Exchange.ManageAsApp`** and grant admin consent.
   - Give the app's service principal Exchange rights. The least-privilege way is a custom role group with the
     **Distribution Groups** and **Security Group Creation and Membership** roles. Use the *Enterprise application*
     object id, not the app registration's:
     ```powershell
     Connect-ExchangeOnline     # as an Exchange admin
     New-ServicePrincipal -AppId <client-id> -ObjectId <enterprise-app-object-id> -DisplayName "IDLifecycle"
     New-RoleGroup -Name "IDLifecycle Group Membership" -Roles "Distribution Groups","Security Group Creation and Membership"
     Add-RoleGroupMember -Identity "IDLifecycle Group Membership" -Member <enterprise-app-object-id>
     ```
     The simpler, broader alternative is to assign the Entra role **Exchange Recipient Administrator** to the app.
   - Install the **ExchangeOnlineManagement** module (3.x) for all users on the server:
     `Install-Module ExchangeOnlineManagement -Scope AllUsers`.
4. **Settings → Microsoft 365 / Entra ID**:
   - Fill in the environment (Global, GCC High, DoD or China), tenant ID, client ID and the certificate thumbprint
     (or client secret).
   - Choose the user matching.
   - Turn on Exchange Online and enter the organization (`contoso.onmicrosoft.com`).
   - Click **Test connection**. It checks the certificate (private key, CSP, days to expiry), the token, the Graph
     permissions in the token (permissions broader than needed get a warning), reading groups and users and, if
     Exchange Online is on, the module, the connection and the Exchange rights.

Secrets are stored encrypted and shown as `********`. A certificate never leaves the server. Anyone holding the
certificate's private key can act as the app, so keep `LocalMachine\My` restricted to administrators and the service
account. Renew the certificate before it expires; **Test connection** warns 30 days ahead.

### Templates

Go `text/template` over the source record: `{{ .FirstName }}`, `{{ index . "currentStatus.statusType" }}`.
Functions: `lower upper title trim first last substr replace default coalesce join ascii digits alnum split`, and
regular expressions (RE2 syntax: linear time, so a pattern cannot hang a run; at most 256 characters):

| Function | Example | Result |
|---|---|---|
| `regexReplace pattern replacement value` | `{{ .Phone \| regexReplace "[^0-9]" "" }}` | `5550109999` (`` etc. refer to groups) |
| `regexFind pattern value` | `{{ regexFind "^([A-Z]+)-" .CostCenter }}` | the first capture group (or the whole match) |
| `regexMatch pattern value` | `{{ if regexMatch "(?i)^contractor" .Type }}c-{{ end }}` | true/false, for `if` |

The value comes last, so they work in a pipe. Example: `{{ first .FirstName 1 | lower }}{{ .LastName | lower | alnum }}`.
`range`, `define` and `template` are rejected.

**Account names and mail** (Settings → Naming): templates for sAMAccountName, the UPN prefix, the CN and - optional -
the mail address's local part (domain: *Mail domain*, default the UPN suffix). Mail from the naming template is set
when the account is created; a mapping that writes `mail` takes precedence and keeps it in sync.

**Name conflicts** (Settings → Naming → *When a name is taken*). Every sync checks a new account's sAMAccountName and UPN
(any object, whole directory), mail (`mail` or `proxyAddresses`, also contacts and groups) and CN (in the target OU)
in AD, and against the other people planned in the same run (`Test-IDLNames.ps1`, read-only):

| Mode | When `jdoe` is taken |
|---|---|
| **Ask a person** (`manual`, default) | the create waits in Approvals with *Set username*: the taken names and who has them, free suggestions, a live check; *Approve* stays disabled until a name is set. An `identity_issues` notification says so. |
| Add a number (`number`) | `jdoe2`, `jdoe3` ... up to `max_candidates` |
| Add random digits (`random`) | `jdoe` + 2-6 digits (the same digits every run for the same person) |
| Add a value from the record (`template`) | `jdoe` + `conflict_suffix_template`, e.g. `{{ last .EmployeeID 4 }}` → `jdoe1042` |

Mail, UPN and CN get the same suffix. When an automatic mode runs out of free names the create waits for a person
the same way. A chosen name is remembered for that person (the next sync proposes the same create) and is in the
History (`change.set_name`).

**Initial passwords** (Settings → New accounts): *Random* (default; `password_length` characters), or *Built from a
template* with the functions above plus `random n`, `randomDigits n`, `randomSymbols n` (crypto-random, 1-64), e.g.
`{{ upper (first .LastName 1) }}{{ last .EmployeeID 4 }}!{{ random 6 }}`. A template password is rendered when the
account is created, never stored or logged, always has *change at next logon* set, and must have 8+ characters with 3
of uppercase / lowercase / digits / symbols and not contain the account name - otherwise that account's create fails
with the reason (the password is never in the message). Anyone who knows the HR data can work out the fixed part, so
keep a random part in it. The preview shows a sample password and whether AD's complexity rules accept it.

### Lifecycle & safety rails

* Terminated (status field matches `terminated_values`, the leaving date has passed, or absent from the feed for more
  than `missing_grace_runs` runs when `missing_means_terminated`) → **disable** (move to the Disabled OU of the
  person's retention policy; groups by *Lifecycle groups*). After the retention policy's days (default
  `delete_after_days`) → **delete**. Active again → **enable**. See *Leavers: termination dates and retention*.
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
  external id before touching it. New accounts get a random (or template, see *Templates*) password that is never stored
  or logged; hand-over is out of scope (use your normal reset process).
* Every action is in an append-only, hash-chained **audit log**, searchable on the **History** page (*Verify integrity* checks the chain).

### Leavers: termination dates and retention

**When someone counts as having left.** Any of these, per source (*Sources* → the source):
* the status field has a terminated value (`terminated_values`);
* **Leaving date**: the date in `termination_date_field` has passed (plus `termination_after_days`, 0 = from the day after
  the date), in the schedule's time zone. Formats: *Automatic* (`2026-10-31`, ISO date-times, `10/31/2026`, `31.10.2026`,
  an Excel day number) or your own Go layout. A future date does nothing yet; the person's page and *Leavers* show
  "Leaving on …". An unreadable date is ignored and counted as a warning. A date that is cleared or moved later makes
  the person active again (re-enabled like any rehire);
* they are missing from the source for `missing_grace_runs` runs (`missing_means_terminated`).

**What happens then.** A **disable** change: the account is disabled, moved to the Disabled OU, removed from its
groups except those you keep (*Lifecycle groups*: *disabled_remove*, *disabled_keep*) and added to the groups for
disabled accounts. After the **retention** period, a **delete** change. Both follow the approval policy, the bulk guard
and the delete limits; a person who is Manual (whole person or state) is never disabled or deleted automatically.

**Retention policies** (*Configure → Retention*) decide how long a disabled account is kept and where it goes:

| Setting | |
|---|---|
| Delete after | N days after the account was disabled, or **Never delete automatically** (e.g. a legal hold) |
| Disabled OU | optional; overrides the global Disabled OU (the snapshot reads every policy's OU, so the account stays tracked) |
| Extra groups to keep | kept on disable in addition to *Lifecycle groups → keep* |
| Deletes always wait for a person | the delete is held for manual approval even when deletes are `auto` |
| Conditions | the role-rule condition builder (ALL / ANY, nested groups), on the person's last HR record (kept after they leave) and `ad.memberOf` (any of the account's groups, by DN or name), `ad.ou`, `ad.sam`, `source`, `origin` (`source` / `manual`) |

Which policy applies, in order: a policy **assigned to the person** (*Identities* → the person → **Change retention…**,
approvers, reason required), else the **first enabled policy by priority whose conditions match**, else the built-in
**Default** (the global *delete after* days and Disabled OU, edited on the same page). A policy without conditions
applies only to people assigned to it. Changing a policy recalculates the delete date of people already disabled.
Each policy has **Test this policy** (against a person), **Preview impact** (who would be deleted now, later or no
longer), a people count and a History link.

**Seeing what is coming.** *Operate → Leavers* lists upcoming deletions (overdue ones first) and people leaving in the
next 7 / 30 / 90 / 365 days, with the policy and why it applies. The person's page shows "Disabled on … · deleted on …
(policy Executives, matched its conditions)" or "Leaving on …". History records `identity.inactive` (reason
`termination_date`), `identity.retention_changed` (another policy now applies) and `identity.retention_set` (assigned
by a person); policy edits are `retention.create|update|delete` with before/after detail.

### Undoing a change

An applied change, part of one, or several changes of a run can be rolled back: attributes, AD groups, an OU move, a disable, an enable, a create, a link, or
cloud group memberships. It is built so that it can't happen by accident or without knowing exactly what it does.

**What makes it possible.** When a script applies a change, it reports what the account looked like *just before*:
the attribute values it replaced, the groups it actually added or removed, the DN, whether the account was enabled,
the ID value and names (links), and the cloud groups it changed. That is stored with the change. Changes applied
before this feature have no such record and can't be undone.

| Change | Undo does |
|---|---|
| update | puts the previous attribute values back, re-adds removed groups, removes added groups, moves back to the previous OU |
| disable | enables the account, moves it back, restores its groups and description |
| enable | disables it again, moves it back, restores its groups |
| create | **disables** the account it created (it is not deleted) |
| link | puts the previous ID value and names (incl. `proxyAddresses`) back; that account is not offered for this person again |
| cloud groups | re-adds the groups it removed and removes the groups it added |
| an undo | can itself be undone (redo) |
| delete | **not possible**: deleted accounts can only be restored from the AD Recycle Bin (`Restore-ADObject`) |

**How it works**

1. **Request** (admins): *Approvals* → status *applied* → **Undo…** on the change. The dialog shows the change being
   undone, every step in plain words, and a **live dry run** against AD (or Microsoft 365): the script runs with
   `-DryRun`, reads the account and reports what it would do, writing nothing.
   * **Drift**: if anything the change set was changed since (by someone, or by a later change), it is listed in red
     and the undo is refused unless you tick *Overwrite the later changes* **and** a second box confirming you checked
     them.
   * You must enter a **reason** (10+ characters) and **type the account's username** to confirm.
   * The server recomputes the undo when you submit; if it differs from what you previewed, it refuses and asks you to
     preview again.
2. **Approve** (approvers): an undo always waits in *Approvals*, red **undo** tag; it is never approved automatically,
   never in bulk. *Approve undo…* shows who asked and why, runs a fresh dry run, and needs the username typed again.
   With **Settings → Undo → Require a second person**, the requester can't approve their own undo.
3. **Apply**: the script checks the account still looks as the original change left it (`expect`); if not, nothing is
   changed and the undo fails with the differences (`drift: title is 'Lead', expected 'Senior'`). The original change is
   marked *undone*; the identity's state follows (an undone disable makes it linked again, and so on).

**Undoing only part of a change.** For an update or a cloud-group change, the dialog lists each part with a tick box:
every attribute, every group added or removed, the OU move, every cloud group. Untick what you want to keep; the
dry run, the drift check and the steps then cover only the ticked parts. A later edit to a part you are *not* undoing
never blocks the undo (the account's OU is only checked when you undo the move). Undo the remaining parts later if you
need to; the change shows *partly undone* until every part is. Creates, deletes, disables, enables and links are undone
whole.

**Undoing several changes of a run.** *Runs* → a run → **Undo changes from this run…** (admins) lists every applied
change of that run by person, with the same part tick boxes (changes that can't be undone are greyed, with the reason).
**Preview** dry-runs the whole selection at once; then a reason, *Mark every undone item as Manual* (ticked) and typing
`UNDO <n>` (the number of undo changes) request one undo per change, as a **batch** (at most 200). In *Approvals*,
**Approve batch…** shows a fresh dry run of each and needs `UNDO <n>` typed again; if any of them would now overwrite a
later change, the batch is refused and those are approved or rejected one at a time. Individual approval still works.

**Manual: stopping the next sync from redoing it.** If the HR data and the rules still say the same thing, the next
sync would plan the change again. So everything an undo rolls back is set to **Manual** by default: IDLifecycle will not
change that item for that person until someone sets it back to **Auto**. Items: an attribute, a group, the OU, a cloud
group, the person's **state** (no create, link, disable, enable or delete), or the **whole person** (nothing at all is
planned for them). Manual items need no end date (one can be set).

*Identities* → a person → **Managed fields** shows every mapped attribute, group and the OU with the value in AD, what
the rules would set, and *Auto* / *Manual*. Approvers can **Make Manual…** (reason required; also without an undo, e.g.
after fixing something in AD by hand) and **Set back to Auto…** (it says what the next sync will then do). Each Manual
item shows what IDLifecycle *would* have set at the last sync. *Identities* has a *Managed* filter and a *Manual (n)*
badge. The planner and *Preview impact* skip Manual items; the run counts them as `on_hold`.

> A person who left HR but is Manual (whole person or state) is **not disabled**. The run counts them as
> `manual_not_disabled`, the *identity issues* email names them and their page shows a warning.

Undo can't be requested while **dry run** is on (nothing is applied in dry run) or when **Settings → Undo** is off.
Every step is in History: `change.undo_requested`, the approval, `change.applied` / `change.failed` of the undo, `change.undone` on the original,
`undo.batch_requested` / `undo.batch_approved` / `undo.batch_rejected`, and `identity.hold_added` / `identity.hold_released`
(shown as *Set to Manual* / *Set back to Auto*).

### OU placement (by department)

**Configure → OU placement** chooses a new account's OU from a source field (default `Department`):

1. an OU someone chose for that person on an approval (*Change OU*) - kept until their department changes;
2. a role rule that sets an OU (keeps working for exceptions such as contractors);
3. the placement rules: department value → OU, picked from the OU tree read from AD (`Get-IDLOUs.ps1`, read-only,
   below the search bases; cached 30 minutes, *Re-read from AD*). Values are compared without regard to case and
   `*` is a wildcard (`Sales*`); exact values win. Rules can be imported/exported as CSV (`department,ou`);
4. a value without a rule: by default the create **waits in Approvals** with *Choose OU* (also when creates are
   auto-approved; *Approve* is disabled until an OU is chosen), or - setting - the default OU with a warning.

The page lists every department among the active people with how many have it and the OU it maps to, unmapped
first, with *Map…* to add a rule. With *move on change* (default), an existing account is moved when its department
changes - and only then, so an OU an administrator sets by hand in AD is left alone; a title change alone never moves
anyone. A department change to a value without a rule waits in Approvals the same way. On an approval, *Change OU*
picks another OU for that person (approvers), optionally also adding the rule for the whole department (admins).
Each choice is in the History (`change.set_ou`).

### HR system API (generic REST connector)

Sources → New source → *HR system API (REST/JSON)* reads people from any HR system that has a REST API returning
JSON - no code needed. Fill in the base URL and path of the endpoint that lists people, the authentication (none, an API
key in a header or query parameter, Basic, a fixed bearer token, or OAuth2 client credentials with token URL, client
ID/secret, scope and audience), where the array of people sits in the response (*Records path*, e.g. `data` or
`result.employees`; empty when the response is the array) and how the API pages:

| Pagination | Parameters |
|---|---|
| none | one request |
| page | page number + page size parameters (first page 0 or 1); stops on an empty or short page |
| offset | offset + limit parameters |
| cursor | cursor parameter + the dotted path of the next cursor in the response (`meta.next_cursor`) |
| next_link | the dotted path of the next page's URL (`links.next`), or the `Link: <...>; rel="next"` header |

Nested JSON becomes dotted field names (`name.first`, `department.name`; arrays of values are comma-joined), which is
what mappings and role rules use; *Only these fields* keeps a short list. **Test these settings** in the editor fetches
once with the form as it is - before saving - and shows the record count, every field name and a sample.

Secrets (API key, password, token, client secret) are stored encrypted, masked in the GUI and never written to errors or
logs; a stored secret is never sent to a changed base or token URL. Requests time out (60 s), retry on 429/5xx (honouring
`Retry-After`), refuse redirects or next links to another host, and cap a page at 50 MiB. More pages than *Maximum pages*
or people than *Maximum records* is an error, never a shorter list - a truncated feed would look like people leaving.

**A dedicated connector** is for an HR system the generic one cannot read (an unusual login, a report that must be
requested and polled, XML ...). It is a small Go file in `internal/sources`: a `Type` (name, label, form fields -
password fields are encrypted automatically) and a `Fetch` that returns the complete population. The package
documentation (`internal/sources/sources.go`) lists the steps, `example_connector_test.go` is a complete ~50-line
connector with its test, and `NewHTTPClient` (retries, size cap, same-host redirects, secrets kept out of errors),
`Flatten` and `RecordsAt` do the plumbing. Register it in `init` in `sources.go`.

### Several sources and people added by hand

Any number of sources can be enabled; each person is one record per source (`external_id` = the AD ID attribute
value). The same ID in two sources is refused (both would drive one account) - give each source its own ID range.

**Add person** (Operate menu, approvers) is for contractors and other out-of-band requests. Create a source of type
*Manual entry* once (Sources): its **form fields** (`Name | Label | flags`, flags `required`, `date`, `email`,
`select: A; B`, `default: x`) decide what the form asks; use the same field names as the HR source (`FirstName`,
`LastName`, `Department` ...) so mappings, roles, OU placement and naming apply unchanged. A new person gets the ID
`<id prefix><6 characters>` (e.g. `C-7K2Q9D`). *Preview* shows the account names, the OU and any name conflicts before
anything is saved; *Create* (optionally with an immediate sync) puts the person into the normal flow - the create
still goes through Approvals. *End* (or an *End date* that has passed) makes them terminated, so the account is
disabled through the normal flow; *Delete* is only possible before an account exists. Every action is in the History
(`manual.*`).

### Matching existing accounts (correlation)

**Configure → Matching** links people to accounts that already exist instead of creating new ones - for the first
rollout into an existing directory (accounts without an ID yet), a contractor who becomes an employee (the account
belongs to the manual record, the HR source now has the person under a new ID), or a rehire with a new ID.

* **Rules** compare a source field with an AD attribute: *exact*, *ignore case* or *name* (also ignores accents,
  punctuation and spaces: `O'Brien` = `obrien`). An account is a candidate only when every *required* rule matches
  (default: first and last name; department is optional).
* **All rules match, one account** → a `link` change: the account's ID attribute is set to the person's ID
  (`Set-IDLLink.ps1`, which checks the current value first). `approval.link` is `auto` by default, so this happens
  without a click (dry-run still applies).
* **Only some rules match, or several accounts qualify** → the link waits in Approvals with the candidates side by side
  (each field: source value, AD value, match). *This is the person* picks one, *None of these* plans a new account,
  *Not this person* (reject) makes sure that account is never proposed for that person again.
* **Name change on link** (`rename`): *transitions* (default) renames the account to the naming of its new record when
  it belonged to another record (`c-jdoe` → `jdoe`; the old mail address stays as an alias); *always* also renames
  accounts found at the first rollout; *never*. Taken names are not used (the summary says why).
* The old record (contractor entry, former ID) becomes **merged** and is skipped from then on, so ending the contract
  later can never disable the employee's account. While a link is open, the old record gets no changes.
* Accounts with `adminCount = 1` (administrators) are never matched. More than `max_auto_links_per_run` (25) automatic
  links in one run are all held for approval.
* **Report**: after every sync, the Matching page lists enabled accounts no record accounts for (no ID, or an ID no
  source knows), matches waiting for a decision and usernames to choose. *Link to person…* tells the next sync to link
  an account to a record by hand; *Ignore* adds the account (or a pattern such as `svc_*` or an OU DN) to
  `ignore_accounts`. The `identity_issues` notification reports new name conflicts, matches to verify and accounts
  that appear in the report for the first time.

## Backups and restore

A backup is one file (`IDLifecycle_<host>_<time>.idlbak`) with every table of the database (rules, identities, changes,
history, users, settings - not sign-in sessions), `config.json` and the encryption key, encrypted with a passphrase
(scrypt + AES-256-GCM in chunks; a changed, truncated or extended file does not decrypt). Because it holds plain rows
it restores into **any** supported database - also to move from SQLite to SQL Server - on this or another server.

* **Settings → Backups**: the passphrase (12+ characters, stored encrypted; keep a copy outside the server - without it
  a backup cannot be restored), scheduled backups (every N hours, keep the newest N, folder - default
  `<data_dir>\backups`, inside the locked-down data folder), *Back up now*, and the list with *Download* (admins).
* `idlifecycled backup [-config F] [-dir D] [-note T]` (passphrase from `IDL_BACKUP_PASSPHRASE` or a prompt).
* **Restore**: in the setup window (*Options → Restore from a backup*), `Install-IDLifecycle.ps1 -RestoreFrom F`, or
  `idlifecycled restore -in F [-config C] [-with-config] [-force]` with the service stopped. The database's contents are
  replaced in one transaction (a wrong passphrase or damaged file changes nothing), the backup's key becomes
  `secret.key` (the old one is kept as `secret.key.replaced-<time>`), all sign-ins end and the history records a
  `backup.restore`. `-with-config` also takes the backup's `config.json` except `data_dir`, `database` and
  `powershell.scripts_dir`. A database that already has data is replaced only with `-force`. A backup from a newer
  version (database schema) is refused. `idlifecycled restore -in F -inspect` shows a backup's header without the
  passphrase.

## History and search

**History** (Operate) searches the hash-chained audit log: every proposed, approved, rejected, applied or failed change,
sync runs, sign-ins and failed sign-ins, settings / mapping / role / source / user changes, people added by hand,
undos and holds, what happened to each person (below), backups and restores, service start and stop. Type words or a
query, or use the filters (*What*, *When*, *Errors and failures only*). **Console activity** shows everything people
did (`-actor:system -actor:oidc -actor:anonymous`), **Sign-ins** the sign-ins:

| Query | Finds |
|---|---|
| `timeout dc01` / `"password expired"` | entries mentioning both words / the phrase (actor, action, entity or detail) |
| `-dryrun` | leaves out entries mentioning a word |
| `actor:alice` (repeat for any of several), `actor:system` | who did it |
| `-actor:system`, `-action:auth.*`, `-type:change` | leaves out an actor, action or entity type |
| `action:change.failed`, `action:change.*` | what happened; `*` at the end = prefix |
| `type:change id:<id>` | everything about one change, run, source, mapping, role, user, manual_person, identity ... |
| `level:error` | failures: `*failed / *denied / *rejected / *blocked` actions, or a non-empty `error` in the detail |
| `since:7d`, `since:2026-10-01`, `before:2026-10-25T08:00` | time range (local time) |

Click an actor, action or entity in the results to add it to the query. *Export CSV* downloads up to 10,000 matching
entries (`/api/audit?q=...&format=csv`; the export itself is recorded).

**What a console change records.** The signed-in user and their IP address, and:
* updates: every changed field with its value before and after (`changes`), shown as a *Field / Before / After* table;
* creates and deletes: the object's fields (`fields`), so a deleted source, mapping, role or user can be read back;
* passwords, API keys, client secrets, DSNs and the backup passphrase: only that they **changed** (or `(hidden)`),
  never a value or its ciphertext.

**History of one object.** Sources, mappings, roles, console users and people added by hand have a **History** link
that opens this page filtered to that object (`type:source id:"<id>"`).

**History of one person.** *Identities* → a person → **History** tab: one timeline, newest first, of everything that
happened to that person and their account:

| Entry | When |
|---|---|
| First seen in *source* / Added in the console by *user* | the identity was created (origin, and who added a manual person) |
| Record changed in *source* | the HR record changed: each field, before → after (only real changes, not every sync) |
| Became inactive / Active again | terminated (status value) or missing from the source, and back |
| Account created, updated, linked, disabled, enabled, deleted | each change: proposed, approved / rejected by whom, applied or failed, with the attributes, groups and OU it set |
| AD account no longer found | the account disappeared from AD outside IDLifecycle (once per disappearance) |
| Account deleted from AD | a delete was applied; the last known username, UPN, DN, GUID and attributes are kept |
| Undo requested / undone (whole or in part), set to Manual / back to Auto | see *Undoing a change* |

The **Overview** tab shows where the person came from and, once the account is gone, a *Removed from AD* box with the
last known account. **Changes** lists every change for that person.

**Nothing is ever purged.** People stay in the app database after they leave HR and after their account is deleted from
AD, with their changes and history; a source that still has people can only be disabled, not deleted. The audit log
holds HR field changes permanently, so it contains whatever personal data those fields hold (a source set to *Only
these fields* keeps it to that list). Backups include all of it.

## Operating notes

* **Permissions**: run the service as a gMSA (or MSA, or a dedicated user account) that is delegated only what it
  needs (create/delete user objects and write the mapped attributes in the target OUs, manage membership of the
  managed groups). IDLifecycle stores nothing for AD authentication; with a user account, Windows keeps its password
  for the service. The account needs read access to the scripts folder and write access to
  `scripts\Logs` and the data dir.
* **CSV sources** can never read the service data directory; set `csv_allowed_dirs` in config.json to restrict them to specific folders.
* **SQL sources** run one `SELECT`/`WITH` query inside a read-only (or always-rolled-back) transaction; queries containing
  `;` or write keywords (INSERT, UPDATE, DELETE, INTO, EXEC, ...) are refused. That check is defence in depth: give the source a
  **read-only DB login**. The connection string is stored encrypted (or use `env:VAR_NAME`); for SQL Server omit `user:pass@` to use
  Windows integrated auth as the service account. More rows than `max_rows` fails the fetch rather than truncating it.
  Drivers: `sqlserver`, `postgres`, `mysql`, `sqlite`, plus **`db2`** (IBM Db2 LUW / z/OS; alias `ibmdb2`) and **`odbc`** (any 64-bit ODBC driver; Db2 for IBM i uses this path, aliases `db2i`, `as400`, `ibmi`, because the IBM i Access driver does not support the LUW text options `db2` adds)
  and **`odbc`** (any other ODBC driver: Oracle, Informix, Snowflake...). These two go through the Windows ODBC driver manager, so install a
  **64-bit** ODBC driver on the service host (the service is 64-bit): the *IBM Data Server Driver for ODBC and CLI* for Db2 LUW/z/OS (z/OS usually
  also needs DB2 Connect licensing), *IBM i Access Client Solutions* (ODBC driver) for IBM i. The DSN is an ODBC connection string, e.g.
  `Driver={IBM DB2 ODBC DRIVER};Hostname=db2.corp;Port=50000;Protocol=TCPIP;Database=HR;Uid=svc_hr_ro;Pwd=...;`, or simply `DSN=HRDB2;`
  for a system DSN made in *ODBC Data Sources (64-bit)*. ODBC cannot open a read-only transaction here, so the read-only login is the real
  guard (grant it SELECT on the HR view only). DECIMAL/NUMERIC come back as floating point: a value wider than 15 significant digits
  (or an integer of 2^53 or more) **fails the fetch** instead of being rounded, and values are printed in shortest form, so `12345.10`
  reads `12345.1` - `CAST` code-like numeric keys (employee numbers) to `CHAR`. DATE reads `2024-01-31`, TIMESTAMP is converted to
  UTC (`2024-01-31T07:30:00Z`), so neither changes with the service's time zone or DST. For `db2` the service appends
  `LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;` unless set (text then arrives as Unicode; `MapCharToWChar=0` is refused);
  binary columns (VARBINARY, BLOB, `FOR BIT DATA`) are refused - select `HEX(col) AS col`. A connect that does not answer within
  `timeout_seconds` is abandoned, and the source refuses to fetch until that attempt has given up.
  The Db2 path has not been tested against a live Db2 server.
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
* **Db2 (LUW 11.5+) as the app database**: `"driver": "db2"` and an ODBC connection string as the DSN, e.g.
  `Driver={IBM DB2 ODBC DRIVER};Hostname=db2.corp;Port=50000;Protocol=TCPIP;Database=IDLDB;Uid=idl;Pwd=...;` or `DSN=IDLDB;Uid=idl;Pwd=...;`
  (prefer `env:IDL_DB_DSN`). Install the **64-bit IBM Data Server Driver for ODBC and CLI** on the service host (the `Driver={...}` name
  depends on the install; a system DSN from *ODBC Data Sources (64-bit)* avoids guessing). Use a dedicated schema (the login's default /
  `CurrentSchema=IDL;`); the service login needs CREATE TABLE / CREATE INDEX in it for the first-run migrations, then only
  SELECT/INSERT/UPDATE/DELETE. The service appends `LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;` unless you set them (needed
  to read CLOBs and get Unicode text through the ODBC driver; `MapCharToWChar=0` is refused, it would garble non-ASCII text), and gives
  up on a connect with no answer within 20s. Use a Unicode (UTF-8) database without Oracle compatibility (`DB2_COMPATIBILITY_VECTOR=ORA`
  turns `''` into NULL, which breaks NOT NULL columns). VARCHAR lengths are bytes in Db2, so a 255-character limit is 255 *bytes* for
  non-ASCII text. If non-ASCII text reads back garbled, set the `DB2CODEPAGE=1208` environment variable for the service.
  **This path is unit/statement-tested but has not been run against a live Db2 server** — run the store suite against one first
  (`$env:IDL_TEST_DB2_DSN='...'; go test ./internal/store/`).
* **Logs**: `<data_dir>\logs\idlifecycle.log` (JSON, rotated), PowerShell logs and CSV reports in `scripts\Logs`.

## Branding (logo, name, colour)

The default mark - a person inside a lifecycle arrow - is in `scripts\branding\` (`logo.svg`, `idlifecycle.ico` with
16-256 px, `logo-256.png`, `brand.json`); `New-IDLBrandAssets.ps1` there redraws them (and the web console's embedded
copies) if you want to restyle it. It appears in the web console (browser tab, top bar, sign-in page), the tray icon and
its settings window, the setup window and the shortcuts.

To use **your own branding**, put any of these files into `%ProgramData%\IDLifecycle-Branding` on the server (the
installer creates the folder with a README; everything is optional and each file is at most 1 MiB):

| File | Changes |
|---|---|
| `brand.json` | `{"product_name": "Acme Identity", "accent": "#0a7d5a"}` - the name in the top bar, sign-in page, page titles, tray tooltip and window titles (1-40 characters), and the web accent colour (`#rrggbb`, optional) |
| `logo.svg` or `logo.png` | the logo in the web console (SVG wins) and the tray settings window (PNG) |
| `favicon.ico` | the browser tab icon |
| `tray.ico` | the tray icon and the desktop windows (falls back to `favicon.ico`, then the default) |

The web console picks changes up within a minute (no restart); the tray and windows on their next start. The folder is
used only when nobody but SYSTEM, Administrators and TrustedInstaller can change it (Windows lets every user create
folders in `ProgramData`, so a folder someone else planted is ignored and a warning is logged). An invalid file (bad
JSON, wrong format, too big) is ignored on its own; SVG logos are served with a sandboxing content security policy. The
`branding_dir` setting in `config.json` points the service at another folder.

## Sign-in: local accounts and optional Microsoft Entra ID

Every page and API call needs a sign-in. Local accounts (username + password, roles viewer / approver / admin)
always exist: the installer creates the first admin, admins add more on **Users**, and anyone who is an
administrator on the server can run `idlifecycled user add|passwd`.

**Sign in with Microsoft (optional, for MFA).** Turn it on under **Settings → Sign-in**:

1. Entra admin center → **App registrations → New registration** (this directory only). Redirect URI: platform
   **Web**, `https://<server>:<port>/api/auth/oidc/callback` (the Settings page has a button that fills in this
   server's address). HTTPS is required, so set `tls` in `config.json`; Entra ID allows plain http only for localhost.
2. **Certificates & secrets → New client secret**; paste the *value* into Settings. No API permissions are needed.
3. **App roles**: create `IDLifecycle.Admin`, `IDLifecycle.Approver` and `IDLifecycle.Viewer` (allowed member types:
   Users/Groups). Then **Enterprise applications → the app → Users and groups**: assign people or groups, and set
   **Assignment required = Yes**.
4. Require MFA with a **Conditional Access** policy that targets the app.
5. Enter the tenant ID, client ID and secret, click **Test sign-in settings**, save, and try **Sign in with
   Microsoft** in a private window.

People get the highest role their app roles (or group object IDs, if you list them) match; with no match the
sign-in is refused. Their IDLifecycle account is created at first sign-in, keeps following Entra ID for its name
and role, and can be disabled or deleted on **Users**. An Entra ID account never takes over a local account with the
same name: rename or delete the local one first.

**Break-glass.** Password sign-in can be limited to local administrators (**Password sign-in → Administrators
only**), so everyone else must come through Entra ID and its MFA. Keep at least one local admin with a strong,
vaulted password: it works when Entra ID is unreachable or misconfigured. Logging out of IDLifecycle does not sign
you out of Microsoft.

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
pwsh -NoProfile -File tests\powershell\Run-Tests.ps1   # Pester tests for the scripts against a stub ActiveDirectory module, a fake Graph and a stub Exchange Online module
$env:IDL_TEST_POSTGRES_DSN = '...'; go test ./internal/store/   # same store suite on PostgreSQL (also IDL_TEST_MSSQL_DSN, IDL_TEST_MYSQL_DSN, IDL_TEST_DB2_DSN)
go run ./cmd/idl-mockui                        # the web console against an in-browser mock API, no server needed
```

[docs/TESTING.md](docs/TESTING.md) explains what each suite proves, how to run a single test or script by hand, and how
to check your own environment (health check, Test buttons, Preview impact, dry run) before going live. The
[test map](https://mumbles27.github.io/IDLifecycle/TESTMAP.html) shows which suite covers which code and what stands in
for AD, Microsoft 365, HR systems and mail.

The PowerShell tests run the real scripts against a **stub** `ActiveDirectory` module with `PSModulePath` restricted
to the stub, so they cannot reach a real domain.

## Known limits

* Only the SQLite path was executed in development; the PostgreSQL/SQL Server/MySQL code paths are covered by the
  shared test suite but need a run against real servers (set the `IDL_TEST_*_DSN` variables). The Db2 store is covered only by
  server-less tests (DDL expansion, paging, a recorder driver that audits every statement for Db2 rules) and has never run on Db2.
* The scripts were verified against a stub AD module, not a live domain. Do a supervised first run in a lab OU.
* The generic HR API connector was tested against simulated APIs only; try your HR system's settings with **Test these settings** before relying on it.
* Not yet done: audit rows are written next to (not inside) the state-change transaction; an auto-approved `enable` will re-enable an account that someone disabled deliberately for another reason (keep `approval.enable` manual); MSSQL string parameters are sent as nvarchar against VARCHAR key columns (correct, but can cost an index seek on very large tables); migrations are not transactional.
* Single-valued string attributes only; single service instance. Correlation compares whole values (no similarity
  scores, nicknames or initials) and a merged record cannot be un-merged in the console.
