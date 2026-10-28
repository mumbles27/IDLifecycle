# PowerShell tests

Pester 3.x tests (Pester 3.4.0 is what is installed - do **not** use v4/v5 syntax such as `Should -Be`) for the
scripts in `..\..\scripts`, run against a **fake `ActiveDirectory` module**. Nothing here can reach a real domain.

## Run everything

```powershell
pwsh -NoProfile -File tests\powershell\Run-Tests.ps1     # exit code 0 = all green, 1 = test failures
```

`Run-Tests.ps1` (1) parses every `.ps1/.psm1/.psd1` for syntax errors, (2) proves the stub is what a child `pwsh`
resolves for `ActiveDirectory` (aborts loudly otherwise), (3) runs every `*.Tests.ps1` here, (4) deletes the temp
state files and any log/CSV files the scripts wrote to `scripts\Logs`. A full run takes about 7 minutes because
each test spawns one or more child `pwsh` processes (the install/uninstall tests alone take about 3 minutes).
Do not run two suites at once: each test deletes the log files that appeared in `scripts\Logs` during its run, so a
parallel run can delete another run's in-flight log.

Run a single file while iterating:

```powershell
pwsh -NoProfile -Command "Import-Module Pester -RequiredVersion 3.4.0; Invoke-Pester -Path tests\powershell\New-IDLUser.Tests.ps1"
```

## Safety model (this machine may be joined to a production domain)

* Every script under test runs in a **child `pwsh`** started by `Invoke-IDLScript` (`TestHelpers.ps1`) with
  `PSModulePath` set to **only** `tests\powershell\StubAD`, and `IDL_STUB_AD_STATE` pointing at a temp JSON file.
* Before the first child runs, `Assert-StubOnlyEnvironment` checks in such a child that `Get-Module ActiveDirectory`
  is version `99.0.0` located under `StubAD`. If not, the run aborts with `SAFETY ABORT`.
* The stub **throws on import if `IDL_STUB_AD_STATE` is not set**, so it can never silently do something else.
* The Pester process itself never imports `ActiveDirectory`.

## Layout

