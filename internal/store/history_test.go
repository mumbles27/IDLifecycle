package store

import (
	"context"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func testIdentityHistory(t *testing.T, s *Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, c := range []model.Change{
		{ID: "hist-c1", RunID: "r", IdentityID: "hist-i1", ExternalID: "H1", Type: model.ChangeCreate, Status: model.StatusApplied, CreatedAt: now},
		{ID: "hist-c2", RunID: "r", IdentityID: "hist-other", ExternalID: "H2", Type: model.ChangeCreate, Status: model.StatusApplied, CreatedAt: now},
	} {
		if err := s.InsertChange(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateManualPerson(ctx, model.ManualPerson{ID: "hist-m1", SourceID: "hist-src", ExternalID: "H1", Attrs: map[string]string{"FirstName": "Ann"},
		CreatedBy: "alice", CreatedAt: now, UpdatedBy: "alice", UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	add := func(action, etype, eid string) {
		t.Helper()
		if err := s.AppendAudit(ctx, "system", action, etype, eid, map[string]any{"n": action}); err != nil {
			t.Fatal(err)
		}
	}
	add("manual.create", "manual_person", "hist-m1")          // 1: the console record (found by source + external id)
	add("identity.first_seen", "identity", "hist-i1")         // 2
	add("change.create", "change", "hist-c1")                 // 3: found through changes.identity_id (no identity in detail)
	add("change.create", "change", "hist-c2")                 // someone else's change
	add("identity.first_seen", "identity", "hist-other")      // someone else
	add("change.applied", "change", "hist-c1")                // 4
	add("identity.state", "identity", "hist-i1")              // 5
	add("manual.update", "manual_person", "hist-m-elsewhere") // another console record

	all, err := s.ListIdentityHistory(ctx, "hist-i1", "hist-src", "H1", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range all {
		actions = append(actions, e.Action)
	}
	want := []string{"identity.state", "change.applied", "change.create", "identity.first_seen", "manual.create"}
	if len(actions) != len(want) {
		t.Fatalf("history = %v, want %v", actions, want)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("history = %v, want %v (newest first)", actions, want)
		}
	}

	page1, err := s.ListIdentityHistory(ctx, "hist-i1", "hist-src", "H1", 0, 2)
	if err != nil || len(page1) != 2 {
		t.Fatalf("page 1: %v %d", err, len(page1))
	}
	page2, err := s.ListIdentityHistory(ctx, "hist-i1", "hist-src", "H1", page1[1].Seq, 2)
	if err != nil || len(page2) != 2 || page2[0].Action != "change.create" || page2[0].Seq >= page1[1].Seq {
		t.Fatalf("page 2: %v %+v", err, page2)
	}

	ev, ok, err := s.LastIdentityEvent(ctx, "hist-i1", "identity.first_seen", "identity.state")
	if err != nil || !ok || ev.Action != "identity.state" {
		t.Fatalf("last event: %v %v %+v", err, ok, ev)
	}
	if _, ok, err := s.LastIdentityEvent(ctx, "hist-i1", "identity.account_deleted"); err != nil || ok {
		t.Fatalf("no deleted event expected: %v %v", err, ok)
	}
}
