package sources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	// Drivers (same set the store uses), imported here too so the source works standalone.
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "modernc.org/sqlite"
)

// sqlDrivers maps accepted driver names/aliases to the canonical name.
var sqlDrivers = map[string]string{
	"sqlserver": "sqlserver", "mssql": "sqlserver",
	"postgres": "postgres", "postgresql": "postgres", "pgx": "postgres",
	"mysql": "mysql", "mariadb": "mysql",
	"sqlite": "sqlite",
}

// sqlOpenName is the database/sql driver name registered by each driver package.
var sqlOpenName = map[string]string{"sqlserver": "sqlserver", "postgres": "pgx", "mysql": "mysql", "sqlite": "sqlite"}

func sqlType() Type {
	return Type{
		Name:  "sql",
		Label: "SQL database (table or view)",
		Fields: []Field{
			{Name: "driver", Label: "Database driver", Type: "string", Required: true, Help: "One of: sqlserver, postgres, mysql, sqlite (aliases: mssql, postgresql, pgx, mariadb)."},
			{Name: "dsn", Label: "Connection string", Type: "password", Required: true, Help: "Stored encrypted; never shown again. Or env:VAR_NAME to read it from the service's environment at run time. " +
				"Examples - sqlserver: sqlserver://user:pass@host:1433?database=HR&encrypt=true (omit user:pass@, e.g. sqlserver://host?database=HR&encrypt=true, " +
				"to use Windows integrated auth as the service account); postgres: postgres://user:pass@host:5432/hr?sslmode=require; " +
				"mysql: user:pass@tcp(host:3306)/hr?tls=true; sqlite: file:C:/hr/hr.db?mode=ro. Use a read-only database login."},
			{Name: "query", Label: "Query", Type: "text", Required: true, Help: "One read-only SELECT (or WITH ... SELECT) statement, e.g. SELECT * FROM dbo.vw_Employees. " +
				"Each column becomes an attribute named after the column (alias computed columns). Statements that modify data are refused, " +
				"but this is defence in depth: the database login itself should only be able to read."},
			{Name: "timeout_seconds", Label: "Timeout (seconds)", Type: "int", Default: 120},
			{Name: "max_rows", Label: "Maximum rows", Type: "int", Default: 200000, Help: "More rows than this fails the fetch (it is never silently truncated)."},
		},
		New: newSQLSource,
	}
}

type sqlSource struct {
	driver  string // canonical name
	dsn     string // literal DSN or env:VAR
	query   string
	timeout time.Duration
	maxRows int
}

func newSQLSource(cfg map[string]any) (Source, error) {
	driver, err := sqlDriver(cfgString(cfg, "driver", ""))
	if err != nil {
		return nil, err
	}
	query := cfgString(cfg, "query", "")
	if err := ValidateSQLQuery(query); err != nil {
		return nil, err
	}
	dsn := cfgString(cfg, "dsn", "")
	if dsn == "" {
		return nil, errors.New("sql: dsn is required")
	}
	if name, ok := strings.CutPrefix(dsn, "env:"); ok && strings.TrimSpace(name) == "" {
		return nil, errors.New("sql: dsn env: needs a variable name, e.g. env:HR_DB_DSN")
	}
	s := &sqlSource{
		driver:  driver,
		dsn:     dsn,
		query:   query,
		timeout: time.Duration(cfgInt(cfg, "timeout_seconds", 120)) * time.Second,
		maxRows: cfgInt(cfg, "max_rows", 200000),
	}
	if s.timeout <= 0 {
		s.timeout = 120 * time.Second
	}
	if s.maxRows < 1 {
		s.maxRows = 200000
	}
	return s, nil
}

func sqlDriver(name string) (string, error) {
	if d, ok := sqlDrivers[strings.ToLower(strings.TrimSpace(name))]; ok {
		return d, nil
	}
	return "", fmt.Errorf("sql: unsupported driver %q (use sqlserver, postgres, mysql or sqlite)", name)
}

// validateSQLConfig is the storage-time check (the dsn may be masked here, so it is not inspected).
// sqliteDSNPath extracts the file path from a SQLite DSN ("file:C:/hr/hr.db?mode=ro", "C:\hr\hr.db").
func sqliteDSNPath(dsn string) string {
	p := strings.TrimPrefix(strings.TrimSpace(dsn), "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimPrefix(p, "//") // file://<path>
	if len(p) > 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // file:///C:/x -> C:/x
	}
	return p
}

func validateSQLConfig(cfg map[string]any) error {
	if _, err := sqlDriver(cfgString(cfg, "driver", "")); err != nil {
		return err
	}
	return ValidateSQLQuery(cfgString(cfg, "query", ""))
}

func (s *sqlSource) resolvedDSN() (string, error) {
	if name, ok := strings.CutPrefix(s.dsn, "env:"); ok {
		name = strings.TrimSpace(name)
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			return "", fmt.Errorf("sql: dsn refers to environment variable %s which is empty", name)
		}
		return v, nil
	}
	return s.dsn, nil
}

