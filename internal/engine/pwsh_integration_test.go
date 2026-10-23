package engine

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// These tests run the REAL PowerShell scripts through the REAL PowerShellClient against the stub
// ActiveDirectory module (PSModulePath is restricted to it, so no domain can be reached).

type stubState struct {
	Users []struct {
		ObjectGUID string         `json:"objectGuid"`
		DN         string         `json:"dn"`
		Sam        string         `json:"sam"`
		UPN        string         `json:"upn"`
		Enabled    bool           `json:"enabled"`
		Attributes map[string]any `json:"attributes"`
		MemberOf   []string       `json:"memberOf"`
		Extra      map[string]any `json:"-"`
	} `json:"users"`
	Calls []struct {
		Cmdlet string `json:"cmdlet"`
	} `json:"calls"`
}

type pwshEnv struct {
	*env
	state string
}

func findPwsh(t *testing.T) string {
	t.Helper()
	for _, n := range []string{"pwsh.exe", "pwsh"} {
		if p, err := exec.LookPath(n); err == nil {
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

func newPwshEnv(t *testing.T) *pwshEnv {
	t.Helper()
	pwsh := findPwsh(t)
	scripts, _ := filepath.Abs(filepath.Join("..", "..", "scripts"))
	stub, _ := filepath.Abs(filepath.Join("..", "..", "tests", "powershell", "StubAD"))
	if !fileExists(filepath.Join(scripts, "New-IDLUser.ps1")) || !fileExists(filepath.Join(stub, "ActiveDirectory")) {
		t.Skip("scripts or stub module not present")
	}

	e := newEnv(t)
	state := filepath.Join(t.TempDir(), "ad-state.json")
	initial := map[string]any{
		"users": []any{},
		"groups": []any{
			map[string]string{"dn": gAll, "sam": "GG-All"},
			map[string]string{"dn": gMkt, "sam": "GG-Mkt"},
		},
		"ous": []string{baseOU, disabledOU, "OU=Groups,DC=corp,DC=example,DC=com"},
	}
	b, _ := json.Marshal(initial)
	if err := os.WriteFile(state, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Safety: prove a child pwsh resolves ActiveDirectory to the stub before anything runs.
	chk := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command",
		"Import-Module ActiveDirectory -ErrorAction Stop; (Get-Module ActiveDirectory).Version.ToString()")
	chk.Env = append(os.Environ(), "PSModulePath="+stub, "IDL_STUB_AD_STATE="+state)
	out, err := chk.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "99.0.0") {
		t.Fatalf("SAFETY ABORT: ActiveDirectory did not resolve to the stub module: %v %s", err, out)
	}

	logs := filepath.Join(scripts, "Logs")
	before := listDir(logs)
	t.Cleanup(func() { // scripts write their own log/CSV next to themselves; remove what this test created
		for name := range listDir(logs) {
			if !before[name] {
				_ = os.Remove(filepath.Join(logs, name))
			}
		}
	})

	e.eng.AD = &ad.PowerShellClient{
		Exe: pwsh, ScriptsDir: scripts, Timeout: 2 * time.Minute,
		ExtraEnv: []string{"PSModulePath=" + stub, "IDL_STUB_AD_STATE=" + state},
	}
	return &pwshEnv{env: e, state: state}
}

func listDir(dir string) map[string]bool {
	out := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

func (p *pwshEnv) read() stubState {
	p.t.Helper()
	b, err := os.ReadFile(p.state)
	if err != nil {
		p.t.Fatal(err)
	}
	var s stubState
	if err := json.Unmarshal(b, &s); err != nil {
		p.t.Fatal(err)
	}
	return s
}

func (p *pwshEnv) account(sam string) (idx int, s stubState) {
	s = p.read()
	for i, u := range s.Users {
		if strings.EqualFold(u.Sam, sam) {
			return i, s
		}
	}
	p.t.Fatalf("no stub account %q in %+v", sam, s.Users)
	return -1, s
}

func attr(u map[string]any, name string) string {
	for k, v := range u {
		if strings.EqualFold(k, name) {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func TestRealScriptsFullLifecycle(t *testing.T) {
	p := newPwshEnv(t)

	// 1. create (two people; Jane gets the marketing role)
	p.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	run := p.run()
	eq(t, "run ok", run.Status, model.RunOK)
	eq(t, "two creates proposed", run.Counters["planned_create"], 2)
	eq(t, "nothing written before approval", len(p.read().Users), 0)

	eq(t, "approved+executed", p.approveAll(model.ChangeCreate), 2)
	for _, c := range p.changes(model.StatusFailed, "") {
		t.Fatalf("change failed: %s", c.Error)
	}
	i, s := p.account("jdoe")
	jane := s.Users[i]
	eq(t, "dn", jane.DN, "CN=Jane Doe,"+baseOU)
	eq(t, "upn", jane.UPN, "jane.doe@corp.example.com")
	eq(t, "enabled", jane.Enabled, true)
	eq(t, "employeeID is the link key", attr(jane.Attributes, "employeeID"), "E1")
	eq(t, "givenName", attr(jane.Attributes, "givenName"), "Jane")
	eq(t, "role attribute", attr(jane.Attributes, "extensionAttribute1"), "MKT")
	eq(t, "jane in both groups", len(jane.MemberOf), 2)
	j, s := p.account("bsmith")
	eq(t, "bob only in the baseline group", len(s.Users[j].MemberOf), 1)

	// the generated password never reaches the audit log or the change rows
	chg, _, _ := p.st.ListChanges(p.ctx, store.ChangeFilter{}, store.Page{Limit: 100})
	raw, _ := json.Marshal(chg)
	if strings.Contains(strings.ToLower(string(raw)), "accountpassword") {
		t.Error("password material in change rows")
	}

	// 2. converge: snapshot parsed from real script output, nothing left to do
	run = p.run()
	eq(t, "converged planned_create", run.Counters["planned_create"], 0)
	eq(t, "converged planned_update", run.Counters["planned_update"], 0)
	eq(t, "converged unchanged", run.Counters["unchanged"], 2)

	// 3. update: title (sync) and a group change via role
	p.writeCSV(
		row("E1", "Jane", "Doe", "Engineering", "Senior Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	run = p.run()
	eq(t, "update proposed", run.Counters["planned_update"], 1)
	p.approveAll(model.ChangeUpdate)
	i, s = p.account("jdoe")
	eq(t, "title pushed by Set-IDLUser", attr(s.Users[i].Attributes, "title"), "Senior Designer")
	eq(t, "marketing group removed", len(s.Users[i].MemberOf), 1)

	// 4. immutable drift is corrected: tamper with the stub directly
	b, _ := os.ReadFile(p.state)
	re := regexp.MustCompile(`"displayName"\s*:\s*"Jane Doe"`)
	if !re.Match(b) {
		t.Fatalf("could not find displayName in the stub state to tamper with: %s", b)
	}
	if err := os.WriteFile(p.state, re.ReplaceAll(b, []byte(`"displayName":"Hacked"`)), 0o600); err != nil {
		t.Fatal(err)
	}
	run = p.run()
	eq(t, "drift detected", run.Counters["planned_update"], 1)
	p.approveAll(model.ChangeUpdate)
	i, s = p.account("jdoe")
	eq(t, "displayName restored", attr(s.Users[i].Attributes, "displayName"), "Jane Doe")
	// 5. terminate -> disable (moved to Disabled OU, groups stripped)
	p.writeCSV(
		row("E1", "Jane", "Doe", "Engineering", "Senior Designer", "T", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	run = p.run()
	eq(t, "disable proposed", run.Counters["planned_disable"], 1)
	p.approveAll(model.ChangeDisable)
	i, s = p.account("jdoe")
	eq(t, "disabled", s.Users[i].Enabled, false)
	eq(t, "in Disabled OU", strings.HasSuffix(s.Users[i].DN, ","+disabledOU), true)
	eq(t, "groups stripped", len(s.Users[i].MemberOf), 0)

	// 6. still visible to the next snapshot (Disabled OU is part of the scope), no repeated proposals
	run = p.run()
	eq(t, "no repeat disable", run.Counters["planned_disable"], 0)
	eq(t, "account still linked", p.identity("E1").State, model.StateDisabled)

	// 7. retention elapsed -> delete
	p.clock = p.clock.Add(31 * 24 * time.Hour)
	run = p.run()
	eq(t, "delete proposed", run.Counters["planned_delete"], 1)
	p.approveAll(model.ChangeDelete)
	for _, u := range p.read().Users {
		if strings.EqualFold(u.Sam, "jdoe") {
			t.Fatal("jdoe should be deleted")
		}
	}
	eq(t, "identity deleted", p.identity("E1").State, model.StateDeleted)
	eq(t, "bob untouched", len(p.read().Users), 1)
	eq(t, "no failed changes anywhere", len(p.changes(model.StatusFailed, "")), 0)
}

func TestRealScriptsDryRunNeverMutates(t *testing.T) {
	p := newPwshEnv(t)
	p.tweak(func(s *model.Settings) { s.Engine.DryRun = true })
	p.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	run := p.run()
	eq(t, "simulated", run.Counters["simulated"], 1)
	s := p.read()
	eq(t, "no users", len(s.Users), 0)
	for _, c := range s.Calls {
		t.Errorf("dry-run made a mutating call: %s", c.Cmdlet)
	}

	// the script-level -DryRun switch: run Create directly with DryRun and check it reports "would"
	res, err := p.eng.AD.Create(p.ctx, ad.ActionOpts{IDAttribute: "employeeID", DryRun: true}, []ad.CreateItem{{
		ChangeID: "c1", ExternalID: "E9", OU: baseOU, Enabled: true, Password: "Zz9!-not-logged-aaaa",
		Candidates: []model.Candidate{{Sam: "tuser", UPN: "tuser@corp.example.com", CN: "Test User"}},
		Attributes: map[string]string{"givenName": "Test"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "script reports would", res[0].Status, ad.StatusWould)
	eq(t, "still nothing written", len(p.read().Users), 0)
}

func TestRealScriptsOutageIsReportedAndRecoverable(t *testing.T) {
	p := newPwshEnv(t)
	p.tweak(func(s *model.Settings) { s.Approval.Create = "auto" })

	// failConnect makes every cmdlet throw ADServerDownException (script exit 4)
	b, _ := os.ReadFile(p.state)
	down := strings.Replace(string(b), `{`, `{"failConnect":true,`, 1)
	_ = os.WriteFile(p.state, []byte(down), 0o600)

	p.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	run := p.run()
	eq(t, "run flagged", run.Status, model.RunError)
	if !strings.Contains(strings.ToLower(run.Error), "snapshot") {
		t.Errorf("error should say the AD snapshot failed: %q", run.Error)
	}
	eq(t, "nothing proposed while blind", len(p.changes("", "")), 0)

	// directory back: the next run converges
	_ = os.WriteFile(p.state, b, 0o600)
	run = p.run()
	eq(t, "recovered and created", run.Counters["applied"], 1)
}

func TestRealScriptsRefuseWrongAccount(t *testing.T) {
	p := newPwshEnv(t)
	p.tweak(func(s *model.Settings) {
		s.Approval = model.ApprovalSettings{Create: "auto", Update: "manual", Disable: "manual", Enable: "manual", Delete: "manual"}
	})
	p.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	p.run()
	i, s := p.account("jdoe")
	guid := s.Users[i].ObjectGUID

	// A stale/forged change pointing at Jane's GUID but claiming a different person must be refused by the script.
	res, err := p.eng.AD.Update(p.ctx, ad.ActionOpts{IDAttribute: "employeeID"}, []ad.UpdateItem{{
		ChangeID: "x", ExternalID: "SOMEONE-ELSE", ObjectGUID: guid, Set: map[string]string{"title": "Pwned"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "refused", res[0].Status, ad.StatusFailed)
	_, s = p.account("jdoe")
	if attr(s.Users[0].Attributes, "title") == "Pwned" {
		t.Fatal("the script modified an account whose employeeID did not match")
	}

	// Deleting an enabled account is refused even with the right ids.
	res, _ = p.eng.AD.Remove(p.ctx, ad.ActionOpts{IDAttribute: "employeeID"}, []ad.RemoveItem{{ChangeID: "y", ExternalID: "E1", ObjectGUID: guid}})
	eq(t, "enabled account not deleted", res[0].Status, ad.StatusFailed)
	eq(t, "still there", len(p.read().Users), 1)
}
