package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
	"idlifecycle/internal/store/storetest"
)

const (
	baseOU     = "OU=Staff,DC=corp,DC=example,DC=com"
	disabledOU = "OU=Disabled,DC=corp,DC=example,DC=com"
	csvHeader  = "EmployeeID,FirstName,LastName,Department,Title,Status,ManagerID"
	gAll       = "CN=GG-All,OU=Groups,DC=corp,DC=example,DC=com"
	gMkt       = "CN=GG-Mkt,OU=Groups,DC=corp,DC=example,DC=com"
)

type env struct {
	t     *testing.T
	ctx   context.Context
	st    *store.Store
	fake  *ad.Fake
	eng   *Engine
	csv   string
	src   model.Source
	clock time.Time
}

func (e *env) now() time.Time { return e.clock }

func newEnv(t *testing.T) *env {
	t.Helper()
	st := storetest.New(t)
	c, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	fake := ad.NewFake()
	fake.AddGroup("GG-All", gAll)
	fake.AddGroup("GG-Mkt", gMkt)

	e := &env{t: t, ctx: context.Background(), st: st, fake: fake, clock: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}
	e.eng = New(st, fake, c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.eng.Now = e.now

	s := model.DefaultSettings()
	s.Engine.DryRun = false
	s.AD.SearchBases = []string{baseOU}
	s.AD.DefaultOU = baseOU
	s.AD.DisabledOU = disabledOU
	s.AD.UPNSuffix = "corp.example.com"
	e.saveSettings(s)

	e.csv = filepath.Join(t.TempDir(), "hr.csv")
	e.src = model.Source{
		ID: "src1", Name: "HR CSV", Type: "csv", Enabled: true, CreatedAt: e.clock, UpdatedAt: e.clock,
		Options: model.SourceOptions{ExternalIDField: "EmployeeID", StatusField: "Status", TerminatedValues: []string{"T"}},
		Config:  map[string]any{"path": e.csv},
	}
	e.must(st.CreateSource(e.ctx, e.src))

	for i, m := range []model.Mapping{
		{Name: "givenName", TargetAttr: "givenName", Template: "{{ .FirstName }}", Mode: model.ModeSync},
		{Name: "sn", TargetAttr: "sn", Template: "{{ .LastName }}", Mode: model.ModeSync},
		{Name: "displayName", TargetAttr: "displayName", Template: "{{ .FirstName }} {{ .LastName }}", Mode: model.ModeImmutable},
		{Name: "title", TargetAttr: "title", Template: "{{ .Title }}", Mode: model.ModeSync, OnEmpty: "clear"},
		{Name: "description", TargetAttr: "description", Template: "{{ .Title }}", Mode: model.ModeCreateOnly},
	} {
		m.ID, m.Enabled, m.SortOrder, m.ValueType, m.CreatedAt, m.UpdatedAt = "m"+m.Name, true, i, "string", e.clock, e.clock
		if m.OnEmpty == "" {
			m.OnEmpty = "skip"
		}
		e.must(st.CreateMapping(e.ctx, m))
	}
	e.must(st.CreateRole(e.ctx, model.RoleRule{ID: "r-all", Name: "Everyone", Enabled: true, Priority: 10, Match: "all",
		Grants: model.Grants{Groups: []string{"GG-All"}}, CreatedAt: e.clock, UpdatedAt: e.clock}))
	e.must(st.CreateRole(e.ctx, model.RoleRule{ID: "r-mkt", Name: "Marketing", Enabled: true, Priority: 20, Match: "all",
		Conditions: []model.Condition{{Attr: "Department", Op: "eq", Value: "Marketing"}},
		Grants:     model.Grants{Groups: []string{"GG-Mkt"}, Attributes: []model.AttrGrant{{TargetAttr: "extensionAttribute1", Template: "MKT", Mode: model.ModeSync, OnEmpty: "skip", ValueType: "string"}}},
		CreatedAt:  e.clock, UpdatedAt: e.clock}))
	return e
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) saveSettings(s model.Settings) {
	e.t.Helper()
	e.must(s.Validate())
	e.must(e.st.SaveSettings(e.ctx, s, "test"))
}

func (e *env) settings() model.Settings {
	s, err := e.st.GetSettings(e.ctx)
	e.must(err)
	return s
}

func (e *env) tweak(fn func(*model.Settings)) {
	e.t.Helper()
	s := e.settings()
	fn(&s)
	e.saveSettings(s)
}

func (e *env) writeCSV(rows ...string) {
	e.t.Helper()
	e.must(os.WriteFile(e.csv, []byte(csvHeader+"\n"+strings.Join(rows, "\n")+"\n"), 0o600))
}

func (e *env) run() model.Run {
	e.t.Helper()
	r, err := e.eng.RunNow(e.ctx, "test", "tester")
	e.must(err)
	return r
}

func (e *env) changes(status, typ string) []model.Change {
	e.t.Helper()
	list, _, err := e.st.ListChanges(e.ctx, store.ChangeFilter{Status: status, Type: typ}, store.Page{Limit: 500})
	e.must(err)
	return list
}

func (e *env) approveAll(typ string) int {
	e.t.Helper()
	list := e.changes(model.StatusPending, typ)
	for _, c := range list {
		if _, err := e.eng.Approve(e.ctx, c.ID, "alice", "ok"); err != nil {
			e.t.Fatalf("approve %s: %v", c.ID, err)
		}
	}
	if len(list) > 0 {
		if _, err := e.eng.Execute(e.ctx); err != nil {
			e.t.Fatal(err)
		}
	}
	return len(list)
}

func (e *env) identity(ext string) model.Identity {
	e.t.Helper()
	list, err := e.st.ListIdentitiesBySource(e.ctx, e.src.ID)
	e.must(err)
	for _, i := range list {
		if i.ExternalID == ext {
			return i
		}
	}
	e.t.Fatalf("identity %s not found", ext)
	return model.Identity{}
}

func (e *env) user(ext string) ad.FakeUser {
	e.t.Helper()
	u, ok := e.fake.ByExternal(ext)
	if !ok {
		e.t.Fatalf("no AD account for %s", ext)
	}
	return u
}

func hasGroup(u ad.FakeUser, dn string) bool {
	for _, g := range u.MemberOf {
		if strings.EqualFold(g, dn) {
			return true
		}
	}
	return false
}

func row(id, first, last, dept, title, status, mgr string) string {
	return strings.Join([]string{id, first, last, dept, title, status, mgr}, ",")
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// provision creates E1 (Marketing) and E2 (Engineering) in AD through the normal approval path.
func (e *env) provision() {
	e.t.Helper()
	e.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	e.run()
	eq(e.t, "created", e.approveAll(model.ChangeCreate), 2)
}

// ---- tests -----------------------------------------------------------------------

func TestCreateNeedsApprovalThenAppliesWithRoles(t *testing.T) {
	e := newEnv(t)
	e.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)

	run := e.run()
	eq(t, "run status", run.Status, model.RunOK)
	eq(t, "pending", run.Counters["pending"], 2)
	eq(t, "planned_create", run.Counters["planned_create"], 2)
	eq(t, "no AD accounts before approval", len(e.fake.Users), 0)
	for _, c := range e.fake.CallLog() {
		if strings.HasPrefix(c, "create") {
			t.Fatalf("nothing may be written before approval: %v", e.fake.CallLog())
		}
	}

	pend := e.changes(model.StatusPending, model.ChangeCreate)
	eq(t, "pending changes", len(pend), 2)
	raw, _ := json.Marshal(pend)
	if strings.Contains(strings.ToLower(string(raw)), "password") {
		t.Fatal("change payloads must never carry passwords")
	}

	eq(t, "approved+applied", e.approveAll(model.ChangeCreate), 2)

	jane := e.user("E1")
	eq(t, "sam", jane.Sam, "jdoe")
	eq(t, "upn", jane.UPN, "jane.doe@corp.example.com")
	eq(t, "enabled", jane.Enabled, true)
	eq(t, "dn", jane.DN, "CN=Jane Doe,"+baseOU)
	eq(t, "givenName", jane.Attrs["givenName"], "Jane")
	eq(t, "displayName", jane.Attrs["displayName"], "Jane Doe")
	eq(t, "role attribute", jane.Attrs["extensionAttribute1"], "MKT")
	eq(t, "baseline group", hasGroup(jane, gAll), true)
	eq(t, "marketing group", hasGroup(jane, gMkt), true)

	bob := e.user("E2")
	eq(t, "bob baseline group", hasGroup(bob, gAll), true)
	eq(t, "bob must NOT get marketing group", hasGroup(bob, gMkt), false)
	eq(t, "bob has no marketing attribute", bob.Attrs["extensionAttribute1"], "")

	id := e.identity("E1")
	eq(t, "identity state", id.State, model.StateLinked)
	eq(t, "identity guid", id.ADGUID, jane.GUID)
	eq(t, "applied tracks pushed attrs", id.Applied["title"], "Designer")

	if len(e.fake.Passwords) != 2 || len(e.fake.Passwords[0]) < 20 || e.fake.Passwords[0] == e.fake.Passwords[1] {
		t.Errorf("each account needs its own strong generated password: %v", len(e.fake.Passwords))
	}
	// no password anywhere in the database-visible audit trail
	audit, _, _ := e.st.ListAudit(e.ctx, store.AuditFilter{}, store.Page{Limit: 500})
	auditJSON, _ := json.Marshal(audit)
	for _, pw := range e.fake.Passwords {
		if strings.Contains(string(auditJSON), pw) {
			t.Fatal("password leaked into the audit log")
		}
	}
	v, err := e.st.VerifyAudit(e.ctx)
	e.must(err)
	eq(t, "audit chain intact", v.OK, true)
	approvals, _, _ := e.st.ListAudit(e.ctx, store.AuditFilter{Action: "change.approve"}, store.Page{})
	if len(approvals) != 2 || approvals[0].Actor != "alice" {
		t.Errorf("approvals must be attributed to the approver: %+v", approvals)
	}

	// Second run is a no-op: the system converged.
	run = e.run()
	eq(t, "converged: planned_create", run.Counters["planned_create"], 0)
	eq(t, "converged: planned_update", run.Counters["planned_update"], 0)
	eq(t, "converged: unchanged", run.Counters["unchanged"], 2)
}

func TestAutoApprovalAppliesWithoutHuman(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) {
		s.Approval = model.ApprovalSettings{Create: "auto", Update: "auto", Disable: "auto", Enable: "auto", Delete: "auto"}
	})
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))

	run := e.run()
	eq(t, "auto_approved", run.Counters["auto_approved"], 1)
	eq(t, "applied in the same run", run.Counters["applied"], 1)
	eq(t, "account exists", e.user("E1").Sam, "jdoe")
	c := e.changes(model.StatusApplied, model.ChangeCreate)[0]
	eq(t, "decided by", c.DecidedBy, "system:auto")
}

