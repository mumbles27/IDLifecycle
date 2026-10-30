package api

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
)

func TestUndoPartsRunsAndManualOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	e.seedSource(admin)
	sync := func() string {
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
		return started.ID
	}
	sync()
	if err := os.WriteFile(e.csv, []byte("EmployeeID,FirstName,LastName,Dept\nE1,Janet,Doe,Marketing\nE2,Bobby,Smith,Eng\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runID := sync()

	// Per-item preview: an update has parts; a create is whole-only.
	var applied struct{ Items []model.Change }
	admin.do("GET", "/api/changes?status=applied&type=update&q=jane", nil, &applied)
	if len(applied.Items) != 1 {
		t.Fatalf("one applied update for Jane: %+v", applied.Items)
	}
	upd := applied.Items[0]
	var pv struct {
		Undoable    bool              `json:"undoable"`
		Items       []engine.UndoItem `json:"items"`
		Selected    []string          `json:"selected"`
		WholeOnly   bool              `json:"whole_only"`
		PreviewHash string            `json:"preview_hash"`
		ConfirmText string            `json:"confirm_text"`
	}
	want(t, "preview one part", admin.do("GET", "/api/changes/"+upd.ID+"/undo?item="+url.QueryEscape("attribute:givenName"), nil, &pv), 200)
	if !pv.Undoable || pv.WholeOnly || len(pv.Items) != 1 || pv.Items[0].Key != "attribute:givenName" || len(pv.Selected) != 1 {
		t.Fatalf("part preview: %+v", pv)
	}
	want(t, "unknown part", admin.do("GET", "/api/changes/"+upd.ID+"/undo?items="+url.QueryEscape(`["attribute:nope"]`), nil, nil), 400)
	var creates struct{ Items []model.Change }
	admin.do("GET", "/api/changes?status=applied&type=create", nil, &creates)
	want(t, "parts of a create", admin.do("GET", "/api/changes/"+creates.Items[0].ID+"/undo?item=ou", nil, nil), 400)

	// Undo across the run (both updates), then approve the batch with UNDO 2.
	want(t, "viewer cannot list a run undo", viewer.do("GET", "/api/runs/"+runID+"/undo", nil, nil), 403)
	var list struct {
		Run     model.Run             `json:"run"`
		Changes []engine.RunUndoEntry `json:"changes"`
	}
	want(t, "run undo list", admin.do("GET", "/api/runs/"+runID+"/undo", nil, &list), 200)
	if len(list.Changes) != 2 || list.Run.ID != runID {
		t.Fatalf("run undo list: %+v", list)
	}
	sel := map[string][]string{}
	for _, c := range list.Changes {
		sel[c.Change.ID] = []string{"*"}
	}
	var rp struct {
		Count       int    `json:"count"`
		People      int    `json:"people"`
		ConfirmText string `json:"confirm_text"`
		PreviewHash string `json:"preview_hash"`
		Changes     []struct {
			ChangeID string            `json:"change_id"`
			DryRun   engine.UndoDryRun `json:"dry_run"`
		} `json:"changes"`
	}
	want(t, "approver cannot preview a run undo", approver.do("POST", "/api/runs/"+runID+"/undo/preview", map[string]any{"selection": sel}, nil), 403)
	want(t, "run undo preview", admin.do("POST", "/api/runs/"+runID+"/undo/preview", map[string]any{"selection": sel}, &rp), 200)
	if rp.Count != 2 || rp.People != 2 || rp.ConfirmText != "UNDO 2" || len(rp.Changes) != 2 || rp.Changes[0].DryRun.Status != "would" {
		t.Fatalf("run preview: %+v", rp)
	}
	req := map[string]any{"selection": sel, "reason": "HR sent wrong names", "confirm": "UNDO 1", "preview_hash": rp.PreviewHash}
	want(t, "wrong count", admin.do("POST", "/api/runs/"+runID+"/undo", req, nil), 400)
	req["confirm"] = "UNDO 2"
	var made struct {
		Batch   string         `json:"batch"`
		Changes []model.Change `json:"changes"`
	}
	want(t, "run undo request", admin.do("POST", "/api/runs/"+runID+"/undo", req, &made), 201)
	if made.Batch == "" || len(made.Changes) != 2 {
		t.Fatalf("batch: %+v", made)
	}
	var bv struct {
		Batch       string `json:"batch"`
		RequestedBy string `json:"requested_by"`
		Total       int    `json:"total"`
		Pending     int    `json:"pending"`
		ConfirmText string `json:"confirm_text"`
		Changes     []struct {
			ChangeID string             `json:"change_id"`
			Change   model.Change       `json:"change"`
			DryRun   *engine.UndoDryRun `json:"dry_run"`
		} `json:"changes"`
	}
	want(t, "viewer cannot see a batch", viewer.do("GET", "/api/undo-batches/"+made.Batch, nil, nil), 403)
	want(t, "batch", approver.do("GET", "/api/undo-batches/"+made.Batch, nil, &bv), 200)
	if bv.Total != 2 || bv.Pending != 2 || bv.ConfirmText != "UNDO 2" || bv.RequestedBy != "admin" || len(bv.Changes) != 2 || bv.Changes[0].DryRun == nil {
		t.Fatalf("batch view: %+v", bv)
	}
	want(t, "unknown batch", approver.do("GET", "/api/undo-batches/nope", nil, nil), 404)
	want(t, "wrong batch confirmation", approver.do("POST", "/api/undo-batches/"+made.Batch+"/approve", map[string]string{"confirm": "UNDO 1"}, nil), 400)
	var ok struct {
		Batch    string         `json:"batch"`
		Approved []model.Change `json:"approved"`
	}
	want(t, "approve batch", approver.do("POST", "/api/undo-batches/"+made.Batch+"/approve", map[string]string{"confirm": "undo 2"}, &ok), 200)
	if len(ok.Approved) != 2 || ok.Approved[0].Status != model.StatusApproved {
		t.Fatalf("approved: %+v", ok)
	}
	if _, err := e.eng.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.fake.ByExternal("E1"); u.Attrs["givenName"] != "Jane" {
		t.Fatalf("Jane's first name back: %+v", u.Attrs)
	}

	// Manual pins: viewers see them, approvers set and release them; the list filters and counts them.
	ident := upd.IdentityID
	want(t, "viewer cannot pin", viewer.do("POST", "/api/identities/"+ident+"/holds", map[string]any{"kind": "all", "reason": "keep everything as it is"}, nil), 403)
	want(t, "short reason", approver.do("POST", "/api/identities/"+ident+"/holds", map[string]any{"kind": "all", "reason": "x"}, nil), 400)
	var pin model.Hold
	want(t, "pin the person", approver.do("POST", "/api/identities/"+ident+"/holds", map[string]any{"kind": "all", "reason": "keep everything as it is"}, &pin), 201)
	var idents struct {
		Items []struct {
			ID          string `json:"id"`
			ManualCount int    `json:"manual_count"`
		}
		Total int
	}
	want(t, "manual filter", viewer.do("GET", "/api/identities?manual=1", nil, &idents), 200)
	counts := map[string]int{}
	for _, it := range idents.Items {
		counts[it.ID] = it.ManualCount
	}
	// Both undos pinned givenName (the run undo marks undone parts Manual by default); Jane also has the person pin.
	if idents.Total != 2 || counts[ident] != 2 {
		t.Fatalf("manual people: %+v", idents)
	}
	var mv engine.ManagedView
	want(t, "managed", viewer.do("GET", "/api/identities/"+ident+"/managed", nil, &mv), 200)
	if mv.Person.Mode != "manual" || len(mv.Attributes) == 0 || !mv.Account {
		t.Fatalf("managed: %+v", mv)
	}
	want(t, "set back to Auto", approver.do("DELETE", "/api/identities/"+ident+"/holds/"+pin.ID, map[string]string{"note": "back from leave"}, nil), 200)
	var hist struct {
		Items []struct {
			Title string   `json:"title"`
			Lines []string `json:"lines"`
		} `json:"items"`
	}
	viewer.do("GET", "/api/identities/"+ident+"/history", nil, &hist)
	titles := []string{}
	for _, it := range hist.Items {
		titles = append(titles, it.Title)
	}
	all := strings.Join(titles, "\n")
	if !strings.Contains(all, "Set back to Auto by approver") || !strings.Contains(all, "Set to Manual: the whole person") {
		t.Fatalf("history titles:\n%s", all)
	}
}
