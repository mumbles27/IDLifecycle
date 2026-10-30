package engine

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

const otherOU = "OU=Other,DC=corp,DC=example,DC=com"

// moveWithOU applies an update for E1 that changes the title (Designer -> Lead), removes GG-Mkt and moves the account
// to otherOU (enforce_ou), and returns it.
func (e *env) moveWithOU() model.Change {
	e.t.Helper()
	e.provision()
	e.tweak(func(s *model.Settings) { s.Engine.EnforceOU, s.AD.DefaultOU = true, otherOU })
	e.writeCSV(
		row("E1", "Jane", "Doe", "Engineering", "Lead", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""),
	)
	e.run()
	e.approveAll(model.ChangeUpdate)
	u := e.user("E1")
	eq(e.t, "title changed", u.Attrs["title"], "Lead")
	eq(e.t, "moved", parentDN(u.DN), otherOU)
	eq(e.t, "left marketing", hasGroup(u, gMkt), false)
	return e.applied(model.ChangeUpdate, "E1")
}

func itemKeys(items []UndoItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Key
	}
	return out
}

func itemByKey(items []UndoItem, key string) UndoItem {
	for _, it := range items {
		if it.Key == key {
			return it
		}
	}
	return UndoItem{}
}

// undoItems requests (alice), approves (bob) and applies an undo of some parts of change id.
func (e *env) undoItems(id string, items []string, holds bool) model.Change {
	e.t.Helper()
	pv, err := e.eng.PreviewUndoItems(e.ctx, id, items)
	e.must(err)
	if !pv.Undoable {
		e.t.Fatalf("undo of %v not possible: %s", items, pv.Reason)
	}
	var hs []model.HoldSpec
	if holds {
		hs = offered(pv)
	}
	c, err := e.eng.RequestUndo(e.ctx, id, "alice", UndoRequest{Reason: "rolled back in a test", Confirm: pv.ConfirmText, Holds: hs, PreviewHash: pv.Hash, Items: items})
	e.must(err)
	_, err = e.eng.ApproveUndo(e.ctx, c.ID, "bob", "ok", pv.ConfirmText)
	e.must(err)
	_, err = e.eng.Execute(e.ctx)
	e.must(err)
	got, _ := e.st.GetChange(e.ctx, c.ID)
	if got.Status != model.StatusApplied {
		e.t.Fatalf("undo not applied: %s %s", got.Status, got.Error)
	}
	return got
}