func TestMixedApprovalPolicyPerChangeType(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.tweak(func(s *model.Settings) { s.Approval.Update = "auto" }) // creates stay manual
	e.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Senior Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
		row("E3", "Cy", "Young", "Engineering", "Intern", "A", ""),
	)
	run := e.run()
	eq(t, "update auto-applied", e.user("E1").Attrs["title"], "Senior Designer")
	eq(t, "applied", run.Counters["applied"], 1)
	eq(t, "create waits for a human", run.Counters["pending"], 1)
	eq(t, "E3 not created", len(e.fake.Users), 2)
}

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
	for _, c := range e.fake.CallLog() {
		if strings.HasPrefix(c, "create") {
			t.Fatalf("dry-run must not call create: %v", e.fake.CallLog())
		}
	}
	sim := e.changes(model.StatusSimulated, "")
	eq(t, "simulated rows", len(sim), 1)
	if _, err := e.eng.Approve(e.ctx, sim[0].ID, "alice", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("simulated changes cannot be approved: %v", err)
	}

	run = e.run()
	eq(t, "re-run adds nothing", run.Counters["simulated"], 0)
	eq(t, "already queued", run.Counters["already_queued"], 1)

	e.tweak(func(s *model.Settings) { s.Engine.DryRun = false })
	run = e.run()
	eq(t, "live run creates real change", run.Counters["auto_approved"], 1)
	eq(t, "and applies it", run.Counters["applied"], 1)
	eq(t, "simulated superseded", run.Counters["superseded"], 1)
	eq(t, "account exists now", e.user("E1").Sam, "jdoe")
}

