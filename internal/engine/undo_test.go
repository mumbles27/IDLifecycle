package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// previewUndo previews an undo and fails the test when it is not possible.
func (e *env) previewUndo(changeID string) UndoPreview {
	e.t.Helper()
	pv, err := e.eng.PreviewUndo(e.ctx, changeID)
	e.must(err)
	if !pv.Undoable {
		e.t.Fatalf("change %s should be undoable: %s", changeID, pv.Reason)
	}
	return pv
}

// offered turns every hold an undo offers into a request.
func offered(pv UndoPreview) []model.HoldSpec {
	var out []model.HoldSpec
	for _, h := range pv.Holds {
		out = append(out, model.HoldSpec{Kind: h.Kind, Key: h.Key})
	}
	return out
}

// undo requests (as alice), approves (as bob) and applies the undo of change id.
func (e *env) undo(changeID string, force bool, holds func(UndoPreview) []model.HoldSpec) model.Change {
	e.t.Helper()
	pv := e.previewUndo(changeID)
	var hs []model.HoldSpec
	if holds != nil {
		hs = holds(pv)
	}
	c, err := e.eng.RequestUndo(e.ctx, changeID, "alice", UndoRequest{Reason: "rolled back in a test", Confirm: pv.ConfirmText, Force: force, Holds: hs, PreviewHash: pv.Hash})
	e.must(err)
	eq(e.t, "an undo always waits for a person", c.Status, model.StatusPending)
	_, err = e.eng.ApproveUndo(e.ctx, c.ID, "bob", "ok", strings.ToUpper(pv.ConfirmText))
	e.must(err)
	_, err = e.eng.Execute(e.ctx)
	e.must(err)
	got, err := e.st.GetChange(e.ctx, c.ID)
	e.must(err)
	if got.Status != model.StatusApplied {
		e.t.Fatalf("undo not applied: %s %s", got.Status, got.Error)
	}
	return got
}

func (e *env) applied(typ, ext string) model.Change {
	e.t.Helper()
	for _, c := range e.changes(model.StatusApplied, typ) {
		if c.ExternalID == ext {
			return c
		}
	}
	e.t.Fatalf("no applied %s change for %s", typ, ext)
	return model.Change{}
}

// moveToEngineering applies an update for E1: title Designer -> Lead, GG-Mkt removed.
func (e *env) moveToEngineering() model.Change {
	e.t.Helper()
	e.provision()
	e.writeCSV(
		row("E1", "Jane", "Doe", "Engineering", "Lead", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	e.run()
	eq(e.t, "update approved", e.approveAll(model.ChangeUpdate), 1)
	u := e.user("E1")
	eq(e.t, "title changed", u.Attrs["title"], "Lead")
	eq(e.t, "left marketing", hasGroup(u, gMkt), false)
	return e.applied(model.ChangeUpdate, "E1")
}

func TestUndoUpdateRestoresValuesAndHoldsStopTheRedo(t *testing.T) {
	e := newEnv(t)
	orig := e.moveToEngineering()
	if orig.AppliedState == nil || orig.AppliedState.Before.Attributes["title"] == nil || *orig.AppliedState.Before.Attributes["title"] != "Designer" {
		t.Fatalf("the state before the update must be recorded: %+v", orig.AppliedState)
	}

	pv := e.previewUndo(orig.ID)
	eq(t, "script", pv.Script, model.UndoScriptSet)
	eq(t, "confirm with the account name", pv.ConfirmText, "jdoe")
	eq(t, "dry run", pv.DryRun.Status, "would")
	eq(t, "no drift", len(pv.Drift), 0)
	steps := strings.Join(pv.Steps, "\n")
	for _, want := range []string{"Set title back to 'Designer' (now 'Lead')", "Add back to group GG-Mkt"} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps miss %q:\n%s", want, steps)
		}
	}
	kinds := map[string]bool{}
	for _, h := range pv.Holds {
		kinds[h.Kind+":"+h.Key] = true
	}
	if !kinds["attribute:title"] || !kinds["group:"+gMkt] {
		t.Fatalf("holds offered: %+v", pv.Holds)
	}
	if calls := strings.Join(e.fake.CallLog(), " "); !strings.Contains(calls, "update(n=1,dry=true)") {
		t.Fatalf("the preview must run the script in dry-run: %s", calls)
	}

	undo := e.undo(orig.ID, false, offered)
	u := e.user("E1")
	eq(t, "title restored", u.Attrs["title"], "Designer")
	eq(t, "back in marketing", hasGroup(u, gMkt), true)
	o, _ := e.st.GetChange(e.ctx, orig.ID)
	eq(t, "original marked undone", o.UndoneBy, undo.ID)
	eq(t, "undo_of", undo.UndoOf, orig.ID)
	holds, _ := e.st.ListHolds(e.ctx, e.identity("E1").ID)
	eq(t, "holds created", len(holds), 2)
	eq(t, "hold reason", holds[0].Reason, "rolled back in a test")

	run := e.run()
	eq(t, "nothing is redone", run.Counters["planned_update"], 0)
	if run.Counters["on_hold"] == 0 {
		t.Fatalf("the held group removal is counted: %v", run.Counters)
	}
	if pv, _ := e.eng.PreviewUndo(e.ctx, orig.ID); pv.Undoable {
		t.Fatal("an undone change cannot be undone twice")
	}

	for _, h := range holds {
		_, err := e.eng.ReleaseHold(e.ctx, h.IdentityID, h.ID, "carol")
		e.must(err)
	}
	run = e.run()
	eq(t, "released: the group removal is planned again", run.Counters["planned_update"], 1)
}

