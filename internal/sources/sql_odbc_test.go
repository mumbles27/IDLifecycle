package sources

import (
	"database/sql"
	"errors"
	"net"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"idlifecycle/internal/connstr"
	"idlifecycle/internal/model"
)

func TestSQLDb2AndODBCDrivers(t *testing.T) {
	typ, _ := Lookup("sql")
	for alias, want := range map[string]string{
		"db2": "db2", "DB2": "db2", "ibmdb2": "db2", "db2i": "odbc", "AS400": "odbc", "ibmi": "odbc",
		"odbc": "odbc", " ODBC ": "odbc",
	} {
		src, err := typ.New(sqlCfg("DSN=HRDB2;", "SELECT 1 FROM SYSIBM.SYSDUMMY1", map[string]any{"driver": alias}))
		if err != nil || src.(*sqlSource).driver != want {
			t.Errorf("alias %q: %v", alias, err)
			continue
		}
		if !isODBC(want) || sqlOpenName[want] != "odbc" {
			t.Errorf("%s must open through database/sql driver odbc", want)
		}
	}
	for _, d := range []string{"sqlserver", "postgres", "mysql", "sqlite"} {
		if isODBC(d) {
			t.Errorf("%s is not an ODBC driver", d)
		}
	}
	base := model.Source{Name: "hr", Type: "sql", Options: model.SourceOptions{ExternalIDField: "id"}}
	base.Config = sqlCfg(model.MaskedSecret, "SELECT EMPNO AS id FROM HRLIB.EMPLOYEE", map[string]any{"driver": "as400"})
	if err := Validate(base); err != nil {
		t.Fatalf("valid db2 source (masked dsn) rejected: %v", err)
	}

	var driverHelp, dsnHelp string
	for _, f := range typ.Fields {
		switch f.Name {
		case "driver":
			driverHelp = f.Help
		case "dsn":
			dsnHelp = f.Help
		}
	}
	for _, w := range []string{"db2", "odbc", "ibmdb2", "db2i", "as400", "ibmi", "64-bit"} {
		if !strings.Contains(driverHelp, w) {
			t.Errorf("driver help lacks %q", w)
		}
	}
	for _, w := range []string{"Driver={IBM DB2 ODBC DRIVER}", "Hostname=", "Protocol=TCPIP", "Driver={IBM i Access ODBC Driver}", "DefaultLibraries=", "DSN=HRDB2;", "DB2 Connect"} {
		if !strings.Contains(dsnHelp, w) {
			t.Errorf("dsn help lacks %q", w)
		}
	}
}

func TestValidateSQLQueryDb2(t *testing.T) {
	for _, q := range []string{
		"SELECT EMPNO, FIRSTNME, LASTNAME FROM HR.EMPLOYEE FETCH FIRST 10 ROWS ONLY",
		"SELECT * FROM HR.EMPLOYEE WITH UR",
		"SELECT * FROM HR.EMPLOYEE FOR READ ONLY",
		"SELECT * FROM HR.EMPLOYEE FOR FETCH ONLY",
		"SELECT CAST(EMPNO AS CHAR(12)) AS EMPNO FROM HRLIB.EMPLOYEE",
		"SELECT 1 FROM SYSIBM.SYSDUMMY1",
	} {
		if err := ValidateSQLQuery(q); err != nil {
			t.Errorf("should accept %q: %v", q, err)
		}
	}
	for _, q := range []string{
		"SELECT * FROM HR.EMPLOYEE FOR UPDATE",
		"SELECT * FROM HR.EMPLOYEE FOR UPDATE OF SALARY",
		"SELECT * FROM FINAL TABLE (INSERT INTO T VALUES (1))",
		"SELECT * FROM OLD TABLE (DELETE FROM HR.EMPLOYEE)",
		"SELECT * FROM NEW TABLE (UPDATE T SET A = 1)",
		"VALUES CURRENT DATE",
		"CALL HR.SYNC()",
		"SELECT 1 FROM SYSIBM.SYSDUMMY1; DELETE FROM HR.EMPLOYEE",
	} {
		if err := ValidateSQLQuery(q); err == nil {
			t.Errorf("should reject %q", q)
		}
	}
}

