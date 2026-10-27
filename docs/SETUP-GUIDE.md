# IDLifecycle Install and Initial Setup Guide

Oct 25, 2026 · @Preston

## 1. Before you start

Have everything in this table ready before you run the installer. The gMSA delegation and the TLS certificate usually take the longest to arrange.

| Item | What you need | Notes |
| --- | --- | --- |
| Server | Windows Server, domain-joined, 64-bit | The service runs here and talks to AD through PowerShell |
| PowerShell | PowerShell 7+ (`pwsh`) | The installer refuses Windows PowerShell 5.1 |
| AD module | RSAT ActiveDirectory module | `Install-WindowsFeature RSAT-AD-PowerShell`, or pass `-InstallPrerequisites` |
| Service account | A gMSA or MSA such as `CORP\svc-idl$` (recommended), or a normal user account such as `CORP\svc-idl` with its password | gMSA/MSA: install it on the server and confirm with `Test-ADServiceAccount svc-idl$`. User account: set its password not to expire. The installer grants "Log on as a service" |
| AD permissions | Delegated on the target OUs only | Create and delete user objects (including **Delete subtree**), write the mapped attributes, and manage membership of the groups your role rules use |
| App database | SQLite (default, nothing to set up) or an empty SQL Server, PostgreSQL, MySQL or Db2 database | Server databases need a login that can create tables. Db2 also needs the 64-bit IBM ODBC driver |
| TLS certificate | PEM certificate + key | Needed if the console listens on anything other than `127.0.0.1`, and for Entra ID sign-in |
| HR source | Paylocity API credentials, a CSV path, or a read-only SQL login and view | Configured after install |
| Build machine | Go 1.22+ | Only to build the release folder; not needed on the server |

Keep the admin password and any connection strings ready. The installer prompts for them and never writes them to logs or command lines.

## 2. Choose where to install from