func (s *sqlSource) Fetch(ctx context.Context) (rows []Row, err error) {
	dsn, err := s.resolvedDSN()
	if err != nil {
		return nil, err
	}
	// Every error leaving this function is scrubbed of the DSN and its password.
	defer func() {
		if err != nil {
			err = scrubDSN(err, dsn)
		}
	}()

	// A SQLite file is a file like a CSV one: the same policy applies, so the service's own database
	// (password hashes, encrypted secrets) can never be read back as "HR data".
	if s.driver == "sqlite" && PathPolicy != nil {
		if err := PathPolicy(sqliteDSNPath(dsn)); err != nil {
			return nil, fmt.Errorf("sql: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	db, err := sql.Open(sqlOpenName[s.driver], dsn)
	if err != nil {
		return nil, fmt.Errorf("sql: open %s: %w", s.driver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("sql: connect %s: %w", s.driver, err)
	}

	tx, err := s.beginReadOnly(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("sql: begin transaction: %w", err)
	}
	// Never committed: whatever the statement did (it should do nothing) is discarded.
	defer tx.Rollback()

	if s.driver == "sqlite" {
		// SQLite accepts but ignores TxOptions.ReadOnly; query_only makes it refuse writes on this connection.
		if _, err := tx.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
			return nil, fmt.Errorf("sql: set query_only: %w", err)
		}
	}

	res, err := tx.QueryContext(ctx, s.query)
	if err != nil {
		return nil, fmt.Errorf("sql: query: %w", err)
	}
	defer res.Close()
	return scanSQLRows(res, s.maxRows)
}

// beginReadOnly starts a read-only transaction where the driver supports it. go-mssqldb rejects
// TxOptions.ReadOnly outright, so SQL Server (and any driver that refuses it) gets a plain
// transaction instead; the caller always rolls it back.
func (s *sqlSource) beginReadOnly(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	if s.driver != "sqlserver" {
		if tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}); err == nil {
			return tx, nil
		} else if ctx.Err() != nil {
			return nil, err
		}
	}
	return db.BeginTx(ctx, nil)
}

// scanSQLRows converts a result set into Rows keyed by (trimmed) column name. Unnamed or
// duplicate (case-insensitive) columns are an error, like duplicate CSV headers.
func scanSQLRows(res *sql.Rows, maxRows int) ([]Row, error) {
	cols, err := res.Columns()
	if err != nil {
		return nil, fmt.Errorf("sql: columns: %w", err)
	}
	if len(cols) == 0 {
		return nil, errors.New("sql: query returned no columns")
	}
	seen := map[string]bool{}
	for i, c := range cols {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, fmt.Errorf("sql: column %d has no name; give it an alias (AS name)", i+1)
		}
		if seen[strings.ToLower(c)] {
			return nil, fmt.Errorf("sql: duplicate column %q; alias one of them", c)
		}
		seen[strings.ToLower(c)] = true
		cols[i] = c
	}

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	var rows []Row
	for res.Next() {
		if len(rows) >= maxRows {
			return nil, fmt.Errorf("sql: query returned more than max_rows (%d) rows; raise max_rows or narrow the query (a truncated feed is never used)", maxRows)
		}
		if err := res.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("sql: row %d: %w", len(rows)+1, err)
		}
		row := make(Row, len(cols))
		for i, c := range cols {
			row[c] = sqlValueString(vals[i])
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("sql: reading rows: %w", err)
	}
	return rows, nil
}

func sqlValueString(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		s = x
	case []byte:
		s = string(x)
	case time.Time:
		s = x.Format(time.RFC3339)
	case bool:
		s = strconv.FormatBool(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case int32:
		s = strconv.FormatInt(int64(x), 10)
	case int16:
		s = strconv.FormatInt(int64(x), 10)
	case int8:
		s = strconv.FormatInt(int64(x), 10)
	case int:
		s = strconv.Itoa(x)
	case uint64:
		s = strconv.FormatUint(x, 10)
	case uint32:
		s = strconv.FormatUint(uint64(x), 10)
	case uint16:
		s = strconv.FormatUint(uint64(x), 10)
	case uint8:
		s = strconv.FormatUint(uint64(x), 10)
	case uint:
		s = strconv.FormatUint(uint64(x), 10)
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		s = strconv.FormatFloat(float64(x), 'f', -1, 32)
	default:
		s = fmt.Sprint(v)
	}
	return strings.TrimSpace(s)
}

// ---- DSN scrubbing ---------------------------------------------------------

// scrubDSN removes the DSN and any password inside it from an error message, in case a driver
// echoes its connection string. The original error chain is dropped on purpose.
func scrubDSN(err error, dsn string) error {
	msg := err.Error()
	needles := []string{dsn}
	needles = append(needles, dsnPasswords(dsn)...)
	changed := false
	for _, s := range needles {
		if len(s) >= 3 && strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "***")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return errors.New(msg)
}