func TestScrubODBCConnectionStrings(t *testing.T) {
	cases := map[string][]string{ // dsn -> secrets a driver might echo
		"Driver={IBM DB2 ODBC DRIVER};Hostname=h;Database=HR;Uid=u;Pwd=Secret123;": {"Secret123"},
		"Driver={IBM DB2 ODBC DRIVER};Hostname=h;UID=u;PWD=Secret123":              {"Secret123"},
		"DSN=HRDB2;UID=u;PASSWORD=Secret123;":                                      {"Secret123"},
		"DSN=HRDB2;Uid=u; pwd = Secret123 ;":                                       {"Secret123"},
		"DSN=HRDB2;Uid=u;Pwd=Secret 123;":                                          {"Secret 123"},
		"DSN=HRDB2;Uid=u;Pwd=oldSecret1;NewPwd=newSecret2;":                        {"oldSecret1", "newSecret2"},
		// ODBC braces: ';' is literal inside and "}}" is an escaped "}".
		"Driver={IBM i Access ODBC Driver};System=s;Uid=u;Pwd={Sec;ret=1}}x};Naming=1": {"Sec;ret=1}}x", "Sec;ret=1}x"},
		"Driver={X};Uid=u;Pwd={Secret;123};":                                           {"Secret;123"},
	}
	for dsn, secrets := range cases {
		for _, secret := range secrets {
			msg := "SQLDriverConnect: {28000} login failed using password " + secret
			got := connstr.Scrub(errors.New(msg), dsn).Error()
			if strings.Contains(got, secret) || strings.Contains(got, "ecret") {
				t.Errorf("scrub %q leaked %q: %s", dsn, secret, got)
			}
			// The whole connection string echoed back is scrubbed too.
			if got := connstr.Scrub(errors.New("cannot use "+dsn), dsn).Error(); strings.Contains(got, "ecret") {
				t.Errorf("scrub of echoed dsn %q: %s", dsn, got)
			}
		}
	}
}

func TestODBCCharDecoding(t *testing.T) {
	if got := odbcCharString([]byte("Zoë Ångström")); got != "Zoë Ångström" {
		t.Errorf("valid UTF-8 must be kept: %q", got)
	}
	if got := odbcCharString([]byte("Doe")); got != "Doe" {
		t.Errorf("ascii: %q", got)
	}
	if runtime.GOOS == "windows" && windowsACP() == 1252 {
		// Windows-1252 "José Müller", as an ODBC driver returns SQL_C_CHAR data on this code page.
		if got := odbcCharString([]byte{'J', 'o', 's', 0xE9, ' ', 'M', 0xFC, 'l', 'l', 'e', 'r'}); got != "José Müller" {
			t.Errorf("ANSI (1252) decoding: %q", got)
		}
	} else {
		t.Logf("ANSI code page is %d, not 1252; skipping the Windows-1252 decoding check", windowsACP())
	}

	// Values as github.com/alexbrainman/odbc returns them for Db2 column types.
	for _, c := range []struct {
		in   any
		want string
	}{
		{int32(7), "7"}, // SMALLINT / INTEGER
		{int64(9007199254740993), "9007199254740993"}, // BIGINT is exact
		{float64(12345.1), "12345.1"},                 // DECIMAL(9,2) 12345.10 arrives as float64 (trailing zero lost)
		{float64(123456789), "123456789"},             // DECIMAL(9,0) employee number: no exponent
		{float64(1e15), "1000000000000000"},
		{float64(1<<53 - 1), "9007199254740991"}, // largest exact integer
		{float64(0.1), "0.1"},
		{float64(-1234567890.12345), "-1234567890.12345"}, // 15 significant digits
		{"Ünïcode", "Ünïcode"},                            // NCHAR/NVARCHAR (and GRAPHIC when described as wide chars)
		{nil, ""},
	} {
		for _, drv := range []string{"db2", "odbc"} {
			if got, err := odbcValueString(drv, kindUnknown, c.in); err != nil || got != c.want {
				t.Errorf("%s %#v: got %q %v want %q", drv, c.in, got, err, c.want)
			}
		}
	}
}

// A DECIMAL that float64 cannot carry exactly fails the fetch: rounding could merge two ids.
func TestODBCNumericTooWide(t *testing.T) {
	var big float64 = 123456789012345678 // DECIMAL(18,0)
	var p53 float64 = 1<<53 + 1          // rounds to 2^53 in float64
	for _, x := range []float64{big, p53, 1 << 53, -(1 << 60), 1234567890.123456 /* 16 significant digits */} {
		if got, err := odbcValueString("db2", kindUnknown, x); err == nil || !strings.Contains(err.Error(), "too wide for exact transfer over ODBC") {
			t.Errorf("%v: want an error, got %q %v", x, got, err)
		}
	}

	// The whole message as Fetch reports it, through scanSQLRows (SQLite also returns REAL as float64).
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Query("SELECT 'E1' AS id, CAST(123456789012345678 AS REAL) AS empno")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	_, err = scanSQLRows(res, 10, "db2")
	want := "sql: column empno: numeric value too wide for exact transfer over ODBC (DECIMAL > 15 digits); CAST it to CHAR/VARCHAR in the query"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
}

