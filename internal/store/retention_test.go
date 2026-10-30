package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func testRetention(t *testing.T, s *Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	p := model.RetentionPolicy{ID: "ret-1", Name: "Executives", Description: "keep a year", Enabled: true, Priority: 5, Match: "any",
		Conditions: []model.Condition{{Attr: "ad.memberOf", Op: "contains", Value: "GG-Exec"}}, DeleteAfterDays: 365,
		DisabledOU: "OU=Legal Hold,DC=x", ExtraKeepGroups: []string{"GG-Exec"}, Approval: model.RetentionApprovalManual, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRetentionPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	dup := p
	dup.ID = "ret-2"
	if err := s.CreateRetentionPolicy(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	got, err := s.GetRetentionPolicy(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Executives" || got.Match != "any" || got.DeleteAfterDays != 365 || got.DisabledOU != p.DisabledOU || got.Approval != "manual" ||
		len(got.Conditions) != 1 || len(got.ExtraKeepGroups) != 1 || !got.Enabled || got.Priority != 5 {
		t.Fatalf("read back: %+v", got)
	}
	never := model.RetentionPolicy{ID: "ret-0", Name: "Never", Enabled: true, Priority: 1, Match: "all", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateRetentionPolicy(ctx, never); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListRetentionPolicies(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "ret-0" {
		t.Fatalf("priority order: %+v %v", list, err)
	}
	if list[0].Conditions == nil || list[0].ExtraKeepGroups == nil {
		t.Fatal("empty lists must come back as [] not null")
	}
	got.DeleteAfterDays = 0
	if err := s.UpdateRetentionPolicy(ctx, got); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetRetentionPolicy(ctx, p.ID); g.DeleteAfterDays != 0 {
		t.Fatalf("update: %+v", g)
	}

	// An identity assigned to a policy, and the resolved retention.
	if err := s.CreateSource(ctx, model.Source{ID: "ret-src", Name: "ret-src", Type: "csv", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	id := model.Identity{ID: "ret-i1", SourceID: "ret-src", ExternalID: "R1", Display: "R One", Attrs: map[string]string{}, FirstSeen: now, LastSeen: now, State: model.StateNew}
	if err := s.InsertIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIdentityRetention(ctx, id.ID, p.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	x, _ := s.GetIdentity(ctx, id.ID)
	if x.RetentionPolicyID != p.ID || x.RetentionAssignedBy != "alice" {
		t.Fatalf("assignment: %+v", x)
	}
	// A sync's UpdateIdentity never touches the assignment.
	x.Display = "R One (renamed)"
	if err := s.UpdateIdentity(ctx, x); err != nil {
		t.Fatal(err)
	}
	if x, _ = s.GetIdentity(ctx, id.ID); x.RetentionPolicyID != p.ID {
		t.Fatalf("assignment lost: %+v", x)
	}
	if err := s.SetIdentityRetention(ctx, "nobody", p.ID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown identity: %v", err)
	}

	if _, err := s.GetIdentityRetention(ctx, id.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing resolved yet: %v", err)
	}
	due := now.Add(365 * 24 * time.Hour)
	r := model.IdentityRetention{IdentityID: id.ID, PolicyID: p.ID, PolicyName: "Executives", Reason: model.RetentionReasonAssigned, DeleteDue: &due, ResolvedAt: now}
	if err := s.PutIdentityRetention(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.DeleteDue, r.Reason = nil, model.RetentionReasonRule
	if err := s.PutIdentityRetention(ctx, r); err != nil {
		t.Fatal(err)
	}
	gr, err := s.GetIdentityRetention(ctx, id.ID)
	if err != nil || gr.Reason != "rule" || gr.DeleteDue != nil || gr.PolicyName != "Executives" {
		t.Fatalf("replaced: %+v %v", gr, err)
	}
	all, _ := s.ListIdentityRetention(ctx)
	counts, _ := s.RetentionCounts(ctx)
	if len(all) != 1 || counts[p.ID] != 1 {
		t.Fatalf("list %+v counts %+v", all, counts)
	}

	// Deleting a policy clears the assignments to it.
	if err := s.DeleteRetentionPolicy(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if x, _ = s.GetIdentity(ctx, id.ID); x.RetentionPolicyID != "" || x.RetentionAssignedBy != "" {
		t.Fatalf("assignment kept after delete: %+v", x)
	}
	if err := s.DeleteRetentionPolicy(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