func TestExecutorDoesNothingInDryRunEvenIfApproved(t *testing.T) {
	e := newEnv(t)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	c := e.changes(model.StatusPending, "")[0]
	_, err := e.eng.Approve(e.ctx, c.ID, "alice", "")
	e.must(err)
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = true })
	n, err := e.eng.Execute(e.ctx)
	e.must(err)
	eq(t, "executed", n, 0)
	eq(t, "still approved", e.changes(model.StatusApproved, "")[0].ID, c.ID)
}

func TestAttributeModes(t *testing.T) {
	e := newEnv(t)
	e.provision()

	// sync: source change is pushed.
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Senior Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "one update", run.Counters["planned_update"], 1)
	u := e.changes(model.StatusPending, model.ChangeUpdate)[0]
	eq(t, "diff from", u.Payload.Set["title"].From, "Designer")
	eq(t, "diff to", u.Payload.Set["title"].To, "Senior Designer")
	eq(t, "diff mode", u.Payload.Set["title"].Mode, model.ModeSync)
	eq(t, "pending, not applied", e.user("E1").Attrs["title"], "Designer")
	e.approveAll(model.ChangeUpdate)
	eq(t, "title pushed", e.user("E1").Attrs["title"], "Senior Designer")

	// sync: a manual AD edit survives while the source is unchanged...
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Attrs["title"] = "Manual Title" })
	run = e.run()
	eq(t, "manual edit left alone", run.Counters["planned_update"], 0)
	// ...and is overwritten when the source changes again.
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Principal Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run = e.run()
	eq(t, "source change wins", run.Counters["planned_update"], 1)
	u = e.changes(model.StatusPending, model.ChangeUpdate)[0]
	eq(t, "from manual value", u.Payload.Set["title"].From, "Manual Title")
	e.approveAll(model.ChangeUpdate)

	// immutable: drift is corrected even though the source did not change.
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Attrs["displayName"] = "Hacked" })
	run = e.run()
	eq(t, "immutable drift detected", run.Counters["planned_update"], 1)
	u = e.changes(model.StatusPending, model.ChangeUpdate)[0]
	eq(t, "restores source value", u.Payload.Set["displayName"].To, "Jane Doe")
	eq(t, "mode recorded", u.Payload.Set["displayName"].Mode, model.ModeImmutable)
	e.approveAll(model.ChangeUpdate)
	eq(t, "displayName restored", e.user("E1").Attrs["displayName"], "Jane Doe")

	// create_only: later source changes never touch it.
	eq(t, "description set at creation", e.user("E1").Attrs["description"], "Designer")
	run = e.run()
	eq(t, "create_only ignored", run.Counters["planned_update"], 0)
	eq(t, "description untouched", e.user("E1").Attrs["description"], "Designer")
}

