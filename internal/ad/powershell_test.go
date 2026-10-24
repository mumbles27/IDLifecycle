package ad

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func pwshPath(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh.exe", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if p := `C:\Program Files\PowerShell\7\pwsh.exe`; fileExists(p) {
		return p
	}
	t.Skip("pwsh not installed")
	return ""
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// writeScripts creates throwaway scripts that exercise only the runner contract (no AD involved).
func writeScripts(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func client(t *testing.T, dir string) *PowerShellClient {
	return &PowerShellClient{Exe: pwshPath(t), ScriptsDir: dir, Timeout: 60 * time.Second}
}

const echoScript = `
[CmdletBinding()] param([switch]$DryRun)
[Console]::InputEncoding = [Text.UTF8Encoding]::new($false)
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$req = [Console]::In.ReadToEnd() | ConvertFrom-Json
$results = foreach ($i in $req.items) {
  [pscustomobject]@{ changeId = $i.changeId; status = $(if ($DryRun) { 'would' } else { 'applied' }); message = ('server=' + $req.server + ' ext=' + $i.externalId) }
}
[pscustomobject]@{ ok = $true; dryRun = [bool]$DryRun; results = @($results) } | ConvertTo-Json -Compress -Depth 5
`

func TestRunnerRoundTripAndDryRunFlag(t *testing.T) {
	dir := writeScripts(t, map[string]string{scriptSet: echoScript})
	c := client(t, dir)
	items := []UpdateItem{{ChangeID: "c1", ExternalID: "Ünï-1", ObjectGUID: "g"}, {ChangeID: "c2", ExternalID: "E2"}}

	res, err := c.Update(context.Background(), ActionOpts{Server: "dc1"}, items)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Status != StatusApplied || !strings.Contains(res[0].Message, "ext=Ünï-1") || !strings.Contains(res[0].Message, "server=dc1") {
		t.Fatalf("stdin JSON (UTF-8) must reach the script intact: %+v", res)
	}

	res, err = c.Update(context.Background(), ActionOpts{DryRun: true}, items)
	if err != nil || res[0].Status != StatusWould {
		t.Fatalf("-DryRun must be passed to the script: %v %+v", err, res)
	}
}

func TestRunnerExitCodes(t *testing.T) {
	dir := writeScripts(t, map[string]string{
		scriptSet:     `[Console]::Out.Write('{"ok":false,"error":"bad request"}'); exit 2`,
		scriptNew:     `[Console]::Out.Write('{"ok":false,"error":"no ad module"}'); exit 3`,
		scriptDisable: `[Console]::Out.Write('{"ok":false,"error":"dc down"}'); exit 4`,
		scriptEnable:  `[Console]::Error.Write('boom'); exit 5`,
		// exit 1 = partial: results are still delivered, and a missing result becomes a failure.
		scriptRemove: `[Console]::Out.Write('{"ok":true,"results":[{"changeId":"a","status":"applied"},{"changeId":"b","status":"failed","error":"nope"}]}'); exit 1`,
	})
	c := client(t, dir)
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
		want error
		code int
	}{
		{"invalid", func() error { _, e := c.Update(ctx, ActionOpts{}, []UpdateItem{{ChangeID: "x"}}); return e }, ErrInvalidInput, 2},
		{"dependency", func() error { _, e := c.Create(ctx, ActionOpts{}, []CreateItem{{ChangeID: "x"}}); return e }, ErrDependency, 3},
		{"connection", func() error { _, e := c.Disable(ctx, ActionOpts{}, []DisableItem{{ChangeID: "x"}}); return e }, ErrConnection, 4},
		{"fatal", func() error { _, e := c.Enable(ctx, ActionOpts{}, []EnableItem{{ChangeID: "x"}}); return e }, ErrFatal, 5},
	}
	for _, tc := range cases {
		err := tc.call()
		var se *ScriptError
		if !errors.Is(err, tc.want) || !errors.As(err, &se) || se.ExitCode != tc.code {
			t.Errorf("%s: got %v", tc.name, err)
		}
	}

	res, err := c.Remove(ctx, ActionOpts{}, []RemoveItem{{ChangeID: "a"}, {ChangeID: "b"}, {ChangeID: "c"}})
	if err != nil {
		t.Fatalf("partial failure must not be an error: %v", err)
	}
	if res[0].Status != StatusApplied || res[1].Status != StatusFailed || res[1].Error != "nope" || res[2].Status != StatusFailed {
		t.Fatalf("partial results/reconcile wrong: %+v", res)
	}
}

