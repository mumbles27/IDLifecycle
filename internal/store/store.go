// Package store implements persistence on top of database/sql for SQLite,
// PostgreSQL, SQL Server and MySQL/MariaDB using one set of portable SQL.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "modernc.org/sqlite"
)

// Supported driver names for the bootstrap config.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
	DriverMSSQL    = "mssql"
	DriverMySQL    = "mysql"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

const tsLayout = "2006-01-02T15:04:05.000Z"

type Store struct {
	db       *sql.DB
	driver   string
	auditMu  sync.Mutex
	auditKey []byte
}

// Open connects to the database and applies pending migrations.
func Open(ctx context.Context, driver, dsn string) (*Store, error) {
	sqlName, err := sqlDriverName(driver)
	if err != nil {
		return nil, err
	}
	switch driver {
	case DriverSQLite:
		dsn = sqliteDSN(dsn)
	case DriverMySQL:
		if !strings.Contains(strings.ToLower(dsn), "clientfoundrows") {
			sep := "?"
			if strings.Contains(dsn, "?") {
				sep = "&"
			}
			dsn += sep + "clientFoundRows=true&parseTime=false"
		}
	}

	db, err := sql.Open(sqlName, dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", driver, err)
	}
	if driver == DriverSQLite {
		// One connection avoids SQLITE_BUSY between readers/writers and makes :memory: usable.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(30 * time.Minute)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect %s: %w", driver, err)
	}

	s := &Store{db: db, driver: driver}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func sqlDriverName(driver string) (string, error) {
	switch driver {
	case DriverSQLite:
		return "sqlite", nil
	case DriverPostgres:
		return "pgx", nil
	case DriverMSSQL:
		return "sqlserver", nil
	case DriverMySQL:
		return "mysql", nil
	}
	return "", fmt.Errorf("unsupported database driver %q (use sqlite, postgres, mssql or mysql)", driver)
}

func sqliteDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
}

// SetAuditKey makes the audit hash chain an HMAC, so editing history needs the key as well as DB write access.
// Set it once at startup, before any audit entry is written.
func (s *Store) SetAuditKey(k []byte) { s.auditKey = append([]byte(nil), k...) }

func (s *Store) Driver() string { return s.driver }
func (s *Store) Close() error   { return s.db.Close() }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// rebind converts ? placeholders to the driver's style.
func (s *Store) rebind(q string) string {
	var prefix string
	switch s.driver {
	case DriverPostgres:
		prefix = "$"
	case DriverMSSQL:
		prefix = "@p"
	default:
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString(prefix)
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// pageClause returns the LIMIT/OFFSET suffix and its args in the order the dialect expects.
// The caller must have an ORDER BY (required by SQL Server).
func (s *Store) pageClause(p Page) (string, []any) {
	p = p.normalized()
	if s.driver == DriverMSSQL {
		return " OFFSET ? ROWS FETCH NEXT ? ROWS ONLY", []any{p.Offset, p.Limit}
	}
	return " LIMIT ? OFFSET ?", []any{p.Limit, p.Offset}
}

func (s *Store) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, s.rebind(q), args...)
}

func (s *Store) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, s.rebind(q), args...)
}

func (s *Store) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, s.rebind(q), args...)
}

// affected runs an UPDATE/DELETE and returns rows affected.
func (s *Store) affected(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := s.exec(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	t, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

// IsUniqueViolation reports whether err looks like a unique/primary key violation on any driver.
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "unique constraint") || // sqlite, mssql ("violation of unique key constraint")
		strings.Contains(m, "duplicate key") || // postgres, mssql
		strings.Contains(m, "duplicate entry") || // mysql
		strings.Contains(m, "violation of primary key") ||
		strings.Contains(m, "constraint failed: unique")
}

// ---- time + json helpers -------------------------------------------------

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func tsp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return ts(*t)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(tsLayout, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func parseTSP(s string) *time.Time {
	t := parseTS(s)
	if t.IsZero() {
		return nil
	}
	return &t
}

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func fromJSON(s string, v any) {
	if s == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), v)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- paging + filters ----------------------------------------------------

type Page struct {
	Limit  int
	Offset int
}

func (p Page) normalized() Page {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Limit > 500 {
		p.Limit = 500
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	return p
}

type where struct {
	parts []string
	args  []any
}

func (w *where) add(clause string, args ...any) {
	w.parts = append(w.parts, clause)
	w.args = append(w.args, args...)
}

func (w *where) sql() string {
	if len(w.parts) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.parts, " AND ")
}

// like adds a case-insensitive substring match across columns (escape char is '!').
func (w *where) like(q string, cols ...string) {
	q = strings.TrimSpace(q)
	if q == "" || len(cols) == 0 {
		return
	}
	pat := "%" + likeEscape(strings.ToLower(q)) + "%"
	ors := make([]string, len(cols))
	for i, c := range cols {
		ors[i] = "LOWER(" + c + ") LIKE ? ESCAPE '!'"
		w.args = append(w.args, pat)
	}
	w.parts = append(w.parts, "("+strings.Join(ors, " OR ")+")")
}

func likeEscape(s string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_", "[", "![")
	return r.Replace(s)
}

func (s *Store) count(ctx context.Context, table string, w *where) (int, error) {
	var n int
	err := s.queryRow(ctx, "SELECT COUNT(*) FROM "+table+w.sql(), w.args...).Scan(&n)
	return n, err
}