| File | Purpose |
|---|---|
| `Run-Tests.ps1` | Entry point (syntax check, safety check, Pester, cleanup). |
| `TestHelpers.ps1` | `Invoke-IDLScript` (spawns the child, feeds stdin JSON, returns `{ExitCode, StdOut, StdErr, Json, SingleJson, LogText, ReportText, ...}`), state-file builders (`New-StubStateFile`, `New-StandardState`, `New-StubUserEntry`), `Get-StubCalls`, `Get-StubUser`. |
| `StubAD\ActiveDirectory\` | The fake module (`ModuleVersion 99.0.0`). |
| `*.Tests.ps1` | One file per script, plus `Common` (pure helpers) and `StubAD` (tests of the stub itself). `Test-IDLNames` covers the name lookup (sam/upn on any object, mail as `mail` or `smtp:` proxyAddresses of users, groups and contacts, cn per OU, `exclude`, missing OUs); `Set-IDLLink` covers linking an account to a new id (expected-id check, id already used elsewhere, renames that must be free except for the account itself, proxyAddresses primary handling, retries, in-batch conflicts, DryRun). `Search-IDLGroups` covers the group search (cn / sAMAccountName / displayName / description, sort, `max` and `truncated`, `*` as the only wildcard and every other LDAP special character matched literally, category and scope from `groupType`, the privileged flag, exit 2 / 4, no mutating call). New, Set and Enable refuse to add an account to a privileged group (item fails before any change); Disable `addGroups` (after the disable, privileged / ambiguous / unknown groups reported but never a failure) and Enable `removeGroups` (privileged / ambiguous kept, unknown ignored) are covered incl. DryRun and re-runs. |
| `SetupTestHelpers.ps1` | For the setup tests: `Invoke-SetupScript` (parameters splatted from JSON, SecureStrings built inside the child from env vars, `IDL_SETUP_TEST=1`, a temp setup log folder), release fixtures with `SHA256SUMS` (`New-SetupFixture`, `Write-ReleaseManifest`, which also lists a root `Setup.cmd` when present), `Grant-TestAce` (user-writable temp files), fake-state readers. |
| `SetupFakes.ps1` | Passed to the setup scripts as the hidden `-TestHook` (honoured only with `IDL_SETUP_TEST=1` and only from this folder); replaces every system-touching function (services, sc.exe, machine env vars, event log source, elevation, AD/gMSA/ODBC checks, the service account password check (`credentialOk`), HTTP probe, owner lookups via `ownerOverrides`, the InstallDir ACL, `Install-IDLifecycleService.ps1`, the GitHub download (`githubSource` is zipped like GitHub's source zip; `githubFail`), Go (`goVersion`, `goBuildFail`), the hand-off to a staged installer (recorded with its arguments, secret names + lengths and the staged release checked by the real `Get-IDLTrustedRelease`; `handOffExit`), the tray / Setup shortcuts (state `shortcuts`; the Startup and Start menu folders are `<fixture>desktop...`, never the real ones), starting the tray at once (recorded as `startTray`; `trayRunning` = one already runs, `noDesktop` = no signed-in desktop in this session) and the branding folder (`<fixture>desktopIDLifecycle-Branding`; `brandingUntrusted` = the reason an existing one is not protected)) with a simulation in the JSON file named by `IDL_FAKE_STATE`, and sends the setup logs to `IDL_SETUP_TEST_LOGDIR`. |
| `FakeDaemon.ps1` | Stand-in for `idlifecycled.exe` (`version`, `init`, `lockdown`, `user add`, `check` with the full check-name list and `skipped`, `restore` incl. `-inspect`: a file starting `IDLBAK1` is a backup, state `backupPassphrase` / `restoreAdmins`, `config get` / `config set` with the contract of the real CLI: only the given fields change, a non-loopback listen without TLS or a bad log level is exit 2, `configSetExit` forces a failure); records each call and whether `IDL_PASSWORD` was present (and the length of `IDL_BACKUP_PASSPHRASE`), never a secret. `echoPassword` / `echoDsn` make it leak the secret in an error so the tests prove the scripts scrub it. |
| `Start-IDLifecycleSetup.Tests.ps1` | The setup window without a window: its helpers are dot-sourced with the hidden `-DefineOnly` switch. It covers the per-page input checks, the install/uninstall command lines (secrets only as SecureStrings for the child environment), the exit-code texts, the log colouring, following a log that is still being written, and the window definition (every control the code uses exists; loaded with WPF when the thread is STA). It also runs the built command lines against the real scripts with `SetupFakes.ps1`, and checks the root `Setup.cmd` (CRLF). |
| `Start-IDLifecycleTray.Tests.ps1` | The tray without an icon (`-DefineOnly`): tray.json validation (nothing in it can make the tray start anything but the browser and its own helper), the menu per service state, the tooltip, the helper's command line (split back with `CommandLineToArgvW`; the execution policy comes from tray.json and is never weakened), the settings checks (HTTPS kept, replaced or switched off), the helper result (incl. a cancelled UAC prompt), the log folder choice, branding (a protected override folder wins, one others can change is ignored - owners simulated), which notification-area entry the tray promotes into the taskbar (only its own, by tooltip and pwsh.exe), the Windows PowerShell 5.1 launcher (`Start-IDLifecycleTrayLauncher.ps1`: parses there and refuses `-ExecutionPolicy Bypass`) and, when the thread is STA, the settings window and the NotifyIcon menu. |
| `Invoke-IDLTrayAction.Tests.ps1` | The tray's elevated helper with `SetupFakes.ps1` (the "installation" is a temp folder passed in `IDL_TRAY_TEST_INSTALLDIR`, honoured only with the hook): restart / stop / start of this installation's service only, the result file (only `IDLTray_<hex>.json` in the user's temp folder), no elevation = exit 4, SetConfig through `config set` with only the given fields and tray.json following, invalid settings refused before anything is written. |
| `SetupSecurity.Tests.ps1` | In-process tests of the setup helpers in the Common module (path guard, ACL/owner trust, manifest reader, DSN scrubbing, protected folders, test-hook gate) and of functions parsed from the scripts (InstallDir ACL, the `sc.exe` empty-password argument, checked with an argv-echoing child). |

The setup tests never create a service, change an ACL outside the temp folder, write Program Files/ProgramData or a
real machine environment variable. Owners of other accounts cannot be set without elevation, so "pre-created by
someone else" is simulated with `ownerOverrides`; the real InstallDir ACL would lock the non-elevated test user out of
its temp folder, so it is faked and its ACL builder is unit-tested instead. One end-to-end test builds the real
`idlifecycled.exe` into a temp folder (`go build`) and runs `init`/`lockdown`/`user add`/`check` for real against a temp
data folder (sqlite, loopback, no service); it is skipped when Go is missing or the tree does not compile.

`Invoke-IDLScript` removes the log/CSV files a run created (the content is returned in `LogText`/`ReportText`);
pass `-KeepFiles` to inspect them on disk. Logs live in `scripts\Logs` (git-ignored) - except for the setup scripts,
which log to a protected folder (in the tests: a temp folder that `Invoke-SetupScript` reads and removes).

## Stub state file (`IDL_STUB_AD_STATE`)

Each stub cmdlet reads the JSON file, acts, and (for mutating cmdlets) writes it back, because every script run is a
separate process.

```json
{
  "users": [ { "objectGuid": "guid", "dn": "CN=Jane Doe,OU=Staff,DC=stub,DC=test", "sam": "jdoe",
               "upn": "jdoe@stub.test", "enabled": true, "objectClass": "user",
               "attributes": { "employeeID": "E1001", "givenName": "Jane", "title": "Analyst" },
               "memberOf": [ "CN=GG-AllStaff,OU=Groups,DC=stub,DC=test" ] } ],
  "groups": [ { "dn": "CN=GG-AllStaff,OU=Groups,DC=stub,DC=test", "sam": "GG-AllStaff",
                "adminCount": 1, "objectSid": "S-1-5-21-1-2-3-512", "attributes": { "mail": "allstaff@stub.test" } } ],
  "ous":    [ "OU=Staff,DC=stub,DC=test", "OU=Groups,DC=stub,DC=test" ],
  "objects": [ { "dn": "CN=ExchangeActiveSyncDevices,CN=Jane Doe,OU=Staff,DC=stub,DC=test",
                 "objectClass": "msExchActiveSyncDevices" },
               { "dn": "CN=Ann Vendor,OU=Sales,DC=stub,DC=test", "objectClass": "contact",
                 "attributes": { "mail": "ann@vendor.example", "proxyAddresses": [ "SMTP:ann@vendor.example" ] } } ],
  "calls":  [ { "cmdlet": "New-ADUser", "args": { "Name": "Jane Doe", "AccountPassword": "***" }, "result": "ok" } ],
  "faults": [ { "cmdlet": "Add-ADGroupMember", "match": "jdoe", "message": "boom {password}" } ],
  "failConnect": false
}
```

* `users[].attributes` holds LDAP attributes by name (case-insensitive). `objectClass` defaults to `user`; use
  `computer` to test the "not a user" refusal. `memberOf` lists group DNs.
* `groups[].adminCount` / `objectSid` are optional; they let tests mark a group privileged (adminCount 1, or a SID
  ending in 512/518/519/544/548/549/550/551). Two groups may share a `sam` to test ambiguity.
* A fault whose `cmdlet` is `Get-ADObject` also makes **reads** throw when `match` appears in the identity/filter/
  search base (used to test OU-verification failures).
* `ous` lists valid OU/container DNs. Any DN made only of `DC=` parts is always valid.
* `objects` (optional) are generic child objects (`dn`, `objectClass`, optional `objectGuid`). An entry whose DN ends
  with `,<user DN>` is a child of that user: `Remove-ADUser` and `Remove-ADObject` without `-Recursive` then throw
  "The directory service can perform the requested operation only on a leaf object", `Remove-ADObject -Recursive`
  deletes the whole subtree (users, groups and objects below it), and `Move-ADObject` moves children along.
  `Get-ADObject` returns them and accepts any existing object DN as `-SearchBase` (`Base`/`OneLevel`/`Subtree`).
  A nested *user* is simply a `users[]` entry whose DN is below another user's DN.
* `groups[]` and `objects[]` may carry an `attributes` table like users (e.g. a contact with `mail` / `proxyAddresses`);
  for groups e.g. `description`, `displayName` and `groupType` (as AD stores it: `-2147483646` global security, `4` domain local
  distribution, `-2147483640` universal security). `Get-ADGroup` honours `-ResultSetSize` and the read-side `faults`.
  multi-valued attributes are JSON arrays and match filters value by value, case-insensitively.
* `Set-ADUser` supports `-Replace`, `-Clear`, `-Add`, `-Remove` (applied Remove, Add, Replace, Clear like the real cmdlet;
  adding an existing or removing a missing value throws and leaves the object unchanged), `-SamAccountName` /
  `-UserPrincipalName` (uniqueness enforced) and `-EmailAddress`. `Rename-ADObject -NewName` changes the RDN value
  (children move along, the new DN must be unique).
* `calls` is appended by every **mutating** cmdlet (`New-ADUser`, `Set-ADUser`, `Enable/Disable-ADAccount`,
  `Remove-ADUser`, `Remove-ADObject`, `Move-ADObject`, `Rename-ADObject`, `Add/Remove-ADGroupMember`), including failed attempts (`result` starts with
  `error`). Passwords are recorded as `***`. Tests use it to prove DryRun made **zero** mutating calls.
* `faults` makes a matching mutating cmdlet throw. `match` is a case-insensitive substring of the call's JSON args
  (`""` = every call). `{password}` in `message` is replaced by the plaintext password so tests can prove the
  scripts scrub secrets from error text.
* `failConnect: true` makes every cmdlet throw `ADServerDownException` (script exit code 4).

Behaviours deliberately copied from real AD: `Get-ADUser -Identity` throws `ADIdentityNotFoundException` when
nothing matches while filters return nothing; only default properties plus `-Properties` are returned (and unset
attributes are simply absent); `Add-ADGroupMember` on an existing member and `Remove-ADGroupMember` on a
non-member throw; `New-ADUser` enforces unique sAMAccountName/UPN/DN, a 20-character sAMAccountName and an
existing `-Path`, creates `CN=<escaped name>,<Path>` and a fresh objectGUID.

Supported LDAP filter syntax: `(attr=value)`, `(&...)`, `(|...)`, `(!...)`, presence `(attr=*)`, `*` wildcards
inside values, RFC 4515 `\hh` escapes (including raw-octet `objectGUID` matching).