func TestOnEmptyClear(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	u := e.changes(model.StatusPending, model.ChangeUpdate)[0]
	eq(t, "clear list", strings.Join(u.Payload.Clear, ","), "title")
	e.approveAll(model.ChangeUpdate)
	if _, has := e.user("E1").Attrs["title"]; has {
		t.Error("title should have been cleared")
	}
}

func TestRoleChangeAddsAndRemovesManagedGroups(t *testing.T) {
	e := newEnv(t)
	e.provision()
	// an unmanaged group the service must never touch
	other := "CN=Unmanaged,OU=Groups,DC=corp,DC=example,DC=com"
	e.fake.AddGroup("Unmanaged", other)
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.MemberOf = append(u.MemberOf, other) })

	e.writeCSV(row("E1", "Jane", "Doe", "Engineering", "Designer", "A", ""), row("E2", "Bob", "Smith", "Marketing", "Developer", "A", ""))
	e.run()
	e.approveAll(model.ChangeUpdate)

	jane, bob := e.user("E1"), e.user("E2")
	eq(t, "jane lost marketing group", hasGroup(jane, gMkt), false)
	eq(t, "jane kept baseline", hasGroup(jane, gAll), true)
	eq(t, "unmanaged group preserved", hasGroup(jane, other), true)
	eq(t, "bob gained marketing group", hasGroup(bob, gMkt), true)
	eq(t, "bob gained the marketing attribute", bob.Attrs["extensionAttribute1"], "MKT")

	e.tweak(func(s *model.Settings) { s.Engine.RemoveStaleGroups = false })
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	e.approveAll(model.ChangeUpdate)
	eq(t, "with remove_stale_groups=false bob keeps marketing", hasGroup(e.user("E2"), gMkt), true)
}