func TestUndoPartsOfAnUpdate(t *testing.T) {
	e := newEnv(t)
	orig := e.moveWithOU()
	pv := e.previewUndo(orig.ID)
	keys := strings.Join(itemKeys(pv.Items), "|")
	if keys != "attribute:title|group_remove:"+gMkt+"|ou" {
		t.Fatalf("parts: %s", keys)
	}
	if pv.WholeOnly || len(pv.Selected) != 3 || pv.Payload.Undo.Items != nil {
		t.Fatalf("no selection = the whole change: %+v / %v", pv.Selected, pv.Payload.Undo.Items)
	}
	if it := itemByKey(pv.Items, "attribute:title"); valueOf(it.From) != "Designer" || valueOf(it.To) != "Lead" || !it.Selectable {
		t.Fatalf("title part: %+v", it)
	}

	// The selection is part of the plan the person confirms.
	title, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{"attribute:title"})
	e.must(err)
	grp, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{"group_remove:" + gMkt})
	e.must(err)
	if title.Hash == grp.Hash || title.Hash == pv.Hash {
		t.Fatal("the preview hash must cover the selection")
	}
	_, err = e.eng.RequestUndo(e.ctx, orig.ID, "alice", UndoRequest{Reason: "rolled back in a test", Confirm: "jdoe", PreviewHash: grp.Hash, Items: []string{"attribute:title"}})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a hash of another selection is refused: %v", err)
	}
	if u := title.Payload.Undo; u.Expect.DN != "" || len(u.Expect.MemberOf)+len(u.Expect.NotMemberOf) != 0 || len(u.Expect.Attributes) != 1 {
		t.Fatalf("expect covers only the title: %+v", u.Expect)
	}
	if len(title.Holds) != 1 || title.Holds[0].Kind != model.HoldAttribute {
		t.Fatalf("holds offered for the title only: %+v", title.Holds)
	}
	for _, bad := range []string{"attribute:nope", "group_add:" + gMkt} {
		if _, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unknown part %q: %v", bad, err)
		}
	}

	// Later changes to other things do not block undoing the title.
	e.fake.Mutate("E1", func(u *ad.FakeUser) {
		u.Attrs["telephoneNumber"] = "555"
		u.MemberOf = append(u.MemberOf, gMkt)
		u.DN = "CN=Jane Doe," + baseOU
	})
	e.undoItems(orig.ID, []string{"attribute:title"}, true)
	u := e.user("E1")
	eq(t, "title back", u.Attrs["title"], "Designer")
	eq(t, "the later move stays", parentDN(u.DN), baseOU)
	eq(t, "the later phone stays", u.Attrs["telephoneNumber"], "555")
	o, _ := e.st.GetChange(e.ctx, orig.ID)
	if o.UndoneBy != "" || len(o.UndoneItems) != 1 || o.UndoneItems["attribute:title"] == "" {
		t.Fatalf("partly undone: %q %+v", o.UndoneBy, o.UndoneItems)
	}
	if _, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{"attribute:title"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an undone part cannot be undone again: %v", err)
	}

	// Put things back as the change left them, then undo the rest in two steps.
	e.fake.Mutate("E1", func(u *ad.FakeUser) {
		u.MemberOf = []string{gAll}
		u.DN = "CN=Jane Doe," + otherOU
	})
	rest := e.previewUndo(orig.ID)
	if strings.Join(rest.Selected, "|") != "group_remove:"+gMkt+"|ou" || itemByKey(rest.Items, "attribute:title").Selectable {
		t.Fatalf("the remaining parts: %v", rest.Selected)
	}
	ouPV, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{"ou"})
	e.must(err)
	ouUndo, err := e.eng.RequestUndo(e.ctx, orig.ID, "alice", UndoRequest{Reason: "rolled back in a test", Confirm: "jdoe", PreviewHash: ouPV.Hash, Items: []string{"ou"}})
	e.must(err)
	if _, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{"ou"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("an open undo blocks its parts: %v", err)
	}
	o, _ = e.st.GetChange(e.ctx, orig.ID)
	if o.UndoPending != "" || o.UndoPendingItems["ou"] != ouUndo.ID {
		t.Fatalf("pending parts: %q %+v", o.UndoPending, o.UndoPendingItems)
	}
	e.undoItems(orig.ID, []string{"group_remove:" + gMkt}, false) // the open ou undo does not block the group
	o, _ = e.st.GetChange(e.ctx, orig.ID)
	eq(t, "not undone while the ou undo waits", o.UndoneBy, "")
	_, err = e.eng.ApproveUndo(e.ctx, ouUndo.ID, "bob", "ok", "jdoe")
	e.must(err)
	_, err = e.eng.Execute(e.ctx)
	e.must(err)
	u = e.user("E1")
	eq(t, "moved back", parentDN(u.DN), baseOU)
	eq(t, "back in marketing", hasGroup(u, gMkt), true)
	o, _ = e.st.GetChange(e.ctx, orig.ID)
	eq(t, "every part undone: the change is undone", o.UndoneBy, ouUndo.ID)
	if pv, _ := e.eng.PreviewUndo(e.ctx, orig.ID); pv.Undoable {
		t.Fatal("nothing is left to undo")
	}
}