// DATE is printed without a time, TIMESTAMP in UTC: neither depends on the service's zone or DST.
func TestODBCDates(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.FixedZone("EST", -5*3600)
	}
	for _, c := range []struct {
		in   time.Time
		want string
	}{
		{time.Date(2024, 1, 31, 0, 0, 0, 0, time.Local), "2024-01-31"}, // DATE (the driver uses local midnight)
		{time.Date(2024, 7, 1, 0, 0, 0, 0, ny), "2024-07-01"},
		{time.Date(2024, 1, 31, 8, 30, 0, 0, ny), "2024-01-31T13:30:00Z"}, // TIMESTAMP
		{time.Date(2024, 7, 31, 8, 30, 0, 0, ny), "2024-07-31T12:30:00Z"}, // in DST
		{time.Date(2024, 1, 31, 0, 0, 0, 5000, time.UTC), "2024-01-31T00:00:00Z"},
		{time.Date(1, 1, 1, 8, 30, 5, 0, time.Local), "08:30:05"}, // TIME
	} {
		if got, err := odbcValueString("db2", kindUnknown, c.in); err != nil || got != c.want {
			t.Errorf("%v: got %q %v want %q", c.in, got, err, c.want)
		}
	}
}

// []byte through ODBC: binary data is never ANSI-decoded into text.
func TestODBCBinaryColumns(t *testing.T) {
	bin := []byte{0x00, 0x1F, 0xE9, 0x41}
	for _, drv := range []string{"db2", "odbc"} {
		if _, err := odbcValueString(drv, kindUnknown, bin); !errors.Is(err, errODBCBinary) {
			t.Errorf("%s: control/NUL bytes must be refused as binary: %v", drv, err)
		}
		if _, err := odbcValueString(drv, kindBinary, []byte("ABC")); !errors.Is(err, errODBCBinary) {
			t.Errorf("%s: a column reported as binary is refused even if it looks like text: %v", drv, err)
		}
		if got, err := odbcValueString(drv, kindUnknown, []byte{}); err != nil || got != "" {
			t.Errorf("%s: an empty value (also an empty wide string) is empty: %q %v", drv, got, err)
		}
		if got, err := odbcValueString(drv, kindUnknown, []byte(" Zoë ")); err != nil || got != "Zoë" {
			t.Errorf("%s: UTF-8 text: %q %v", drv, got, err)
		}
	}
	// db2 text arrives as wide strings (MapCharToWChar=1): bytes that are not UTF-8 are binary there;
	// only the generic odbc driver decodes them from the ANSI code page.
	ansi := []byte{'J', 'o', 's', 0xE9}
	if _, err := odbcValueString("db2", kindUnknown, ansi); !errors.Is(err, errODBCBinary) {
		t.Errorf("db2: non-UTF-8 bytes must not be ANSI-guessed: %v", err)
	}
	if _, err := odbcValueString("db2", kindChar, ansi); !errors.Is(err, errODBCBinary) {
		t.Errorf("db2: non-UTF-8 bytes must not be ANSI-guessed: %v", err)
	}
	if got, err := odbcValueString("odbc", kindChar, ansi); err != nil || got != ansiToUTF8(ansi) {
		t.Errorf("odbc: char column decoded from ANSI: %q %v", got, err)
	}
	for name, want := range map[string]colKind{
		"": kindUnknown, "VARCHAR": kindChar, "NVARCHAR": kindChar, "CLOB": kindChar, "GRAPHIC": kindChar,
		"VARBINARY": kindBinary, "BLOB": kindBinary, "CHAR () FOR BIT DATA": kindBinary, "INTEGER": kindUnknown,
	} {
		if got := columnKind(name); got != want {
			t.Errorf("columnKind(%q) = %d, want %d", name, got, want)
		}
	}
}

