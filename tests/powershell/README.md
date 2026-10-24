# PowerShell tests

Pester 3.x tests (Pester 3.4.0 is what is installed - do **not** use v4/v5 syntax such as `Should -Be`) for the
scripts in `..\..\scripts`, run against a **fake `ActiveDirectory` module**. Nothing here can reach a real domain.

## Run everything

```powershell
pwsh -NoProfile -File tests\powershell\Run-Tests.ps1     # exit code 0 = all green, 1 = test failures
```

`Run-Tests.ps1` (1) parses every `.ps1/.psm1/.psd1` for syntax errors, (2) proves the stub is what a child `pwsh`
resolves for `ActiveDirectory` (aborts loudly otherwise), (3) runs every `*.Tests.ps1` here, (4) deletes the temp
state files and any log/CSV files the scripts wrote to `scripts\Logs`. A full run takes about 2.5 minutes because
each test spawns one or more child `pwsh` processes.

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
| `*.Tests.ps1` | One file per script, plus `Common` (pure helpers) and `StubAD` (tests of the stub itself). |

`Invoke-IDLScript` removes the log/CSV files a run created (the content is returned in `LogText`/`ReportText`);
pass `-KeepFiles` to inspect them on disk. Logs live in `scripts\Logs` (git-ignored).

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
                "adminCount": 1, "objectSid": "S-1-5-21-1-2-3-512" } ],
  "ous":    [ "OU=Staff,DC=stub,DC=test", "OU=Groups,DC=stub,DC=test" ],
  "objects": [ { "dn": "CN=ExchangeActiveSyncDevices,CN=Jane Doe,OU=Staff,DC=stub,DC=test",
                 "objectClass": "msExchActiveSyncDevices" } ],
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
* `calls` is appended by every **mutating** cmdlet (`New-ADUser`, `Set-ADUser`, `Enable/Disable-ADAccount`,
  `Remove-ADUser`, `Remove-ADObject`, `Move-ADObject`, `Add/Remove-ADGroupMember`), including failed attempts (`result` starts with
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