func TestDisableThenDeleteLifecycle(t *testing.T) {
	e := newEnv(t)
	e.provision()

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "disable proposed", run.Counters["planned_disable"], 1)
	eq(t, "still enabled until approved", e.user("E1").Enabled, true)

	eq(t, "disable applied", e.approveAll(model.ChangeDisable), 1)
	jane := e.user("E1")
	eq(t, "disabled", jane.Enabled, false)
	eq(t, "moved to disabled OU", strings.HasSuffix(jane.DN, ","+disabledOU), true)
	eq(t, "groups stripped", len(jane.MemberOf), 0)
	eq(t, "description stamped", strings.HasPrefix(jane.Attrs["description"], "Disabled by IDLifecycle on 2026-03-01"), true)
	eq(t, "identity state", e.identity("E1").State, model.StateDisabled)

	run = e.run()
	eq(t, "no repeat disable", run.Counters["planned_disable"], 0)
	eq(t, "no delete before retention", run.Counters["planned_delete"], 0)

	e.clock = e.clock.Add(31 * 24 * time.Hour)
	run = e.run()
	eq(t, "delete proposed after retention", run.Counters["planned_delete"], 1)
	eq(t, "account still there", e.user("E1").Enabled, false)
	eq(t, "delete applied", e.approveAll(model.ChangeDelete), 1)
	if _, ok := e.fake.ByExternal("E1"); ok {
		t.Fatal("account should be deleted")
	}
	eq(t, "identity state", e.identity("E1").State, model.StateDeleted)
	_, ok := e.fake.ByExternal("E2")
	eq(t, "bystander untouched", ok, true)

	// quiet afterwards
	run = e.run()
	eq(t, "nothing more to do", run.Counters["planned_delete"]+run.Counters["planned_create"]+run.Counters["planned_disable"], 0)

	// rehire: active again -> a brand new account is proposed
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run = e.run()
	eq(t, "re-create on return", run.Counters["planned_create"], 1)
}

func TestRehireReenablesAndMovesBack(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	e.approveAll(model.ChangeDisable)

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "enable proposed", run.Counters["planned_enable"], 1)
	e.approveAll(model.ChangeEnable)
	jane := e.user("E1")
	eq(t, "enabled", jane.Enabled, true)
	eq(t, "moved back to base OU", strings.HasSuffix(jane.DN, ","+baseOU), true)
	eq(t, "groups restored", hasGroup(jane, gAll) && hasGroup(jane, gMkt), true)
	eq(t, "identity relinked", e.identity("E1").State, model.StateLinked)
}

func TestMissingFromFeedTerminatesAfterGrace(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.src.Options.MissingMeansTerminated, e.src.Options.MissingGraceRuns = true, 1
	e.must(e.st.UpdateSource(e.ctx, e.src))

	e.writeCSV(row("E2", "Bob", "Smith", "Engineering", "Developer", "A", "")) // E1 vanished
	run := e.run()
	eq(t, "first absence is within grace", run.Counters["planned_disable"], 0)
	run = e.run()
	eq(t, "second absence terminates", run.Counters["planned_disable"], 1)
	eq(t, "terminated_by_absence", run.Counters["terminated_by_absence"], 1)

	// coming back resets the counter and cancels the proposal
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run = e.run()
	eq(t, "obsolete proposal superseded", run.Counters["superseded"], 1)
	eq(t, "nothing pending", len(e.changes(model.StatusPending, "")), 0)
}

func TestEmptyFeedIsNeverTreatedAsEveryoneLeft(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.src.Options.MissingMeansTerminated = true
	e.must(e.st.UpdateSource(e.ctx, e.src))

	e.writeCSV() // header only
	run := e.run()
	eq(t, "run flagged", run.Status, model.RunError)
	if !strings.Contains(run.Error, "zero records") {
		t.Errorf("error should explain: %q", run.Error)
	}
	eq(t, "no disables", run.Counters["planned_disable"], 0)
	eq(t, "identities untouched", e.identity("E1").Active, true)
	eq(t, "no changes", len(e.changes(model.StatusPending, "")), 0)
}

