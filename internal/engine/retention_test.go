package engine

import (
	"os"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

const legalOU = "OU=Legal Hold,DC=corp,DC=example,DC=com"

func (e *env) policy(p model.RetentionPolicy) model.RetentionPolicy {
	e.t.Helper()
	if p.ID == "" {
		p.ID = "pol-" + strings.ToLower(strings.ReplaceAll(p.Name, " ", "-"))
	}
	p.Enabled = true
	NormalizeRetentionPolicy(&p)
	e.must(ValidateRetentionPolicy(p))
	p.CreatedAt, p.UpdatedAt = e.clock, e.clock
	e.must(e.st.CreateRetentionPolicy(e.ctx, p))
	return p
}

func (e *env) planned(typ, ext string) model.Change {
	e.t.Helper()
	for _, st := range []string{model.StatusPending, model.StatusApproved} {
		for _, c := range e.changes(st, typ) {
			if c.ExternalID == ext {
				return c
			}
		}
	}
	e.t.Fatalf("no open %s change for %s", typ, ext)
	return model.Change{}
}

func (e *env) noOpen(typ, ext string) {
	e.t.Helper()
	for _, st := range []string{model.StatusPending, model.StatusApproved} {
		for _, c := range e.changes(st, typ) {
			if c.ExternalID == ext {
				e.t.Fatalf("unexpected %s for %s: %v", typ, ext, c.Summary)
			}
		}
	}
}

func (e *env) terminate(ids ...string) {
	e.t.Helper()
	all := map[string]bool{}
	for _, id := range ids {
		all[id] = true
	}
	st := func(id string) string {
		if all[id] {
			return "T"
		}
		return "A"
	}
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", st("E1"), ""), row("E2", "Bob", "Smith", "Engineering", "Developer", st("E2"), ""))
}

func (e *env) retention(ext string) model.IdentityRetention {
	e.t.Helper()
	r, err := e.eng.IdentityRetentionInfo(e.ctx, e.identity(ext))
	e.must(err)
	return r
}

func TestRetentionPoliciesDecideOUKeptGroupsAndDeleteDate(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.policy(model.RetentionPolicy{Name: "Executives", Priority: 10, DeleteAfterDays: 365, DisabledOU: legalOU, ExtraKeepGroups: []string{"GG-Mkt"},
		Conditions: []model.Condition{{Attr: RetFieldMemberOf, Op: "contains", Value: "gg-mkt"}}})
	never := e.policy(model.RetentionPolicy{Name: "Engineers never", Priority: 20, DeleteAfterDays: 0,
		Conditions: []model.Condition{{Attr: "Department", Op: "eq", Value: "Engineering"}}})

	e.terminate("E1", "E2")
	e.run()
	d1 := e.planned(model.ChangeDisable, "E1")
	eq(t, "E1 goes to the policy's OU", d1.Payload.MoveTo, legalOU)
	for _, g := range d1.Payload.RemoveGroups {
		if strings.EqualFold(g, gMkt) {
			t.Fatalf("GG-Mkt is kept by the policy: %v", d1.Payload.RemoveGroups)
		}
	}
	if !strings.Contains(strings.Join(d1.Summary, "\n"), "Retention policy: Executives (matched rule; deleted 365 days after the disable)") {
		t.Errorf("summary names the policy: %v", d1.Summary)
	}
	d2 := e.planned(model.ChangeDisable, "E2")
	eq(t, "E2 goes to the default disabled OU", d2.Payload.MoveTo, disabledOU)
	eq(t, "approved", e.approveAll(model.ChangeDisable), 2)
	eq(t, "E1 still in GG-Mkt", hasGroup(e.user("E1"), gMkt), true)
	eq(t, "E1 out of GG-All", hasGroup(e.user("E1"), gAll), false)

	e.run() // records the disable date
	r1 := e.retention("E1")
	eq(t, "E1 policy", r1.PolicyName, "Executives")
	eq(t, "E1 why", r1.Reason, model.RetentionReasonRule)
	if r1.DeleteDue == nil || r1.DeleteDue.Sub(*e.identity("E1").DisabledAt) != 365*24*time.Hour {
		t.Fatalf("E1 delete due: %+v", r1.DeleteDue)
	}
	r2 := e.retention("E2")
	eq(t, "E2 policy", r2.PolicyID, never.ID)
	if r2.DeleteDue != nil {
		t.Fatalf("never-delete has no due date: %v", r2.DeleteDue)
	}

	// The default (30 days) would delete both now; the policies do not.
	e.clock = e.clock.Add(40 * 24 * time.Hour)
	run := e.run()
	eq(t, "no deletes", run.Counters["planned_delete"], 0)

	e.clock = e.clock.Add(330 * 24 * time.Hour)
	e.run()
	del := e.planned(model.ChangeDelete, "E1")
	if !strings.Contains(del.Summary[len(del.Summary)-1], "policy Executives: 365 days") {
		t.Errorf("delete summary: %v", del.Summary)
	}
	e.noOpen(model.ChangeDelete, "E2")
}

func TestRetentionAssignmentWinsAndDeletingAPolicyClearsIt(t *testing.T) {
	e := newEnv(t)
	e.provision()
	keep := e.policy(model.RetentionPolicy{Name: "Keep a year", Priority: 10, DeleteAfterDays: 365,
		Conditions: []model.Condition{{Attr: "Department", Op: "eq", Value: "Nobody"}}})

	if _, err := e.eng.SetIdentityRetention(e.ctx, e.identity("E2").ID, keep.ID, "short", "alice"); err == nil {
		t.Fatal("a reason of under 10 characters must be refused")
	}
	if _, err := e.eng.SetIdentityRetention(e.ctx, e.identity("E2").ID, "nope", "legal asked for it", "alice"); err == nil {
		t.Fatal("an unknown policy must be refused")
	}
	_, err := e.eng.SetIdentityRetention(e.ctx, e.identity("E2").ID, keep.ID, "legal asked for it", "alice")
	e.must(err)
	r := e.retention("E2")
	eq(t, "shown at once", r.PolicyName, "Keep a year")
	eq(t, "assigned", r.Reason, model.RetentionReasonAssigned)
	eq(t, "by", r.AssignedBy, "alice")

	e.terminate("E1", "E2")
	e.run()
	e.approveAll(model.ChangeDisable)
	e.run()
	eq(t, "resolved by the sync", e.retention("E2").PolicyID, keep.ID)
	e.clock = e.clock.Add(31 * 24 * time.Hour)
	e.run()
	e.planned(model.ChangeDelete, "E1") // default policy
	e.noOpen(model.ChangeDelete, "E2")

	e.must(e.st.DeleteRetentionPolicy(e.ctx, keep.ID))
	eq(t, "assignment cleared", e.identity("E2").RetentionPolicyID, "")
	e.run()
	e.planned(model.ChangeDelete, "E2")

	hist, _, err := e.st.ListAudit(e.ctx, store.AuditFilter{EntityType: "identity", EntityID: e.identity("E2").ID, Action: ActRetentionSet}, store.Page{Limit: 10})
	e.must(err)
	eq(t, "assignment in the history", len(hist), 1)
}

func TestRetentionManualApprovalAndHistoryOnlyOnChange(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.Approval.Delete = "auto"; s.Approval.Disable = "auto" })
	e.provision()
	p := e.policy(model.RetentionPolicy{Name: "Careful", Priority: 10, DeleteAfterDays: 10, Approval: model.RetentionApprovalManual,
		Conditions: []model.Condition{{Attr: "Department", Op: "eq", Value: "Marketing"}}})
	e.terminate("E1", "E2")
	e.run() // disables auto-applied
	e.run()
	e.clock = e.clock.Add(31 * 24 * time.Hour)
	e.run()
	c := e.planned(model.ChangeDelete, "E1")
	eq(t, "manual policy: waits for a person", c.Status, model.StatusPending)
	if !strings.Contains(c.Summary[0], `retention policy "Careful" requires a person`) {
		t.Errorf("hold line: %v", c.Summary)
	}
	eq(t, "default policy: auto", e.changes(model.StatusApplied, model.ChangeDelete)[0].ExternalID, "E2")

	id := e.identity("E1").ID
	count := func() int {
		list, _, err := e.st.ListAudit(e.ctx, store.AuditFilter{EntityType: "identity", EntityID: id, Action: ActRetentionChanged}, store.Page{Limit: 10})
		e.must(err)
		return len(list)
	}
	before := count() // the first sync after the policy was created moved E1 from Default to it
	e.run()
	eq(t, "no history row while nothing changes", count(), before)
	p.Conditions = []model.Condition{{Attr: "Department", Op: "eq", Value: "Sales"}}
	e.must(e.st.UpdateRetentionPolicy(e.ctx, p))
	e.run()
	eq(t, "a history row when the policy changes", count(), before+1)
	eq(t, "back to default", e.retention("E1").PolicyID, model.RetentionDefaultID)
}