// db2 sources get the store's IBM CLI keywords; generic odbc sources are left alone.
func TestDB2SourceDSNAugmentation(t *testing.T) {
	typ, _ := Lookup("sql")
	mk := func(driver string, timeout int) *sqlSource {
		src, err := typ.New(sqlCfg("DSN=HR;Pwd=x", "SELECT 1 FROM SYSIBM.SYSDUMMY1", map[string]any{"driver": driver, "timeout_seconds": timeout}))
		if err != nil {
			t.Fatal(err)
		}
		return src.(*sqlSource)
	}
	if got, err := mk("db2", 120).driverDSN("DSN=HR;Pwd=x"); err != nil || got != "DSN=HR;Pwd=x;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;" {
		t.Errorf("db2: %q %v", got, err)
	}
	// IBM i Access does not know the LUW CLI keywords: its connection string is passed through untouched.
	for _, d := range []string{"as400", "ibmi", "db2i"} {
		const ibmi = "Driver={IBM i Access ODBC Driver};System=as400.corp;Pwd=x;"
		if got, err := mk(d, 120).driverDSN(ibmi); err != nil || got != ibmi {
			t.Errorf("%s must not be augmented: %q %v", d, got, err)
		}
	}
	if got, err := mk("ibmdb2", 5).driverDSN("DSN=HR;Pwd=x;"); err != nil || got != "DSN=HR;Pwd=x;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=5;" {
		t.Errorf("db2 alias, short timeout: %q %v", got, err)
	}
	if got, err := mk("db2", 120).driverDSN("DSN=HR;Pwd=x;MapCharToWChar=1;ConnectTimeout=60;LONGDATACOMPAT=1"); err != nil || got != "DSN=HR;Pwd=x;MapCharToWChar=1;ConnectTimeout=60;LONGDATACOMPAT=1;" {
		t.Errorf("db2 with keywords set: %q %v", got, err)
	}
	if _, err := mk("db2", 120).driverDSN("DSN=HR;mapchartowchar = 0"); err == nil || !strings.HasPrefix(err.Error(), "sql: db2: ") {
		t.Errorf("db2 MapCharToWChar=0 must be refused: %v", err)
	}
	if got, err := mk("odbc", 120).driverDSN("DSN=HR;Pwd=x"); err != nil || got != "DSN=HR;Pwd=x" {
		t.Errorf("odbc must be unchanged: %q %v", got, err)
	}
}

// odbcFetch runs a real Fetch through the odbc driver (odbc32.dll). There is no Db2 here, so the
// connections are meant to fail; they prove the driver is wired up and that errors are clean.
func odbcFetch(t *testing.T, driver, dsn string, timeout int) (time.Duration, error) {
	t.Helper()
	if !odbcAvailable {
		t.Skip("ODBC sources are Windows-only (odbc32.dll); skipping on " + runtime.GOOS)
	}
	start := time.Now()
	_, err := fetchSQL(t, sqlCfg(dsn, "SELECT 1 AS x FROM SYSIBM.SYSDUMMY1", map[string]any{"driver": driver, "timeout_seconds": timeout}))
	return time.Since(start), err
}

func TestODBCRoundTripFailsCleanly(t *testing.T) {
	if !odbcAvailable {
		t.Skip("ODBC sources are Windows-only (odbc32.dll); skipping on " + runtime.GOOS)
	}
	if !slices.Contains(sql.Drivers(), "odbc") {
		t.Fatalf("database/sql driver odbc is not registered: %v", sql.Drivers())
	}
	checkODBCConnectErrors(t, []struct{ driver, dsn string }{
		{"db2", "Driver={No Such Driver};Hostname=db2.invalid;Uid=u;Pwd=" + odbcTestSecret + ";"},
		{"odbc", "Driver={No Such Driver};Pwd={" + odbcTestSecret + "};"},
		{"db2", "DSN=IDLifecycleNoSuchDSN;Uid=u;Pwd=" + odbcTestSecret + ";"},
	})
}

// TestODBCRealDriverFailsCleanly goes through a real ODBC driver (the in-box Windows "SQL Server"
// one) to a server that is not there. That driver has a fixed ~15s login timeout that cannot be
// lowered from the connection string, so it runs in parallel and is skipped with -short.
func TestODBCRealDriverFailsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (~16s driver login timeout); skipped with -short")
	}
	if !odbcAvailable {
		t.Skip("ODBC sources are Windows-only (odbc32.dll); skipping on " + runtime.GOOS)
	}
	t.Parallel()
	checkODBCConnectErrors(t, []struct{ driver, dsn string }{
		{"odbc", "Driver={SQL Server};Server=127.0.0.1,1;Database=x;Uid=u;Pwd=" + odbcTestSecret + ";"},
	})
}

const odbcTestSecret = "Secret123"