func TestUndoRefusesToOverwriteLaterChangesUnlessForced(t *testing.T) {
	e := newEnv(t)
	orig := e.moveToEngineering()
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Attrs["title"] = "Manager" })

	pv, err := e.eng.PreviewUndo(e.ctx, orig.ID)
	e.must(err)
	eq(t, "dry run fails", pv.DryRun.Status, "failed")
	if len(pv.Drift) != 1 || pv.Drift[0] != "title is 'Manager', expected 'Lead'" {
		t.Fatalf("drift: %+v", pv.Drift)
	}
	_, err = e.eng.RequestUndo(e.ctx, orig.ID, "alice", UndoRequest{Reason: "rolled back in a test", Confirm: "jdoe", PreviewHash: pv.Hash})
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "later changes would be overwritten") {
		t.Fatalf("drift without force must be refused: %v", err)
	}
	e.undo(orig.ID, true, nil)
	eq(t, "forced: title restored", e.user("E1").Attrs["title"], "Designer")
}

func TestUndoDisableEnablesAgainAndStateHoldExpires(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "T", ""))
	e.run()
	eq(t, "disable approved", e.approveAll(model.ChangeDisable), 1)
	u := e.user("E2")
	eq(t, "disabled", u.Enabled, false)
	orig := e.applied(model.ChangeDisable, "E2")

	pv := e.previewUndo(orig.ID)
	eq(t, "script", pv.Script, model.UndoScriptEnable)
	until := e.clock.Add(48 * time.Hour)
	e.undo(orig.ID, false, func(pv UndoPreview) []model.HoldSpec {
		return []model.HoldSpec{{Kind: model.HoldState, Until: &until}}
	})
	u = e.user("E2")
	eq(t, "enabled again", u.Enabled, true)
	eq(t, "moved back", strings.HasSuffix(u.DN, ","+baseOU), true)
	eq(t, "groups back", hasGroup(u, gAll), true)
	eq(t, "description back", u.Attrs["description"], "Developer")
	eq(t, "record follows", e.identity("E2").State, model.StateLinked)

	run := e.run()
	eq(t, "the state hold stops the disable", run.Counters["planned_disable"], 0)
	eq(t, "counted", run.Counters["on_hold"], 1)
	e.clock = e.clock.Add(72 * time.Hour)
	run = e.run()
	eq(t, "after the hold expires the disable is planned again", run.Counters["planned_disable"], 1)
}

func TestUndoEnableDisablesAgain(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "T", ""))
	e.run()
	e.approveAll(model.ChangeDisable)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	eq(t, "enable approved", e.approveAll(model.ChangeEnable), 1)
	eq(t, "enabled", e.user("E2").Enabled, true)
	orig := e.applied(model.ChangeEnable, "E2")
	pv := e.previewUndo(orig.ID)
	eq(t, "script", pv.Script, model.UndoScriptDisable)
	e.undo(orig.ID, false, offered)
	u := e.user("E2")
	eq(t, "disabled again", u.Enabled, false)
	eq(t, "back in the disabled OU", strings.HasSuffix(u.DN, ","+disabledOU), true)
	eq(t, "record follows", e.identity("E2").State, model.StateDisabled)
	run := e.run()
	eq(t, "the state hold stops the enable", run.Counters["planned_enable"], 0)
}

