package sources

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/connstr"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// newTestSQLite creates a temp SQLite database with a small people table and returns its path.
func newTestSQLite(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hr.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE people (id TEXT, first_name TEXT, last_name TEXT, salary REAL, grade INTEGER, active BOOLEAN, hired DATETIME, manager TEXT)`,
		`INSERT INTO people VALUES ('E1', ' Jane ', 'Doe', 1234.5, 7, 1, '2021-03-04 05:06:07', 'E9')`,
		`INSERT INTO people VALUES ('E2', 'Zoë', 'Ångström', 1e7, 3, 0, NULL, NULL)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return p
}

func sqlCfg(dsn, query string, extra map[string]any) map[string]any {
	cfg := map[string]any{"driver": "sqlite", "dsn": dsn, "query": query}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func fetchSQL(t *testing.T, cfg map[string]any) ([]Row, error) {
	t.Helper()
	typ, ok := Lookup("sql")
	if !ok {
		t.Fatal("sql type not registered")
	}
	src, err := typ.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return src.Fetch(context.Background())
}

func TestSQLSourceFetch(t *testing.T) {
	p := newTestSQLite(t)
	rows, err := fetchSQL(t, sqlCfg(p, "SELECT id AS EmployeeID, first_name, last_name, salary, grade, active, hired, manager, 1.0/4 AS frac FROM people ORDER BY id;", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	want0 := map[string]string{
		"EmployeeID": "E1", "first_name": "Jane", "last_name": "Doe", "salary": "1234.5", "grade": "7",
		"manager": "E9", "frac": "0.25",
	}
	for k, w := range want0 {
		if rows[0][k] != w {
			t.Errorf("row0 %s: got %q want %q", k, rows[0][k], w)
		}
	}
	if !strings.HasPrefix(rows[0]["hired"], "2021-03-04") || !strings.Contains(rows[0]["hired"], "05:06:07") {
		t.Errorf("date: got %q", rows[0]["hired"])
	}
	if a := rows[0]["active"]; a != "1" && a != "true" {
		t.Errorf("bool: got %q", a)
	}
	if rows[1]["first_name"] != "Zoë" || rows[1]["last_name"] != "Ångström" {
		t.Errorf("unicode: %+v", rows[1])
	}
	if rows[1]["salary"] != "10000000" {
		t.Errorf("float must not use an exponent: got %q", rows[1]["salary"])
	}
	for _, k := range []string{"hired", "manager"} {
		if v, ok := rows[1][k]; !ok || v != "" {
			t.Errorf("NULL %s must be present and empty: %q %v", k, v, ok)
		}
	}
	if len(rows[0]) != 9 {
		t.Errorf("every column must be a key: %+v", rows[0])
	}
}

func TestSQLValueString(t *testing.T) {
	loc := time.FixedZone("X", -5*3600)
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""}, {[]byte(" b "), "b"}, {true, "true"}, {false, "false"}, {int64(-42), "-42"}, {uint8(7), "7"},
		{float64(1e21), "1000000000000000000000"}, {float32(0.5), "0.5"},
		{time.Date(2020, 1, 2, 3, 4, 5, 0, loc), "2020-01-02T03:04:05-05:00"},
		{time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC), "2020-01-02T00:00:00Z"},
	}
	for _, c := range cases {
		if got := sqlValueString(c.in); got != c.want {
			t.Errorf("%#v: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestSQLMaxRowsAndColumns(t *testing.T) {
	p := newTestSQLite(t)
	_, err := fetchSQL(t, sqlCfg(p, "SELECT id FROM people", map[string]any{"max_rows": 1}))
	if err == nil || !strings.Contains(err.Error(), "max_rows") {
		t.Fatalf("exceeding max_rows must fail, not truncate: %v", err)
	}
	if rows, err := fetchSQL(t, sqlCfg(p, "SELECT id FROM people", map[string]any{"max_rows": "2"})); err != nil || len(rows) != 2 {
		t.Fatalf("exactly max_rows is fine: %v %d", err, len(rows))
	}
	_, err = fetchSQL(t, sqlCfg(p, "SELECT id, first_name AS ID FROM people", nil))
	if err == nil || !strings.Contains(err.Error(), "duplicate column") {
		t.Fatalf("duplicate columns must error: %v", err)
	}
	if _, err := fetchSQL(t, sqlCfg(p, "SELECT id FROM no_such_table", nil)); err == nil || !strings.HasPrefix(err.Error(), "sql: ") {
		t.Fatalf("query error must be wrapped: %v", err)
	}
	rows, err := fetchSQL(t, sqlCfg(p, "SELECT id FROM people WHERE 1=0", nil))
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty result: %v %v", err, rows)
	}
}

func TestSQLiteIsQueryOnly(t *testing.T) {
	p := newTestSQLite(t)
	// Bypass validation to prove the transaction itself refuses writes.
	s := &sqlSource{driver: "sqlite", dsn: p, query: "DELETE FROM people RETURNING id", timeout: 10 * time.Second, maxRows: 10}
	if _, err := s.Fetch(context.Background()); err == nil {
		t.Fatal("a write must fail inside the read-only transaction")
	}
	rows, err := fetchSQL(t, sqlCfg(p, "SELECT id FROM people", nil))
	if err != nil || len(rows) != 2 {
		t.Fatalf("data must be untouched: %v %d", err, len(rows))
	}
}

func TestValidateSQLQuery(t *testing.T) {
	ok := []string{
		"SELECT * FROM people",
		"  select id from people;  ",
		"-- people feed\nSELECT id FROM people -- trailing\n",
		"/* header; with semicolon */ SELECT id FROM dbo.vw_Employees",
		"WITH cte AS (SELECT id FROM people) SELECT * FROM cte",
		"SELECT '-- x' AS x FROM people",                // a comment marker inside a string is not a comment
		"SELECT id, created_at, updated_by FROM people", // keywords only as whole words
		"SELECT 10--5 AS x",                             // "--" without a space is MySQL arithmetic: kept as code
	}
	for _, q := range ok {
		if err := ValidateSQLQuery(q); err != nil {
			t.Errorf("should accept %q: %v", q, err)
		}
	}
	bad := []string{
		"",
		"   ;",
		"DELETE FROM people",
		"SELECT id INTO backup FROM people",
		"select * into #tmp from people",
		"SELECT 1; DROP TABLE x",
		"SELECT 1 ; drop table x;",
		"SELECT 1 /*;*/ delete from people",
		"/* x */ UPDATE people SET a=1",
		"WITH d AS (DELETE FROM people RETURNING *) SELECT * FROM d",
		"EXEC sp_who",
		"SELECT * FROM people; EXECUTE x",
		"CALL do_stuff()",
		"MERGE INTO t USING s ON 1=1",
		"TRUNCATE TABLE people",
		"SELECT 1 /*! ; DELETE FROM people */",      // MySQL executable comment
		"SELECT 'a\\'' -- ' \n; DELETE FROM people", // backslash-escape ambiguity
		"SELECT [x-- ] ; DELETE FROM people",
		"SELECT $$ -- $$; DELETE FROM people",
		"SELECT 1 --1; DELETE FROM people",
		"VALUES (1)",
		"GRANT SELECT ON people TO bob",
		"SELECT 1 FOR UPDATE",
		"SELECT 'a;b' AS x",                   // conservative: string contents are checked too
		"SELECT '-- ' ; DELETE FROM people",   // a quoted "--" must not hide the rest of the line
		"SELECT \"/*\" ; DELETE FROM x -- */", // nor a quoted "/*"
	}
	for _, q := range bad {
		if err := ValidateSQLQuery(q); err == nil {
			t.Errorf("should reject %q", q)
		}
	}
}

func TestSQLNewRejectsBadConfig(t *testing.T) {
	typ, _ := Lookup("sql")
	if _, err := typ.New(sqlCfg("x.db", "SELECT 1", map[string]any{"driver": "oracle"})); err == nil {
		t.Fatal("unknown driver must be rejected")
	}
	if _, err := typ.New(sqlCfg("x.db", "DELETE FROM x", nil)); err == nil {
		t.Fatal("write query must be rejected at build time")
	}
	if _, err := typ.New(sqlCfg("", "SELECT 1", nil)); err == nil {
		t.Fatal("missing dsn must be rejected")
	}
	if _, err := typ.New(sqlCfg("env:", "SELECT 1", nil)); err == nil {
		t.Fatal("env: without a name must be rejected")
	}
	for alias, want := range map[string]string{"MSSQL": "sqlserver", "postgresql": "postgres", "pgx": "postgres", "mariadb": "mysql", "sqlite": "sqlite"} {
		src, err := typ.New(sqlCfg("x", "SELECT 1", map[string]any{"driver": alias}))
		if err != nil || src.(*sqlSource).driver != want {
			t.Errorf("alias %s: %v", alias, err)
		}
	}
	src, _ := typ.New(sqlCfg("x", "SELECT 1", map[string]any{"timeout_seconds": 0, "max_rows": -1}))
	if s := src.(*sqlSource); s.timeout != 120*time.Second || s.maxRows != 200000 {
		t.Fatalf("defaults: %+v", s)
	}

	base := model.Source{Name: "hr", Type: "sql", Options: model.SourceOptions{ExternalIDField: "id"}}
	base.Config = sqlCfg(model.MaskedSecret, "SELECT id FROM people", map[string]any{"driver": "mssql"})
	if err := Validate(base); err != nil {
		t.Fatalf("valid sql source (masked dsn) rejected: %v", err)
	}
	for name, cfg := range map[string]map[string]any{
		"bad driver": sqlCfg("x", "SELECT 1", map[string]any{"driver": "oracle"}),
		"bad query":  sqlCfg("x", "SELECT 1; DROP TABLE people", nil),
		"no dsn":     {"driver": "sqlite", "query": "SELECT 1"},
		"no query":   {"driver": "sqlite", "dsn": "x"},
	} {
		s := base
		s.Config = cfg
		if err := Validate(s); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestSQLEnvDSN(t *testing.T) {
	p := newTestSQLite(t)
	t.Setenv("IDL_TEST_HR_DSN", p)
	rows, err := fetchSQL(t, sqlCfg("env:IDL_TEST_HR_DSN", "SELECT id FROM people", nil))
	if err != nil || len(rows) != 2 {
		t.Fatalf("env dsn: %v %d", err, len(rows))
	}
	t.Setenv("IDL_TEST_HR_DSN", "")
	if _, err := fetchSQL(t, sqlCfg("env:IDL_TEST_HR_DSN", "SELECT id FROM people", nil)); err == nil || !strings.Contains(err.Error(), "IDL_TEST_HR_DSN") {
		t.Fatalf("empty env var must error by name: %v", err)
	}
}

func TestSQLErrorsNeverLeakDSN(t *testing.T) {
	const pass = "S3cr3tP@ss!"
	cases := map[string]string{
		"postgres":  "postgres://hruser:S3cr3tP%40ss%21@127.0.0.1:1/hr?connect_timeout=3&sslmode=disable",
		"mysql":     "hruser:" + pass + "@tcp(127.0.0.1:1)/hr?timeout=3s",
		"sqlserver": "sqlserver://hruser:S3cr3tP%40ss%21@127.0.0.1:1?database=hr&dial+timeout=3",
		"sqlite":    filepath.Join(t.TempDir(), "missing-dir", pass, "hr.db") + "?mode=ro",
	}
	for driver, dsn := range cases {
		_, err := fetchSQL(t, sqlCfg(dsn, "SELECT 1 AS x", map[string]any{"driver": driver, "timeout_seconds": 10}))
		if err == nil {
			t.Errorf("%s: expected a connection error", driver)
			continue
		}
		msg := err.Error()
		if strings.Contains(msg, pass) || strings.Contains(msg, "S3cr3tP%40ss") || strings.Contains(msg, dsn) {
			t.Errorf("%s: error leaks the DSN/password: %s", driver, msg)
		}
		if !strings.HasPrefix(msg, "sql: ") {
			t.Errorf("%s: error not wrapped: %s", driver, msg)
		}
	}

	// A driver that echoes its whole DSN, in URL, mysql and key=value forms.
	for _, dsn := range []string{
		"sqlserver://u:hunter22@h?database=x",
		"u:hunter22@tcp(h:3306)/x",
		"server=h;user id=u;password={hunter22};database=x",
		"host=h user=u password='hunter22' dbname=x",
	} {
		err := connstr.Scrub(errors.New("cannot connect using "+dsn+" (password hunter22)"), dsn)
		if strings.Contains(err.Error(), "hunter22") {
			t.Errorf("scrub %q: %s", dsn, err)
		}
	}
}

func TestSQLSecretHandling(t *testing.T) {
	c, _ := secrets.New(make([]byte, 32))
	const dsn = "sqlserver://u:pw123@h?database=HR"
	in := sqlCfg(dsn, "SELECT 1", map[string]any{"driver": "sqlserver"})
	if !passwordFields("sql")["dsn"] {
		t.Fatal("dsn must be treated as a secret field")
	}

	stored, err := PrepareConfigForStorage("sql", in, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.IsEncrypted(stored["dsn"].(string)) || stored["query"] != "SELECT 1" {
		t.Fatalf("dsn must be encrypted at rest: %+v", stored)
	}
	masked := MaskConfig("sql", stored)
	if masked["dsn"] != model.MaskedSecret || masked["driver"] != "sqlserver" {
		t.Fatalf("mask: %+v", masked)
	}
	again, err := PrepareConfigForStorage("sql", map[string]any{"driver": "sqlserver", "query": "SELECT 2", "dsn": model.MaskedSecret}, stored, c)
	if err != nil || again["dsn"] != stored["dsn"] || again["query"] != "SELECT 2" {
		t.Fatalf("masked round trip: %v %+v", err, again)
	}
	plain, err := DecryptConfig("sql", again, c)
	if err != nil || plain["dsn"] != dsn {
		t.Fatalf("decrypt: %v %+v", err, plain)
	}
	if _, err := Build(model.Source{Type: "sql", Config: again}, c); err != nil {
		t.Fatalf("build from stored config: %v", err)
	}

	// env: indirection is stored encrypted like any other value and still resolves after decrypt.
	envStored, _ := PrepareConfigForStorage("sql", sqlCfg("env:HR_DSN", "SELECT 1", nil), nil, c)
	if p, _ := DecryptConfig("sql", envStored, c); p["dsn"] != "env:HR_DSN" {
		t.Fatalf("env dsn round trip: %+v", p)
	}

	// No base_url: editing the query/driver with a masked dsn is not an endpoint change.
	if EndpointChangedWithoutSecret("sql", stored, map[string]any{"driver": "sqlserver", "query": "SELECT 3", "dsn": model.MaskedSecret}) {
		t.Fatal("sql has no base_url; masked dsn edits must not be flagged")
	}
}

func TestSQLiteHonoursThePathPolicy(t *testing.T) {
	p := newTestSQLite(t)
	forbidden := filepath.Dir(p)
	old := PathPolicy
	PathPolicy = func(path string) error {
		if strings.HasPrefix(strings.ToLower(filepath.Clean(path)), strings.ToLower(forbidden)) {
			return errors.New("files inside the service data directory cannot be used as a source")
		}
		return nil
	}
	defer func() { PathPolicy = old }()

	for _, dsn := range []string{p, "file:" + filepath.ToSlash(p) + "?mode=ro", "file:///" + filepath.ToSlash(p)} {
		if _, err := fetchSQL(t, sqlCfg(dsn, "SELECT id FROM people", nil)); err == nil || !strings.Contains(err.Error(), "data directory") {
			t.Errorf("%s: the path policy must apply to SQLite files, got %v", dsn, err)
		}
	}
	t.Setenv("IDL_TEST_SQLITE_DSN", p)
	if _, err := fetchSQL(t, sqlCfg("env:IDL_TEST_SQLITE_DSN", "SELECT id FROM people", nil)); err == nil {
		t.Error("env: indirection must not bypass the path policy")
	}
}
