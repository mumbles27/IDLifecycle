package connstr

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrConnectPending is returned while an earlier, abandoned connect attempt to the same
// database is still blocked inside the ODBC driver.
var ErrConnectPending = errors.New("an earlier connection attempt to this database timed out and is still waiting for the ODBC driver to give up; try again later (set the driver's connect-timeout option so it gives up sooner)")

var (
	pendingMu sync.Mutex
	pending   = map[string]int{} // key -> abandoned attempts still blocked in the driver
)

// Key identifies a database for the in-flight guard without keeping its connection string.
func Key(dsn string) string {
	sum := sha256.Sum256([]byte(dsn))
	return hex.EncodeToString(sum[:8])
}

// Pending reports whether an abandoned connect attempt for key is still blocked in the driver.
func Pending(key string) bool {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	return pending[key] > 0
}

// BoundedPing opens db's first connection, giving up after limit. The ODBC driver ignores the
// context while connecting (database/sql calls driver.Open without one and SQLDriverConnect
// blocks until the driver's own login timeout, which for some drivers is unbounded), so the
// wait is bounded here: on timeout the caller gets an error naming limit (plus hint, if any)
// and the attempt is abandoned; database/sql closes its connection when it finally completes
// (the caller closes db). Until then a new attempt for the same key is refused with
// ErrConnectPending, so a slow server cannot pile up blocked goroutines and driver handles.
func BoundedPing(ctx context.Context, db *sql.DB, key string, limit time.Duration, hint string) error {
	if Pending(key) {
		return ErrConnectPending
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	var mu sync.Mutex
	abandoned, finished := false, false
	done := make(chan error, 1)
	go func() {
		err := db.PingContext(ctx)
		mu.Lock()
		finished = true
		if abandoned {
			pendingMu.Lock()
			if pending[key]--; pending[key] <= 0 {
				delete(pending, key)
			}
			pendingMu.Unlock()
		}
		mu.Unlock()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		mu.Lock()
		if !finished {
			abandoned = true
			pendingMu.Lock()
			pending[key]++
			pendingMu.Unlock()
		}
		mu.Unlock()
		msg := fmt.Sprintf("no answer within %s", limit)
		if hint != "" {
			msg += " (" + hint + ")"
		}
		return fmt.Errorf("%s: %w", msg, ctx.Err())
	}
}
