package sources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"idlifecycle/internal/connstr"

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
	// IBM Db2 (LUW, z/OS) and any other 64-bit ODBC driver, both through the Windows ODBC driver manager
	// (odbc32.dll, called without cgo). See sql_odbc_windows.go. Db2 for IBM i goes through the generic odbc
	// path: its IBM i Access driver does not know the LUW CLI keywords (MapCharToWChar, LongDataCompat)
	// that "db2" adds, so its text arrives in the ANSI code page and must be decoded as such.
	"db2": "db2", "ibmdb2": "db2", "db2i": "odbc", "as400": "odbc", "ibmi": "odbc",
	"odbc": "odbc",
}

// sqlOpenName is the database/sql driver name registered by each driver package.
var sqlOpenName = map[string]string{"sqlserver": "sqlserver", "postgres": "pgx", "mysql": "mysql", "sqlite": "sqlite", "db2": "odbc", "odbc": "odbc"}

// isODBC reports whether the canonical driver goes through the ODBC driver manager; its DSN is
// then an ODBC connection string (Driver={...};...;Pwd=...; or DSN=name;...).
func isODBC(driver string) bool { return driver == "db2" || driver == "odbc" }

func sqlType() Type {
	return Type{
		Name:  "sql",
		Label: "SQL database (table or view)",
		Fields: []Field{
			{Name: "driver", Label: "Database driver", Type: "string", Required: true, Help: "One of: sqlserver, postgres, mysql, sqlite, db2, odbc (aliases: mssql, postgresql, pgx, mariadb; ibmdb2 for db2; db2i, as400, ibmi for odbc). " +
				"db2 and odbc (Windows only) use an installed 64-bit ODBC driver: db2 for IBM Db2 LUW and z/OS, odbc for Db2 for IBM i and any other ODBC driver (Oracle, Informix, Snowflake...)."},
			{Name: "dsn", Label: "Connection string", Type: "password", Required: true, Help: "Stored encrypted; never shown again. Or env:VAR_NAME to read it from the service's environment at run time. " +
				"Examples - sqlserver: sqlserver://user:pass@host:1433?database=HR&encrypt=true (omit user:pass@, e.g. sqlserver://host?database=HR&encrypt=true, " +
				"to use Windows integrated auth as the service account); postgres: postgres://user:pass@host:5432/hr?sslmode=require; " +
				"mysql: user:pass@tcp(host:3306)/hr?tls=true; sqlite: file:C:/hr/hr.db?mode=ro. " +
				"db2 / odbc take an ODBC connection string. Db2 LUW or z/OS (IBM Data Server Driver for ODBC and CLI; z/OS usually also needs DB2 Connect licensing; " +
				"the driver name depends on how it was installed): Driver={IBM DB2 ODBC DRIVER};Hostname=db2.corp;Port=50000;Protocol=TCPIP;Database=HR;Uid=svc_hr_ro;Pwd=...; " +
				"Db2 for IBM i (IBM i Access Client Solutions ODBC driver): Driver={IBM i Access ODBC Driver};System=as400.corp;DefaultLibraries=HRLIB;Uid=...;Pwd=...; " +
				"simplest: DSN=HRDB2; naming a system DSN created in ODBC Data Sources (64-bit). Use a read-only database login. " +
				"For db2 the service appends LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20; unless set (MapCharToWChar=0 is refused)."},
			{Name: "query", Label: "Query", Type: "text", Required: true, Help: "One read-only SELECT (or WITH ... SELECT) statement, e.g. SELECT * FROM dbo.vw_Employees. " +
				"Each column becomes an attribute named after the column (alias computed columns). Statements that modify data are refused, " +
				"but this is defence in depth: the database login itself should only be able to read. " +
				"db2/odbc: DECIMAL/NUMERIC arrive as floating point, printed in shortest form (12345.10 reads 12345.1), and a value wider than " +
				"15 digits fails the fetch: CAST code-like numeric keys to CHAR. DATE reads 2024-01-31, TIMESTAMP as UTC (2024-01-31T07:30:00Z). " +
				"Binary columns (VARBINARY, BLOB, FOR BIT DATA) are refused: select HEX(col) AS col."},
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
	return "", fmt.Errorf("sql: unsupported driver %q (use sqlserver, postgres, mysql, sqlite, db2 or odbc)", name)
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

// driverDSN adds what the driver needs to the resolved DSN: for db2 (not generic odbc) the same
// IBM CLI keywords as the db2 store (CLOB readable, text as wide strings, MapCharToWChar=0
// refused) and the driver's own ConnectTimeout, never longer than timeout_seconds.
func (s *sqlSource) driverDSN(dsn string) (string, error) {
	if s.driver != "db2" {
		return dsn, nil
	}
	out, err := connstr.AugmentDB2(dsn, min(connstr.DB2ConnectTimeout, max(1, int(s.timeout/time.Second))))
	if err != nil {
		return "", fmt.Errorf("sql: db2: %w", err)
	}
	return out, nil
}

func (s *sqlSource) Fetch(ctx context.Context) (rows []Row, err error) {
	dsn, err := s.resolvedDSN()
	if err != nil {
		return nil, err
	}
	// Every error leaving this function is scrubbed of the DSN and its password.
	orig := dsn
	defer func() {
		if err != nil {
			err = connstr.Scrub(connstr.Scrub(err, dsn), orig)
		}
	}()

	// A SQLite file is a file like a CSV one: the same policy applies, so the service's own database
	// (password hashes, encrypted secrets) can never be read back as "HR data".
	if s.driver == "sqlite" && PathPolicy != nil {
		if err := PathPolicy(sqliteDSNPath(dsn)); err != nil {
			return nil, fmt.Errorf("sql: %w", err)
		}
	}

	if isODBC(s.driver) && !odbcAvailable {
		return nil, fmt.Errorf("sql: the %s driver uses the Windows ODBC driver manager (odbc32.dll) and is only available when the service runs on Windows", s.driver)
	}
	if dsn, err = s.driverDSN(dsn); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	db, err := sql.Open(sqlOpenName[s.driver], dsn)
	if err != nil {
		return nil, fmt.Errorf("sql: open %s: %w", s.driver, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := s.connect(ctx, db, dsn); err != nil {
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
		if isODBC(s.driver) && strings.Contains(err.Error(), "unsupported column type") {
			// The ODBC layer cannot read some types (e.g. Db2 GRAPHIC/CLOB/DECFLOAT/XML as described by the driver).
			return nil, fmt.Errorf("sql: query: %w (CAST that column to VARCHAR/CHAR in the query)", err)
		}
		return nil, fmt.Errorf("sql: query: %w", err)
	}
	defer res.Close()
	return scanSQLRows(res, s.maxRows, s.driver)
}

// connect opens the (single) connection. The ODBC driver ignores the context while connecting,
// so for ODBC the wait is bounded by connstr.BoundedPing: on timeout Fetch returns and the
// abandoned attempt is closed by database/sql when it finally completes (the DB is closed by
// then). Until it does, a new Fetch for the same connection string is refused.
func (s *sqlSource) connect(ctx context.Context, db *sql.DB, dsn string) error {
	if !isODBC(s.driver) {
		return db.PingContext(ctx)
	}
	return connstr.BoundedPing(ctx, db, connstr.Key(dsn), s.timeout,
		"add the driver's connect-timeout option to the connection string, or raise timeout_seconds")
}

// beginReadOnly starts a read-only transaction where the driver supports it. go-mssqldb rejects
// TxOptions.ReadOnly outright, so SQL Server (and any driver that refuses it) gets a plain
// transaction instead; the caller always rolls it back. github.com/alexbrainman/odbc (db2, odbc)
// does not implement driver.ConnBeginTx, so database/sql itself refuses ReadOnly ("driver does not
// support read-only transactions") without touching the connection and the fallback is used: a
// manual-commit unit of work (autocommit off) that is always rolled back. There is no portable
// ODBC way to make it read-only from here (SQL_ATTR_ACCESS_MODE is a hint most drivers ignore and
// is not reachable through database/sql), so for Db2 the read-only login is the real guard.
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
// duplicate (case-insensitive) columns are an error, like duplicate CSV headers. driver is the
// canonical driver name: db2/odbc values go through odbcValueString, which refuses values it
// cannot transfer exactly.
func scanSQLRows(res *sql.Rows, maxRows int, driver string) ([]Row, error) {
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
	kinds := make([]colKind, len(cols))
	if isODBC(driver) {
		if cts, err := res.ColumnTypes(); err == nil && len(cts) == len(cols) {
			for i, ct := range cts {
				kinds[i] = columnKind(ct.DatabaseTypeName())
			}
		}
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
			if isODBC(driver) {
				v, err := odbcValueString(driver, kinds[i], vals[i])
				if err != nil {
					return nil, fmt.Errorf("sql: column %s: %w", c, err)
				}
				row[c] = v
				continue
			}
			row[c] = sqlValueString(vals[i])
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("sql: reading rows: %w", err)
	}
	return rows, nil
}

// odbcCharString decodes character data fetched through ODBC as SQL_C_CHAR (how
// github.com/alexbrainman/odbc reads CHAR/VARCHAR/LONGVARCHAR). The driver converts it to the
// process's ANSI code page (e.g. Windows-1252), not UTF-8, so non-ASCII names would otherwise come
// out garbled. Bytes that are already valid UTF-8 (ASCII, or a driver configured to send UTF-8)
// are kept as they are. NCHAR/NVARCHAR/GRAPHIC described as wide types arrive as Go strings.
// Only used for the generic odbc driver: db2 connections get MapCharToWChar=1 (wide strings).
func odbcCharString(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return ansiToUTF8(b)
}

// colKind is what a result column's reported database type says about its []byte values.
type colKind int

const (
	kindUnknown colKind = iota // no type reported (github.com/alexbrainman/odbc reports none)
	kindChar
	kindBinary
)

// columnKind classifies a DatabaseTypeName. github.com/alexbrainman/odbc implements neither
// ColumnTypeDatabaseTypeName nor ColumnTypeScanType, so through it every column is kindUnknown
// and odbcValueString falls back to inspecting the bytes; a driver that reports types is used.
func columnKind(name string) colKind {
	n := strings.ToUpper(name)
	switch {
	case n == "":
		return kindUnknown
	case strings.Contains(n, "BINARY"), strings.Contains(n, "BLOB"), strings.Contains(n, "BIT DATA"),
		strings.Contains(n, "BYTEA"), strings.Contains(n, "IMAGE"):
		return kindBinary
	case strings.Contains(n, "CHAR"), strings.Contains(n, "TEXT"), strings.Contains(n, "CLOB"),
		strings.Contains(n, "GRAPHIC"), strings.Contains(n, "XML"), strings.Contains(n, "STRING"):
		return kindChar
	}
	return kindUnknown
}

// looksBinary reports bytes no text column would hold: NUL or other C0 control characters
// (tab, CR and LF excepted).
func looksBinary(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\r' && c != '\n' {
			return true
		}
	}
	return false
}

// errODBCBinary is returned for binary column data (VARBINARY, BLOB, CHAR ... FOR BIT DATA):
// it is never guessed into text, because a guessed encoding could change between runs.
var errODBCBinary = errors.New("binary data (VARBINARY/BLOB/CHAR FOR BIT DATA) is not used as text; convert it in the query, e.g. HEX(col) AS col, or CAST it to CHAR/VARCHAR if it really is text")

// odbcValueString renders a value fetched through github.com/alexbrainman/odbc (db2, odbc):
//   - SMALLINT/INTEGER arrive as int32, BIGINT as int64: exact.
//   - DECIMAL/NUMERIC/FLOAT/DOUBLE arrive as float64. A value that float64 cannot carry exactly
//     (an integer of 2^53 or more, or more than 15 significant digits) fails the fetch rather than
//     being rounded: two employee numbers could otherwise collapse into one identity. Values are
//     printed in shortest form, so DECIMAL 12345.10 reads "12345.1" (CAST code-like numeric keys
//     to CHAR to keep their exact text).
//   - DATE arrives as time.Time at local midnight and is printed 2006-01-02; TIMESTAMP is printed
//     as RFC 3339 in UTC; TIME as 15:04:05. Neither depends on the service's time zone or DST.
//   - Wide character types arrive as strings. []byte is character data for the generic odbc
//     driver (SQL_C_CHAR, decoded from the ANSI code page) but for db2, whose character data
//     is wide (MapCharToWChar=1), it is binary unless it is plain UTF-8 text (e.g. a driver that
//     ignores MapCharToWChar); binary data is refused (errODBCBinary), never ANSI-decoded.
func odbcValueString(driver string, kind colKind, v any) (string, error) {
	switch x := v.(type) {
	case float64:
		s, ok := exactFloatString(x)
		if !ok {
			return "", errors.New("numeric value too wide for exact transfer over ODBC (DECIMAL > 15 digits); CAST it to CHAR/VARCHAR in the query")
		}
		return s, nil
	case time.Time:
		switch {
		case x.Year() == 1 && x.Month() == time.January && x.Day() == 1:
			return x.Format("15:04:05"), nil // TIME (the driver dates it 0001-01-01)
		case x.Hour() == 0 && x.Minute() == 0 && x.Second() == 0 && x.Nanosecond() == 0:
			return x.Format("2006-01-02"), nil
		}
		return x.UTC().Format(time.RFC3339), nil
	case []byte:
		if len(x) == 0 {
			return "", nil // an empty wide string also arrives as an empty []byte
		}
		switch {
		case kind == kindBinary:
			return "", errODBCBinary
		case driver == "db2":
			if !utf8.Valid(x) || looksBinary(x) {
				return "", errODBCBinary
			}
			return strings.TrimSpace(string(x)), nil
		case kind == kindUnknown && looksBinary(x):
			return "", errODBCBinary
		}
		return strings.TrimSpace(odbcCharString(x)), nil
	}
	return sqlValueString(v), nil
}

// exactFloatString formats x in shortest form and reports whether that text is exactly the
// value the database sent: integers below 2^53 and values of at most 15 significant digits
// (the most every decimal is guaranteed to survive a float64 round trip with).
func exactFloatString(x float64) (string, bool) {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return strconv.FormatFloat(x, 'g', -1, 64), true
	}
	s := strconv.FormatFloat(x, 'f', -1, 64)
	if x == math.Trunc(x) {
		return s, math.Abs(x) < 1<<53
	}
	mant := strconv.FormatFloat(math.Abs(x), 'e', -1, 64) // d.ddddde±XX
	mant, _, _ = strings.Cut(mant, "e")
	return s, len(strings.Replace(mant, ".", "", 1)) <= 15
}

// sqlValueString renders a scanned value as text (the drivers other than db2/odbc).
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