func TestBulkDestructiveGuard(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.Approval.Create = "auto" })
	var rows, gone []string
	for _, id := range []string{"E1", "E2", "E3"} {
		rows = append(rows, row(id, "Pat", "Person"+id, "Engineering", "Dev", "A", ""))
		gone = append(gone, row(id, "Pat", "Person"+id, "Engineering", "Dev", "T", ""))
	}
	e.writeCSV(rows...)
	e.run()
	eq(t, "3 accounts", len(e.fake.Users), 3)

	e.tweak(func(s *model.Settings) { s.Approval.Disable, s.Engine.MaxDestructivePerRun = "auto", 2 })
	e.writeCSV(gone...)
	run := e.run()
	eq(t, "guard tripped", run.Counters["guard_tripped"], 1)
	eq(t, "all held", run.Counters["held"], 3)
	eq(t, "nothing disabled", run.Counters["applied"], 0)
	pend := e.changes(model.StatusPending, model.ChangeDisable)
	eq(t, "pending for a human", len(pend), 3)
	if !strings.HasPrefix(pend[0].Summary[0], "HELD") {
		t.Errorf("summary should explain the hold: %v", pend[0].Summary)
	}

	// within the limit, auto works.
	e2 := newEnv(t)
	e2.tweak(func(s *model.Settings) {
		s.Approval = model.ApprovalSettings{Create: "auto", Update: "auto", Disable: "auto", Enable: "auto", Delete: "auto"}
		s.Engine.MaxDestructivePerRun = 2
	})
	e2.writeCSV(rows[:2]...)
	e2.run()
	e2.writeCSV(gone[:2]...)
	run = e2.run()
	eq(t, "2 disables within limit are auto-applied", run.Counters["applied"], 2)
	eq(t, "disabled", e2.user("E1").Enabled, false)
}

func TestSourceFailureIsIsolated(t *testing.T) {
	e := newEnv(t)
	bad := model.Source{ID: "src2", Name: "Broken", Type: "csv", Enabled: true, CreatedAt: e.clock, UpdatedAt: e.clock,
		Options: model.SourceOptions{ExternalIDField: "EmployeeID"}, Config: map[string]any{"path": filepath.Join(t.TempDir(), "missing.csv")}}
	e.must(e.st.CreateSource(e.ctx, bad))
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))

	run := e.run()
	eq(t, "run flagged as error", run.Status, model.RunError)
	eq(t, "source_errors", run.Counters["source_errors"], 1)
	eq(t, "healthy source still planned", run.Counters["planned_create"], 1)
	s, _ := e.st.GetSource(e.ctx, "src2")
	eq(t, "source status recorded", s.LastStatus, "error")
}

func TestPendingChangesAreSupersededAndObsoleted(t *testing.T) {
	e := newEnv(t)
	e.provision()

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Senior Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	first := e.changes(model.StatusPending, model.ChangeUpdate)[0]

	e.run() // identical plan: kept, not duplicated
	eq(t, "no duplicate pending", len(e.changes(model.StatusPending, model.ChangeUpdate)), 1)
	eq(t, "same row kept", e.changes(model.StatusPending, model.ChangeUpdate)[0].ID, first.ID)

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Lead Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	cur := e.changes(model.StatusPending, model.ChangeUpdate)
	eq(t, "one open update", len(cur), 1)
	eq(t, "newer plan", cur[0].Payload.Set["title"].To, "Lead Designer")
	eq(t, "old one superseded", e.changes(model.StatusSuperseded, "")[0].ID, first.ID)

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", "")) // reverted
	e.run()
	eq(t, "obsolete proposal withdrawn", len(e.changes(model.StatusPending, "")), 0)
}

func TestApprovalStateMachine(t *testing.T) {
	e := newEnv(t)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	list := e.changes(model.StatusPending, "")
	a, b := list[0], list[1]

	if _, err := e.eng.Reject(e.ctx, a.ID, "alice", "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reject needs a note: %v", err)
	}
	if _, err := e.eng.Reject(e.ctx, a.ID, "alice", "not hired yet"); err != nil {
		t.Fatal(err)
	}
	got, _ := e.st.GetChange(e.ctx, a.ID)
	eq(t, "rejected", got.Status, model.StatusRejected)
	eq(t, "note kept", got.DecisionNote, "not hired yet")
	if _, err := e.eng.Approve(e.ctx, a.ID, "bob", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a rejected change cannot be approved: %v", err)
	}

	if _, err := e.eng.Approve(e.ctx, b.ID, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.Approve(e.ctx, b.ID, "carol", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("double approval must conflict: %v", err)
	}
	if _, err := e.eng.Retry(e.ctx, b.ID, "alice", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("only failed changes can be retried: %v", err)
	}
	if _, err := e.eng.Approve(e.ctx, "nope", "alice", ""); !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrConflict) {
		t.Fatalf("unknown id: %v", err)
	}

	// rejected changes are not re-proposed unchanged on every run... but a fresh plan may return as pending
	e.eng.Execute(e.ctx)
	run := e.run()
	eq(t, "rejection does not block a new proposal", run.Counters["planned_create"], 1)
}

