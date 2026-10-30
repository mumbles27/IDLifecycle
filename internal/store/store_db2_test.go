package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

// ---- server-less unit tests for the db2 dialect ------------------------------

func TestDB2DriverName(t *testing.T) {
	got, err := sqlDriverName(DriverDB2)
	if runtime.GOOS != "windows" {
		if err == nil {
			t.Fatalf("db2 must be rejected where the ODBC driver is not linked, got %q", got)
		}
		return
	}
	if err != nil || got != "odbc" {
		t.Fatalf("sqlDriverName(db2) = %q, %v; want odbc", got, err)
	}
	if _, err := sqlDriverName("oracle"); err == nil || !strings.Contains(err.Error(), "db2") {
		t.Fatalf("unsupported-driver error should list db2: %v", err)
	}
}

func TestDB2DDL(t *testing.T) {
	s := &Store{driver: DriverDB2}
	all := []string{createMigrations}
	for _, m := range migrations {
		all = append(all, m.stmts...)
	}
	sawClob := false
	for _, raw := range all {
		got := s.ddl(raw)
		if strings.ContainsAny(got, "{}") {
			t.Errorf("unexpanded token in:\n%s", got)
		}
		if regexp.MustCompile(`\bTEXT\b|NVARCHAR|LONGTEXT|ENGINE=`).MatchString(got) {
			t.Errorf("foreign type/option in db2 DDL:\n%s", got)
		}
		if strings.ContainsAny(got, "\"`[") {
			t.Errorf("identifiers must stay unquoted (Db2 folds them to upper case):\n%s", got)
		}
		if strings.Contains(raw, "{TEXT}") {
			if !strings.Contains(got, "CLOB(16M)") {
				t.Errorf("{TEXT} must become CLOB(16M):\n%s", got)
			}
			sawClob = true
		}
		if strings.Contains(raw, "{STR}") && !strings.Contains(got, "VARCHAR(255)") {
			t.Errorf("{STR} must become VARCHAR(255):\n%s", got)
		}
		if strings.HasSuffix(strings.TrimSpace(got), " ") || strings.Contains(got, "IF NOT EXISTS") {
			t.Errorf("bad statement tail:\n%s", got)
		}
	}
	if !sawClob {
		t.Fatal("expected at least one CLOB column")
	}
}

func TestDB2PageClause(t *testing.T) {
	s := &Store{driver: DriverDB2}
	cases := []struct {
		p    Page
		want string
	}{
		{Page{Limit: 10, Offset: 20}, " OFFSET 20 ROWS FETCH FIRST 10 ROWS ONLY"},
		{Page{}, " OFFSET 0 ROWS FETCH FIRST 50 ROWS ONLY"},
		{Page{Limit: 99999, Offset: -5}, " OFFSET 0 ROWS FETCH FIRST 500 ROWS ONLY"},
	}
	for _, c := range cases {
		q, args := s.pageClause(c.p)
		if q != c.want || len(args) != 0 {
			t.Errorf("pageClause(%+v) = %q %v; want %q and no args", c.p, q, args, c.want)
		}
	}
	if got := s.rebind("a = ? AND b = ?"); got != "a = ? AND b = ?" {
		t.Errorf("db2/ODBC uses ? natively, rebind changed it to %q", got)
	}
}

func TestDB2UniqueViolation(t *testing.T) {
	yes := []string{
		`SQLExecute: {23505} [IBM][CLI Driver][DB2/NT64] SQL0803N  One or more values in the INSERT statement, UPDATE statement, or foreign key update caused by a DELETE statement are not valid because the primary key, unique constraint or unique index identified by "2" constrains table "IDL.USERS" from having duplicate values for the index key.  SQLSTATE=23505`,
		`SQLExecute: {23505} [IBM][CLI Driver][DB2/LINUXX8664] SQL0803N  ...`,
		`[IBM][CLI Driver][DB2] SQL0803N  SQLSTATE=23505`,
	}
	for _, m := range yes {
		if !IsUniqueViolation(errors.New(m)) {
			t.Errorf("not recognised as unique violation: %s", m)
		}
	}
	no := `SQLExecute: {23503} [IBM][CLI Driver][DB2/NT64] SQL0530N  The INSERT or UPDATE value of the FOREIGN KEY "IDL.X.FK" is not equal to any value of the parent key of the parent table.  SQLSTATE=23503`
	if IsUniqueViolation(errors.New(no)) {
		t.Errorf("FK violation misread as unique violation")
	}
}