func TestManualPinStillStopsADeleteUnderAPolicy(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.terminate("E2")
	e.run()
	e.approveAll(model.ChangeDisable)
	e.run()
	e.must(e.st.InsertHold(e.ctx, model.Hold{ID: "h1", IdentityID: e.identity("E2").ID, Kind: model.HoldState, Reason: "legal hold for audit", CreatedBy: "alice", CreatedAt: e.clock}))
	e.clock = e.clock.Add(31 * 24 * time.Hour)
	e.run()
	e.noOpen(model.ChangeDelete, "E2")
	items, err := e.eng.RetentionUpcoming(e.ctx, 30)
	e.must(err)
	if len(items) != 1 || items[0].Kind != "delete" || !items[0].ManualHold {
		t.Fatalf("upcoming shows the overdue delete and the pin: %+v", items)
	}
}

func (e *env) writeDatedCSV(rows ...string) {
	e.t.Helper()
	e.must(os.WriteFile(e.csv, []byte("EmployeeID,FirstName,LastName,Department,Title,Status,EndDate\n"+strings.Join(rows, "\n")+"\n"), 0o600))
}

func TestTerminationDateDisablesTheDayAfterAndRehires(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.Schedule.Timezone = "America/Chicago" })
	e.src.Options.TerminationDateField = "EndDate"
	e.must(e.st.UpdateSource(e.ctx, e.src))
	e.writeDatedCSV("E1,Jane,Doe,Marketing,Designer,A,", "E2,Bob,Smith,Engineering,Developer,A,")
	e.run()
	e.approveAll(model.ChangeCreate)

	// 2026-03-01 09:00 UTC is still March 1 in Chicago: the last working day.
	e.writeDatedCSV("E1,Jane,Doe,Marketing,Designer,A,03/01/2026", "E2,Bob,Smith,Engineering,Developer,A,2026-03-20")
	e.run()
	e.noOpen(model.ChangeDisable, "E1")
	eq(t, "leave date shown", e.retention("E1").LeaveDate, "2026-03-01")
	items, err := e.eng.RetentionUpcoming(e.ctx, 30)
	e.must(err)
	if len(items) != 2 || items[0].Kind != "leaving" || items[0].ExternalID != "E1" || items[1].LeaveDate != "2026-03-20" {
		t.Fatalf("upcoming leavers: %+v", items)
	}

	e.clock = time.Date(2026, 3, 2, 4, 0, 0, 0, time.UTC) // March 1, 22:00 in Chicago
	e.run()
	e.noOpen(model.ChangeDisable, "E1")
	e.clock = time.Date(2026, 3, 2, 7, 0, 0, 0, time.UTC) // March 2, 01:00 in Chicago
	e.run()
	e.planned(model.ChangeDisable, "E1")
	e.noOpen(model.ChangeDisable, "E2")
	e.approveAll(model.ChangeDisable)
	hist, _, err := e.st.ListAudit(e.ctx, store.AuditFilter{EntityType: "identity", EntityID: e.identity("E1").ID, Action: ActInactive}, store.Page{Limit: 5})
	e.must(err)
	if len(hist) != 1 || hist[0].Detail["reason"] != "termination_date" || hist[0].Detail["termination_date"] != "2026-03-01" {
		t.Fatalf("inactive row: %+v", hist)
	}

	// The date moved into the future (rehire): active again.
	e.writeDatedCSV("E1,Jane,Doe,Marketing,Designer,A,2027-01-31", "E2,Bob,Smith,Engineering,Developer,A,garbage")
	run := e.run()
	e.planned(model.ChangeEnable, "E1")
	if run.Counters["warnings"] == 0 {
		t.Errorf("an unreadable date is a warning: %v", run.Counters)
	}
	e.noOpen(model.ChangeDisable, "E2")
}

