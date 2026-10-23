// Package storetest provides throwaway stores for other packages' tests.
package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"idlifecycle/internal/store"
)

// New opens a fresh, migrated SQLite store in a temp dir and closes it on cleanup.
func New(t testing.TB) *store.Store {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "t.db"))
	s, err := store.Open(context.Background(), store.DriverSQLite, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