func TestDB2DSN(t *testing.T) {
	cases := map[string]string{
		"DSN=IDLDB;Uid=idl;Pwd=x":  "DSN=IDLDB;Uid=idl;Pwd=x;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		"DSN=IDLDB;Uid=idl;Pwd=x;": "DSN=IDLDB;Uid=idl;Pwd=x;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		"Driver={IBM DB2 ODBC DRIVER};Hostname=h;Port=50000;Protocol=TCPIP;Database=IDLDB;Uid=u;Pwd=p;longdatacompat = 1;": "Driver={IBM DB2 ODBC DRIVER};Hostname=h;Port=50000;Protocol=TCPIP;Database=IDLDB;Uid=u;Pwd=p;longdatacompat = 1;MapCharToWChar=1;ConnectTimeout=20;",
		"DSN=X;ConnectTimeout=5;mapchartowchar=1": "DSN=X;ConnectTimeout=5;mapchartowchar=1;LongDataCompat=1;",
		// Values are never trimmed (a password may start or end with a space).
		"DSN=X;Pwd= p w ":  "DSN=X;Pwd= p w ;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		"DSN=X;Pwd= p ;\t": "DSN=X;Pwd= p ;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
	}
	for in, want := range cases {
		if got, err := db2DSN(in); err != nil || got != want {
			t.Errorf("db2DSN(%q)\n got %q %v\nwant %q", in, got, err, want)
		}
	}
	// MapCharToWChar=0 would garble non-ASCII text (and the audit hashes over it): refused.
	for _, in := range []string{"MapCharToWChar=0;DSN=X", "DSN=X; MAPCHARTOWCHAR = 0 ;", "DSN=X;mapchartowchar=0"} {
		if _, err := db2DSN(in); err == nil || !strings.Contains(err.Error(), "MapCharToWChar") {
			t.Errorf("db2DSN(%q) must be refused: %v", in, err)
		}
	}
	if runtime.GOOS == "windows" {
		_, err := Open(context.Background(), DriverDB2, "DSN=X;Pwd=Secret123;MapCharToWChar=0;")
		if err == nil || !strings.HasPrefix(err.Error(), "db2: ") || strings.Contains(err.Error(), "Secret123") {
			t.Errorf("Open must refuse MapCharToWChar=0 before connecting: %v", err)
		}
	}
}

// Open scrubs the DSN and its password from open, connect and migrate errors (check prints
// them and the service logs them).
func TestOpenScrubsDSN(t *testing.T) {
	const dsn = "postgres://idl:Secret123@db.invalid:5432/idl"
	saved := openDB
	t.Cleanup(func() { openDB = saved })
	openDB = func(name, dsn string) (*sql.DB, error) {
		return nil, errors.New("cannot parse " + dsn + " (password Secret123)")
	}
	_, err := Open(context.Background(), DriverPostgres, dsn)
	if err == nil || !strings.HasPrefix(err.Error(), "open postgres: cannot parse ***") || strings.Contains(err.Error(), "Secret123") {
		t.Fatalf("open error not scrubbed: %v", err)
	}
	// The resolved (augmented) DSN is scrubbed as well as the one passed in.
	openDB = func(name, dsn string) (*sql.DB, error) { return nil, errors.New("bad: " + dsn) }
	if _, err := Open(context.Background(), DriverSQLite, "file:C:/x/Secret123.db"); err == nil || strings.Contains(err.Error(), "Secret123") {
		t.Fatalf("augmented sqlite dsn echoed: %v", err)
	}
}

func TestDB2Like(t *testing.T) {
	if got := likeEscape(DriverDB2, "a[b]_c%d!e"); got != "a[b]!_c!%d!!e" {
		t.Errorf("db2 likeEscape = %q (Db2 rejects '!' followed by anything but %%, _ or !)", got)
	}
	if got := likeEscape(DriverMSSQL, "a[b"); got != "a![b" {
		t.Errorf("mssql must still escape '[': %q", got)
	}
	w := &where{}
	w.like(DriverDB2, " 100%_X ", "i.display", "i.ad_dn")
	want := "(LOWER(i.display) LIKE CAST(? AS VARCHAR(32672)) ESCAPE '!' OR LOWER(CAST(i.ad_dn AS VARCHAR(32672))) LIKE CAST(? AS VARCHAR(32672)) ESCAPE '!')"
	if len(w.parts) != 1 || w.parts[0] != want {
		t.Errorf("db2 like clause:\n got %q\nwant %q", w.parts, want)
	}
	if len(w.args) != 2 || w.args[0] != "%100!%!_x%" {
		t.Errorf("db2 like args: %v", w.args)
	}
	long := &where{}
	long.like(DriverDB2, strings.Repeat("é", db2LikeMax+50), "display")
	if n := len([]rune(long.args[0].(string))); n != db2LikeMax+2 {
		t.Errorf("db2 search text must be capped, pattern has %d runes", n)
	}
	// Other dialects are unchanged.
	o := &where{}
	o.like(DriverSQLite, "x", "display")
	if o.parts[0] != "(LOWER(display) LIKE ? ESCAPE '!')" {
		t.Errorf("sqlite like clause changed: %q", o.parts[0])
	}
}

func TestTextColumns(t *testing.T) {
	tc := textColumns()
	for _, c := range []string{"setting_value", "options_json", "config_json", "last_error", "attrs_json", "ad_dn", "applied_json",
		"template", "description", "conditions_json", "grants_json", "counters_json", "error", "payload_json", "summary_json", "decision_note", "detail_json"} {
		if !tc[c] {
			t.Errorf("%s should be a TEXT/CLOB column", c)
		}
	}
	for _, c := range []string{"name", "status", "display", "external_id", "id", "seq", "hash", "username"} {
		if tc[c] {
			t.Errorf("%s must not be TEXT", c)
		}
	}
	if !isTextColumn("i.ad_dn") || isTextColumn("i.display") {
		t.Error("isTextColumn must ignore the table alias")
	}
}