func TestTerminationAfterDaysAndCustomFormat(t *testing.T) {
	e := newEnv(t)
	e.src.Options.TerminationDateField = "EndDate"
	e.src.Options.TerminationDateFormat = "02.01.2006"
	e.src.Options.TerminationAfterDays = 2
	e.must(e.st.UpdateSource(e.ctx, e.src))
	e.writeDatedCSV("E1,Jane,Doe,Marketing,Designer,A,")
	e.run()
	e.approveAll(model.ChangeCreate)
	e.writeDatedCSV("E1,Jane,Doe,Marketing,Designer,A,27.02.2026")
	e.run() // March 1: 27.02 + 2 days = March 1, not passed yet
	e.noOpen(model.ChangeDisable, "E1")
	e.clock = e.clock.Add(24 * time.Hour)
	e.run()
	e.planned(model.ChangeDisable, "E1")
}

func TestImpactPreviewWithProposedRetentionPolicies(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.terminate("E1", "E2")
	e.run()
	e.approveAll(model.ChangeDisable)
	e.run()
	e.clock = e.clock.Add(31 * 24 * time.Hour)

	proposed := []model.RetentionPolicy{{Name: "Keep marketing", Enabled: true, Priority: 1, DeleteAfterDays: 0,
		Conditions: []model.Condition{{Attr: "Department", Op: "eq", Value: "Marketing"}}}}
	res, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{RetentionPolicies: &proposed, OnlyChanged: true})
	e.must(err)
	eq(t, "one person differs", res.Summary.ChangedPeople, 1)
	eq(t, "E1", res.Items[0].ExternalID, "E1")
	eq(t, "saved: delete", res.Items[0].Baseline.Type, model.ChangeDelete)
	if res.Items[0].Proposed != nil {
		t.Errorf("proposed: nothing, got %+v", res.Items[0].Proposed)
	}

	bad := []model.RetentionPolicy{{Name: "x", Enabled: true, Match: "all", DeleteAfterDays: -1}}
	if _, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{RetentionPolicies: &bad}); err == nil {
		t.Fatal("an invalid proposed policy must be refused")
	}
}

