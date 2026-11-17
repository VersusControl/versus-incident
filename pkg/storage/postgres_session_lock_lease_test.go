package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockDriver stands in for a Postgres server behind a pinned lock session:
// it grants every advisory lock, answers the lock check from held/checkErr,
// and, when hangUnlock is set, never answers the unlock statement (an
// unreachable database) until the caller's context ends.
type lockDriver struct {
	mu         sync.Mutex
	hangUnlock bool
	held       bool
	checkErr   error
	opened     int
	closed     int
}

func (d *lockDriver) Connect(context.Context) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened++
	return &lockConn{d: d}, nil
}

func (d *lockDriver) Driver() driver.Driver { return nil }

func (d *lockDriver) counts() (opened, closed int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.opened, d.closed
}

type lockConn struct{ d *lockDriver }

func (c *lockConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c *lockConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func (c *lockConn) Close() error {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	c.d.closed++
	return nil
}

func (c *lockConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "pg_try_advisory_lock"):
		return &boolRows{v: true}, nil
	case strings.Contains(query, "pg_locks"):
		c.d.mu.Lock()
		held, err := c.d.held, c.d.checkErr
		c.d.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return &boolRows{v: held}, nil
	}
	return nil, errors.New("unexpected query: " + query)
}

func (c *lockConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.Contains(query, "pg_advisory_unlock") {
		c.d.mu.Lock()
		hang := c.d.hangUnlock
		c.d.mu.Unlock()
		if hang {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return driver.RowsAffected(0), nil
	}
	return nil, errors.New("unexpected statement: " + query)
}

type boolRows struct {
	v    bool
	done bool
}

func (r *boolRows) Columns() []string { return []string{"v"} }
func (r *boolRows) Close() error      { return nil }
func (r *boolRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.v
	return nil
}

func lockTestProvider(t *testing.T, d *lockDriver) *postgresProvider {
	t.Helper()
	db := sql.OpenDB(d)
	t.Cleanup(func() { _ = db.Close() })
	return &postgresProvider{db: db}
}

// TestSessionUnlockGivesUpWhenTheDatabaseDoesNotAnswer: the database stops
// answering while a replica holds a session lock. The unlock gives up after
// its deadline instead of hanging the caller, and the session is discarded,
// not pooled, so the server ends the lock once the connection drops and no
// pooled connection keeps the key.
func TestSessionUnlockGivesUpWhenTheDatabaseDoesNotAnswer(t *testing.T) {
	previous := sessionUnlockTimeout
	sessionUnlockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sessionUnlockTimeout = previous })

	d := &lockDriver{hangUnlock: true}
	p := lockTestProvider(t, d)
	unlock, acquired, err := p.TryLockSession(context.Background(), 42)
	if err != nil || !acquired {
		t.Fatalf("lock acquired=%v err=%v", acquired, err)
	}
	done := make(chan struct{})
	began := time.Now()
	go func() {
		unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the unlock hung on a database that does not answer")
	}
	if took := time.Since(began); took < sessionUnlockTimeout {
		t.Fatalf("the unlock returned after %s, before its deadline: it did not wait for the answer", took)
	}
	if opened, closed := d.counts(); opened != 1 || closed != 1 {
		t.Fatalf("the unanswered session was pooled: opened %d, closed %d", opened, closed)
	}
	if p.db.Stats().OpenConnections != 0 {
		t.Fatalf("the pool kept %d connections after the discard", p.db.Stats().OpenConnections)
	}
	if p.sessionLocks.held != 0 {
		t.Fatalf("the pinned connection was not given back to the pool limit: %d held", p.sessionLocks.held)
	}
	if held, err := p.SessionLockHeld(context.Background(), 42); held || err != nil {
		t.Fatalf("a released key reads held=%v err=%v", held, err)
	}
}

// TestSessionLockHeldAnswersFromThePinnedSession: while the holding session
// has the key the check answers held; when that session lost it, or the
// check cannot reach it, the answer is not held, so the work stops.
func TestSessionLockHeldAnswersFromThePinnedSession(t *testing.T) {
	d := &lockDriver{held: true}
	p := lockTestProvider(t, d)
	if held, err := p.SessionLockHeld(context.Background(), 7); held || err != nil {
		t.Fatalf("a key never taken reads held=%v err=%v", held, err)
	}
	unlock, acquired, err := p.TryLockSession(context.Background(), 7)
	if err != nil || !acquired {
		t.Fatalf("lock acquired=%v err=%v", acquired, err)
	}
	defer unlock()
	if held, err := p.SessionLockHeld(context.Background(), 7); !held || err != nil {
		t.Fatalf("a held key reads held=%v err=%v", held, err)
	}

	d.mu.Lock()
	d.held = false
	d.mu.Unlock()
	if held, err := p.SessionLockHeld(context.Background(), 7); held || err != nil {
		t.Fatalf("a key the session lost reads held=%v err=%v", held, err)
	}

	d.mu.Lock()
	d.checkErr = io.ErrUnexpectedEOF
	d.mu.Unlock()
	if held, err := p.SessionLockHeld(context.Background(), 7); held || err == nil {
		t.Fatalf("a check on a dropped session reads held=%v err=%v", held, err)
	}
}