func TestDB2MigrateCatalogCheck(t *testing.T) {
	want := `SELECT 1 FROM SYSCAT.TABLES WHERE TABSCHEMA = CURRENT SCHEMA AND TABNAME = 'SCHEMA_MIGRATIONS'`
	if db2MigrationsExists != want {
		t.Fatalf("catalog check = %q", db2MigrationsExists)
	}
	for mode, wantCreate := range map[recMode]bool{recEmpty: true, recOneRow: false} {
		s, rec := newRecorderStore(t, mode)
		if err := s.migrate(context.Background()); err != nil && mode == recOneRow {
			t.Fatalf("migrate: %v", err)
		}
		st := rec.statements()
		if len(st) == 0 || st[0] != want {
			t.Fatalf("mode %d: first statement must be the catalog check, got %q", mode, st)
		}
		created := len(st) > 1 && strings.HasPrefix(st[1], "CREATE TABLE schema_migrations ")
		if created != wantCreate {
			t.Errorf("mode %d: schema_migrations created=%v, want %v (%q)", mode, created, wantCreate, st)
		}
	}
}

// ---- statement recorder: a fake database/sql driver -------------------------

type recMode int

const (
	recEmpty  recMode = iota // queries return no rows
	recOneRow                // queries return one row of "0" values
)

type recorder struct {
	mode  recMode
	mu    sync.Mutex
	stmts []string
	errs  []string
}

func (r *recorder) record(q string, nargs int) {
	q = strings.Join(strings.Fields(q), " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, q)
	if n := countPlaceholders(q); n != nargs {
		r.errs = append(r.errs, "placeholder/arg mismatch ("+strconv.Itoa(n)+" vs "+strconv.Itoa(nargs)+"): "+q)
	}
}

func (r *recorder) statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stmts...)
}

// countPlaceholders counts ? outside single-quoted literals.
func countPlaceholders(q string) int {
	n, inStr := 0, false
	for _, r := range q {
		switch {
		case r == '\'':
			inStr = !inStr
		case r == '?' && !inStr:
			n++
		}
	}
	return n
}

var (
	recRegister sync.Once
	recMu       sync.Mutex
	recByDSN    = map[string]*recorder{}
)

type recDriver struct{}

func (recDriver) Open(dsn string) (driver.Conn, error) {
	recMu.Lock()
	defer recMu.Unlock()
	r, ok := recByDSN[dsn]
	if !ok {
		return nil, errors.New("unknown recorder " + dsn)
	}
	return &recConn{r}, nil
}

type recConn struct{ r *recorder }

func (c *recConn) Prepare(q string) (driver.Stmt, error) { return &recStmt{c.r, q}, nil }
func (c *recConn) Close() error                          { return nil }
func (c *recConn) Begin() (driver.Tx, error)             { return recTx{}, nil }

type recTx struct{}

func (recTx) Commit() error   { return nil }
func (recTx) Rollback() error { return nil }

type recStmt struct {
	r *recorder
	q string
}

func (s *recStmt) Close() error  { return nil }
func (s *recStmt) NumInput() int { return -1 }
func (s *recStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.r.record(s.q, len(args))
	return driver.RowsAffected(1), nil
}
func (s *recStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.r.record(s.q, len(args))
	cols := selectColumns(s.q)
	return &recRows{cols: cols, left: map[bool]int{true: 1, false: 0}[s.r.mode == recOneRow]}, nil
}

type recRows struct {
	cols []string
	left int
}

func (r *recRows) Columns() []string { return r.cols }
func (r *recRows) Close() error      { return nil }
func (r *recRows) Next(dest []driver.Value) error {
	if r.left == 0 {
		return io.EOF
	}
	r.left--
	for i := range dest {
		dest[i] = "0"
	}
	return nil
}

// selectColumns derives result column names (upper case, as Db2 reports them) from the select list.
func selectColumns(q string) []string {
	q = strings.Join(strings.Fields(q), " ")
	if !strings.HasPrefix(strings.ToUpper(q), "SELECT ") {
		return nil
	}
	body := q[len("SELECT "):]
	var items []string
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				items = append(items, body[start:i])
				start = i + 1
			}
		case ' ':
			if depth == 0 && strings.HasPrefix(strings.ToUpper(body[i:]), " FROM ") {
				items = append(items, body[start:i])
				out := make([]string, len(items))
				for j, it := range items {
					it = strings.TrimSpace(it)
					if k := strings.LastIndexAny(it, ". "); k >= 0 {
						it = it[k+1:]
					}
					out[j] = strings.ToUpper(it)
				}
				return out
			}
		}
	}
	return nil
}

