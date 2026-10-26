package engine

import (
	"errors"
	"strings"
	"testing"

	"idlifecycle/internal/model"
)

const (
	ouFin   = "OU=Finance,OU=Staff,DC=corp,DC=example,DC=com"
	ouMkt   = "OU=Marketing,OU=Staff,DC=corp,DC=example,DC=com"
	ouSales = "OU=Sales,OU=Staff,DC=corp,DC=example,DC=com"
	ouOther = "OU=Contractors,OU=Staff,DC=corp,DC=example,DC=com"
)

func TestMatchPlacementRule(t *testing.T) {
	rules := []model.PlacementRule{{Match: "Sales*", OU: ouSales}, {Match: " finance ", OU: ouFin}, {Match: "Sales - EMEA", OU: ouOther}}
	cases := map[string]string{"Finance": ouFin, "FINANCE ": ouFin, "Sales - US": ouSales, "Sales - EMEA": ouOther, "sales": ouSales, "Legal": "", "": ""}
	for v, want := range cases {
		got, ok := MatchPlacementRule(rules, v)
		if got != want || ok != (want != "") {
			t.Errorf("%q: got %q %v want %q", v, got, ok, want)
		}
	}
}

func TestResolvePlacementPrecedence(t *testing.T) {
	s := model.DefaultSettings()
	s.AD.DefaultOU = baseOU
	id := model.Identity{Attrs: map[string]string{"department": "Finance"}}
	if p := ResolvePlacement(s, id, ""); p.OU != baseOU || p.Source != "default" {
		t.Errorf("placement off: %+v", p)
	}
	s.Placement.Enabled = true
	s.Placement.Rules = []model.PlacementRule{{Match: "Finance", OU: ouFin}}
	if p := ResolvePlacement(s, id, ""); p.OU != ouFin || p.Source != "rule" || p.Value != "Finance" {
		t.Errorf("rule (field name matched without case): %+v", p)
	}
	if p := ResolvePlacement(s, id, ouOther); p.OU != ouOther || p.Source != "role" {
		t.Errorf("a role rule's OU wins over the department map: %+v", p)
	}
	id.OUOverride, id.OUOverrideValue = ouMkt, "finance"
	if p := ResolvePlacement(s, id, ouOther); p.OU != ouMkt || p.Source != "override" {
		t.Errorf("a person's choice wins while the department is unchanged: %+v", p)
	}
	id.Attrs["department"] = "Legal"
	p := ResolvePlacement(s, id, "")
	if !p.Needs || p.OU != "" || !strings.Contains(p.Reason, `"Legal"`) {
		t.Errorf("department changed and unmapped -> a person must choose: %+v", p)
	}
	s.Placement.Unmapped = model.UnmappedDefault
	if p := ResolvePlacement(s, id, ""); p.Needs || p.OU != baseOU || !strings.Contains(p.Reason, "default OU") {
		t.Errorf("unmapped = default: %+v", p)
	}
	delete(id.Attrs, "department")
	s.Placement.Unmapped = model.UnmappedHold
	if p := ResolvePlacement(s, id, ""); !p.Needs || !strings.Contains(p.Reason, "has no Department") {
		t.Errorf("missing department: %+v", p)
	}
}