func TestUndoPartDriftIsOnlyAboutTheChosenParts(t *testing.T) {
	e := newEnv(t)
	orig := e.moveWithOU()
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Attrs["title"] = "Manager" })
	title, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{"attribute:title"})
	e.must(err)
	if title.DryRun.Status != "failed" || len(title.Drift) != 1 || title.Drift[0] != "title is 'Manager', expected 'Lead'" {
		t.Fatalf("the same attribute changed later is drift: %+v %v", title.DryRun, title.Drift)
	}
	grp, err := e.eng.PreviewUndoItems(e.ctx, orig.ID, []string{"group_remove:" + gMkt})
	e.must(err)
	if grp.DryRun.Status != "would" || len(grp.Drift) != 0 {
		t.Fatalf("another part is not blocked: %+v %v", grp.DryRun, grp.Drift)
	}
}

func TestUndoWholeOnlyTypesRefuseParts(t *testing.T) {
	e := newEnv(t)
	e.provision()
	cr := e.applied(model.ChangeCreate, "E1")
	pv := e.previewUndo(cr.ID)
	if !pv.WholeOnly || len(pv.Items) != 0 || pv.Payload.Undo.Items != nil {
		t.Fatalf("a create is undone as a whole: %+v", pv.Items)
	}
	if _, err := e.eng.PreviewUndoItems(e.ctx, cr.ID, []string{"attribute:title"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("parts of a create: %v", err)
	}
	_, err := e.eng.RequestUndo(e.ctx, cr.ID, "alice", UndoRequest{Reason: "rolled back in a test", Confirm: "jdoe", PreviewHash: pv.Hash, Items: []string{"ou"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("request with parts of a create: %v", err)
	}
}

func TestUndoOneCloudGroup(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	e.grantRole("r-mkt", ref(cgSales))
	e.tweak(func(s *model.Settings) { s.Lifecycle.ActiveAdd = []string{ref(cgAll)} })
	e.provision()
	e.entraSync(f)
	e.run()
	e.approveAll(model.ChangeCloudGroups)
	eq(t, "both groups", cloudMembers(f, "user-e1"), "All Staff,Sales Team")
	orig := e.applied(model.ChangeCloudGroups, "E1")
	pv := e.previewUndo(orig.ID)
	if len(pv.Items) != 2 {
		t.Fatalf("one part per cloud group: %+v", pv.Items)
	}
	key := ""
	for _, it := range pv.Items {
		if strings.EqualFold(it.Group, cgSales) {
			key = it.Key
		}
	}
	if !strings.HasPrefix(key, "cloud_add:") {
		t.Fatalf("no part for the sales group: %+v", pv.Items)
	}
	e.undoItems(orig.ID, []string{key}, true)
	eq(t, "only sales removed", cloudMembers(f, "user-e1"), "All Staff")
	run := e.run()
	if run.Counters["on_hold"] == 0 {
		t.Fatalf("the Manual pin stops the re-add: %v", run.Counters)
	}
	holds, _ := e.st.ListHolds(e.ctx, e.identity("E1").ID)
	if len(holds) != 1 || holds[0].Suppressed == nil || !strings.Contains(holds[0].Suppressed.Summary, "would add to Entra group Sales Team") {
		t.Fatalf("what the pin stopped: %+v", holds)
	}
}

// twoTitleUpdates applies one run that changes the titles of E1 and E2 and returns that run.
func (e *env) twoTitleUpdates() model.Run {
	e.t.Helper()
	e.provision()
	e.writeCSV(
		row("E1", "Jane", "Doe", "Marketing", "Senior Designer", "A", ""),
		row("E2", "Bob", "Smith", "Engineering", "Senior Developer", "A", ""),
	)
	run := e.run()
	eq(e.t, "two updates", e.approveAll(model.ChangeUpdate), 2)
	return run
}

func (e *env) runSelection(run model.Run) RunUndoSelection {
	e.t.Helper()
	_, list, err := e.eng.RunUndoList(e.ctx, run.ID)
	e.must(err)
	sel := RunUndoSelection{}
	for _, en := range list {
		if !en.Undoable || en.WholeOnly || len(en.Items) == 0 {
			e.t.Fatalf("run entry: %+v", en)
		}
		if en.Change.ExternalID == "E1" {
			sel[en.Change.ID] = []string{"attribute:title"}
		} else {
			sel[en.Change.ID] = []string{"*"}
		}
	}
	return sel
}

func TestUndoAcrossARun(t *testing.T) {
	e := newEnv(t)
	run := e.twoTitleUpdates()
	sel := e.runSelection(run)
	eq(t, "both changes listed", len(sel), 2)

	pv, err := e.eng.PreviewRunUndo(e.ctx, run.ID, sel)
	e.must(err)
	eq(t, "count", pv.Count, 2)
	eq(t, "people", pv.People, 2)
	eq(t, "confirm", pv.ConfirmText, "UNDO 2")
	eq(t, "no drift", pv.DriftCount, 0)
	for _, c := range pv.Changes {
		if c.DryRun.Status != "would" || len(c.Steps) == 0 {
			t.Fatalf("dry run per change: %+v", c)
		}
	}
	req := RunUndoRequest{Selection: sel, Reason: "wrong titles from HR", Confirm: "UNDO 2", PreviewHash: pv.Hash}
	bad := req
	bad.Confirm = "UNDO 1"
	if _, _, err := e.eng.RequestRunUndo(e.ctx, run.ID, "alice", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong count: %v", err)
	}
	bad = req
	bad.PreviewHash = "x"
	if _, _, err := e.eng.RequestRunUndo(e.ctx, run.ID, "alice", bad); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale preview: %v", err)
	}
	bad = req
	bad.Reason = "oops"
	if _, _, err := e.eng.RequestRunUndo(e.ctx, run.ID, "alice", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short reason: %v", err)
	}
	batch, list, err := e.eng.RequestRunUndo(e.ctx, run.ID, "alice", req)
	e.must(err)
	eq(t, "two undo changes", len(list), 2)
	for _, c := range list {
		if c.Status != model.StatusPending || c.Payload.Undo.Batch != batch || len(c.Payload.Undo.Holds) == 0 {
			t.Fatalf("batch member: %+v", c.Payload.Undo)
		}
		if _, err := e.eng.Approve(e.ctx, c.ID, "bob", ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("plain approve of an undo: %v", err)
		}
	}

	v, err := e.eng.CheckUndoBatch(e.ctx, batch)
	e.must(err)
	if v.Pending != 2 || v.Total != 2 || v.ConfirmText != "UNDO 2" || v.RequestedBy != "alice" || v.RunID != run.ID || len(v.Changes) != 2 || v.Changes[0].DryRun == nil {
		t.Fatalf("batch view: %+v", v)
	}
	e.tweak(func(s *model.Settings) { s.Undo.SecondPerson = true })
	if _, err := e.eng.ApproveUndoBatch(e.ctx, batch, "alice", "", "UNDO 2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the requester cannot approve: %v", err)
	}
	if _, err := e.eng.ApproveUndoBatch(e.ctx, batch, "bob", "", "UNDO 3"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong typed count: %v", err)
	}
	approved, err := e.eng.ApproveUndoBatch(e.ctx, batch, "bob", "checked", "undo 2")
	e.must(err)
	eq(t, "approved", len(approved), 2)
	_, err = e.eng.Execute(e.ctx)
	e.must(err)
	eq(t, "E1 title back", e.user("E1").Attrs["title"], "Designer")
	eq(t, "E2 title back", e.user("E2").Attrs["title"], "Developer")
	for _, ext := range []string{"E1", "E2"} {
		holds, _ := e.st.ListHolds(e.ctx, e.identity(ext).ID)
		if len(holds) != 1 || holds[0].Kind != model.HoldAttribute || holds[0].Key != "title" {
			t.Fatalf("undone parts are Manual: %s %+v", ext, holds)
		}
	}
	next := e.run()
	eq(t, "nothing redone", next.Counters["planned_update"], 0)
	if _, err := e.eng.ApproveUndoBatch(e.ctx, batch, "bob", "", "UNDO 0"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("nothing left to approve: %v", err)
	}
}

func TestUndoBatchDriftAndReject(t *testing.T) {
	e := newEnv(t)
	run := e.twoTitleUpdates()
	sel := e.runSelection(run)
	pv, err := e.eng.PreviewRunUndo(e.ctx, run.ID, sel)
	e.must(err)
	no := false
	batch, _, err := e.eng.RequestRunUndo(e.ctx, run.ID, "alice", RunUndoRequest{Selection: sel, Reason: "wrong titles from HR", Confirm: "UNDO 2", PreviewHash: pv.Hash, Manual: &no})
	e.must(err)
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Attrs["title"] = "Manager" })
	v, err := e.eng.CheckUndoBatch(e.ctx, batch)
	e.must(err)
	eq(t, "drift seen", v.DriftCount, 1)
	if _, err := e.eng.ApproveUndoBatch(e.ctx, batch, "bob", "", "UNDO 2"); !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "one at a time") {
		t.Fatalf("drift blocks the batch: %v", err)
	}
	if _, err := e.eng.RejectUndoBatch(e.ctx, batch, "bob", " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reject needs a note: %v", err)
	}
	rejected, err := e.eng.RejectUndoBatch(e.ctx, batch, "bob", "not needed after all")
	e.must(err)
	eq(t, "rejected", len(rejected), 2)
	for _, c := range rejected {
		eq(t, "status", c.Status, model.StatusRejected)
		if len(c.Payload.Undo.Holds) != 0 {
			t.Fatalf("manual:false asks for no pins: %+v", c.Payload.Undo.Holds)
		}
	}
	if _, err := e.eng.CheckUndoBatch(e.ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown batch: %v", err)
	}
}