func checkODBCConnectErrors(t *testing.T, cases []struct{ driver, dsn string }) {
	t.Helper()
	const secret = odbcTestSecret
	for _, c := range cases {
		took, err := odbcFetch(t, c.driver, c.dsn, 30)
		if err == nil {
			t.Errorf("%s: expected a connection error", c.dsn)
			continue
		}
		msg := err.Error()
		t.Logf("%s (%s): %s", c.driver, took.Round(time.Millisecond), msg)
		if !strings.HasPrefix(msg, "sql: connect "+c.driver+": ") {
			t.Errorf("error not wrapped as a connect error: %s", msg)
		}
		if strings.Contains(msg, secret) {
			t.Errorf("error leaks the password: %s", msg)
		}
		if strings.Contains(strings.ToLower(msg), "unknown driver") {
			t.Errorf("odbc driver not registered: %s", msg)
		}
		if !strings.Contains(msg, "SQLDriverConnect") {
			t.Errorf("expected the ODBC driver manager's own error: %s", msg)
		}
	}
}

func TestODBCEnvDSN(t *testing.T) {
	t.Setenv("IDL_TEST_DB2_DSN", "Driver={No Such Driver};Uid=u;Pwd=Secret123;")
	_, err := odbcFetch(t, "ibmdb2", "env:IDL_TEST_DB2_DSN", 20)
	if err == nil || !strings.Contains(err.Error(), "SQLDriverConnect") || strings.Contains(err.Error(), "Secret123") {
		t.Fatalf("env: dsn must reach the ODBC driver and be scrubbed: %v", err)
	}
	t.Setenv("IDL_TEST_DB2_DSN", "")
	if _, err := odbcFetch(t, "db2", "env:IDL_TEST_DB2_DSN", 20); err == nil || !strings.Contains(err.Error(), "IDL_TEST_DB2_DSN") {
		t.Fatalf("empty env var must error by name: %v", err)
	}
}

// silentListener accepts TCP connections and never answers, so a driver's login waits for its
// own timeout. stop closes it and the accepted connections.
func silentListener(t *testing.T) (port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = ln.Close()
			mu.Lock()
			for _, c := range conns {
				_ = c.Close()
			}
			mu.Unlock()
		})
	}
	t.Cleanup(stop)
	return ln.Addr().(*net.TCPAddr).Port, stop
}

// TestODBCConnectIsBoundedByTimeout: the in-box SQL Server ODBC driver connects to a listener
// that never answers and would wait for its fixed ~15s login timeout. Fetch must give up after
// timeout_seconds, and while that abandoned attempt is still blocked a new Fetch is refused.
func TestODBCConnectIsBoundedByTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("slow (~16s: the abandoned attempt is waited for until the driver gives up); skipped with -short")
	}
	if !odbcAvailable {
		t.Skip("ODBC sources are Windows-only (odbc32.dll); skipping on " + runtime.GOOS)
	}
	t.Parallel()
	port, stop := silentListener(t)
	dsn := "Driver={SQL Server};Server=127.0.0.1," + strconv.Itoa(port) + ";Database=x;Uid=u;Pwd=" + odbcTestSecret + ";"
	took, err := odbcFetch(t, "odbc", dsn, 1)
	if err == nil || strings.Contains(err.Error(), odbcTestSecret) || !strings.HasPrefix(err.Error(), "sql: connect odbc: no answer within 1s") {
		t.Fatalf("expected the timeout branch's error: %v", err)
	}
	if took > 5*time.Second {
		t.Fatalf("connect took %s with timeout_seconds=1", took)
	}
	t.Logf("%s: %v", took.Round(time.Millisecond), err)

	// Immediately again: the first attempt is still blocked inside the driver.
	_, err = odbcFetch(t, "odbc", dsn, 1)
	if !errors.Is(err, connstr.ErrConnectPending) || !strings.HasPrefix(err.Error(), "sql: connect odbc: an earlier connection attempt") {
		t.Fatalf("a second Fetch must be refused while the first is blocked: %v", err)
	}
	if !connstr.Pending(connstr.Key(dsn)) {
		t.Fatal("abandoned attempt not tracked")
	}

	// When the driver finally returns (the in-box driver waits out its ~15s login timeout even
	// after the hang-up), the guard clears.
	stop()
	deadline := time.Now().Add(30 * time.Second)
	for connstr.Pending(connstr.Key(dsn)) {
		if time.Now().After(deadline) {
			t.Fatal("guard not cleared after the driver returned")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
