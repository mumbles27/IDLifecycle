package connstr

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScrub(t *testing.T) {
	const dsn = "Driver={IBM DB2 ODBC DRIVER};Hostname=h;Uid=u;Pwd=Secret123;"
	for _, msg := range []string{
		"connect db2: SQLDriverConnect: {08001} login failed using Secret123",
		"cannot use " + dsn,
	} {
		if got := Scrub(errors.New(msg), dsn).Error(); strings.Contains(got, "Secret123") {
			t.Errorf("leaked: %s", got)
		}
	}
	plain := errors.New("connection refused")
	if Scrub(plain, dsn) != plain {
		t.Error("an error without the DSN must be returned unchanged (chain kept)")
	}
	if Scrub(nil, dsn) != nil {
		t.Error("nil stays nil")
	}
}

func TestODBCValue(t *testing.T) {
	const dsn = "Driver={IBM DB2 ODBC DRIVER};Hostname=h; Pwd={a;MapCharToWChar=0}}b} ;mapchartowchar = 0 ;Uid= u x ;Last=z"
	for key, want := range map[string]string{
		"Driver": "IBM DB2 ODBC DRIVER", "pwd": "a;MapCharToWChar=0}b", "MapCharToWChar": " 0 ", "UID": " u x ", "last": "z",
	} {
		if got, ok := ODBCValue(dsn, key); !ok || got != want {
			t.Errorf("%s = %q, %v; want %q", key, got, ok, want)
		}
	}
	if _, ok := ODBCValue(dsn, "Database"); ok {
		t.Error("absent key found")
	}
	// A keyword inside a braced value is not a keyword.
	if _, ok := ODBCValue("Pwd={x;LongDataCompat=0};", "LongDataCompat"); ok {
		t.Error("keyword inside a braced value must not count")
	}
}

func TestAugmentDB2(t *testing.T) {
	cases := map[string]string{
		"DSN=IDLDB;Uid=idl;Pwd=x":  "DSN=IDLDB;Uid=idl;Pwd=x;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		"DSN=IDLDB;Uid=idl;Pwd=x;": "DSN=IDLDB;Uid=idl;Pwd=x;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		"DSN=IDLDB;Pwd=x;  \r\n":   "DSN=IDLDB;Pwd=x;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		// Values are never trimmed: a password ending in a space keeps it.
		"DSN=IDLDB;Pwd= x ": "DSN=IDLDB;Pwd= x ;LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		"DSN=X;longdatacompat = 1;MAPCHARTOWCHAR=1;connecttimeout=5;": "DSN=X;longdatacompat = 1;MAPCHARTOWCHAR=1;connecttimeout=5;",
		"DSN=X;Pwd={a;LongDataCompat=0}":                              "DSN=X;Pwd={a;LongDataCompat=0};LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
		"":                                                            "LongDataCompat=1;MapCharToWChar=1;ConnectTimeout=20;",
	}
	for in, want := range cases {
		if got, err := AugmentDB2(in, 20); err != nil || got != want {
			t.Errorf("AugmentDB2(%q)\n got %q %v\nwant %q", in, got, err, want)
		}
	}
	for _, in := range []string{"MapCharToWChar=0;DSN=X", "DSN=X; mapchartowchar = 0 ;", "DSN=X;MAPCHARTOWCHAR=0"} {
		if _, err := AugmentDB2(in, 20); err == nil || !strings.Contains(err.Error(), "MapCharToWChar") {
			t.Errorf("%q must be refused: %v", in, err)
		}
	}
}

// ---- BoundedPing, with a driver whose connect blocks until released ----------

type blockDriver struct{}

var (
	blockOnce sync.Once
	blockMu   sync.Mutex
	blockGate = map[string]chan struct{}{}
)

func (blockDriver) Open(name string) (driver.Conn, error) {
	blockMu.Lock()
	gate := blockGate[name]
	blockMu.Unlock()
	<-gate
	return nil, errors.New("driver gave up")
}

func TestBoundedPingTimesOutAndGuardsAbandonedAttempt(t *testing.T) {
	blockOnce.Do(func() { sql.Register("idl-block", blockDriver{}) })
	name := t.Name()
	gate := make(chan struct{})
	blockMu.Lock()
	blockGate[name] = gate
	blockMu.Unlock()
	key := Key(name)

	db, _ := sql.Open("idl-block", name)
	defer db.Close()
	start := time.Now()
	err := BoundedPing(context.Background(), db, key, 50*time.Millisecond, "a hint")
	if err == nil || !strings.HasPrefix(err.Error(), "no answer within 50ms (a hint)") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the timeout error, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("not bounded: %s", time.Since(start))
	}
	if !Pending(key) {
		t.Fatal("the abandoned attempt must be tracked")
	}
	db2, _ := sql.Open("idl-block", name)
	defer db2.Close()
	if err := BoundedPing(context.Background(), db2, key, time.Second, ""); !errors.Is(err, ErrConnectPending) {
		t.Fatalf("a new attempt while the old one is blocked must be refused: %v", err)
	}
	if Pending(Key("other")) {
		t.Fatal("the guard is per key")
	}

	close(gate) // the driver finally gives up
	deadline := time.Now().Add(5 * time.Second)
	for Pending(key) {
		if time.Now().After(deadline) {
			t.Fatal("guard not cleared after the attempt returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := BoundedPing(context.Background(), db2, key, time.Second, ""); err == nil || !strings.Contains(err.Error(), "driver gave up") {
		t.Fatalf("after the guard clears the driver's own error comes through: %v", err)
	}
}
