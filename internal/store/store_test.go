package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

// dialects returns the databases under test. SQLite always runs; the others run
// when a DSN is provided through the environment so CI can opt in.
func dialects(t *testing.T) map[string]func(t *testing.T) *Store {
	t.Helper()
	m := map[string]func(t *testing.T) *Store{
		DriverSQLite: func(t *testing.T) *Store {
			dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db"))
			return mustOpen(t, DriverSQLite, dsn)
		},
	}
	for drv, env := range map[string]string{
		DriverPostgres: "IDL_TEST_POSTGRES_DSN",
		DriverMSSQL:    "IDL_TEST_MSSQL_DSN",
		DriverMySQL:    "IDL_TEST_MYSQL_DSN",
		DriverDB2:      "IDL_TEST_DB2_DSN", // full ODBC connection string; needs the IBM ODBC/CLI driver
	} {
		if dsn := os.Getenv(env); dsn != "" {
			drv, dsn := drv, dsn
			m[drv] = func(t *testing.T) *Store {
				s := mustOpen(t, drv, dsn)
				resetTables(t, s)
				return s
			}
		}
	}
	return m
}

func mustOpen(t *testing.T, drv, dsn string) *Store {
	t.Helper()
	s, err := Open(context.Background(), drv, dsn)
	if err != nil {
		t.Fatalf("open %s: %v", drv, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func resetTables(t *testing.T, s *Store) {
	t.Helper()
	for _, tbl := range []string{"audit_log", "manual_people", "identity_retention", "retention_policies", "identity_hold_state", "identity_holds", "change_undo_items", "change_undo", "changes", "sync_runs", "role_rules", "mappings", "identities", "sources", "sessions", "users", "settings"} {
		if _, err := s.exec(context.Background(), "DELETE FROM "+tbl); err != nil {
			t.Fatalf("reset %s: %v", tbl, err)
		}
	}
}

func TestStoreSuite(t *testing.T) {
	for name, open := range dialects(t) {
		open := open
		t.Run(name, func(t *testing.T) {
			s := open(t)
			t.Run("settings", func(t *testing.T) { testSettings(t, s) })
			t.Run("users_sessions", func(t *testing.T) { testUsers(t, s) })
			t.Run("sources_identities", func(t *testing.T) { testSourcesIdentities(t, s) })
			t.Run("mappings_roles", func(t *testing.T) { testMappingsRoles(t, s) })
			t.Run("changes", func(t *testing.T) { testChanges(t, s) })
			t.Run("audit", func(t *testing.T) { testAudit(t, s) })
			t.Run("undo_holds", func(t *testing.T) { testUndoHolds(t, s) })
			t.Run("retention", func(t *testing.T) { testRetention(t, s) })
			t.Run("identity_history", func(t *testing.T) { testIdentityHistory(t, s) })
			t.Run("tables", func(t *testing.T) { testTables(t, s) })
		})
	}
}

func testSettings(t *testing.T, s *Store) {
	ctx := context.Background()
	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Engine.DryRun || got.Approval.Create != "manual" {
		t.Fatalf("defaults must be conservative: %+v", got)
	}
	got.Engine.DryRun = false
	got.Schedule.IntervalMinutes = 15
	if err := s.SaveSettings(ctx, got, "tester"); err != nil {
		t.Fatal(err)
	}
	got.Schedule.IntervalMinutes = 30
	if err := s.SaveSettings(ctx, got, "tester"); err != nil { // second save exercises the update path
		t.Fatal(err)
	}
	again, _ := s.GetSettings(ctx)
	if again.Engine.DryRun || again.Schedule.IntervalMinutes != 30 {
		t.Fatalf("settings not persisted: %+v", again)
	}
}

func testUsers(t *testing.T, s *Store) {
	ctx := context.Background()
	u := model.User{ID: "u1", Username: "alice", PasswordHash: "h", Role: model.RoleAdmin, CreatedAt: time.Now()}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, model.User{ID: "u2", Username: "alice", PasswordHash: "h", Role: model.RoleViewer, CreatedAt: time.Now()}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username should conflict, got %v", err)
	}
	got, err := s.GetUserByName(ctx, "alice")
	if err != nil || got.ID != "u1" {
		t.Fatalf("lookup: %v %+v", err, got)
	}
	if _, err := s.GetUser(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.RecordLoginFailure(ctx, "u1", 3, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.GetUser(ctx, "u1")
	if got.LockedUntil == nil || !got.LockedUntil.After(time.Now()) {
		t.Fatalf("expected lockout after 3 failures: %+v", got)
	}
	if err := s.RecordLoginSuccess(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUser(ctx, "u1")
	if got.LockedUntil != nil || got.LastLoginAt == nil {
		t.Fatalf("success should clear lock and set last login: %+v", got)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 1 {
		t.Fatalf("active admins = %d", n)
	}

	se := model.Session{TokenHash: "th", UserID: "u1", CSRFToken: "c", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), LastSeenAt: time.Now()}
	if err := s.CreateSession(ctx, se); err != nil {
		t.Fatal(err)
	}
	gs, err := s.GetSession(ctx, "th")
	if err != nil || gs.CSRFToken != "c" {
		t.Fatalf("session: %v %+v", err, gs)
	}
	if err := s.DeleteUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "th"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a user must delete their sessions, got %v", err)
	}
}

func testSourcesIdentities(t *testing.T, s *Store) {
	ctx := context.Background()
	now := time.Now()
	src := model.Source{ID: "s1", Name: "csv", Type: "csv", Enabled: true, CreatedAt: now, UpdatedAt: now,
		Options: model.SourceOptions{ExternalIDField: "ID", TerminatedValues: []string{"T"}}, Config: map[string]any{"path": "x.csv"}}
	if err := s.CreateSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSource(ctx, model.Source{ID: "s2", Name: "csv", Type: "csv", CreatedAt: now, UpdatedAt: now}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate source name: %v", err)
	}
	got, err := s.GetSource(ctx, "s1")
	if err != nil || got.Options.ExternalIDField != "ID" || got.Config["path"] != "x.csv" || got.LastStatus != "never" {
		t.Fatalf("source round trip: %v %+v", err, got)
	}

	id := model.Identity{ID: "i1", SourceID: "s1", ExternalID: "E1", Display: "Jane Doe", Attrs: map[string]string{"a": "b"}, Active: true,
		FirstSeen: now, LastSeen: now, Applied: map[string]string{}, State: model.StateNew}
	if err := s.InsertIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	dup := id
	dup.ID = "i2"
	if err := s.InsertIdentity(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("(source, external_id) must be unique: %v", err)
	}
	id.ADGUID, id.State, id.Applied = "g1", model.StateLinked, map[string]string{"title": "Dev"}
	if err := s.UpdateIdentity(ctx, id); err != nil {
		t.Fatal(err)
	}
	list, total, err := s.ListIdentities(ctx, IdentityFilter{Q: "jane"}, Page{})
	if err != nil || total != 1 || list[0].SourceName != "csv" || list[0].Applied["title"] != "Dev" || list[0].State != model.StateLinked {
		t.Fatalf("identity list: %v total=%d %+v", err, total, list)
	}
	if _, total, _ = s.ListIdentities(ctx, IdentityFilter{Q: "100%"}, Page{}); total != 0 {
		t.Fatal("LIKE wildcards in user input must be escaped")
	}
	if err := s.DeleteSource(ctx, "s1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("source with identities must not be deletable: %v", err)
	}
}

func testMappingsRoles(t *testing.T, s *Store) {
	ctx := context.Background()
	now := time.Now()
	m := model.Mapping{ID: "m1", Name: "Title", TargetAttr: "title", Template: "{{ .Title }}", Mode: model.ModeSync, OnEmpty: "skip", ValueType: "string", Enabled: true, SortOrder: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	m2 := m
	m2.ID, m2.Name, m2.SortOrder = "m2", "Dept", 1
	if err := s.CreateMapping(ctx, m2); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListMappings(ctx)
	if len(list) != 2 || list[0].ID != "m2" {
		t.Fatalf("mappings must be ordered by sort_order: %+v", list)
	}
	m.Mode = model.ModeImmutable
	if err := s.UpdateMapping(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMapping(ctx, "m1"); got.Mode != model.ModeImmutable {
		t.Fatalf("update not persisted: %+v", got)
	}
	if err := s.DeleteMapping(ctx, "m1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMapping(ctx, "m1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	r := model.RoleRule{ID: "r1", Name: "Marketing", Enabled: true, Priority: 10, Match: "all",
		Conditions: []model.Condition{{Attr: "Dept", Op: "eq", Value: "Marketing"}},
		Grants:     model.Grants{Groups: []string{"GG-Mkt"}, Attributes: []model.AttrGrant{{TargetAttr: "extensionAttribute1", Template: "Mkt", Mode: model.ModeSync}}},
		CreatedAt:  now, UpdatedAt: now}
	if err := s.CreateRole(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRole(ctx, "r1")
	if err != nil || got.Grants.Groups[0] != "GG-Mkt" || got.Conditions[0].Value != "Marketing" || got.Grants.Attributes[0].TargetAttr != "extensionAttribute1" {
		t.Fatalf("role round trip: %v %+v", err, got)
	}
}

func newChange(id, ident, typ, status string, at time.Time) model.Change {
	return model.Change{ID: id, RunID: "run1", IdentityID: ident, ExternalID: "E-" + ident, Display: "Person " + ident, Type: typ, Status: status,
		Payload: model.ChangePayload{Attributes: map[string]string{"title": "x"}}, Summary: []string{"s"}, PayloadHash: "h" + id, CreatedAt: at}
}

func testChanges(t *testing.T, s *Store) {
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, c := range []model.Change{
		newChange("c1", "a", model.ChangeCreate, model.StatusPending, base),
		newChange("c2", "b", model.ChangeUpdate, model.StatusPending, base.Add(time.Minute)),
		newChange("c3", "c", model.ChangeDisable, model.StatusApproved, base.Add(2*time.Minute)),
	} {
		if err := s.InsertChange(ctx, c); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	list, total, err := s.ListChanges(ctx, ChangeFilter{Status: model.StatusPending}, Page{})
	if err != nil || total != 2 || list[0].ID != "c2" {
		t.Fatalf("filter+order: %v total=%d %+v", err, total, list)
	}
	pg, total, _ := s.ListChanges(ctx, ChangeFilter{}, Page{Limit: 2, Offset: 2})
	if total != 3 || len(pg) != 1 || pg[0].ID != "c1" {
		t.Fatalf("paging: total=%d %+v", total, pg)
	}

	// compare-and-set protects against double approval.
	if err := s.TransitionChange(ctx, "c1", model.StatusPending, model.StatusApproved, "alice", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionChange(ctx, "c1", model.StatusPending, model.StatusRejected, "bob", "no"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second decision must conflict, got %v", err)
	}
	got, _ := s.GetChange(ctx, "c1")
	if got.Status != model.StatusApproved || got.DecidedBy != "alice" || got.DecidedAt == nil {
		t.Fatalf("decision not recorded: %+v", got)
	}

	if err := s.TransitionChange(ctx, "c1", model.StatusApproved, model.StatusApplying, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishChange(ctx, "c1", model.StatusApplied, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetChange(ctx, "c1")
	if got.Status != model.StatusApplied || got.Attempts != 1 || got.AppliedAt == nil {
		t.Fatalf("finish: %+v", got)
	}
	if err := s.FinishChange(ctx, "c1", model.StatusFailed, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("an applied change must not change again: %v", err)
	}

	_ = s.TransitionChange(ctx, "c3", model.StatusApproved, model.StatusApplying, "", "")
	if n, err := s.FailInterruptedChanges(ctx); err != nil || n != 1 {
		t.Fatalf("interrupted: %v %d", err, n)
	}
	got, _ = s.GetChange(ctx, "c3")
	if got.Status != model.StatusFailed || got.Error == "" {
		t.Fatalf("interrupted change: %+v", got)
	}

	run := model.Run{ID: "run1", Trigger: "manual", TriggeredBy: "alice", StartedAt: time.Now(), Status: model.RunRunning, Counters: map[string]int{}}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.MarkStaleRunsAborted(ctx); n != 1 {
		t.Fatalf("stale runs = %d", n)
	}
	r, _ := s.GetRun(ctx, "run1")
	if r.Status != model.RunAborted || r.FinishedAt == nil {
		t.Fatalf("run: %+v", r)
	}
}

func testAudit(t *testing.T, s *Store) {
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := s.AppendAudit(ctx, "alice", "change.approve", "change", "c1", map[string]any{"n": i, "note": "ünïcode"}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.VerifyAudit(ctx)
	if err != nil || !res.OK || res.Checked != 5 {
		t.Fatalf("verify: %v %+v", err, res)
	}
	list, total, _ := s.ListAudit(ctx, AuditFilter{Actor: "alice"}, Page{Limit: 2})
	if total != 5 || len(list) != 2 || list[0].Seq != 5 {
		t.Fatalf("list audit: total=%d %+v", total, list)
	}

	// Tampering with a row must be detected, and so must deleting one.
	if _, err := s.exec(ctx, `UPDATE audit_log SET actor = ? WHERE seq = ?`, "mallory", 3); err != nil {
		t.Fatal(err)
	}
	res, _ = s.VerifyAudit(ctx)
	if res.OK || res.FirstBadSeq != 3 {
		t.Fatalf("tamper not detected: %+v", res)
	}
	if _, err := s.exec(ctx, `UPDATE audit_log SET actor = ? WHERE seq = ?`, "alice", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exec(ctx, `DELETE FROM audit_log WHERE seq = ?`, 2); err != nil {
		t.Fatal(err)
	}
	res, _ = s.VerifyAudit(ctx)
	if res.OK || res.FirstBadSeq != 3 {
		t.Fatalf("deletion not detected: %+v", res)
	}
}

func testTables(t *testing.T, s *Store) {
	ctx := context.Background()
	v, err := s.BrowseTable(ctx, "changes", "", Page{})
	if err != nil || len(v.Columns) == 0 || v.Total == 0 {
		t.Fatalf("browse: %v %+v", err, v)
	}
	if _, err := s.BrowseTable(ctx, "users", "", Page{}); !errors.Is(err, ErrNotFound) {
		t.Fatal("users table must not be browsable")
	}
	if _, err := s.BrowseTable(ctx, "sources; DROP TABLE users", "", Page{}); !errors.Is(err, ErrNotFound) {
		t.Fatal("table names must be allow-listed")
	}
	for _, c := range v.Columns {
		if c == "password_hash" || c == "config_json" {
			t.Fatalf("sensitive column %s exposed", c)
		}
	}
}

func TestUnsupportedDriver(t *testing.T) {
	if _, err := Open(context.Background(), "oracle", "x"); err == nil {
		t.Fatal("expected error for unsupported driver")
	}
}

func TestRebind(t *testing.T) {
	cases := map[string]string{DriverPostgres: "a = $1 AND b = $2", DriverMSSQL: "a = @p1 AND b = @p2", DriverSQLite: "a = ? AND b = ?", DriverMySQL: "a = ? AND b = ?"}
	for drv, want := range cases {
		s := &Store{driver: drv}
		if got := s.rebind("a = ? AND b = ?"); got != want {
			t.Errorf("%s: got %q want %q", drv, got, want)
		}
	}
	ms := &Store{driver: DriverMSSQL}
	q, args := ms.pageClause(Page{Limit: 10, Offset: 20})
	if q != " OFFSET ? ROWS FETCH NEXT ? ROWS ONLY" || args[0] != 20 || args[1] != 10 {
		t.Errorf("mssql paging: %q %v", q, args)
	}
}
