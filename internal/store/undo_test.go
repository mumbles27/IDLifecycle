package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func testUndoHolds(t *testing.T, s *Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	orig := model.Change{ID: "undo-orig", RunID: "r", IdentityID: "i1", ExternalID: "E1", Type: model.ChangeUpdate, Status: model.StatusApplied, CreatedAt: now}
	if err := s.InsertChange(ctx, orig); err != nil {
		t.Fatal(err)
	}
	title := "Analyst"
	st := model.AppliedState{Before: model.BeforeState{DN: "CN=a,OU=x", Attributes: map[string]*string{"title": &title, "pager": nil}}, Sam: "jdoe", At: now}
	if err := s.SetChangeAppliedState(ctx, orig.ID, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetChange(ctx, orig.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AppliedState == nil || got.AppliedState.Sam != "jdoe" || *got.AppliedState.Before.Attributes["title"] != "Analyst" {
		t.Fatalf("applied state not read back: %+v", got.AppliedState)
	}
	if v, ok := got.AppliedState.Before.Attributes["pager"]; !ok || v != nil {
		t.Fatalf("an empty attribute must come back as null: %+v", got.AppliedState.Before.Attributes)
	}

	undo := model.Change{ID: "undo-1", RunID: "", IdentityID: "i1", ExternalID: "E1", Type: model.ChangeUndo, Status: model.StatusPending, CreatedAt: now,
		Payload: model.ChangePayload{Undo: &model.UndoPayload{Of: orig.ID, OfType: model.ChangeUpdate, Script: model.UndoScriptSet}}}
	if err := s.InsertChange(ctx, undo); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetChange(ctx, orig.ID)
	if got.UndoPending != undo.ID {
		t.Fatalf("pending undo not reported: %q", got.UndoPending)
	}
	u, _ := s.GetChange(ctx, undo.ID)
	if u.UndoOf != orig.ID {
		t.Fatalf("undo_of not set: %q", u.UndoOf)
	}
	if err := s.SetChangeUndoneBy(ctx, orig.ID, undo.ID); err != nil {
		t.Fatal(err)
	}
	list, _, err := s.ListChanges(ctx, ChangeFilter{IdentityID: "i1", Status: model.StatusApplied}, Page{Limit: 10})
	if err != nil || len(list) != 1 || list[0].UndoneBy != undo.ID || list[0].AppliedState == nil {
		t.Fatalf("list: %v %+v", err, list)
	}

	later := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	for _, h := range []model.Hold{
		{ID: "h1", IdentityID: "i1", Kind: model.HoldAttribute, Key: "title", Reason: "wrong title", CreatedBy: "alice", CreatedAt: now, ChangeID: undo.ID},
		{ID: "h2", IdentityID: "i1", Kind: model.HoldState, Until: &later, CreatedBy: "alice", CreatedAt: now.Add(time.Second)},
		{ID: "h3", IdentityID: "i2", Kind: model.HoldOU, Until: &past, CreatedBy: "alice", CreatedAt: now},
	} {
		if err := s.InsertHold(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	hs, err := s.ListHolds(ctx, "i1")
	if err != nil || len(hs) != 2 || hs[0].Key != "title" || hs[0].Until != nil || hs[1].Until == nil || !hs[1].Until.Equal(later) {
		t.Fatalf("list holds: %v %+v", err, hs)
	}
	active, err := s.ActiveHolds(ctx, now)
	if err != nil || len(active["i1"]) != 2 || len(active["i2"]) != 0 {
		t.Fatalf("active holds (the expired one must be left out): %v %+v", err, active)
	}
	if _, err := s.DeleteHold(ctx, "i2", "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a hold of another identity must not be released: %v", err)
	}
	h, err := s.DeleteHold(ctx, "i1", "h1")
	if err != nil || h.Reason != "wrong title" {
		t.Fatalf("release: %v %+v", err, h)
	}
	if hs, _ := s.ListHolds(ctx, "i1"); len(hs) != 1 {
		t.Fatalf("hold not released: %+v", hs)
	}
}