func TestExecutionFailureAndRetry(t *testing.T) {
	e := newEnv(t)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	e.fake.FailExternal["E1"] = "directory said no"
	eq(t, "attempted", e.approveAll(model.ChangeCreate), 1)

	c := e.changes(model.StatusFailed, "")[0]
	eq(t, "error recorded", c.Error, "directory said no")
	eq(t, "attempts", c.Attempts, 1)
	eq(t, "identity not linked", e.identity("E1").ADGUID, "")

	delete(e.fake.FailExternal, "E1")
	if _, err := e.eng.Retry(e.ctx, c.ID, "alice", "fixed"); err != nil {
		t.Fatal(err)
	}
	_, err := e.eng.Execute(e.ctx)
	e.must(err)
	eq(t, "applied after retry", e.changes(model.StatusApplied, "")[0].ID, c.ID)
	eq(t, "attempts counted", e.changes(model.StatusApplied, "")[0].Attempts, 2)
	eq(t, "account exists", e.user("E1").Sam, "jdoe")
}

func TestADOutageFailsBatchWithoutLosingWork(t *testing.T) {
	e := newEnv(t)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	e.fake.ActionErr = ad.ErrFakeDown
	e.approveAll(model.ChangeCreate)
	c := e.changes(model.StatusFailed, "")
	eq(t, "failed", len(c), 1)
	if !strings.Contains(c[0].Error, "unreachable") {
		t.Errorf("error should say why: %q", c[0].Error)
	}
	e.fake.ActionErr = nil
	_, err := e.eng.Retry(e.ctx, c[0].ID, "alice", "")
	e.must(err)
	_, err = e.eng.Execute(e.ctx)
	e.must(err)
	eq(t, "recovered", e.user("E1").Sam, "jdoe")
}

func TestCreateIsIdempotentWhenAccountAlreadyExists(t *testing.T) {
	e := newEnv(t)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	// Someone/something created the account between planning and approval (e.g. crash after create).
	e.fake.AddUser(ad.FakeUser{DN: "CN=Jane Doe," + baseOU, Sam: "jdoe", UPN: "jane.doe@corp.example.com", External: "E1", Enabled: true})
	e.approveAll(model.ChangeCreate)
	eq(t, "single account", len(e.fake.Users), 1)
	eq(t, "change completed", len(e.changes(model.StatusApplied, model.ChangeCreate)), 1)
	eq(t, "linked", e.identity("E1").State, model.StateLinked)
}

func TestRecoverMarksInterruptedWork(t *testing.T) {
	e := newEnv(t)
	e.must(e.st.CreateRun(e.ctx, model.Run{ID: "r1", Trigger: "x", StartedAt: e.clock, Status: model.RunRunning, Counters: map[string]int{}}))
	e.must(e.st.InsertChange(e.ctx, model.Change{ID: "c1", RunID: "r1", IdentityID: "i", Type: model.ChangeUpdate, Status: model.StatusApplying, CreatedAt: e.clock}))
	e.must(e.eng.Recover(e.ctx))
	r, _ := e.st.GetRun(e.ctx, "r1")
	eq(t, "run aborted", r.Status, model.RunAborted)
	c, _ := e.st.GetChange(e.ctx, "c1")
	eq(t, "change failed for manual verification", c.Status, model.StatusFailed)
}

func TestADSnapshotSafetyGuards(t *testing.T) {
	e := newEnv(t)
	e.provision()

	e.fake.Reset() // directory suddenly looks empty (wrong search base, lost permissions...)
	run := e.run()
	eq(t, "aborted", run.Status, model.RunError)
	if !strings.Contains(run.Error, "no users") {
		t.Errorf("error: %q", run.Error)
	}
	eq(t, "nothing proposed", len(e.changes(model.StatusPending, "")), 0)

	e.fake.SnapshotErr = ad.ErrFakeDown
	run = e.run()
	eq(t, "snapshot failure aborts", run.Status, model.RunError)
	eq(t, "nothing proposed", len(e.changes(model.StatusPending, "")), 0)
}