func TestUndoCreateDisablesTheAccountAndRedoEnablesIt(t *testing.T) {
	e := newEnv(t)
	e.provision()
	orig := e.applied(model.ChangeCreate, "E1")
	pv := e.previewUndo(orig.ID)
	eq(t, "script", pv.Script, model.UndoScriptDisable)
	if !strings.Contains(strings.Join(pv.Warnings, " "), "not deleted") {
		t.Fatalf("warnings: %v", pv.Warnings)
	}
	undo := e.undo(orig.ID, false, nil)
	eq(t, "disabled, not deleted", e.user("E1").Enabled, false)
	eq(t, "record follows", e.identity("E1").State, model.StateDisabled)

	// Redo: the undo itself can be rolled back.
	if undo.AppliedState == nil {
		t.Fatal("an applied undo records its own state (for redo)")
	}
	redo := e.previewUndo(undo.ID)
	eq(t, "redo enables", redo.Script, model.UndoScriptEnable)
	e.undo(undo.ID, false, nil)
	eq(t, "enabled again", e.user("E1").Enabled, true)
	eq(t, "record follows", e.identity("E1").State, model.StateLinked)
}

func TestUndoRedoOfAnUpdate(t *testing.T) {
	e := newEnv(t)
	orig := e.moveToEngineering()
	undo := e.undo(orig.ID, false, nil)
	eq(t, "undone", e.user("E1").Attrs["title"], "Designer")
	e.undo(undo.ID, false, nil)
	u := e.user("E1")
	eq(t, "redone", u.Attrs["title"], "Lead")
	eq(t, "redone group", hasGroup(u, gMkt), false)
}

func TestUndoLinkHandsTheAccountBack(t *testing.T) {
	e := newEnv(t)
	e.correlation(nil)
	guid := e.hrAccount("jdoe", "Jane", "Doe", "Marketing")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	orig := e.applied(model.ChangeLink, "E1")

	pv := e.previewUndo(orig.ID)
	eq(t, "script", pv.Script, model.UndoScriptLink)
	if !strings.Contains(strings.Join(pv.Steps, " "), "Clear employeeID") {
		t.Fatalf("steps: %v", pv.Steps)
	}
	e.undo(orig.ID, false, offered)
	eq(t, "id attribute cleared", e.fake.Users[guid].External, "")
	id := e.identity("E1")
	eq(t, "record unlinked", id.State, model.StateNew)
	eq(t, "no account", id.ADGUID, "")

	run := e.run()
	eq(t, "state hold: no new account", run.Counters["planned_create"], 0)
	eq(t, "state hold: no new link", run.Counters["planned_link"], 0)

	holds, _ := e.st.ListHolds(e.ctx, id.ID)
	for _, h := range holds {
		_, err := e.eng.ReleaseHold(e.ctx, id.ID, h.ID, "carol")
		e.must(err)
	}
	run = e.run()
	eq(t, "the handed-back account is never proposed again", run.Counters["planned_link"], 0)
	eq(t, "a new account is planned instead", run.Counters["planned_create"], 1)
}

func TestUndoCloudGroups(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	e.tweak(func(s *model.Settings) { s.Lifecycle.ActiveAdd = []string{ref(cgAll)} })
	e.provision()
	e.entraSync(f)
	e.run()
	e.approveAll(model.ChangeCloudGroups)
	eq(t, "added", cloudMembers(f, "user-e1"), "All Staff")
	orig := e.applied(model.ChangeCloudGroups, "E1")

	pv := e.previewUndo(orig.ID)
	eq(t, "script", pv.Script, model.UndoScriptCloud)
	eq(t, "dry run", pv.DryRun.Status, "would")
	e.undo(orig.ID, false, offered)
	eq(t, "removed again", cloudMembers(f, "user-e1"), "")
	run := e.run()
	if run.Counters["on_hold"] == 0 {
		t.Fatalf("the cloud_group hold must stop the re-add: %v", run.Counters)
	}
	for _, c := range e.changes(model.StatusPending, model.ChangeCloudGroups) {
		if c.ExternalID == "E1" {
			t.Fatalf("no cloud change for the held group: %+v", c)
		}
	}
}

