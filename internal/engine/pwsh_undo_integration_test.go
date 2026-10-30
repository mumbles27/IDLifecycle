package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"idlifecycle/internal/model"
)

// Undo through the REAL scripts and the REAL PowerShellClient against the stub AD: the "before" the scripts report,
// the inverse the engine builds from it, and the scripts' drift check must fit together.
func TestRealScriptsUndoRoundTrip(t *testing.T) {
	p := newPwshEnv(t)
	p.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	p.run()
	eq(t, "created", p.approveAll(model.ChangeCreate), 2)

	// 1. update, then someone edits the title by hand: the undo is refused until forced
	p.writeCSV(
		row("E1", "Jane", "Doe", "Engineering", "Senior Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	p.run()
	eq(t, "update applied", p.approveAll(model.ChangeUpdate), 1)
	orig := p.applied(model.ChangeUpdate, "E1")
	if orig.AppliedState == nil || orig.AppliedState.Before.Attributes["title"] == nil {
		t.Fatalf("Set-IDLUser must report the title it replaced: %+v", orig.AppliedState)
	}
	eq(t, "before from the script", *orig.AppliedState.Before.Attributes["title"], "Designer")

	b, _ := os.ReadFile(p.state)
	re := regexp.MustCompile(`"title"\s*:\s*"Senior Designer"`)
	if !re.Match(b) {
		t.Fatalf("no title to tamper with in the stub state: %s", b)
	}
	_ = os.WriteFile(p.state, re.ReplaceAll(b, []byte(`"title":"Manager"`)), 0o600)
	pv, err := p.eng.PreviewUndo(p.ctx, orig.ID)
	p.must(err)
	eq(t, "the real dry run sees the drift", pv.DryRun.Status, "failed")
	if len(pv.Drift) == 0 || !strings.Contains(strings.Join(pv.Drift, ";"), "title is 'Manager'") {
		t.Fatalf("drift from Set-IDLUser: %+v / %+v", pv.Drift, pv.DryRun)
	}
	calls := len(p.read().Calls)
	p.undo(orig.ID, true, offered)
	i, s := p.account("jdoe")
	eq(t, "title restored", attr(s.Users[i].Attributes, "title"), "Designer")
	eq(t, "back in both groups", len(s.Users[i].MemberOf), 2)
	if len(s.Calls) == calls {
		t.Fatal("the forced undo wrote nothing")
	}
	run := p.run()
	eq(t, "holds keep the next sync from redoing it", run.Counters["planned_update"], 0)

	// 2. disable, then undo it: enabled, moved back, groups and description restored
	p.writeCSV(
		row("E1", "Jane", "Doe", "Engineering", "Senior Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "T", ""),
	)
	p.run()
	eq(t, "disable applied", p.approveAll(model.ChangeDisable), 1)
	j, s := p.account("bsmith")
	eq(t, "disabled", s.Users[j].Enabled, false)
	dis := p.applied(model.ChangeDisable, "E2")
	p.undo(dis.ID, false, offered)
	j, s = p.account("bsmith")
	bob := s.Users[j]
	eq(t, "enabled again", bob.Enabled, true)
	eq(t, "moved back", bob.DN, "CN=Bob Smith,"+baseOU)
	eq(t, "group back", len(bob.MemberOf), 1)
	eq(t, "description back (the disable had stamped its own)", attr(bob.Attributes, "description"), "Developer")
	eq(t, "record follows", p.identity("E2").State, model.StateLinked)

	// 3. undo a create: the account it made is disabled, not deleted
	cr := p.applied(model.ChangeCreate, "E1")
	p.undo(cr.ID, false, offered)
	i, s = p.account("jdoe")
	eq(t, "created account disabled", s.Users[i].Enabled, false)
	eq(t, "and kept", s.Users[i].DN != "", true)

	for _, c := range p.changes(model.StatusFailed, "") {
		t.Errorf("change failed: %s %s", c.Type, c.Error)
	}
}

// Undo one part through the real scripts: only the title of an update that also removed a group; the group stays
// removed, and Set-IDLUser checks only the title.
func TestRealScriptsUndoOnePart(t *testing.T) {
	p := newPwshEnv(t)
	p.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	p.run()
	eq(t, "created", p.approveAll(model.ChangeCreate), 1)
	p.writeCSV(row("E1", "Jane", "Doe", "Engineering", "Senior Designer", "A", ""))
	p.run()
	eq(t, "update applied", p.approveAll(model.ChangeUpdate), 1)
	i, s := p.account("jdoe")
	eq(t, "marketing group removed", len(s.Users[i].MemberOf), 1)
	orig := p.applied(model.ChangeUpdate, "E1")
	pv := p.previewUndo(orig.ID)
	if len(pv.Items) < 2 {
		t.Fatalf("the update has a title part and a group part: %+v", pv.Items)
	}
	p.undoItems(orig.ID, []string{"attribute:title"}, true)
	i, s = p.account("jdoe")
	eq(t, "title restored", attr(s.Users[i].Attributes, "title"), "Designer")
	eq(t, "the group stays removed", len(s.Users[i].MemberOf), 1)
	o, _ := p.st.GetChange(p.ctx, orig.ID)
	eq(t, "partly undone", o.UndoneBy, "")
	eq(t, "title part undone", o.UndoneItems["attribute:title"] != "", true)
	run := p.run()
	eq(t, "the Manual title is not redone", run.Counters["planned_update"], 0)
}