var dsnPasswordKV = regexp.MustCompile(`(?i)(?:^|[;\s])(?:password|pwd)\s*=\s*("[^"]*"|'[^']*'|\{[^}]*\}|[^;\s]*)`)

// dsnPasswords extracts password candidates from URL (user:pass@), mysql (user:pass@tcp(...))
// and key=value (password=...;) style connection strings, raw and percent-decoded.
func dsnPasswords(dsn string) []string {
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
			if dec, err := url.PathUnescape(p); err == nil && dec != p {
				out = append(out, dec)
			}
		}
	}
	rest := dsn
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if at := strings.LastIndex(rest, "@"); at > 0 {
		if _, pass, ok := strings.Cut(rest[:at], ":"); ok {
			add(pass)
		}
	}
	for _, m := range dsnPasswordKV.FindAllStringSubmatch(dsn, -1) {
		add(strings.Trim(m[1], `"'{}`))
	}
	return out
}

// ---- query validation ------------------------------------------------------

var sqlForbiddenWord = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|MERGE|DROP|ALTER|CREATE|TRUNCATE|EXEC|EXECUTE|GRANT|REVOKE|INTO|CALL)\b`)

var sqlLeadingWord = regexp.MustCompile(`(?i)^(SELECT|WITH)\b`)

// ValidateSQLQuery accepts a single read-only SELECT / WITH statement. It is a conservative
// lexical check (defence in depth; the database login must still be read-only): comments are
// removed, then the text - string literals included, so a keyword inside a string is also
// refused - must start with SELECT or WITH, contain no ';' except one trailing one, and contain
// none of the data-changing keywords as whole words.
//
// Comment removal must never hide code the server would run, and dialects disagree about string
// escaping, so it is done twice (standard ” escapes, and MySQL-style backslash escapes) and both
// results must pass. "--" only starts a comment when followed by whitespace (MySQL's rule) and
// MySQL "/*! ... */" executable comments are kept as code.
func ValidateSQLQuery(q string) error {
	if strings.TrimSpace(q) == "" {
		return errors.New("sql: query is required")
	}
	for _, backslash := range []bool{false, true} {
		if err := checkSQLStatement(stripSQLComments(q, backslash)); err != nil {
			return err
		}
	}
	return nil
}

func checkSQLStatement(code string) error {
	code = strings.TrimSpace(code)
	code = strings.TrimSpace(strings.TrimSuffix(code, ";"))
	if code == "" {
		return errors.New("sql: query is empty")
	}
	if strings.Contains(code, ";") {
		return errors.New("sql: query must be a single statement (no ';' except one at the end)")
	}
	if !sqlLeadingWord.MatchString(code) {
		return errors.New("sql: query must start with SELECT or WITH")
	}
	if m := sqlForbiddenWord.FindString(code); m != "" {
		return fmt.Errorf("sql: query must be read-only (%s is not allowed)", strings.ToUpper(m))
	}
	return nil
}

// stripSQLComments replaces comments with a space, leaving quoted text ('...', "...", `...`,
// [...], $tag$...$tag$) untouched. backslash selects MySQL-style \-escapes inside quotes.
func stripSQLComments(q string, backslash bool) string {
	var b strings.Builder
	n := len(q)
	for i := 0; i < n; {
		c := q[i]
		switch {
		case c == '-' && i+1 < n && q[i+1] == '-' && (i+2 == n || isSQLSpace(q[i+2])):
			for i < n && q[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < n && q[i+1] == '*' && !(i+2 < n && q[i+2] == '!'):
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				i = n
			} else {
				i += 2 + end + 2
			}
			b.WriteByte(' ')
		case c == '\'' || c == '"' || c == '`' || c == '[':
			closer := c
			if c == '[' {
				closer = ']'
			}
			j := i + 1
			for j < n {
				if backslash && q[j] == '\\' && c != '[' {
					j += 2
					continue
				}
				if q[j] == closer {
					if j+1 < n && q[j+1] == closer { // doubled quote = escaped
						j += 2
						continue
					}
					j++
					break
				}
				j++
			}
			if j > n {
				j = n
			}
			b.WriteString(q[i:j])
			i = j
		case c == '$':
			// PostgreSQL dollar quoting: $$...$$ or $tag$...$tag$.
			if tag := sqlDollarTag(q[i:]); tag != "" {
				end := strings.Index(q[i+len(tag):], tag)
				j := n
				if end >= 0 {
					j = i + len(tag) + end + len(tag)
				}
				b.WriteString(q[i:j])
				i = j
				continue
			}
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func sqlDollarTag(s string) string {
	for j := 1; j < len(s); j++ {
		ch := s[j]
		if ch == '$' {
			return s[:j+1]
		}
		if !(ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || j > 1 && ch >= '0' && ch <= '9') {
			return ""
		}
	}
	return ""
}

func isSQLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