func TestUndoNotPossible(t *testing.T) {
	e := newEnv(t)
	orig := e.moveToEngineering()
	why := func(id string) string {
		t.Helper()
		pv, err := e.eng.PreviewUndo(e.ctx, id)
		e.must(err)
		if pv.Undoable {
			t.Fatalf("%s should not be undoable", id)
		}
		return pv.Reason
	}
	for _, c := range []model.Change{
		{ID: "del", Type: model.ChangeDelete, Status: model.StatusApplied, IdentityID: orig.IdentityID},
		{ID: "old", Type: model.ChangeUpdate, Status: model.StatusApplied, IdentityID: orig.IdentityID},
		{ID: "pend", Type: model.ChangeUpdate, Status: model.StatusPending, IdentityID: orig.IdentityID},
	} {
		c.CreatedAt = e.clock
		e.must(e.st.InsertChange(e.ctx, c))
	}
	for id, want := range map[string]string{"del": "Recycle Bin", "old": "before undo was available", "pend": "Only applied changes"} {
		if r := why(id); !strings.Contains(r, want) {
			t.Errorf("%s: %q should mention %q", id, r, want)
		}
	}

	e.tweak(func(s *model.Settings) { s.Undo.Enabled = false })
	if r := why(orig.ID); !strings.Contains(r, "turned off") {
		t.Errorf("undo off: %q", r)
	}
	e.tweak(func(s *model.Settings) { s.Undo.Enabled, s.Engine.DryRun = true, true })
	if r := why(orig.ID); !strings.Contains(r, "dry run") {
		t.Errorf("dry run: %q", r)
	}
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = false })

	// Request validation.
	pv := e.previewUndo(orig.ID)
	req := func(mod func(*UndoRequest)) error {
		r := UndoRequest{Reason: "rolled back in a test", Confirm: "jdoe", PreviewHash: pv.Hash}
		mod(&r)
		_, err := e.eng.RequestUndo(e.ctx, orig.ID, "alice", r)
		return err
	}
	if err := req(func(r *UndoRequest) { r.Confirm = "jdoe2" }); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong confirmation: %v", err)
	}
	if err := req(func(r *UndoRequest) { r.Reason = "short" }); !errors.Is(err, ErrInvalid) {
		t.Errorf("short reason: %v", err)
	}
	if err := req(func(r *UndoRequest) { r.PreviewHash = "stale" }); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale preview: %v", err)
	}
	if err := req(func(r *UndoRequest) { r.Holds = []model.HoldSpec{{Kind: model.HoldAttribute, Key: "manager"}} }); !errors.Is(err, ErrInvalid) {
		t.Errorf("hold not offered: %v", err)
	}
	past := e.clock.Add(-time.Hour)
	if err := req(func(r *UndoRequest) {
		r.Holds = []model.HoldSpec{{Kind: model.HoldAttribute, Key: "TITLE", Until: &past}}
	}); !errors.Is(err, ErrInvalid) {
		t.Errorf("hold in the past: %v", err)
	}
	e.must(req(func(r *UndoRequest) {}))
	if r := why(orig.ID); !strings.Contains(r, "already waiting") {
		t.Errorf("pending undo: %q", r)
	}
	if err := req(func(r *UndoRequest) {}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second request: %v", err)
	}

	// Approval rules.
	undo := e.changes(model.StatusPending, model.ChangeUndo)[0]
	if _, err := e.eng.Approve(e.ctx, undo.ID, "bob", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("plain approve (bulk) must refuse an undo: %v", err)
	}
	if _, err := e.eng.ApproveUndo(e.ctx, undo.ID, "bob", "", "someone"); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong confirmation: %v", err)
	}
	e.tweak(func(s *model.Settings) { s.Undo.SecondPerson = true })
	if _, err := e.eng.ApproveUndo(e.ctx, undo.ID, "alice", "", "jdoe"); !errors.Is(err, ErrForbidden) {
		t.Errorf("second person: %v", err)
	}
	if _, err := e.eng.ApproveUndo(e.ctx, undo.ID, "bob", "", "jdoe"); err != nil {
		t.Errorf("another person approves: %v", err)
	}

	// A sync never supersedes an undo waiting to run.
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = true }) // the executor does nothing in dry run
	e.run()
	got, _ := e.st.GetChange(e.ctx, undo.ID)
	eq(t, "still approved", got.Status, model.StatusApproved)
}