func TestRunUndoSelectionIsChecked(t *testing.T) {
	e := newEnv(t)
	run := e.twoTitleUpdates()
	if _, err := e.eng.PreviewRunUndo(e.ctx, run.ID, RunUndoSelection{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty selection: %v", err)
	}
	big := RunUndoSelection{}
	for i := 0; i <= MaxUndoBatch; i++ {
		big[fmt.Sprintf("c%03d", i)] = nil
	}
	if _, err := e.eng.PreviewRunUndo(e.ctx, run.ID, big); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "at most 200") {
		t.Fatalf("200 limit: %v", err)
	}
	cr := e.applied(model.ChangeCreate, "E1") // made by the first run
	if _, err := e.eng.PreviewRunUndo(e.ctx, run.ID, RunUndoSelection{cr.ID: {"*"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a change of another run: %v", err)
	}
	if _, _, err := e.eng.RunUndoList(e.ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
}

func TestManualPinsWithoutUndo(t *testing.T) {
	e := newEnv(t)
	e.provision()
	jane, bob := e.identity("E1"), e.identity("E2")
	pin := func(id string, k, key, reason string) (model.Hold, error) {
		return e.eng.PinManual(e.ctx, id, "carol", PinRequest{Kind: k, Key: key, Reason: reason})
	}
	h, err := pin(jane.ID, model.HoldAttribute, "title", "fixed by hand in AD")
	e.must(err)
	for _, bad := range []struct{ kind, key, reason string }{
		{model.HoldAttribute, "title", "fixed by hand in AD"}, // already Manual
		{"nope", "", "fixed by hand in AD"},
		{model.HoldAttribute, "ti tle", "fixed by hand in AD"},
		{model.HoldOU, "x", "fixed by hand in AD"},
		{model.HoldCloudGroup, "not-a-guid", "fixed by hand in AD"},
		{model.HoldGroup, gMkt, "short"},
	} {
		if _, err := pin(jane.ID, bad.kind, bad.key, bad.reason); !errors.Is(err, ErrInvalid) && !errors.Is(err, store.ErrConflict) {
			t.Fatalf("%+v must be refused: %v", bad, err)
		}
	}
	if _, err := pin("nope", model.HoldAll, "", "fixed by hand in AD"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown person: %v", err)
	}

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Principal", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "the Manual title is not planned", run.Counters["planned_update"], 0)
	holds, _ := e.st.ListHolds(e.ctx, jane.ID)
	if len(holds) != 1 || holds[0].Suppressed == nil || holds[0].Suppressed.Summary != "would set title to 'Principal'" {
		t.Fatalf("what the pin stopped: %+v", holds)
	}
	// The impact preview plans too, but never writes what a pin stopped.
	e.must(e.st.SetHoldSuppressed(e.ctx, h.ID, "", e.clock))
	if _, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{}); err != nil {
		t.Fatal(err)
	}
	holds, _ = e.st.ListHolds(e.ctx, jane.ID)
	if holds[0].Suppressed != nil {
		t.Fatalf("the preview recorded a suppression: %+v", holds[0].Suppressed)
	}
	_, err = e.eng.ReleaseHoldNote(e.ctx, jane.ID, h.ID, "carol", "HR fixed the title")
	e.must(err)
	run = e.run()
	eq(t, "set back to Auto: planned again", run.Counters["planned_update"], 1)

	// The whole person Manual: nothing is planned, not even the disable of a leaver.
	n := &recordingNotifier{}
	e.eng.Notifier = n
	_, err = pin(bob.ID, model.HoldAll, "", "on extended leave, keep as is")
	e.must(err)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Principal", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Architect", "T", ""))
	run = e.run()
	eq(t, "no disable", run.Counters["planned_disable"], 0)
	eq(t, "reported", run.Counters["manual_not_disabled"], 1)
	for _, c := range e.changes(model.StatusPending, "") {
		if c.ExternalID == "E2" {
			t.Fatalf("nothing for a Manual person: %+v", c)
		}
	}
	if got := n.events(model.EventIdentityIssues); len(got) != 1 || !strings.Contains(strings.Join(got[0].Lines, "|"), "Bob Smith (E2)") {
		t.Fatalf("the Manual leaver is reported: %+v", got)
	}
	bh, _ := e.st.ListHolds(e.ctx, bob.ID)
	if len(bh) != 1 || bh[0].Suppressed == nil || bh[0].Suppressed.Summary != "would disable the account" {
		t.Fatalf("what the person pin stopped: %+v", bh)
	}
}