func TestRetentionPolicyValidationAndTest(t *testing.T) {
	for _, p := range []model.RetentionPolicy{
		{Name: "", Match: "all"},
		{Name: "Default", Match: "all"},
		{Name: "x", Match: "some"},
		{Name: "x", Match: "all", DeleteAfterDays: MaxRetentionDays + 1},
		{Name: "x", Match: "all", DisabledOU: "Disabled Users"},
		{Name: "x", Match: "all", ExtraKeepGroups: []string{"entra:a0000000-0000-0000-0000-000000000001"}},
		{Name: "x", Match: "all", Approval: "auto"},
		{Name: "x", Match: "all", Conditions: []model.Condition{{Attr: "Dept", Op: "bogus"}}},
	} {
		if ValidateRetentionPolicy(p) == nil {
			t.Errorf("accepted %+v", p)
		}
	}

	e := newEnv(t)
	e.provision()
	e.run() // caches the AD snapshot for the account's groups
	p := model.RetentionPolicy{Name: "Execs", Match: "all", Conditions: []model.Condition{
		{Attr: RetFieldMemberOf, Op: "eq", Value: "GG-Mkt"}, {Attr: RetFieldOrigin, Op: "eq", Value: "source"}, {Attr: RetFieldSource, Op: "eq", Value: "HR CSV"}}}
	out, err := e.eng.TestRetentionPolicy(e.ctx, p, e.identity("E1").ID, nil)
	e.must(err)
	eq(t, "E1 matches", out.Matched, true)
	out, err = e.eng.TestRetentionPolicy(e.ctx, p, e.identity("E2").ID, nil)
	e.must(err)
	eq(t, "E2 does not", out.Matched, false)
	if !strings.Contains(out.Explain[0].Actual, "GG-All") {
		t.Errorf("explain shows the groups: %+v", out.Explain[0])
	}
	neg := model.RetentionPolicy{Name: "n", Match: "all", Conditions: []model.Condition{{Attr: RetFieldMemberOf, Op: "not_contains", Value: "mkt"}}}
	out, _ = e.eng.TestRetentionPolicy(e.ctx, neg, e.identity("E1").ID, nil)
	eq(t, "not_contains over every group", out.Matched, false)
	out, _ = e.eng.TestRetentionPolicy(e.ctx, model.RetentionPolicy{Name: "r", Match: "all", Conditions: []model.Condition{{Attr: "Department", Op: "eq", Value: "Sales"}}}, "", map[string]string{"Department": "sales"})
	eq(t, "a typed record", out.Matched, true)
}

func TestRetentionPolicyWithoutConditionsOnlyForAssignedPeople(t *testing.T) {
	e := newEnv(t)
	e.provision()
	hold := e.policy(model.RetentionPolicy{Name: "Legal hold", Priority: 1, DeleteAfterDays: 0})
	e.terminate("E1", "E2")
	e.run()
	e.approveAll(model.ChangeDisable)
	_, err := e.eng.SetIdentityRetention(e.ctx, e.identity("E1").ID, hold.ID, "litigation hold #42", "alice")
	e.must(err)
	e.run()
	eq(t, "assigned", e.retention("E1").PolicyID, hold.ID)
	eq(t, "never deleted", e.retention("E1").DeleteAfterDays, 0)
	eq(t, "not everyone", e.retention("E2").PolicyID, model.RetentionDefaultID)
	eq(t, "default days", e.retention("E2").DeleteAfterDays, 30)
}
