# Testing IDLifecycle

How the automated tests work, what each one proves, and how to check for yourself that IDLifecycle will do what you
expect in your own environment before it changes anything.

**See it as a picture:** [TESTMAP.html](TESTMAP.html) maps every test suite to the code it runs, the stand-ins it uses in
place of AD, Microsoft 365, HR systems and mail, and the checks that reach the real systems. Click a suite or a box to
follow its connections. [CODEMAP.html](CODEMAP.html) maps the code itself.

* [1. The short version](#1-the-short-version)
* [2. How the test suites fit together](#2-how-the-test-suites-fit-together)
* [3. Go tests](#3-go-tests)
* [4. PowerShell tests](#4-powershell-tests)
* [5. The web console without a server](#5-the-web-console-without-a-server)
* [6. Checking your own environment](#6-checking-your-own-environment)
* [7. Run one script by hand](#7-run-one-script-by-hand)
* [8. Writing a test](#8-writing-a-test)
* [9. Before going live](#9-before-going-live)
* [10. Troubleshooting](#10-troubleshooting)

## 1. The short version

From the repository root:

```powershell
go test ./... -count=1                                   # Go: engine, API, store, sources, mail, CLI, browser end-to-end
pwsh -NoProfile -File tests\powershell\Run-Tests.ps1     # PowerShell: every script, against stubs (about 20 minutes)
go run ./cmd/idl-mockui                                  # the console with sample data, no server needed
```

Both suites exit with code 0 when everything passed. Neither can reach a real domain, tenant, mail server or HR
system. Then, on the server, check the real environment without changing it (section 6):

```powershell
idlifecycled check -cloud           # config, data folder, database, PowerShell, scripts, AD module, Microsoft 365 sign-in
```

…followed by the **Test** buttons in the console, **Preview impact**, and dry-run syncs.

## 2. How the test suites fit together

There are three layers, and each one stops at a different place:

| Layer | Runs | Stops at (stand-in) | Proves |
|---|---|---|---|
| **Go tests** | the engine, API, store, sources, mail, CLI | in-memory AD and Entra ID (`ad.FakeClient`, `cloud.Fake`), a temporary SQLite database, fake HR APIs, a fake mail server | the logic: who gets created, changed, disabled or deleted; what waits for approval; that dry run and the limits hold |
| **PowerShell tests** | every script in `scripts\` | a stub `ActiveDirectory` module, a fake Graph, a stub Exchange Online module, fakes for the installer | the scripts: the right cmdlets with the right arguments, re-runs that change nothing, dry runs that write nothing, refusals, error handling, secrets kept out of logs |
| **Your checks** | the installed service and the scripts | nothing: your real AD, tenant, HR systems and mail | your environment: permissions, connections, data, and what your rules would do to your people |

Two tests join the layers so the hand-off between them is tested too:

* **The PowerShell bridge** (`internal/engine/pwsh_integration_test.go`) runs the real AD scripts through the real Go
  `PowerShellClient` against the stub AD module. It proves the JSON contract between the exe and the scripts end to end.
* **The browser end-to-end tests** (`internal/app/e2e_test.go`, `e2e_oidc_test.go`) start the real server with the
  production Content Security Policy and drive the real console in headless Edge.

The fakes are kept honest by contract tests: `TestFakeContract` (`internal/ad`) and `TestFakeRefusesWhatTheScriptsRefuse`
(`internal/cloud`) check that the Go fakes answer like the scripts do, and the stub modules copy the real cmdlets'
errors (already a member, not found, duplicate names, non-leaf deletes; Graph's error codes for mail-enabled,
dynamic, synced and role-assignable groups).

**What is not covered by any automated test:** a real domain, a real tenant, a real HR system, and database servers
other than SQLite unless you point the store suite at one. That is what section 6 is for.

## 3. Go tests

**Needs:** the Go version in `go.mod`. PowerShell 7 for the bridge test, Edge or Chrome for the browser tests; each is
skipped, not failed, when missing (`go test -v` shows `SKIP`).

```powershell
go test ./... -count=1                                  # everything; -count=1 skips Go's result cache
go test ./internal/engine -count=1                      # one package
go test ./internal/engine -run TestDryRun -v            # tests whose name matches, with their output
go test ./internal/engine -run 'TestCloud' -v           # e.g. all cloud-group engine tests
go vet ./...                                            # static checks
```

| Package | What its tests prove |
|---|---|
| `internal/engine` | Whole sync runs against `ad.FakeClient` and SQLite: create, update, disable, rehire, delete; approval per change type; dry run never writes (`TestDryRunNeverWritesAndIsReplacedWhenLive`); bulk and delete limits; crash recovery; naming conflicts; matching people to existing accounts; role conditions; OU placement; lifecycle and cloud groups; the impact preview. Undo of every change type (drift, force, holds, redo, what is refused) and the per-person history events (only real changes recorded, an account gone from AD recorded once, the identity kept after a delete). |
| `internal/api` | Every endpoint on an `httptest` server: sign-in, lockout, CSRF, viewer / approver / admin permissions, settings validation and secret masking, Entra ID sign-in, backups, group searches, the Microsoft 365 connection test, the impact preview. Undo preview, request and approval (typed confirmation, second person, no bulk), person history, and before/after detail on every console change with no secret in the audit log. |
| `internal/store` | One shared suite for every database driver (see below). |
| `internal/sources` | CSV fixtures; the REST connector against fake HR APIs (every auth and paging mode, retries, refused cross-host links); SQL against a temporary database; manual entry; `example_connector_test.go`. |
| `internal/notify` | A fake SMTP server and token endpoint: STARTTLS, TLS, OAuth2, passwords scrubbed from errors, header injection refused. |
| `internal/ad`, `internal/cloud` | The PowerShell runner: request and response JSON, exit codes, stray output, timeouts, passwords scrubbed, scripts blocked by Mark of the Web, and the fakes' contract. |
| `internal/app` | Headless-browser end-to-end tests (above). |
| `cmd/idlifecycled` | `init`, `lockdown`, `check`, `user`, `config get/set` against a temporary data folder. |
| others | config validation, encryption and DPAPI, backup and restore round trips, schedules and time zones, folder ACLs, DSN scrubbing, branding. |

**Databases other than SQLite.** The store suite runs against a real server when you set its variable. Use an empty
throwaway database: the suite creates and drops tables.

```powershell
$env:IDL_TEST_POSTGRES_DSN = 'postgres://idltest:...@dbhost/idltest?sslmode=require'
go test ./internal/store -count=1 -v
# likewise IDL_TEST_MSSQL_DSN, IDL_TEST_MYSQL_DSN, IDL_TEST_DB2_DSN
Remove-Item Env:\IDL_TEST_POSTGRES_DSN
```

Set the variable in the session rather than on the command line, so the password doesn't end up in your shell history.

## 4. PowerShell tests

**Needs:** PowerShell 7 and Pester **3.4.0**, the version that ships with Windows (do not use Pester 4/5 syntax such as
`Should -Be`).

```powershell
pwsh -NoProfile -File tests\powershell\Run-Tests.ps1
# one file while you work on a script:
pwsh -NoProfile -Command "Import-Module Pester -RequiredVersion 3.4.0; Invoke-Pester -Path tests\powershell\Disable-IDLUser.Tests.ps1"
```

`Run-Tests.ps1` checks every script for syntax errors, proves the stub is the only `ActiveDirectory` module a child
`pwsh` can load, runs every `*.Tests.ps1`, and cleans up. Exit codes: 0 all passed, 1 test failures, 2 syntax error,
3 Pester missing, 5 safety check failed or unexpected error. Its log is in `tests\powershell\Logs`.

**How a script test works.** Each test:

1. writes a JSON file describing a small directory: users, groups, OUs, and optional faults to inject;
2. runs the real script in a child `pwsh` whose module path holds only the stubs, with a request on stdin, exactly as
   the service does;
3. checks the script's JSON result, its exit code, its log, and the directory afterwards, including the list of every
   change the stub was asked to make. That list is how a test proves a dry run made **zero** changes.

**Why it can't reach your domain**, even on a domain-joined machine:

* the child `pwsh` sees only `tests\powershell\StubAD` (and `StubEXO`) on its module path, and the run aborts with
  `SAFETY ABORT` unless `ActiveDirectory` resolves to the stub's version 99.0.0;
* the stubs refuse to load unless a test state file is set (`IDL_STUB_AD_STATE`, `IDL_STUB_EXO_STATE`);
* the cloud module redirects Graph and sign-in only when `IDL_CLOUD_TEST=1` and only to `127.0.0.1`, where
  `FakeGraph.ps1` checks the certificate-signed sign-in like Entra ID does;
* the installer fakes load only with `IDL_SETUP_TEST=1` and only from the tests folder.

The stub's state file format, every fault you can inject, and which real-AD behaviours it copies are documented in
[tests/powershell/README.md](../tests/powershell/README.md).

## 5. The web console without a server

```powershell
go run ./cmd/idl-mockui            # prints two URLs on http://127.0.0.1:8099
```

* **Mock console:** `/mock/mock.html?autologin=admin#/dashboard`. Every page works against `mock-api.js`, an in-browser
  API with sample people, changes, runs and settings. Sign in as `admin` / `admin-pass` or `approver1` /
  `approver-pass` to see the other roles. Changes live only in that browser tab; reload to start over.
* **Mock selftest:** `/mock/mock-selftest.html?user=admin&pass=admin-pass` clicks through the console and prints `PASS`
  or the failing step.

The same selftest runs against the **real** server inside `go test ./internal/app`. A few selftest steps (field mapper,
Entra ID sign-in mask, history filter) need the real API and fail against the mock; that is expected.

Use `-port` to change the port. It listens on the loopback address only and is not part of the release build.

## 6. Checking your own environment

These are the checks that touch your real systems. Each one reads, tests a sign-in, or plans without writing. In order:

### 6.1 The installer's test run

```powershell
.\scripts\Install-IDLifecycle.ps1 -ServiceAccount 'CORP\svc-idl$' -DryRun
```

Shows every install step and its preflight checks without doing them. The setup window calls this **Test run (no
changes)**. `Uninstall-IDLifecycle.ps1 -DryRun` does the same for removal.

### 6.2 The health check

```powershell
idlifecycled check -json            # config, data folder ACL, database, admin user, PowerShell, scripts, AD module
idlifecycled check -cloud           # also signs in to Microsoft 365 with the saved connection (changes nothing)
```

Every check is listed in order; one that couldn't run is marked skipped. Run it **as the service account** when you
can (for example from a scheduled task), because permissions are the most common difference between "works for me"
and "works for the service".

### 6.3 The Test buttons in the console

| Where | Button | What it proves |
|---|---|---|
| Sources → a source | **Test these settings** | fetches once with the settings in the form, without saving them, and shows what came back |
| Settings → Microsoft 365 / Entra ID | **Test connection** | certificate, sign-in, Graph permissions, reading groups and users, the Exchange Online module, sign-in and rights; warns 30 days before the certificate expires |
| Settings → Notifications (email) | **Send test email** | the mail settings deliver |
| Settings → Sign-in | **Test sign-in settings** | the Entra ID app registration for console sign-in |
| Roles → a rule | **Test this rule** | which condition matches for a sample record |
| Approvals → a create waiting for a name | **Set username** | the live name check against AD |

### 6.4 Preview impact

**Roles**, **Mappings** and **Lifecycle groups** have **Preview impact** in the editor. It plans everyone with the
saved configuration and with your unsaved change, against one live AD snapshot (and the Microsoft 365 connection, if
on), and lists every person whose result would differ: attributes, groups, OU, cloud groups. Nothing is saved or
written. Use it before every rule change, not only the first.

### 6.5 Dry-run syncs

A fresh install starts with **Settings → Engine → Dry run** on. In dry run a sync does all the work up to the point of
changing AD: it reads the sources and AD, plans every change, and records each one as **simulated**. Simulated changes
can't be approved, and while dry run is on the executor applies nothing, not even changes approved earlier. Review them in
**Approvals** and **History**, and the counts on **Runs**. When you turn dry run off, the next sync replaces them with
real changes, which then follow your approval settings.

### 6.6 A supervised first live run

1. Keep every approval type on **manual**, and keep the bulk and delete limits low.
2. Point a search base at a lab OU with a handful of test people, or limit the source to them.
3. Turn dry run off and run a sync. Approve **one** change of each type: create, update, disable, enable, link, cloud
   groups.
4. Check each result in AD (and Entra ID / Exchange for cloud groups), in **History**, and in the script logs
   (`scripts\Logs` on the server).
5. Widen the scope step by step. Switch a change type to **auto** only after its manual approvals have looked right
   for a while; keep `enable` and `delete` manual.

## 7. Run one script by hand

Every script reads one JSON request on stdin and writes one JSON result on stdout, so you can run any of them exactly
as the service does. The request formats are in [ARCHITECTURE.md §6](ARCHITECTURE.md#6-powershell-contract-go---scripts).

Run them from a checkout or a copy of the `scripts` folder: they write their log and CSV report to a `Logs` folder next
to themselves.

### 7.1 Against the stub (safe anywhere)

Use a **new** PowerShell window and close it afterwards, because the module path is changed for that session:

```powershell
$env:PSModulePath = "$PWD\tests\powershell\StubAD"          # only the stub can load
$env:IDL_STUB_AD_STATE = "$env:TEMP\idl-stub.json"
Get-Module -ListAvailable ActiveDirectory                   # must show version 99.0.0 under StubAD
@'
{ "users":  [ { "objectGuid": "11111111-1111-1111-1111-111111111111", "dn": "CN=Jane Doe,OU=Staff,DC=stub,DC=test",
                "sam": "jdoe", "upn": "jdoe@stub.test", "enabled": true,
                "attributes": { "employeeID": "E1001", "title": "Analyst" },
                "memberOf": [ "CN=GG-AllStaff,OU=Groups,DC=stub,DC=test" ] } ],
  "groups": [ { "dn": "CN=GG-AllStaff,OU=Groups,DC=stub,DC=test", "sam": "GG-AllStaff" } ],
  "ous":    [ "OU=Staff,DC=stub,DC=test", "OU=Groups,DC=stub,DC=test", "OU=Disabled,DC=stub,DC=test" ],
  "calls": [], "faults": [], "failConnect": false }
'@ | Set-Content -Encoding utf8NoBOM $env:IDL_STUB_AD_STATE

'{"server":"","idAttribute":"employeeID","items":[{"changeId":"c1","externalId":"E1001",
  "objectGuid":"11111111-1111-1111-1111-111111111111","moveTo":"OU=Disabled,DC=stub,DC=test",
  "removeGroups":["GG-AllStaff"],"description":"Disabled by test"}]}' |
  pwsh -NoProfile -NonInteractive -File .\scripts\Disable-IDLUser.ps1 -DryRun
```

The result says `"status":"would"` with each step it would take, and the state file's `calls` stays empty. Run it again
without `-DryRun` and the state file shows Jane disabled, moved and out of the group, with each cmdlet listed in
`calls`. Edit the state file to try other cases: add a `faults` entry to make a cmdlet fail, or set `failConnect` to
simulate an unreachable domain controller (exit code 4).

### 7.2 Against your domain, read-only or dry run

In a normal PowerShell 7 window with the RSAT `ActiveDirectory` module, as an account that can read the OUs:

```powershell
# What IDLifecycle sees: the managed accounts, the attributes it reads and the groups' members
'{"server":"","searchBases":["OU=Staff,DC=corp,DC=example,DC=com"],"idAttribute":"employeeID",
  "properties":["title","department","manager"],"groups":["GG-AllStaff"]}' |
  pwsh -NoProfile -NonInteractive -File .\scripts\Get-IDLSnapshot.ps1 | ConvertFrom-Json

# What disabling one test account would do, without doing it
'{"server":"","idAttribute":"employeeID","items":[{"changeId":"t1","externalId":"E1001",
  "objectGuid":"<the account objectGUID>","moveTo":"OU=Disabled,DC=corp,DC=example,DC=com",
  "removeGroups":["GG-AllStaff"],"description":"Disabled by IDLifecycle"}]}' |
  pwsh -NoProfile -NonInteractive -File .\scripts\Disable-IDLUser.ps1 -DryRun
```

* `Get-IDLSnapshot`, `Get-IDLSchema`, `Get-IDLOUs`, `Search-IDLGroups` and `Test-IDLNames` only read.
* `New-`, `Set-`, `Disable-`, `Enable-`, `Remove-IDLUser` and `Set-IDLLink` read but never write with `-DryRun`: each
  successful item comes back as `would` with what it would do, and the PowerShell tests prove that a dry run makes no
  change. Without `-DryRun` they **do** change AD, so try them on a test account in a lab OU first.
* Each script checks that the account's ID attribute equals `externalId` before touching it, so a wrong GUID fails
  instead of changing someone else.

### 7.3 The Microsoft 365 connection by hand

The **Test connection** button and `idlifecycled check -cloud` run this script as the service. To run it yourself,
with certificate authentication (no secret on the command line or in a file):

```powershell
'{"cloud":{"environment":"global","tenantId":"<tenant id>","clientId":"<client id>","auth":"certificate",
  "certThumbprint":"<thumbprint>","exchange":{"enabled":true,"organization":"contoso.onmicrosoft.com"}}}' |
  pwsh -NoProfile -NonInteractive -File .\scripts\Test-IDLCloudConnection.ps1 | ConvertFrom-Json |
  Select-Object -ExpandProperty checks
```

The private key normally grants access only to the service account, so run this elevated or as the service account.
If the key can't be read, the `certificate` check says so. `Search-IDLCloudGroups.ps1` (`"query":"fin"` next to
`cloud`) and `Get-IDLCloudSnapshot.ps1` are read-only too; `Set-IDLCloudGroups.ps1 -DryRun` re-reads each group
and reports what it would add or remove without writing.

## 8. Writing a test

**Engine behaviour (Go).** The engine tests build a small world in a few lines (`newEnv` in
`internal/engine/engine_test.go`): settings, a CSV source, the fake directory, and a sync run. For example, the dry-run
test:

```go
func TestDryRunNeverWritesAndIsReplacedWhenLive(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) {
		s.Engine.DryRun = true
		s.Approval.Create = "auto"
	})
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))

	run := e.run()
	eq(t, "simulated", run.Counters["simulated"], 1)
	eq(t, "no accounts", len(e.fake.Users), 0)
	// ...
}
```

To pin down a scenario from your environment (a rule that should match someone, a rehire, a department move), copy
the closest test, change the CSV row and settings, and assert on the run counters, `e.changes(...)` and `e.fake`. Use
the same helpers for cloud groups (`withCloud`, `grantRole` in `cloud_test.go`).

**A script (Pester).** Build a stub state, run the script, check the result and the stub (from
`tests/powershell/Disable-IDLUser.Tests.ps1`):

```powershell
It 'DryRun makes zero mutating calls and reports would' {
    $f = New-Fixture
    $item = [ordered]@{ changeId = 'c1'; externalId = 'E1001'; objectGuid = $f.Jane.objectGuid; moveTo = "OU=Disabled,$script:Root"; removeGroups = @('GG-AllStaff'); description = 'x1234' }
    $r = Invoke-IDLScript -Script 'Disable-IDLUser' -Request (New-DisableRequest @($item)) -StatePath $f.State -DryRun

    $r.ExitCode | Should Be 0
    $r.Json.results[0].status | Should Be 'would'
    (Get-StubCalls -Path $f.State).Count | Should Be 0
}
```

Pester 3 syntax: `Should Be`, `Should Match`, `Should Throw`, without the dash. Cloud script tests use
`Invoke-CloudScript` with `Start-FakeGraph` and the Exchange stub helpers in `CloudTestHelpers.ps1`.

**The console.** Add sample data to `db` in `internal/web/testdata/mock/mock-api.js` to see a case in the mock
console. Add a step to `internal/web/testdata/selftest/selftest.js` to have it checked against the real server by
`go test ./internal/app`.

## 9. Before going live

- [ ] `go test ./... -count=1` and `Run-Tests.ps1` pass on the build you are installing.
- [ ] The store suite passed against your database server, if it isn't SQLite.
- [ ] `idlifecycled check -json` passes on the server, run as the service account.
- [ ] **Test these settings** for every source shows the people and fields you expect.
- [ ] **Test connection** for Microsoft 365 is all green, if cloud groups are on.
- [ ] **Send test email** arrives.
- [ ] A dry-run sync's simulated changes are what you expected, in number and content. Look closely at disables and
      deletes.
- [ ] **Preview impact** for each role and mapping shows only the people you meant.
- [ ] Approvals are manual and the bulk and delete limits are low.
- [ ] A supervised live run in a lab OU produced the right result for each change type.
- [ ] You know where to look afterwards: **Runs**, **History**, and `scripts\Logs` on the server.

## 10. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `SAFETY ABORT` from `Run-Tests.ps1` | The child `pwsh` didn't load the stub `ActiveDirectory` module. Check that `tests\powershell\StubAD` is intact. Nothing ran, so nothing was touched. |
| `Should -Be` or `-Not` errors, or exit code 3 | Pester 4/5 was loaded, or Pester 3.4.0 is missing. The runner imports 3.4.0 explicitly; install it side by side if it's missing. |
| Random PowerShell test failures | Two Pester runs at once: each deletes the log files that appear during its run. Run one at a time. |
| An install test fails once, then passes on its own | The installer tests use many temporary folders; one has been seen to lose its temp folder once. Re-run that file alone; a real failure repeats. |
| `SKIP` on the browser tests | No Edge or Chrome on the machine. Install Edge to run them. |
| `SKIP` on the PowerShell bridge test | `pwsh` isn't on `PATH`. |
| Go tests pass without running | Go's test cache: add `-count=1`. |
| A script by hand prints nothing useful | Read its log in the `Logs` folder next to the script; stdout is reserved for the single JSON result. Exit codes: 0 ok, 1 some items failed, 2 bad request, 3 missing dependency, 4 sign-in or connection, 5 unexpected. |