func TestManagedView(t *testing.T) {
	e := newEnv(t)
	e.provision()
	jane := e.identity("E1")
	_, err := e.eng.PinManual(e.ctx, jane.ID, "carol", PinRequest{Kind: model.HoldAttribute, Key: "title", Reason: "fixed by hand in AD"})
	e.must(err)
	v, err := e.eng.Managed(e.ctx, jane.ID, true)
	e.must(err)
	if !v.Account || v.Person.Mode != "auto" || v.OU == nil || v.OU.Current != baseOU {
		t.Fatalf("view: %+v", v)
	}
	attrs := map[string]ManagedAttr{}
	for _, a := range v.Attributes {
		attrs[a.Name] = a
	}
	ti := attrs["title"]
	if ti.Mode != "manual" || ti.Hold == nil || valueOf(ti.ADValue) != "Designer" || valueOf(ti.Desired) != "Designer" {
		t.Fatalf("title: %+v", ti)
	}
	if g := attrs["givenName"]; g.Mode != "auto" || valueOf(g.Desired) != "Jane" {
		t.Fatalf("givenName: %+v", g)
	}
	groups := map[string]ManagedGroup{}
	for _, g := range v.Groups {
		groups[g.Name] = g
	}
	for _, n := range []string{"GG-All", "GG-Mkt"} {
		if g := groups[n]; g.Member == nil || !*g.Member || !g.Desired || g.Mode != "auto" {
			t.Fatalf("group %s: %+v", n, g)
		}
	}
	_, err = e.eng.PinManual(e.ctx, jane.ID, "carol", PinRequest{Kind: model.HoldAll, Reason: "keep everything as it is"})
	e.must(err)
	v, _ = e.eng.Managed(e.ctx, jane.ID, false)
	eq(t, "person Manual", v.Person.Mode, "manual")
	eq(t, "holds listed", len(v.Holds), 2)
}