func TestPlacementCreateHoldChooseAndMoveOnDepartmentChange(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) {
		s.Approval.Create, s.Approval.Update = "auto", "auto" // a create without an OU must still wait for a person
		s.Placement = model.PlacementSettings{Enabled: true, Field: "Department", Unmapped: model.UnmappedHold, MoveOnChange: true,
			Rules: []model.PlacementRule{{Match: "Marketing", OU: ouMkt}}}
	})
	e.fake.AddOU(ouMkt, ouFin, ouOther)
	e.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""),
		row("E2", "Bob", "Smith", "Finance", "Analyst", "A", ""),
	)
	e.run()
	if _, err := e.eng.Execute(e.ctx); err != nil {
		t.Fatal(err)
	}
	jane := e.user("E1")
	eq(t, "mapped department -> its OU", parentDN(jane.DN), ouMkt)

	pend := e.changes(model.StatusPending, model.ChangeCreate)
	if len(pend) != 1 || pend[0].ExternalID != "E2" || !pend[0].Payload.NeedsOU || pend[0].Payload.OU != "" {
		t.Fatalf("unmapped department must wait for an OU even with auto-approval: %+v", pend)
	}
	if !strings.Contains(strings.Join(pend[0].Summary, "\n"), `no OU is mapped for Department "Finance"`) {
		t.Errorf("summary: %v", pend[0].Summary)
	}
	if _, err := e.eng.Approve(e.ctx, pend[0].ID, "alice", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approving without an OU must be refused: %v", err)
	}

	// Choose the OU (and add the rule for Finance); the next sync keeps the change instead of replacing it.
	c, err := e.eng.SetChangeOU(e.ctx, pend[0].ID, ouFin, "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Payload.OU != ouFin || c.Payload.NeedsOU || !strings.HasPrefix(c.Summary[0], "OU chosen by alice") {
		t.Errorf("after choosing: %+v %v", c.Payload, c.Summary)
	}
	if s := e.settings(); len(s.Placement.Rules) != 2 || s.Placement.Rules[1].Match != "Finance" {
		t.Errorf("rule added: %+v", s.Placement.Rules)
	}
	run := e.run()
	eq(t, "the chosen change is kept", run.Counters["already_queued"], 1)
	eq(t, "nothing superseded", run.Counters["superseded"], 0)
	eq(t, "approve + apply", e.approveAll(model.ChangeCreate), 1)
	bob := e.user("E2")
	eq(t, "created in the chosen OU", parentDN(bob.DN), ouFin)

	// A title change alone moves nobody; a department change moves the account (auto-approved here).
	e.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Lead Designer", "A", ""),
		row("E2", "Bob", "Smith", "Marketing", "Analyst", "A", ""),
	)
	e.run()
	if _, err := e.eng.Execute(e.ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "title change: same OU", parentDN(e.user("E1").DN), ouMkt)
	eq(t, "department change: moved", parentDN(e.user("E2").DN), ouMkt)

	// Someone moves Jane by hand in AD: without a department change she stays where she was put.
	e.fake.MoveUser("E1", ouOther)
	run = e.run()
	if _, err := e.eng.Execute(e.ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "manual AD placement kept", parentDN(e.user("E1").DN), ouOther)

	// A department change to one without a rule: the move waits for a person.
	e.writeCSV(
		row("E1", "Jane", "Doe", "Legal", "Lead Designer", "A", ""),
		row("E2", "Bob", "Smith", "Marketing", "Analyst", "A", ""),
	)
	e.run()
	upd := e.changes(model.StatusPending, model.ChangeUpdate)
	if len(upd) != 1 || !upd[0].Payload.NeedsOU || upd[0].Payload.MoveTo != "" {
		t.Fatalf("an unmapped department change must wait for an OU: %+v", upd)
	}
	if _, err := e.eng.SetChangeOU(e.ctx, upd[0].ID, ouFin, "bob", false); err != nil {
		t.Fatal(err)
	}
	eq(t, "approve the move", e.approveAll(model.ChangeUpdate), 1)
	eq(t, "moved to the chosen OU", parentDN(e.user("E1").DN), ouFin)
	run = e.run()
	eq(t, "converged after the move", run.Counters["planned_update"], 0)
	if _, err := e.eng.SetChangeOU(e.ctx, upd[0].ID, ouFin, "bob", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("an applied change cannot be edited: %v", err)
	}
	if _, err := e.eng.SetChangeOU(e.ctx, "nope", "not a dn", "bob", false); !errors.Is(err, ErrInvalid) {
		t.Errorf("a value that is not a DN is refused: %v", err)
	}
}