Most servers can install straight from GitHub: put a copy of the repository on the server once (git clone, or GitHub's Download ZIP into a folder only administrators can change), double-click `Setup.cmd`, and pick the source on the window's first page.

| Source | Use it when | Needs on the server |
| --- | --- | --- |
| GitHub | The server can reach github.com and proxy.golang.org | Go 1.22 or newer (go.dev/dl). While the repository is private, a fine-grained token with read-only Contents access |
| Release zip or folder | No Go or no internet on the server | A zip built elsewhere with `New-IDLifecycleRelease.ps1 -Zip`, copied to the server |
| This release folder | `Setup.cmd` was started from a release folder | Nothing extra |
| Any folder or zip, unchecked (least safe) | Testing only: you have idlifecycled.exe next to the scripts folder but no release with SHA256SUMS | Tick 'Skip the SHA-256 file check' under Options, or add -SkipIntegrityCheck |

For GitHub and zip sources the installer prepares a release in a locked staging folder (`C:\ProgramData\IDLifecycle-Setup\Staging`), checks it, and hands over to that version's own installer. The staging folder is removed when the install succeeds.

**Skipping the SHA-256 check is the least safe way to install.** Without `SHA256SUMS` nothing proves the program and scripts are the ones that were built, and the service runs them with its AD rights. Use it to try things out, not for production. The installer warns in its log (`UNVERIFIED INSTALL`), the setup window asks you to confirm, and the folder still has to be one only administrators can change. It writes its own `SHA256SUMS` into the program folder, so a later upgrade or uninstall works normally. Reinstall from a proper release before going live.

### Building a release yourself (servers without Go)

The installer runs only from a release folder made by `New-IDLifecycleRelease.ps1`. That folder holds `idlifecycled.exe`, the `scripts\` folder and a `SHA256SUMS` manifest listing every file.

1. On the build machine, from the repository root:

   ```powershell
   .\scripts\New-IDLifecycleRelease.ps1 -OutDir C:\Install\IDLifecycle-1.0.0 -Build -Version 1.0.0 -Zip
   ```
2. Copy the whole folder to the server, for example to `C:\Install\IDLifecycle-1.0.0`.
3. Make sure only SYSTEM, Administrators, TrustedInstaller and you can change that folder and its parents. A folder under `C:\Users` or a writable share will be refused.
4. Optional: sign `idlifecycled.exe` with your code-signing certificate and add `-RequireSignature` when you install.

The release folder also holds `Setup.cmd`, the double-click launcher for the setup window (section 3). It is listed in `SHA256SUMS` like every other file.

The installer checks every file against `SHA256SUMS` and refuses extra files in `scripts\`. This proves nothing changed after the build; the folder permissions are what stop someone tampering with it. A trust failure exits with code 2, and `-Force` does not override it.

## 3. Run the installer

Use the setup window unless you script installs. Both routes run the same installer with the same checks, logs and exit codes.

### Option A: the setup window (recommended)

You need PowerShell 7 and a Windows edition with the desktop experience. On Server Core, use option B.

1. Double-click `Setup.cmd` in the release folder and accept the administrator prompt. If you decline, the window still opens, but it can only do test runs.
2. The start page shows what is already on the server: the service, its account, folders and database. Choose **Install IDLifecycle**. If it is already installed, this button reads **Upgrade or repair IDLifecycle**.
3. Work through the pages. Each one is checked when you click **Next**, so a mistake is reported on the page where you made it.
4. On **Review**, click **Test run (no changes)**. The log appears live: blue WOULD lines show what will change, red lines are problems.
5. When the test run is clean, click **Install now**. When it finishes, click **Open the web console** and continue with section 4.

| Page | What you enter |
| --- | --- |
| Source | This release folder, GitHub (repository, branch or tag, and a token for a private repository; the window shows whether Go is installed), or a release zip or folder |
| Locations | Program and data folders; the defaults suit most servers. The service name is under Advanced |
| Service account | gMSA/MSA (recommended), a domain or local user account plus its password, or a built-in account |
| Database | SQLite, or SQL Server, PostgreSQL, MySQL or Db2 with a connection string (examples are shown) |
| Web console | Address (empty means this server only), HTTPS certificate and key for network access, the first administrator and password |
| Options | Start the service, install the AD PowerShell module, upgrade (replace program files), require a signed exe, add the tray icon (on by default), skip the SHA-256 file check (least safe, testing only), and Restore from a backup (an .idlbak file, its passphrase, and optionally its settings) |
| Review | A summary and the equivalent PowerShell command, with passwords replaced by Read-Host |

Passwords and the connection string never appear in a log or on a command line. **Open log folder** on the last page shows the installer's log and CSV report.

### Option B: PowerShell

Run a dry run first. It needs no elevation and prints `WOULD:` for every change. Then run the real install from an elevated PowerShell 7 window.

```powershell
# 1. Preview (no changes)
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -DryRun

# 2a. Install with SQLite, console on https://<server>:8443
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' `
    -Listen '0.0.0.0:8443' -TlsCertFile C:\Certs\idl.crt -TlsKeyFile C:\Certs\idl.key

# 2b. Or install with SQL Server as the app database
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' `
    -DbDriver mssql -DbDsn (Read-Host -AsSecureString 'DSN') `
    -AdminPassword (Read-Host -AsSecureString 'Admin password')
```

The same installer also builds from GitHub, installs from a release zip, or restores a backup:

```powershell
# From GitHub (Go must be installed; the token only while the repository is private)
.\scripts\Install-IDLifecycle.ps1 -FromGitHub -GitHubRef main -GitHubToken (Read-Host -AsSecureString 'GitHub token') `
    -ServiceAccount 'CORP\svc-idl$'

# From a release zip
.\scripts\Install-IDLifecycle.ps1 -SourceDir D:\Downloads\IDLifecycle-1.0.0.zip -ServiceAccount 'CORP\svc-idl$'

# Recovery or a move: install and restore a backup (its users replace the admin you would create)
.\scripts\Install-IDLifecycle.ps1 -FromGitHub -ServiceAccount 'CORP\svc-idl$' `
    -RestoreFrom D:\Backups\IDLifecycle_OLD01_20261024_020000.idlbak -RestoreConfig
```

If you leave out `-Listen`, the console listens on `127.0.0.1:8443` only, and you can open it only on the server itself. You'll be prompted for the first admin password (at least 12 characters) unless you pass `-AdminPassword`.

**What the installer does, in order.** A step that is already done is reported as Skipped, so re-running it is safe.

1. Checks the release folder's permissions and every file against `SHA256SUMS`.
2. Checks PowerShell 7, elevation, the AD module, the gMSA and (for Db2) the ODBC driver.
3. Copies the program to `C:\Program Files\IDLifecycle` and locks down its permissions.
4. Creates `C:\ProgramData\IDLifecycle\config.json` if it doesn't exist. An existing config is never overwritten.
5. Restricts the data folder to SYSTEM, Administrators, you and the service account, then stores the database connection string there.
6. Creates the database schema and the first admin account (`admin` by default).
7. Registers the Windows service and the `IDLifecycle` event log source.
8. Runs `idlifecycled check`. If any required check fails, the service is not started.
9. Starts the service, checks the console answers with HTTP 200, and prints its URL.

**Using a normal user account instead of a gMSA.** Add `-ServiceAccountPassword`; if you leave it out, you're prompted for it. The installer checks the password against AD before it changes anything, and Windows stores it for the service. When the password changes, run the installer again with the new one, or the service won't start.

```powershell
C:\Install\IDLifecycle-1.0.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl' `
    -ServiceAccountPassword (Read-Host -AsSecureString 'Service account password')
```

**Useful parameters**

| Parameter | Default | Use |
| --- | --- | --- |
| `-ServiceAccount` | prompted | gMSA/MSA (`DOMAIN\name$`), user account (`DOMAIN\name`, `.\name`) or a built-in account |
| `-ServiceAccountPassword` | prompted | Password of a normal user service account (not for gMSA/MSA or built-in accounts) |
| `-InstallDir` | `C:\Program Files\IDLifecycle` | Program files |
| `-DataDir` | `C:\ProgramData\IDLifecycle` | Config, SQLite database, `secret.key`, logs |
| `-DbDriver` | `sqlite` | `mssql`, `postgres`, `mysql` or `db2` |
| `-DbDsn` | none | Connection string as a SecureString; stored in the locked-down `config.json` |
| `-DbDsnEnvVar` | none | Keep the connection string in a machine variable `IDL_*` instead. Any local user can read it, and the service only sees it after a reboot |
| `-Listen` | `127.0.0.1:8443` | Any other address needs `-TlsCertFile` and `-TlsKeyFile` |
| `-AdminUser` / `-AdminPassword` | `admin` / prompted | First console admin |
| `-Start:$false` | starts | Install without starting the service |
| `-NonInteractive` | off | Fail instead of prompting (for automation) |
| `-Force` | off | Needed for upgrades; never skips the trust checks |

Exit codes: 0 OK, 1 some steps failed, 2 bad input or untrusted release, 3 missing dependency, 4 not elevated, 5 unexpected error. The installer's log and CSV report are in `C:\ProgramData\IDLifecycle-Setup\Logs`.

## 4. First sign-in

Open the URL the installer printed (for example `https://idl01.corp.local:8443/`) and sign in as `admin` with the password you set. A banner says the app is in **dry run**: syncs plan changes and record them, but nothing is written to AD until you turn dry run off (step 5.8).

Then add the people who will use the console on **Users**:

| Role | Can |
| --- | --- |
| Viewer | See everything; change nothing |
| Approver | Approve or reject proposed changes |
| Admin | Everything, including settings, sources, mappings and users |

Server administrators can also manage local accounts from the command line:

```powershell
$exe = 'C:\Program Files\IDLifecycle\idlifecycled.exe'
$cfg = 'C:\ProgramData\IDLifecycle\config.json'
& $exe user add  -config $cfg -username jdoe -role approver   # prompts for the password
& $exe user list -config $cfg
& $exe user passwd -config $cfg -username admin
```

Store the `admin` password in your vault. It's your way back in if Entra ID sign-in (section 6) ever fails.

## 5. Initial configuration

Work through these steps in order, signed in as an admin. Everything stays in dry run until step 5.8, so nothing reaches AD while you set things up.

### 5.1 Active Directory (Settings → Active Directory)

| Field | Set to | Default |
| --- | --- | --- |
| Server | A domain controller or the domain name to send AD commands to | blank |
| Search bases | The OUs that hold managed users, one DN per line | none |
| Default OU | Where new accounts are created | none |
| Disabled OU | Where disabled accounts are moved (optional) | none |
| ID attribute | The AD attribute that holds the HR employee ID | `employeeID` |
| UPN suffix | For example `@corp.com` | none |
| Strip groups on disable | Remove group memberships when an account is disabled | on |

### 5.2 Naming and new accounts

**Naming (new accounts)** sets the templates for `sAMAccountName`, UPN and CN. By default these are first initial + last name, `first.last`, and `First Last`. Use **Naming preview** to check the result on a sample name. What happens when a name is taken is set below.

**New accounts** sets the random password length (default 20), whether the user must change it at first logon (default on), and whether new accounts start enabled. The password is never stored or shown, so hand-over goes through your normal reset process.

**Email addresses** are optional in Naming: a mail template such as `{{ .FirstName | lower | alnum }}.{{ .LastName | lower | alnum }}` plus a mail domain (default the UPN suffix). The address gets the same suffix as the account when a name is taken (below). If an attribute mapping writes `mail`, that mapping wins and keeps it in sync.

**When a name is taken** (Naming), every sync checks a new account's sAMAccountName, UPN, mail (including contacts, groups and `proxyAddresses`) and display name in AD, and against the other people in the same run. Pick what happens:

| Setting | `jdoe` is taken |
| --- | --- |
| Ask a person (default) | The create waits in Approvals with **Set username**: it shows who has the name, offers free suggestions and checks your choice in AD. You also get an *Identity issues* email. |
| Add a number | `jdoe2`, `jdoe3` and so on |
| Add random digits | `jdoe` plus 2-6 digits, the same each run for that person |
| Add a value from the record | for example the last 4 of the employee ID (`jdoe1042`) |

A name you choose is remembered for that person.

**Initial passwords** can be random (the default) or built from a template, for example `{{ upper (first .LastName 1) }}{{ last .EmployeeID 4 }}!{{ random 6 }}`. Keep a random part: anyone who knows the HR data can work out the rest. The preview shows a sample password and whether AD's complexity rules accept it. Template passwords always force a change at next logon. An account whose password would be refused fails with the reason, never the password.

Templates also support regular expressions: `regexReplace`, `regexFind` and `regexMatch`. For example, `{{ .Phone | regexReplace "[^0-9]" "" }}` keeps only the digits.

### 5.3 Sources

On **Sources**, add your HR feed and click **Test** until it returns sample records.

- **Paylocity**: API credentials (stored encrypted).
- **CSV**: a file path the service account can read. Set `csv_allowed_dirs` in `config.json` to limit which folders sources may read.
- **SQL**: a driver, a connection string and one `SELECT` query. Use a login that can only read the HR view. For SQL Server, leave out the user and password to connect as the service account.

### 5.4 Attribute mappings

On **Attribute mappings**, map source fields to AD attributes. The **field mapper** lists the source's fields and the AD attributes side by side. It marks which fields are already used, and which AD attributes can't be mapped (system, constructed or protected ones such as `sAMAccountName` or `proxyAddresses`). Click a field chip to insert it into a mapping template.

### 5.5 Role rules

On **Role rules**, define who gets which groups, OU and extra attributes, based on source fields such as department or job title.

### 5.6 Run a sync in dry run

On the **Dashboard**, click **Run sync now**. Then review the run on **Runs** and the proposed creates, updates and disables on **Approvals**. Fix mappings or rules and run again until the plan looks right. Do this especially for the first load, which can propose many creates.

### 5.7 Approvals and safety limits

**Approval policy** sets each change type (create, update, disable, enable, delete) to `manual` or `auto`. All are `manual` by default. A common start is `auto` for updates and `manual` for everything else.

**Engine** holds the safety limits:

| Setting | Default | Effect |
| --- | --- | --- |
| Max destructive per run | 10 | More disables or deletes than this in one run are held for manual approval, even when set to `auto` |
| Max deletes per run | 5 | More deletes than this in one run puts all of that run's deletes on manual approval |
| Max deletes per day | 20 | At most this many deletes complete in any 24 hours; the rest wait |
| Delete after days | 30 | Days a terminated account stays disabled before it's deleted |
| Remove stale groups | on | Remove groups that role rules no longer grant |

### 5.8 Go live

In **Settings**, turn **Dry run** off and confirm. Approved changes are now written to AD. Start with a run you watch, and check the result on **Runs** and **Audit log**.

### 5.9 Schedule

In **Schedule**, turn on automatic runs: every N minutes (default 60), daily at a set time, or a cron expression, with a time zone. Until then, runs happen only when you click **Run sync now**.

### 5.10 Email notifications (recommended)

In **Settings → Notifications**, choose **Generic SMTP**, **Microsoft 365 – SMTP AUTH** or **Microsoft 365 – Graph API**. Add the sender and recipients, then **Send test email** before saving. By default you're notified of failed runs, waiting approvals, failed AD changes and safety limits being reached. The README has the Entra ID setup steps for both Microsoft 365 options.

### 5.11 OU placement by department

In **Configure → OU placement**, tick *Place accounts by the field below* and keep the field as `Department` (or pick another HR field). Add one rule per department and use **Pick…** to choose its OU from the tree read from AD. You can also import a CSV with the columns `department,ou`. The table at the bottom lists every department among active people, unmapped ones first; **Map…** adds a rule. Save when done.

- A role rule that sets an OU still wins, so keep using role rules for exceptions such as contractors.
- A new person whose department has no rule waits in **Approvals** with **Choose OU…**, even if creates are auto-approved. Approve stays disabled until an OU is chosen. An admin can tick *Also use this OU for everyone in …* to add the rule at the same time.
- **Change OU…** on any create, or on a move, puts that one person somewhere else. The choice holds until their department changes.
- When someone's department changes, their account is moved to the new department's OU (and waits for a choice when there is no rule). A title change alone never moves anyone, and an account you move by hand in AD stays where you put it.

### 5.12 Backups

In **Settings → Backups**, set a passphrase of at least 12 characters and store a copy outside the server: without it, no backup can be restored. Turn on *Make backups automatically* (for example every 24 hours, keeping 14), and click **Back up now** once to check it works. Backups go to the `backups` folder inside the locked-down data folder by default. Copy them to other storage regularly; **Download** works from the same page.

### 5.13 Contractors and people added by hand

For contractors and other requests that are not in HR, add a source of type **Manual entry** on **Sources** once. Its *Form fields* decide what the form asks (one per line: `Name | Label | flags`, flags `required`, `date`, `email`, `select: A; B`). Use the same field names as your HR source (`FirstName`, `LastName`, `Department`), so mappings, roles, OU placement and naming work unchanged.

Then use **Add person** (Operate): fill the form, click **Preview** to see the username, the OU and any name conflicts, then **Create** (tick *run a sync now* to see the change at once). The account still goes through Approvals. **End** (or an end date that has passed) disables the account through the normal flow. **Delete** works only before an account exists.

### 5.14 Matching existing accounts

Use **Configure → Matching** before the first live run in an existing domain, and to keep one account when a contractor becomes an employee. Turn on *Look for an existing AD account before creating a new one* and review the rules: by default first and last name must match (accents, apostrophes and spaces ignored) and department is compared too.

| Result | What happens |
| --- | --- |
| Every rule matches one account | The account gets the person's ID and is linked, no new account. With *Link* approval on `auto` (default) this needs no click. |
| Some rules match, or several accounts match | A link waits in Approvals with the accounts side by side. Pick **This is the person**, **None of these** (a new account is planned) or **Not this person** (never proposed again). |
| The account belonged to a contractor record | It is renamed to the employee naming (`c-jdoe` → `jdoe`; the old address stays as an alias), and the contractor record is retired so ending the contract cannot disable it. |

Administrator accounts (`adminCount = 1`) are never matched. After each sync the page lists enabled accounts nobody matches: use **Link to person…** for real people and **Ignore** for service accounts (patterns such as `svc_*` work). New items here are sent in the *Identity issues* email. Run the first syncs in dry run and check this list before going live.

### 5.15 Tray icon

The installer adds a tray icon for everyone who signs in to the server (untick *Add the tray icon* under Options, or use `-NoTray`, to leave it out). It is also in the Start menu under IDLifecycle. Hover it to see whether the service is running; right-click it for:

| Menu item | What it does |
| --- | --- |
| Open admin console | Opens the web console (also a double-click) |
| Settings… | Change the web console address, the HTTPS certificate and key, and the log level, and restart to apply |
| Restart / Stop / Start service | Controls the Windows service |
| Open log folder | Opens the logs you can read |
| Exit | Closes the tray icon; the service keeps running |

Saving settings and service actions ask for administrator rights (a UAC prompt) each time. Everything else is set in the web console.

### 5.16 Your own branding (optional)

To show your company's name, logo and colour instead of IDLifecycle's, copy any of these files into `C:\ProgramData\IDLifecycle-Branding` (the installer creates the folder with a README; only administrators can change it):

| File | Changes |
| --- | --- |
| `brand.json` | The product name and the web accent colour, for example `{"product_name": "Acme Identity", "accent": "#0a7d5a"}` |
| `logo.svg` or `logo.png` | The logo in the web console (PNG also in the tray settings window) |
| `favicon.ico` | The browser tab icon |
| `tray.ico` | The tray icon and the setup windows |

The web console shows the change within a minute; the tray icon the next time it starts. A file that is invalid or larger than 1 MiB is ignored, and so is the whole folder if anyone other than administrators can change it.

## 6. Optional: Sign in with Microsoft (Entra ID)

Entra ID sign-in is off by default. Turn it on to put console sign-in behind your MFA and Conditional Access policies. It needs HTTPS (section 3, `-Listen` with a certificate).

1. **Register the app.** In the Entra admin center, go to **App registrations → New registration** and choose **this directory only**. Set the redirect URI to platform **Web**, `https://<server>:8443/api/auth/oidc/callback`. The Settings page has a button that fills in this server's address.
2. **Create a secret.** Under **Certificates & secrets → New client secret**, copy the secret's *value*. No API permissions are needed.
3. **Create app roles.** Add `IDLifecycle.Admin`, `IDLifecycle.Approver` and `IDLifecycle.Viewer`, with allowed member types Users/Groups.
4. **Assign people.** In **Enterprise applications → the app → Users and groups**, assign users or groups to roles, and set **Assignment required** to **Yes**.
5. **Require MFA.** Create a Conditional Access policy that targets the app.
6. **Configure IDLifecycle.** In **Settings → Sign-in**, enter the tenant ID, client ID and secret. Click **Test sign-in settings**, then save.
7. **Try it.** In a private browser window, click **Sign in with Microsoft**.

People get the highest role their app roles match; with no match, sign-in is refused. Their account is created the first time they sign in and follows Entra ID for its name and role.

**Break-glass.** Once Entra ID sign-in works, set **Password sign-in** to **Administrators only**. Everyone else must then come through Entra ID and its MFA. Keep at least one local admin with a vaulted password for when Entra ID is down or misconfigured. Signing out of IDLifecycle doesn't sign you out of Microsoft.

## 7. Check it works

After install, all of these should be true:

- [ ] `Get-Service IDLifecycle` shows **Running**.
- [ ] `idlifecycled check` passes every check:

  ```powershell
  & 'C:\Program Files\IDLifecycle\idlifecycled.exe' check -config C:\ProgramData\IDLifecycle\config.json
  ```

  It covers config, data folder permissions, database, an admin user, PowerShell, the scripts, and program folder permissions.
- [ ] The console opens at the printed URL, and you can sign in as `admin`.
- [ ] **Sources → Test** returns records.
- [ ] A dry-run **Run sync now** finishes, and **Approvals** shows the changes you expect.
- [ ] **Audit log → Verify integrity** reports the chain intact.

To see what happened and why, use **History** (Operate). Search with words or filters such as `level:error since:24h`, `actor:alice` or `action:change.failed`. Click an actor, action or entity to narrow the results, and **Export CSV** saves what you found. The *Search syntax* box on the page lists every filter.

Logs to check:

| What | Where |
| --- | --- |
| Service log (JSON, rotated) | `C:\ProgramData\IDLifecycle\logs\idlifecycle.log` |
| PowerShell logs and CSV reports | `C:\Program Files\IDLifecycle\scripts\Logs` |
| Installer log and report | `C:\ProgramData\IDLifecycle-Setup\Logs` |
| Windows events | Event Viewer, source `IDLifecycle` |

## 8. Upgrade, back up and uninstall

**Upgrade.** Build a new release folder and run its installer with `-Force`, using the same service account. The service is stopped, the files replaced and the service started again. Your config, database and settings are kept.

With the setup window, double-click `Setup.cmd` in the **new** release folder. It finds the installation, fills in its folders and account, and ticks **Upgrade**. Run a test run, then **Upgrade now**. A user service account needs its password only if it changed.

To upgrade from GitHub, start `Setup.cmd` from the program folder (or your repository copy), choose **GitHub** and the new tag, then **Upgrade now**.

```powershell
C:\Install\IDLifecycle-1.2.0\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -Force
```

**Back up** with **Settings → Backups** (step 5.12) or `idlifecycled backup`. A backup is one encrypted `.idlbak` file holding the database (rules, people, changes, history, users), `config.json` and the encryption key. Saved secrets stay readable after a restore, and you can restore it on another server or into another database type (for example SQLite to SQL Server).

**Restore** after a failure, or to move servers: run `Setup.cmd` on the new server, then on **Options** tick *Restore from a backup*, choose the file and enter its passphrase. Tick *Also take its settings* to keep the web address and HTTPS files. On the command line, use `Install-IDLifecycle.ps1 -RestoreFrom <file> -RestoreConfig`. The restore replaces the database contents in one step: a wrong passphrase or a damaged file changes nothing. Replacing a database that already has data needs *Upgrade: replace program files* (`-Force`). Everyone signs in again afterwards, with the accounts from the backup.

**Uninstall.** This removes the service, the event log source and the program files. The data folder is kept unless you ask for it to be removed, and then only with a backup or an explicit `-NoBackup`. It never touches AD and never drops a server database.

With the setup window, double-click `Setup.cmd` in the release folder or in the program folder, `C:\Program Files\IDLifecycle`, then choose **Uninstall IDLifecycle**. Removing the program files is ticked by default. To delete the data folder too, tick it, then pick a backup folder or **No backup**, and type the service name to confirm. Run a test run first, then **Uninstall now**.

```powershell
.\scripts\Uninstall-IDLifecycle.ps1 -DryRun
.\scripts\Uninstall-IDLifecycle.ps1                                       # keeps the data
.\scripts\Uninstall-IDLifecycle.ps1 -RemoveData -BackupTo D:\Backup -Force  # removes data after a verified backup
```

## 9. Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Installer exits 2: untrusted release | Release folder (or a parent) is writable by other users, a file doesn't match `SHA256SUMS`, or `scripts\` has extra files | Move the folder somewhere only admins can write, such as `C:\Install`, or rebuild it |
| Installer exits 3 | ActiveDirectory module or Db2 ODBC driver missing | `Install-WindowsFeature RSAT-AD-PowerShell` or `-InstallPrerequisites`; install the 64-bit IBM driver |
| Installer exits 4 | Not elevated | Run PowerShell 7 as Administrator |
| Existing folder refused | `C:\ProgramData\IDLifecycle` or the program folder is owned by another account | Delete it if you didn't create it, then re-run |
| Service won't start | Data folder readable by Users or Everyone | `idlifecycled lockdown -config C:\ProgramData\IDLifecycle\config.json -account 'CORP\svc-idl$'` |
| Non-loopback `-Listen` refused | No TLS certificate | Add `-TlsCertFile` and `-TlsKeyFile` |
| Scripts won't load | Downloaded scripts are marked as from the internet | Re-run the installer, or `Unblock-File` the scripts |
| Service doesn't see a new `IDL_*` variable | Machine variables reach services only after a reboot | Reboot, or store the DSN in `config.json` (the default) |
| Entra ID sign-in refused | No app role assigned, or the redirect URI doesn't match | Assign a role on the enterprise app; check the URI matches exactly |
| Password sign-in refused for a user | Password sign-in is set to Administrators only | Have them use **Sign in with Microsoft** |
| Installer exits 3: service account password rejected | Wrong password, or the account is disabled, locked or expired | Fix the account in AD and re-run; nothing was changed |
| Service stops starting with a logon failure (error 1069) | The user account's password changed or expired | Re-run the installer with the new -ServiceAccountPassword; set the password not to expire, or move to a gMSA |
| Service stops starting after a Group Policy refresh | A GPO defines "Log on as a service" without the account, replacing the local grant | Add the service account to that GPO setting |
| Setup.cmd says pwsh.exe was not found | PowerShell 7 is not installed | Install it from https://aka.ms/powershell and run Setup.cmd again |
| Setup window doesn't open (exit 3) | Server Core has no WPF | Use option B (the PowerShell scripts) |
| Install button greyed out in the setup window | Not running as administrator | Click Restart as administrator in the yellow bar |
| Install button disabled on the start page | The window was started from the installed program folder | Start Setup.cmd from the new release folder to upgrade |
| Install from GitHub exits 3: Go was not found | Go is not installed, or not on PATH | Install Go 1.22+ from go.dev/dl and restart the window, or install from a release zip |
| GitHub download: repository or ref not found | The repository is private and no token was given, or the branch or tag name is wrong | Enter a read-only token on the Source page; check the branch or tag |
| Restore: the backup cannot be decrypted | Wrong passphrase, or the file was damaged in transfer | Nothing was changed; check the passphrase, or copy the file again |
| Approve is greyed out on a new account | Its department has no OU placement rule | Click Choose OU…, or add the rule on the OU placement page |
| New account waits with "WAITING for a username" | The generated username, UPN or mail is already used in AD or by another new person | Click Set username and pick a suggestion, or choose an automatic mode under Settings → Naming |
| A link waits with "VERIFY the match" or "choose the matching account" | Only some matching rules agree, or several accounts match the person | Check the side-by-side fields, then This is the person, None of these, or Not this person |
| A contractor got a second account when they joined HR | Matching was off, or the names differ between the two records | Turn matching on. Delete the extra new account in AD; the next sync then proposes linking the old one (or use Link to person… on the Matching page) |
| Folder has no SHA256SUMS, so it is not a release folder | The folder was not built with New-IDLifecycleRelease.ps1 (for example a copy of the repository with a self-built exe) | Build a release, install from GitHub, or for testing only tick 'Skip the SHA-256 file check' (least safe) |
