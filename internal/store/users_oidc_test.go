package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func TestUserAuthSourceAndExternalID(t *testing.T) {
	ctx := context.Background()
	for name, open := range dialects(t) {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			if err := s.CreateUser(ctx, model.User{ID: "u1", Username: "local1", PasswordHash: "h", Role: model.RoleAdmin, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateUser(ctx, model.User{ID: "u2", Username: "jane@corp.test", Role: model.RoleViewer, CreatedAt: time.Now(),
				AuthSource: model.AuthOIDC, ExternalID: "tid:oid-1"}); err != nil {
				t.Fatal(err)
			}
			u, err := s.GetUser(ctx, "u1")
			if err != nil || u.AuthSource != model.AuthLocal || u.ExternalID != "" {
				t.Fatalf("local user defaults: %+v %v", u, err)
			}
			o, err := s.GetUserByExternalID(ctx, "tid:oid-1")
			if err != nil || o.ID != "u2" || o.AuthSource != model.AuthOIDC {
				t.Fatalf("by external id: %+v %v", o, err)
			}
			if _, err := s.GetUserByExternalID(ctx, ""); !errors.Is(err, ErrNotFound) {
				t.Errorf("an empty external id must never match local users: %v", err)
			}
			if _, err := s.GetUserByExternalID(ctx, "tid:other"); !errors.Is(err, ErrNotFound) {
				t.Errorf("unknown external id: %v", err)
			}
		})
	}
}

// A database created before migration 2 gets the new columns with defaults; existing users stay local.
func TestMigrationTwoUpgradesExistingUsers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	dsn := "file:" + filepath.ToSlash(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	old := &Store{driver: DriverSQLite}
	for _, st := range migrations[0].stmts {
		if _, err := db.Exec(old.ddl(st)); err != nil {
			t.Fatal(err)
		}
	}
	for _, st := range []string{
		old.ddl(createMigrations),
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, '2026-01-01T00:00:00Z')`,
		`INSERT INTO users (id, username, password_hash, role, disabled, failed_logins, locked_until, created_at, last_login_at)
		 VALUES ('u1', 'admin', 'h', 'admin', 0, 0, '', '2026-01-01T00:00:00Z', '')`,
	} {
		if _, err := db.Exec(st); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s := mustOpen(t, DriverSQLite, dsn)
	u, err := s.GetUserByName(ctx, "admin")
	if err != nil || u.AuthSource != model.AuthLocal || u.ExternalID != "" || u.Role != model.RoleAdmin {
		t.Fatalf("upgraded user: %+v %v", u, err)
	}
}
