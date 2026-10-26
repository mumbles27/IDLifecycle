//go:build windows

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexbrainman/odbc"
)

// The ODBC driver must be linked and reach the Windows driver manager without cgo:
// an unknown ODBC driver has to fail inside ODBC (IM002), not with "unknown driver".
func TestODBCDriverLinkedWithoutCgo(t *testing.T) {
	db, err := sql.Open("odbc", "Driver={No Such Driver};")
	if err != nil {
		t.Fatalf("sql.Open(odbc): %v (driver not registered?)", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = db.PingContext(ctx)
	var oe *odbc.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want an ODBC driver-manager error, got %T: %v", err, err)
	}
	if len(oe.Diag) == 0 || oe.Diag[0].State != "IM002" {
		t.Fatalf("want SQLSTATE IM002 (driver not found), got %v", oe)
	}
}

// Open(db2) goes through the same path and reports a connect error that names the driver.
func TestOpenDB2WithoutClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := Open(ctx, DriverDB2, "Driver={No Such Driver};Database=IDLDB;Uid=u;Pwd=Secret123;")
	if err == nil {
		t.Fatal("expected a connect error")
	}
	if !strings.HasPrefix(err.Error(), "connect db2:") || !strings.Contains(err.Error(), "IM002") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(err.Error(), "Secret123") {
		t.Fatalf("connect error leaks the password: %v", err)
	}
}
