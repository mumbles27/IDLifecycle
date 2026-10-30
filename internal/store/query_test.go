package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseAuditQuery(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	f, err := ParseAuditQuery(`actor:alice by:bob action:change.* type:change id:c-1 level:error since:7d before:2026-10-25 "time out" disk -dryrun`, now, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.Actors, ",") != "alice,bob" || strings.Join(f.Actions, ",") != "change.*" || f.EntityType != "change" || f.EntityID != "c-1" || !f.ErrorsOnly {
		t.Errorf("filters: %+v", f)
	}
	if !f.From.Equal(now.Add(-7*24*time.Hour)) || !f.To.Equal(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("times: %v %v", f.From, f.To)
	}
	if strings.Join(f.Text, "|") != "time out|disk" || strings.Join(f.NotText, "|") != "dryrun" {
		t.Errorf("text: %q not %q", f.Text, f.NotText)
	}
	f, err = ParseAuditQuery(`actor:"system:auto" since:2026-10-01T08:30`, now, time.UTC)
	if err != nil || strings.Join(f.Actors, ",") != "system:auto" || f.From.Hour() != 8 {
		t.Errorf("quoted value / local datetime: %+v %v", f, err)
	}
	for _, bad := range []string{"colour:red", "level:info", "since:yesterday", "since:1d before:2d", "-id:x", "-level:error", "-actor:", "actor:"} {
		if _, err := ParseAuditQuery(bad, now, time.UTC); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	f, err = ParseAuditQuery(`-actor:system -by:bob -action:sync.* -type:Run`, now, time.UTC)
	if err != nil || strings.Join(f.NotActors, ",") != "system,bob" || strings.Join(f.NotActions, ",") != "sync.*" || strings.Join(f.NotEntityTypes, ",") != "run" || len(f.Actors) != 0 {
		t.Errorf("negated field terms: %+v %v", f, err)
	}
	if f, err := ParseAuditQuery("   ", now, time.UTC); err != nil || len(f.Text) != 0 {
		t.Errorf("empty query: %+v %v", f, err)
	}
}

func TestSearchAudit(t *testing.T) {
	for name, open := range dialects(t) {
		open := open
		t.Run(name, func(t *testing.T) { testSearchAudit(t, open(t)) })
	}
}

func testSearchAudit(t *testing.T, s *Store) {
	ctx := context.Background()
	add := func(actor, action, etype, eid string, detail map[string]any) {
		t.Helper()
		if err := s.AppendAudit(ctx, actor, action, etype, eid, detail); err != nil {
			t.Fatal(err)
		}
	}
	add("alice", "change.approve", "change", "c-1", map[string]any{"note": "looks fine"})
	add("system", "change.failed", "change", "c-2", map[string]any{"error": "LDAP timeout talking to dc01"})
	add("system", "change.applied", "change", "c-3", map[string]any{"error": ""})
	add("bob", "auth.login_failed", "user", "", map[string]any{"username": "bob"})
	add("system", "sync.finish", "run", "r-1", map[string]any{"status": "ok", "error": ""})
	add("alice", "settings.update", "settings", "settings", map[string]any{"changed": []string{"engine.dry_run"}})

	search := func(q string) []string {
		t.Helper()
		f, err := ParseAuditQuery(q, time.Now(), time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		items, total, err := s.ListAudit(ctx, f, Page{Limit: 50})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if total != len(items) {
			t.Errorf("%s: total %d for %d items", q, total, len(items))
		}
		var out []string
		for _, e := range items {
			out = append(out, e.Action)
		}
		return out
	}
	cases := map[string]string{
		"level:error":                  "auth.login_failed,change.failed",
		"action:change.*":              "change.applied,change.failed,change.approve",
		"action:change.* -approve":     "change.applied,change.failed",
		"actor:alice":                  "settings.update,change.approve",
		"actor:alice actor:bob":        "settings.update,auth.login_failed,change.approve",
		"timeout":                      "change.failed",
		"TIMEOUT dc01":                 "change.failed",
		`"looks fine"`:                 "change.approve",
		"dry_run":                      "settings.update",
		"type:user":                    "auth.login_failed",
		"id:c-3":                       "change.applied",
		"since:1h action:sync.finish":  "sync.finish",
		"before:2000-01-01":            "",
		"100%_wild":                    "",
		"-actor:system":                "settings.update,auth.login_failed,change.approve",
		"-actor:system -actor:bob":     "settings.update,change.approve",
		"action:change.* -actor:alice": "change.applied,change.failed",
		"-action:change.* -type:run":   "settings.update,auth.login_failed",
	}
	for q, want := range cases {
		if got := strings.Join(search(q), ","); got != want {
			t.Errorf("%s: got %q want %q", q, got, want)
		}
	}
}