func TestDuplicateADAccountsAreReportedNotGuessed(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.fake.AddUser(ad.FakeUser{DN: "CN=Imposter," + baseOU, Sam: "imposter", UPN: "imposter@corp.example.com", External: "E1", Enabled: true})
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "New Title", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "plan error", run.Counters["plan_errors"], 1)
	if !strings.Contains(run.Error, "AD accounts share") {
		t.Errorf("error: %q", run.Error)
	}
	eq(t, "no change for the ambiguous identity", run.Counters["planned_update"], 0)
}

func TestNameCollisionPicksNextCandidate(t *testing.T) {
	e := newEnv(t)
	e.fake.AddUser(ad.FakeUser{DN: "CN=John Doe," + baseOU, Sam: "jdoe", UPN: "john.doe@corp.example.com", External: "E0", Enabled: true})
	e.writeCSV(row("E0", "John", "Doe", "Engineering", "Dev", "A", ""), row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	e.approveAll(model.ChangeCreate)
	jane := e.user("E1")
	eq(t, "second candidate", jane.Sam, "jdoe2")
	eq(t, "upn follows the same candidate index as the sam", jane.UPN, "jane.doe2@corp.example.com")
}

func TestUnknownRoleGroupWarnsButOtherGroupsStillApply(t *testing.T) {
	e := newEnv(t)
	e.must(e.st.CreateRole(e.ctx, model.RoleRule{ID: "r-bad", Name: "Ghost", Enabled: true, Priority: 5, Match: "all",
		Grants: model.Grants{Groups: []string{"GG-Does-Not-Exist"}}, CreatedAt: e.clock, UpdatedAt: e.clock}))
	e.writeCSV(row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	if run.Counters["warnings"] == 0 {
		t.Error("missing group must be surfaced as a warning")
	}
	e.approveAll(model.ChangeCreate)
	eq(t, "baseline group still applied", hasGroup(e.user("E2"), gAll), true)
}

func TestUserRefAttributeResolvesManagerDN(t *testing.T) {
	e := newEnv(t)
	e.must(e.st.CreateMapping(e.ctx, model.Mapping{ID: "mgr", Name: "manager", TargetAttr: "manager", Template: "{{ .ManagerID }}",
		Mode: model.ModeSync, OnEmpty: "skip", ValueType: "user_ref", Enabled: true, SortOrder: 99, CreatedAt: e.clock, UpdatedAt: e.clock}))
	e.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Director", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", "E1"),
	)
	run := e.run()
	if run.Counters["warnings"] == 0 {
		t.Error("manager without an AD account yet should warn, not fail")
	}
	e.approveAll(model.ChangeCreate)
	eq(t, "bob created without manager first pass", e.user("E2").Attrs["manager"], "")

	run = e.run() // second pass: manager now exists
	eq(t, "manager link proposed", run.Counters["planned_update"], 1)
	e.approveAll(model.ChangeUpdate)
	eq(t, "manager is Jane's DN", e.user("E2").Attrs["manager"], e.user("E1").DN)
}

func TestSourceSecretsAreDecryptedForFetch(t *testing.T) {
	// A source with an encrypted secret must be usable by the engine (CSV has none; this guards Build's path).
	e := newEnv(t)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	run := e.run()
	eq(t, "ok", run.Status, model.RunOK)
}

func TestConcurrentRunsAreRejected(t *testing.T) {
	e := newEnv(t)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.eng.runMu.Lock()
	_, _, err := e.eng.Start(e.ctx, "test", "tester")
	e.eng.runMu.Unlock()
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("second concurrent run must be refused: %v", err)
	}
}

func TestMisconfigurationFailsFastAndLoudly(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.AD.SearchBases = nil })
	run := e.run()
	eq(t, "status", run.Status, model.RunError)
	if !strings.Contains(run.Error, "search_bases") {
		t.Errorf("error: %q", run.Error)
	}
	e.tweak(func(s *model.Settings) { s.AD.SearchBases = []string{baseOU}; s.AD.UPNSuffix = "" })
	run = e.run()
	if !strings.Contains(run.Error, "upn_suffix") {
		t.Errorf("live mode needs a UPN suffix: %q", run.Error)
	}
	if len(e.fake.CallLog()) != 0 {
		t.Errorf("misconfigured runs must not touch AD: %v", e.fake.CallLog())
	}
}
