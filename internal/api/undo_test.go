package api

import (
	"context"
	"os"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
)

func TestUndoOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	e.seedSource(admin)
	var started struct{ ID string }
	admin.do("POST", "/api/runs", nil, &started)
	e.waitRun(admin, started.ID)
	var pend struct{ Items []model.Change }
	approver.do("GET", "/api/changes?status=pending&type=create", nil, &pend)
	for _, c := range pend.Items {
		want(t, "approve create", approver.do("POST", "/api/changes/"+c.ID+"/approve", nil, nil), 200)
	}
	if _, err := e.eng.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var applied struct{ Items []model.Change }
	viewer.do("GET", "/api/changes?status=applied&type=create&q=jane", nil, &applied)
	if len(applied.Items) != 1 || applied.Items[0].AppliedState == nil {
		t.Fatalf("an applied change carries its applied_state: %+v", applied.Items)
	}
	orig := applied.Items[0]

	want(t, "viewer cannot preview an undo", viewer.do("GET", "/api/changes/"+orig.ID+"/undo", nil, nil), 403)
	want(t, "approver cannot request an undo", approver.do("GET", "/api/changes/"+orig.ID+"/undo", nil, nil), 403)
	var pv struct {
		Undoable    bool                    `json:"undoable"`
		Script      string                  `json:"script"`
		Steps       []string                `json:"steps"`
		DryRun      *engine.UndoDryRun      `json:"dry_run"`
		Drift       []string                `json:"drift"`
		Holds       []engine.UndoHoldOption `json:"holds"`
		ConfirmText string                  `json:"confirm_text"`
		Approval    string                  `json:"approval"`
		PreviewHash string                  `json:"preview_hash"`
		Reason      string                  `json:"reason"`
	}
	want(t, "preview", admin.do("GET", "/api/changes/"+orig.ID+"/undo", nil, &pv), 200)
	if !pv.Undoable || pv.Script != model.UndoScriptDisable || pv.ConfirmText != "jdoe" || pv.DryRun == nil || pv.DryRun.Status != "would" ||
		pv.Approval != "manual" || len(pv.Holds) != 1 || pv.PreviewHash == "" || pv.Drift == nil {
		t.Fatalf("preview: %+v", pv)
	}
	body := func(mod func(m map[string]any)) map[string]any {
		m := map[string]any{"reason": "created by mistake", "confirm": "jdoe", "preview_hash": pv.PreviewHash,
			"holds": []map[string]any{{"kind": model.HoldState, "key": ""}}}
		mod(m)
		return m
	}
	want(t, "wrong confirmation", admin.do("POST", "/api/changes/"+orig.ID+"/undo", body(func(m map[string]any) { m["confirm"] = "nope" }), nil), 400)
	want(t, "short reason", admin.do("POST", "/api/changes/"+orig.ID+"/undo", body(func(m map[string]any) { m["reason"] = "oops" }), nil), 400)
	want(t, "stale preview", admin.do("POST", "/api/changes/"+orig.ID+"/undo", body(func(m map[string]any) { m["preview_hash"] = "x" }), nil), 409)
	want(t, "approver cannot request", approver.do("POST", "/api/changes/"+orig.ID+"/undo", body(func(map[string]any) {}), nil), 403)
	var undo model.Change
	want(t, "request", admin.do("POST", "/api/changes/"+orig.ID+"/undo", body(func(map[string]any) {}), &undo), 201)
	if undo.Type != model.ChangeUndo || undo.Status != model.StatusPending || undo.UndoOf != orig.ID || !strings.HasPrefix(undo.Summary[0], "UNDO of create") {
		t.Fatalf("undo change: %+v", undo)
	}
	want(t, "only one undo at a time", admin.do("POST", "/api/changes/"+orig.ID+"/undo", body(func(map[string]any) {}), nil), 409)
	var o model.Change
	viewer.do("GET", "/api/changes/"+orig.ID, nil, &o)
	if o.UndoPending != undo.ID {
		t.Fatalf("undo_pending: %+v", o)
	}

	var check struct {
		DryRun      engine.UndoDryRun `json:"dry_run"`
		Steps       []string          `json:"steps"`
		ConfirmText string            `json:"confirm_text"`
	}
	want(t, "viewer cannot check", viewer.do("GET", "/api/changes/"+undo.ID+"/undo-check", nil, nil), 403)
	want(t, "undo check", approver.do("GET", "/api/changes/"+undo.ID+"/undo-check", nil, &check), 200)
	if check.DryRun.Status != "would" || check.ConfirmText != "jdoe" || len(check.Steps) == 0 {
		t.Fatalf("check: %+v", check)
	}

	var bulk struct {
		Results []struct {
			OK    bool
			Error string
		}
	}
	approver.do("POST", "/api/changes/bulk", map[string]any{"ids": []string{undo.ID}, "action": "approve"}, &bulk)
	if len(bulk.Results) != 1 || bulk.Results[0].OK || !strings.Contains(bulk.Results[0].Error, "one at a time") {
		t.Fatalf("bulk never approves an undo: %+v", bulk)
	}
	want(t, "approve without confirmation", approver.do("POST", "/api/changes/"+undo.ID+"/approve", nil, nil), 400)
	want(t, "approve with a wrong confirmation", approver.do("POST", "/api/changes/"+undo.ID+"/approve", map[string]string{"confirm": "bsmith"}, nil), 400)

	// undo.second_person: the requester (admin) may not approve; a settings PUT without "undo" keeps it.
	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	s.Undo.SecondPerson = true
	want(t, "settings", admin.do("PUT", "/api/settings", s, nil), 200)
	var raw map[string]any
	admin.do("GET", "/api/settings", nil, &raw)
	delete(raw, "undo")
	want(t, "settings without undo", admin.do("PUT", "/api/settings", raw, nil), 200)
	admin.do("GET", "/api/settings", nil, &s)
	if !s.Undo.SecondPerson || !s.Undo.Enabled {
		t.Fatalf("undo settings must be kept when omitted: %+v", s.Undo)
	}
	want(t, "requester cannot approve", admin.do("POST", "/api/changes/"+undo.ID+"/approve", map[string]string{"confirm": "jdoe"}, nil), 403)
	want(t, "another person approves", approver.do("POST", "/api/changes/"+undo.ID+"/approve", map[string]string{"confirm": "JDOE"}, nil), 200)
	if _, err := e.eng.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	u, _ := e.fake.ByExternal("E1")
	if u.Enabled {
		t.Fatal("the created account must be disabled by the undo")
	}
	viewer.do("GET", "/api/changes/"+orig.ID, nil, &o)
	if o.UndoneBy != undo.ID {
		t.Fatalf("undone_by: %+v", o)
	}
	var ident struct {
		model.Identity
		Holds []model.Hold `json:"holds"`
	}
	want(t, "identity", viewer.do("GET", "/api/identities/"+undo.IdentityID, nil, &ident), 200)
	if len(ident.Holds) != 1 || ident.Holds[0].Kind != model.HoldState || ident.Holds[0].CreatedBy != "admin" {
		t.Fatalf("holds: %+v", ident.Holds)
	}
	h := ident.Holds[0]
	want(t, "viewer cannot release", viewer.do("DELETE", "/api/identities/"+undo.IdentityID+"/holds/"+h.ID, nil, nil), 403)
	want(t, "release", approver.do("DELETE", "/api/identities/"+undo.IdentityID+"/holds/"+h.ID, nil, nil), 200)
	want(t, "released twice", approver.do("DELETE", "/api/identities/"+undo.IdentityID+"/holds/"+h.ID, nil, nil), 404)

	var notUndoable struct {
		Undoable bool   `json:"undoable"`
		Reason   string `json:"reason"`
	}
	admin.do("GET", "/api/changes/"+orig.ID+"/undo", nil, &notUndoable)
	if notUndoable.Undoable || !strings.Contains(notUndoable.Reason, "already undone") {
		t.Fatalf("already undone: %+v", notUndoable)
	}
	want(t, "missing change", admin.do("GET", "/api/changes/nope/undo", nil, nil), 404)
}

func TestUndoDriftNeedsForceOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver := e.login("admin"), e.login("approver")
	e.seedSource(admin)
	run := func() {
		var started struct{ ID string }
		admin.do("POST", "/api/runs", nil, &started)
		e.waitRun(admin, started.ID)
		var pend struct{ Items []model.Change }
		approver.do("GET", "/api/changes?status=pending", nil, &pend)
		for _, c := range pend.Items {
			approver.do("POST", "/api/changes/"+c.ID+"/approve", nil, nil)
		}
		if _, err := e.eng.Execute(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if err := os.WriteFile(e.csv, []byte("EmployeeID,FirstName,LastName,Dept\nE1,Jane,Doe,Marketing\nE2,Bobby,Smith,Eng\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run()
	var applied struct{ Items []model.Change }
	admin.do("GET", "/api/changes?status=applied&type=update", nil, &applied)
	if len(applied.Items) != 1 {
		t.Fatalf("one applied update: %+v", applied.Items)
	}
	upd := applied.Items[0]
	e.fake.Mutate("E2", func(u *ad.FakeUser) { u.Attrs["givenName"] = "Robert" })

	var pv struct {
		Drift       []string `json:"drift"`
		Warnings    []string `json:"warnings"`
		PreviewHash string   `json:"preview_hash"`
	}
	admin.do("GET", "/api/changes/"+upd.ID+"/undo", nil, &pv)
	if len(pv.Drift) != 1 || pv.Drift[0] != "givenName is 'Robert', expected 'Bobby'" || !strings.Contains(strings.Join(pv.Warnings, " "), "overwritten") {
		t.Fatalf("drift: %+v", pv)
	}
	req := map[string]any{"reason": "wrong first name", "confirm": "bsmith", "preview_hash": pv.PreviewHash}
	want(t, "drift without force", admin.do("POST", "/api/changes/"+upd.ID+"/undo", req, nil), 409)
	req["force"] = true
	var undo model.Change
	want(t, "with force", admin.do("POST", "/api/changes/"+upd.ID+"/undo", req, &undo), 201)
	if !undo.Payload.Undo.Force || !strings.HasPrefix(undo.Summary[1], "OVERWRITES later changes") {
		t.Fatalf("a forced undo says so: %+v", undo.Summary)
	}
}