func TestRunnerRejectsStrayOutputAndTimeouts(t *testing.T) {
	dir := writeScripts(t, map[string]string{
		scriptSnapshot: `[Console]::Out.WriteLine('hello'); [Console]::Out.WriteLine('{"ok":true,"users":[],"groups":[]}')`,
		scriptSet:      `Start-Sleep -Seconds 30`,
	})
	c := client(t, dir)
	if _, err := c.Snapshot(context.Background(), SnapshotRequest{}); err == nil {
		t.Fatal("stdout with extra text must be rejected so contract regressions are caught early")
	}

	c.Timeout = 2 * time.Second
	start := time.Now()
	_, err := c.Update(context.Background(), ActionOpts{}, []UpdateItem{{ChangeID: "x"}})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 20*time.Second {
		t.Fatalf("script must be killed on timeout: %v after %s", err, time.Since(start))
	}
}

func TestSnapshotParsing(t *testing.T) {
	dir := writeScripts(t, map[string]string{
		scriptSnapshot: `
$req = [Console]::In.ReadToEnd() | ConvertFrom-Json
'{"ok":true,"users":[{"objectGuid":"g1","dn":"CN=J,OU=Staff,DC=x","sam":"j","upn":"j@x","enabled":true,"parentOu":"OU=Staff,DC=x","externalId":"E1","attributes":{"title":null,"sn":"Doe"},"memberOf":["CN=G,DC=x"]}],"groups":[{"identity":"G","dn":"CN=G,DC=x","found":true}]}'`,
	})
	snap, err := client(t, dir).Snapshot(context.Background(), SnapshotRequest{IDAttribute: "employeeID"})
	if err != nil {
		t.Fatal(err)
	}
	u := snap.Users[0]
	if u.ExternalID != "E1" || u.Attr("SN") != "Doe" || u.Attr("title") != "" || u.Attributes["title"] != nil || u.MemberOf[0] != "CN=G,DC=x" || !snap.Groups[0].Found {
		t.Fatalf("snapshot decode: %+v", snap)
	}
}

func TestPasswordIsScrubbedFromErrors(t *testing.T) {
	dir := writeScripts(t, map[string]string{
		scriptNew: `[Console]::Error.Write('failed to set password Hunter2-Secret!'); exit 5`,
	})
	_, err := client(t, dir).Create(context.Background(), ActionOpts{}, []CreateItem{{ChangeID: "x", Password: "Hunter2-Secret!"}})
	if err == nil || strings.Contains(err.Error(), "Hunter2") {
		t.Fatalf("password leaked into error: %v", err)
	}
}

func TestFakeContract(t *testing.T) {
	f := NewFake()
	f.AddGroup("GG-All", "CN=GG-All,OU=Groups,DC=x")
	ctx := context.Background()
	o := ActionOpts{IDAttribute: "employeeID"}
	f.AddUser(FakeUser{DN: "CN=Taken,OU=Staff,DC=x", Sam: "jdoe", UPN: "jdoe@x", External: "E0", Enabled: true})

	item := CreateItem{ChangeID: "c", ExternalID: "E1", OU: "OU=Staff,DC=x", Enabled: true, Groups: []string{"GG-All", "GG-Missing"},
		Candidates: []model.Candidate{{Sam: "jdoe", UPN: "jdoe@x", CN: "John Doe"}, {Sam: "jdoe2", UPN: "jdoe2@x", CN: "John Doe 2"}}}
	res, _ := f.Create(ctx, o, []CreateItem{item})
	if res[0].Sam != "jdoe2" || res[0].Status != StatusFailed || !strings.Contains(res[0].Error, "GG-Missing") || res[0].ObjectGUID == "" {
		t.Fatalf("2nd candidate + created-but-group-failed: %+v", res[0])
	}
	res, _ = f.Create(ctx, o, []CreateItem{item})
	if res[0].Status != StatusSkipped || res[0].Message != "exists" {
		t.Fatalf("create must be idempotent: %+v", res[0])
	}
	guid := res[0].ObjectGUID

	res, _ = f.Remove(ctx, o, []RemoveItem{{ChangeID: "r", ExternalID: "E1", ObjectGUID: guid}})
	if res[0].Status != StatusFailed {
		t.Fatalf("must refuse to delete an enabled account: %+v", res[0])
	}
	res, _ = f.Update(ctx, o, []UpdateItem{{ChangeID: "u", ExternalID: "WRONG", ObjectGUID: guid, Set: map[string]string{"title": "x"}}})
	if res[0].Status != StatusFailed {
		t.Fatalf("external id mismatch must fail: %+v", res[0])
	}
}

func TestBlockedScriptsFindsMarkOfTheWeb(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "Clean.ps1")
	blocked := filepath.Join(dir, "sub", "Blocked.psm1")
	_ = os.MkdirAll(filepath.Dir(blocked), 0o700)
	for _, p := range []string{clean, blocked, filepath.Join(dir, "notes.txt")} {
		if err := os.WriteFile(p, []byte("#"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(blocked+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o600); err != nil {
		t.Skip("alternate data streams not supported here:", err)
	}
	got := BlockedScripts(dir)
	if len(got) != 1 || got[0] != blocked {
		t.Fatalf("BlockedScripts = %v, want [%s]", got, blocked)
	}
}