func newRecorderStore(t *testing.T, mode recMode) (*Store, *recorder) {
	t.Helper()
	recRegister.Do(func() { sql.Register("idl-recorder", recDriver{}) })
	r := &recorder{mode: mode}
	dsn := t.Name() + "/" + strconv.Itoa(int(mode))
	recMu.Lock()
	recByDSN[dsn] = r
	recMu.Unlock()
	db, err := sql.Open("idl-recorder", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Store{db: db, driver: DriverDB2}, r
}

// ---- static audit of every statement issued on the db2 path -----------------

var (
	reCompareL  = regexp.MustCompile(`(?i)(?:\b\w+\.)?\b([a-z_][a-z0-9_]*)\s*(?:=|<>|!=|<=|>=|<|>)`)
	reCompareR  = regexp.MustCompile(`(?i)(?:=|<>|!=|<=|>=|<|>)\s*(?:\w+\.)?([a-z_][a-z0-9_]*)\b`)
	reIn        = regexp.MustCompile(`(?i)(?:\b\w+\.)?\b([a-z_][a-z0-9_]*)\s+(?:NOT\s+)?(?:IN|BETWEEN)\b`)
	reOrderBy   = regexp.MustCompile(`(?i)\b(?:ORDER|GROUP) BY (.*?)(?: OFFSET | FETCH |\)|$)`)
	reCaseFn    = regexp.MustCompile(`(?i)\b(?:LOWER|UPPER|LCASE|UCASE)\(\s*(?:\w+\.)?(\w+)\s*\)`)
	reLike      = regexp.MustCompile(`(?i)\bLIKE\b`)
	reLikeOK    = regexp.MustCompile(`(?i)\bLIKE CAST\(\? AS VARCHAR\(\d+\)\) ESCAPE '(.)'`)
	reEscape    = regexp.MustCompile(`(?i)\bESCAPE '([^']*)'`)
	reIndex     = regexp.MustCompile(`(?i)^CREATE (?:UNIQUE )?INDEX \w+ ON \w+ \(([^)]*)\)`)
	reClobKey   = regexp.MustCompile(`(?i)CLOB\(\d+[KMG]?\)[^,]*\b(?:PRIMARY KEY|UNIQUE)\b`)
	reStrLit    = regexp.MustCompile(`'[^']*'`)
	reUpdateSet = regexp.MustCompile(`(?i)^UPDATE \w+ SET .*? WHERE `)
	rePaged     = regexp.MustCompile(`(?i) ORDER BY [^?]+ OFFSET \d+ ROWS FETCH FIRST \d+ ROWS ONLY$`)
	reForeign   = regexp.MustCompile("(?i)\\bLIMIT\\b|IF NOT EXISTS|IF OBJECT_ID|\\$\\d|@p\\d|\\bTOP\\b|\\bDISTINCT\\b|FETCH NEXT|[\"`\\[]")
)

func baseName(expr string) string {
	expr = strings.TrimSpace(expr)
	for _, suf := range []string{" DESC", " ASC", " desc", " asc"} {
		expr = strings.TrimSuffix(expr, suf)
	}
	if i := strings.LastIndexByte(expr, '.'); i >= 0 {
		expr = expr[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(expr))
}

// auditDB2 returns the Db2 rule violations in one statement.
func auditDB2(q string) []string {
	clob := textColumns()
	var bad []string
	add := func(msg string) { bad = append(bad, msg+": "+q) }

	noLit := reStrLit.ReplaceAllString(q, "''")
	if reForeign.MatchString(noLit) {
		add("non-Db2 syntax or quoted identifier (" + reForeign.FindString(noLit) + ")")
	}
	upper := strings.ToUpper(q)
	switch {
	case strings.HasPrefix(upper, "CREATE TABLE"):
		if reClobKey.MatchString(q) {
			add("CLOB column used as key")
		}
		return bad
	case strings.HasPrefix(upper, "CREATE"):
		if m := reIndex.FindStringSubmatch(q); m != nil {
			for _, c := range strings.Split(m[1], ",") {
				if clob[baseName(c)] {
					add("CLOB column " + baseName(c) + " in an index")
				}
			}
		}
		return bad
	case strings.HasPrefix(upper, "INSERT"):
		return bad
	}

	pred := noLit
	if m := reUpdateSet.FindString(pred); m != "" {
		pred = " WHERE " + pred[len(m):] // assignments in SET are not comparisons
	} else if strings.HasPrefix(upper, "UPDATE") {
		pred = "" // UPDATE without WHERE: only assignments
	}
	pred = strings.ReplaceAll(pred, "CAST(", "(") // CAST(x AS VARCHAR(n)) is how CLOBs are made comparable
	pred = regexp.MustCompile(`(?i)\(\s*(?:\w+\.)?\w+ AS VARCHAR\(\d+\)\s*\)`).ReplaceAllString(pred, "(casted)")
	for _, re := range []*regexp.Regexp{reCompareL, reCompareR, reIn} {
		for _, m := range re.FindAllStringSubmatch(pred, -1) {
			if clob[strings.ToLower(m[1])] {
				add("CLOB column " + m[1] + " compared")
			}
		}
	}
	for _, m := range reCaseFn.FindAllStringSubmatch(pred, -1) {
		if clob[strings.ToLower(m[1])] {
			add("LOWER/UPPER applied directly to CLOB column " + m[1])
		}
	}
	for _, m := range reOrderBy.FindAllStringSubmatch(noLit, -1) {
		for _, item := range strings.Split(m[1], ",") {
			if clob[baseName(item)] {
				add("CLOB column " + baseName(item) + " in ORDER/GROUP BY")
			}
		}
	}
	if n := len(reLike.FindAllString(q, -1)); n != len(reLikeOK.FindAllString(q, -1)) {
		add("LIKE pattern must be CAST(? AS VARCHAR(n)) with a single-character ESCAPE")
	}
	for _, m := range reEscape.FindAllStringSubmatch(q, -1) {
		if len([]rune(m[1])) != 1 {
			add("ESCAPE must be a one-character literal")
		}
	}
	if strings.Contains(upper, " ROWS ONLY") && !rePaged.MatchString(q) {
		add("paged query must be ORDER BY ... OFFSET n ROWS FETCH FIRST m ROWS ONLY")
	}
	return bad
}

// The checker itself must catch the mistakes it exists for.
func TestDB2AuditCatchesMistakes(t *testing.T) {
	for _, q := range []string{
		"SELECT id FROM sync_runs WHERE error = ?",
		"SELECT id FROM sync_runs WHERE ? <> error",
		"UPDATE sync_runs SET status = ? WHERE error = ''",
		"SELECT id FROM mappings ORDER BY description",
		"SELECT id FROM t ORDER BY name LIMIT ? OFFSET ?",
		"SELECT id FROM identities i WHERE (LOWER(i.ad_dn) LIKE CAST(? AS VARCHAR(10)) ESCAPE '!')",
		"SELECT id FROM identities i WHERE (LOWER(i.display) LIKE ? ESCAPE '!')",
		"SELECT id FROM identities WHERE display LIKE CAST(? AS VARCHAR(9)) ESCAPE '!!'",
		"CREATE INDEX ix ON audit_log (detail_json)",
		"CREATE TABLE IF NOT EXISTS x (a INT)",
		"SELECT DISTINCT error FROM changes",
		`SELECT "name" FROM sources`,
		"SELECT id FROM sources ORDER BY name OFFSET ? ROWS FETCH FIRST ? ROWS ONLY",
	} {
		if len(auditDB2(q)) == 0 {
			t.Errorf("audit missed a Db2 problem in %q", q)
		}
	}
	for _, q := range []string{
		"UPDATE changes SET status = ?, error = '' WHERE id = ? AND status = ?",
		"SELECT COUNT(*) FROM sync_runs WHERE (LOWER(CAST(error AS VARCHAR(32672))) LIKE CAST(? AS VARCHAR(32672)) ESCAPE '!')",
		"SELECT id FROM sources ORDER BY name OFFSET 0 ROWS FETCH FIRST 50 ROWS ONLY",
	} {
		if b := auditDB2(q); len(b) != 0 {
			t.Errorf("false positive: %v", b)
		}
	}
}

// exercises maps every exported *Store method to at least one call.
func db2Exercises() map[string]func(ctx context.Context, s *Store) error {
	now := time.Now()
	ok := func(err error) error { return err }
	ign := func(_ any, err error) error { return err }
	ign3 := func(_ any, _ int, err error) error { return err }
	return map[string]func(ctx context.Context, s *Store) error{
		"Driver":       func(ctx context.Context, s *Store) error { _ = s.Driver(); return nil },
		"Ping":         func(ctx context.Context, s *Store) error { return s.Ping(ctx) },
		"SetAuditKey":  func(ctx context.Context, s *Store) error { s.SetAuditKey([]byte("k")); return nil },
		"Close":        func(ctx context.Context, s *Store) error { return nil }, // closing would end the run; nothing SQL to audit
		"GetSettings":  func(ctx context.Context, s *Store) error { return ign(s.GetSettings(ctx)) },
		"SaveSettings": func(ctx context.Context, s *Store) error { return s.SaveSettings(ctx, model.DefaultSettings(), "t") },
		"CreateUser": func(ctx context.Context, s *Store) error {
			return s.CreateUser(ctx, model.User{ID: "u", Username: "a", CreatedAt: now})
		},
		"GetUser": func(ctx context.Context, s *Store) error { return ign(s.GetUser(ctx, "u")) },
		"GetUserByExternalID": func(ctx context.Context, s *Store) error {
			return ign(s.GetUserByExternalID(ctx, "t:o"))
		},
		"GetUserByName": func(ctx context.Context, s *Store) error {
			return ign(s.GetUserByName(ctx, "a"))
		},
		"ListUsers":          func(ctx context.Context, s *Store) error { return ign(s.ListUsers(ctx)) },
		"CountUsers":         func(ctx context.Context, s *Store) error { return ign(s.CountUsers(ctx)) },
		"CountActiveAdmins":  func(ctx context.Context, s *Store) error { return ign(s.CountActiveAdmins(ctx)) },
		"UpdateUser":         func(ctx context.Context, s *Store) error { return s.UpdateUser(ctx, model.User{ID: "u"}) },
		"DeleteUser":         func(ctx context.Context, s *Store) error { return s.DeleteUser(ctx, "u") },
		"RecordLoginFailure": func(ctx context.Context, s *Store) error { return s.RecordLoginFailure(ctx, "u", 1, time.Minute) },
		"RecordLoginSuccess": func(ctx context.Context, s *Store) error { return s.RecordLoginSuccess(ctx, "u") },
		"CreateSession": func(ctx context.Context, s *Store) error {
			return s.CreateSession(ctx, model.Session{TokenHash: "t", UserID: "u", CreatedAt: now, ExpiresAt: now, LastSeenAt: now})
		},
		"GetSession":            func(ctx context.Context, s *Store) error { return ign(s.GetSession(ctx, "t")) },
		"TouchSession":          func(ctx context.Context, s *Store) error { return s.TouchSession(ctx, "t", now) },
		"DeleteSession":         func(ctx context.Context, s *Store) error { return s.DeleteSession(ctx, "t") },
		"DeleteUserSessions":    func(ctx context.Context, s *Store) error { return s.DeleteUserSessions(ctx, "u") },
		"DeleteExpiredSessions": func(ctx context.Context, s *Store) error { return s.DeleteExpiredSessions(ctx) },
		"CreateSource": func(ctx context.Context, s *Store) error {
			return s.CreateSource(ctx, model.Source{ID: "s", Name: "n", CreatedAt: now})
		},
		"GetSource":       func(ctx context.Context, s *Store) error { return ign(s.GetSource(ctx, "s")) },
		"ListSources":     func(ctx context.Context, s *Store) error { return ign(s.ListSources(ctx)) },
		"UpdateSource":    func(ctx context.Context, s *Store) error { return s.UpdateSource(ctx, model.Source{ID: "s"}) },
		"SetSourceStatus": func(ctx context.Context, s *Store) error { return s.SetSourceStatus(ctx, "s", "ok", "") },
		"DeleteSource":    func(ctx context.Context, s *Store) error { return s.DeleteSource(ctx, "s") },
		"CreateMapping": func(ctx context.Context, s *Store) error {
			return s.CreateMapping(ctx, model.Mapping{ID: "m", CreatedAt: now})
		},
		"GetMapping":    func(ctx context.Context, s *Store) error { return ign(s.GetMapping(ctx, "m")) },
		"ListMappings":  func(ctx context.Context, s *Store) error { return ign(s.ListMappings(ctx)) },
		"UpdateMapping": func(ctx context.Context, s *Store) error { return s.UpdateMapping(ctx, model.Mapping{ID: "m"}) },
		"DeleteMapping": func(ctx context.Context, s *Store) error { return s.DeleteMapping(ctx, "m") },
		"CreateRole": func(ctx context.Context, s *Store) error {
			return s.CreateRole(ctx, model.RoleRule{ID: "r", CreatedAt: now})
		},
		"GetRole":    func(ctx context.Context, s *Store) error { return ign(s.GetRole(ctx, "r")) },
		"ListRoles":  func(ctx context.Context, s *Store) error { return ign(s.ListRoles(ctx)) },
		"UpdateRole": func(ctx context.Context, s *Store) error { return s.UpdateRole(ctx, model.RoleRule{ID: "r"}) },
		"DeleteRole": func(ctx context.Context, s *Store) error { return s.DeleteRole(ctx, "r") },
		"InsertIdentity": func(ctx context.Context, s *Store) error {
			return s.InsertIdentity(ctx, model.Identity{ID: "i", FirstSeen: now})
		},
		"UpdateIdentity": func(ctx context.Context, s *Store) error { return s.UpdateIdentity(ctx, model.Identity{ID: "i"}) },
		"GetIdentity":    func(ctx context.Context, s *Store) error { return ign(s.GetIdentity(ctx, "i")) },
		"ListIdentitiesBySource": func(ctx context.Context, s *Store) error {
			return ign(s.ListIdentitiesBySource(ctx, "s"))
		},
		"ListIdentities": func(ctx context.Context, s *Store) error {
			if err := ign3(s.ListIdentities(ctx, IdentityFilter{Q: "Jo[h]n_100%!", State: "linked"}, Page{Limit: 5, Offset: 10})); err != nil {
				return err
			}
			return ign3(s.ListIdentities(ctx, IdentityFilter{Manual: true, ManualAt: now}, Page{Limit: 5}))
		},
		"CreateRun":            func(ctx context.Context, s *Store) error { return s.CreateRun(ctx, model.Run{ID: "r", StartedAt: now}) },
		"UpdateRun":            func(ctx context.Context, s *Store) error { return s.UpdateRun(ctx, model.Run{ID: "r"}) },
		"GetRun":               func(ctx context.Context, s *Store) error { return ign(s.GetRun(ctx, "r")) },
		"ListRuns":             func(ctx context.Context, s *Store) error { return ign3(s.ListRuns(ctx, Page{})) },
		"LatestRun":            func(ctx context.Context, s *Store) error { return ign(s.LatestRun(ctx)) },
		"MarkStaleRunsAborted": func(ctx context.Context, s *Store) error { return ign(s.MarkStaleRunsAborted(ctx)) },
		"InsertChange": func(ctx context.Context, s *Store) error {
			return s.InsertChange(ctx, model.Change{ID: "c", CreatedAt: now})
		},
		"GetChange": func(ctx context.Context, s *Store) error { return ign(s.GetChange(ctx, "c")) },
		"ListChanges": func(ctx context.Context, s *Store) error {
			if err := ign3(s.ListChanges(ctx, ChangeFilter{}, Page{})); err != nil {
				return err
			}
			return ign3(s.ListChanges(ctx, ChangeFilter{Status: "pending", Type: "create", Q: "x", IdentityID: "i", RunID: "r"}, Page{Limit: 2, Offset: 2}))
		},
		"ListChangesWithStatus": func(ctx context.Context, s *Store) error {
			return ign(s.ListChangesWithStatus(ctx, model.StatusPending, model.StatusApproved))
		},
		"CountAppliedSince": func(ctx context.Context, s *Store) error { return ign(s.CountAppliedSince(ctx, "create", now)) },
		"CountChanges":      func(ctx context.Context, s *Store) error { return ign(s.CountChanges(ctx, "pending")) },
		"TransitionChange": func(ctx context.Context, s *Store) error {
			for _, to := range []string{model.StatusApproved, model.StatusApplying, model.StatusFailed} {
				if err := s.TransitionChange(ctx, "c", "x", to, "b", "n"); err != nil {
					return err
				}
			}
			return nil
		},
		"FinishChange":           func(ctx context.Context, s *Store) error { return s.FinishChange(ctx, "c", model.StatusApplied, "") },
		"FailInterruptedChanges": func(ctx context.Context, s *Store) error { return ign(s.FailInterruptedChanges(ctx)) },
		"AppendAudit":            func(ctx context.Context, s *Store) error { return s.AppendAudit(ctx, "a", "x", "t", "i", nil) },
		"ListAudit": func(ctx context.Context, s *Store) error {
			if err := ign3(s.ListAudit(ctx, AuditFilter{}, Page{})); err != nil {
				return err
			}
			if err := ign3(s.ListAudit(ctx, AuditFilter{Actor: "a", Action: "x", EntityType: "t", EntityID: "i"}, Page{Limit: 1})); err != nil {
				return err
			}
			// The search: prefix and exact actions, free text and its negation (over the detail CLOB), time range, errors.
			return ign3(s.ListAudit(ctx, AuditFilter{Actors: []string{"a"}, Actions: []string{"change.*", "x"}, Text: []string{"a[b]_%!"}, NotText: []string{"z"},
				From: time.Unix(1, 0), To: time.Now(), ErrorsOnly: true}, Page{Limit: 1}))
		},
		"DataRows":      func(ctx context.Context, s *Store) error { return ign(s.DataRows(ctx)) },
		"ClearSessions": func(ctx context.Context, s *Store) error { return s.ClearSessions(ctx) },
		"UpdateChangePayload": func(ctx context.Context, s *Store) error {
			return s.UpdateChangePayload(ctx, "c", model.StatusPending, model.ChangePayload{}, nil, "h")
		},
		"SetIdentityOU":           func(ctx context.Context, s *Store) error { return s.SetIdentityOU(ctx, "i", "OU=x", "Finance") },
		"SetIdentityLinkDecision": func(ctx context.Context, s *Store) error { return s.SetIdentityLinkDecision(ctx, "i", "new") },
		"SetIdentityNameOverride": func(ctx context.Context, s *Store) error {
			return s.SetIdentityNameOverride(ctx, "i", &model.Candidate{Sam: "a", UPN: "a@b", CN: "A"})
		},
		"AddIdentityLinkRejects": func(ctx context.Context, s *Store) error { return s.AddIdentityLinkRejects(ctx, "i", "g") },
		"SetChangeAppliedState": func(ctx context.Context, s *Store) error {
			return s.SetChangeAppliedState(ctx, "c", model.AppliedState{At: now})
		},
		"SetChangeUndoneBy": func(ctx context.Context, s *Store) error { return s.SetChangeUndoneBy(ctx, "c", "u") },
		"OpenUndos":         func(ctx context.Context, s *Store) error { _, err := s.OpenUndos(ctx); return err },
		"OpenUndoList":      func(ctx context.Context, s *Store) error { _, err := s.OpenUndoList(ctx); return err },
		"AddUndoneItems": func(ctx context.Context, s *Store) error {
			return s.AddUndoneItems(ctx, "c", "u", []string{"attribute:title", "group_add:CN=G,OU=Groups,DC=x"})
		},
		"UndoneItems": func(ctx context.Context, s *Store) error { _, err := s.UndoneItems(ctx, "c"); return err },
		"SetHoldSuppressed": func(ctx context.Context, s *Store) error {
			return s.SetHoldSuppressed(ctx, "h", "would set title to 'X'", now)
		},
		"ManualCounts": func(ctx context.Context, s *Store) error { _, err := s.ManualCounts(ctx, now); return err },
		"InsertHold": func(ctx context.Context, s *Store) error {
			return s.InsertHold(ctx, model.Hold{ID: "h", IdentityID: "i", Kind: model.HoldState, CreatedAt: now})
		},
		"ListHolds": func(ctx context.Context, s *Store) error { _, err := s.ListHolds(ctx, "i"); return err },
		"ListIdentityHistory": func(ctx context.Context, s *Store) error {
			_, err := s.ListIdentityHistory(ctx, "i", "s", "e", 10, 50)
			return err
		},
		"LastIdentityEvent": func(ctx context.Context, s *Store) error {
			_, _, err := s.LastIdentityEvent(ctx, "i", "identity.state")
			return err
		},
		"ActiveHolds": func(ctx context.Context, s *Store) error { _, err := s.ActiveHolds(ctx, now); return err },
		"CreateRetentionPolicy": func(ctx context.Context, s *Store) error {
			return s.CreateRetentionPolicy(ctx, model.RetentionPolicy{ID: "p", Name: "Execs", Match: "all", CreatedAt: now, UpdatedAt: now})
		},
		"GetRetentionPolicy":    func(ctx context.Context, s *Store) error { return ign(s.GetRetentionPolicy(ctx, "p")) },
		"ListRetentionPolicies": func(ctx context.Context, s *Store) error { return ign(s.ListRetentionPolicies(ctx)) },
		"UpdateRetentionPolicy": func(ctx context.Context, s *Store) error {
			return s.UpdateRetentionPolicy(ctx, model.RetentionPolicy{ID: "p", Name: "Execs", Match: "all"})
		},
		"DeleteRetentionPolicy": func(ctx context.Context, s *Store) error { return s.DeleteRetentionPolicy(ctx, "p") },
		"SetIdentityRetention":  func(ctx context.Context, s *Store) error { return s.SetIdentityRetention(ctx, "i", "p", "alice") },
		"GetIdentityRetention":  func(ctx context.Context, s *Store) error { return ign(s.GetIdentityRetention(ctx, "i")) },
		"ListIdentityRetention": func(ctx context.Context, s *Store) error { return ign(s.ListIdentityRetention(ctx)) },
		"PutIdentityRetention": func(ctx context.Context, s *Store) error {
			return s.PutIdentityRetention(ctx, model.IdentityRetention{IdentityID: "i", PolicyID: "p", PolicyName: "Execs", Reason: "rule", DeleteDue: &now, ResolvedAt: now})
		},
		"RetentionCounts": func(ctx context.Context, s *Store) error { return ign(s.RetentionCounts(ctx)) },
		"DeleteHold":      func(ctx context.Context, s *Store) error { _, err := s.DeleteHold(ctx, "i", "h"); return err },
		"CreateManualPerson": func(ctx context.Context, s *Store) error {
			return s.CreateManualPerson(ctx, model.ManualPerson{ID: "m", SourceID: "s", ExternalID: "C-1", CreatedAt: now, UpdatedAt: now})
		},
		"UpdateManualPerson": func(ctx context.Context, s *Store) error {
			return s.UpdateManualPerson(ctx, model.ManualPerson{ID: "m", SourceID: "s", UpdatedAt: now})
		},
		"GetManualPerson":    func(ctx context.Context, s *Store) error { return ign(s.GetManualPerson(ctx, "s", "m")) },
		"ListManualPeople":   func(ctx context.Context, s *Store) error { return ign(s.ListManualPeople(ctx, "s")) },
		"DeleteManualPerson": func(ctx context.Context, s *Store) error { return s.DeleteManualPerson(ctx, "s", "m") },
		"PutState":           func(ctx context.Context, s *Store) error { return s.PutState(ctx, "x", map[string]int{"a": 1}) },
		"GetState": func(ctx context.Context, s *Store) error {
			var v map[string]int
			return ign(s.GetState(ctx, "x", &v))
		},
		"ExportTable": func(ctx context.Context, s *Store) error {
			for _, t := range BackupTables {
				if _, err := s.ExportTable(ctx, t, func(map[string]any) error { return nil }); err != nil {
					return err
				}
			}
			return nil
		},
		"ImportTables": func(ctx context.Context, s *Store) error {
			// No rows: the fake driver does not report the real columns; the DELETE/SELECT statements are what is audited.
			_, err := s.ImportTables(ctx, func(string, func(map[string]any) error) error { return nil })
			return err
		},
		"VerifyAudit": func(ctx context.Context, s *Store) error { return ign(s.VerifyAudit(ctx)) },
		"BrowseTable": func(ctx context.Context, s *Store) error {
			for _, tbl := range BrowsableTables() {
				for _, q := range []string{"", "a[b]_%!"} {
					v, err := s.BrowseTable(ctx, tbl, q, Page{})
					if err != nil {
						return err
					}
					for _, c := range v.Columns {
						if c != strings.ToLower(c) {
							return errors.New("db2 column names must be lower-cased: " + c)
						}
					}
				}
			}
			return nil
		},
		"_migrate": func(ctx context.Context, s *Store) error { return ok(s.migrate(ctx)) },
	}
}

func TestDB2StatementAudit(t *testing.T) {
	ex := db2Exercises()

	// Every exported method must be exercised, so new queries are audited automatically.
	var missing []string
	st := reflect.TypeOf(&Store{})
	for i := 0; i < st.NumMethod(); i++ {
		if _, ok := ex[st.Method(i).Name]; !ok {
			missing = append(missing, st.Method(i).Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("add these *Store methods to db2Exercises: %v", missing)
	}

	names := make([]string, 0, len(ex))
	for n := range ex {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, mode := range []recMode{recEmpty, recOneRow} {
		s, rec := newRecorderStore(t, mode)
		ctx := context.Background()
		for _, n := range names {
			err := ex[n](ctx, s)
			if mode == recOneRow && err != nil {
				t.Errorf("%s against one-row fake: %v", n, err)
			}
		}
		stmts := rec.statements()
		if len(stmts) < 60 {
			t.Fatalf("mode %d: only %d statements recorded", mode, len(stmts))
		}
		for _, e := range rec.errs {
			t.Error(e)
		}
		seen := map[string]bool{}
		paged := 0
		for _, q := range stmts {
			if seen[q] {
				continue
			}
			seen[q] = true
			for _, b := range auditDB2(q) {
				t.Error(b)
			}
			if strings.Contains(q, " ROWS FETCH FIRST ") {
				paged++
			}
		}
		t.Logf("mode %d: %d statements, %d distinct, %d paged", mode, len(stmts), len(seen), paged)
		if mode == recOneRow && paged < 4 {
			t.Errorf("mode %d: expected the paged list queries to be exercised, saw %d", mode, paged)
		}
	}
}
