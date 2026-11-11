package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"time"
)

// sessionUnlockTimeout bounds the unlock statement. When the database stops
// answering at the end of the work, the holder must not hang on the unlock:
// after this the session is discarded, which ends the lock on the server
// once the connection drops. A test shortens it.
var sessionUnlockTimeout = 5 * time.Second

// sessionLockPool tracks the connections pinned by held session locks, so the
// pool keeps its configured headroom for the work done while a lock is held.
type sessionLockPool struct {
	mu   sync.Mutex
	held int
	base int
	// conns maps each held key to the pinned connection whose session holds
	// it, so a holder can ask whether that session still has the lock.
	conns map[int64]*sql.Conn
}

// reserve adds one connection to the pool limit for a pinned lock session,
// so a lock holder whose work needs the pool cannot starve itself when the
// limit is small. release gives it back.
func (pool *sessionLockPool) reserve(p *postgresProvider) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.held == 0 {
		pool.base = p.db.Stats().MaxOpenConnections
	}
	pool.held++
	if pool.base > 0 {
		p.db.SetMaxOpenConns(pool.base + pool.held)
	}
}

func (pool *sessionLockPool) release(p *postgresProvider) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.held--
	if pool.base > 0 {
		p.db.SetMaxOpenConns(pool.base + pool.held)
	}
}

func (pool *sessionLockPool) pin(key int64, conn *sql.Conn) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.conns == nil {
		pool.conns = map[int64]*sql.Conn{}
	}
	pool.conns[key] = conn
}

func (pool *sessionLockPool) unpin(key int64, conn *sql.Conn) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.conns[key] == conn {
		delete(pool.conns, key)
	}
}

func (pool *sessionLockPool) pinned(key int64) *sql.Conn {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.conns[key]
}

// sessionLockQueried runs between the lock query and the read of its answer.
// It is nil in production; a test sets it to land a cancellation exactly
// where the server may already have granted the lock.
var sessionLockQueried func()

// discardSession closes a pinned connection's session instead of returning
// it to the pool. Returning driver.ErrBadConn from Raw makes database/sql
// close the driver connection, and the server ends every session lock that
// connection held.
func discardSession(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

// TryLockSession implements SessionLocker with a session-level
// pg_try_advisory_lock on a pinned connection. It never waits: when another
// session holds the key it returns acquired=false. unlock releases the lock
// and returns the connection to the pool. When the lock query fails or its
// answer cannot be read (a cancelled context, a dropped connection), the
// server may have granted the lock anyway, so the session is discarded
// rather than pooled; the same holds when the unlock statement fails or does
// not answer within sessionUnlockTimeout. A pooled connection never keeps
// the key.
func (p *postgresProvider) TryLockSession(ctx context.Context, key int64) (func(), bool, error) {
	pool := &p.sessionLocks
	pool.reserve(p)
	conn, err := p.db.Conn(ctx)
	if err != nil {
		pool.release(p)
		return nil, false, fmt.Errorf("storage: session lock: acquire connection: %w", err)
	}
	var acquired bool
	row := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key)
	if sessionLockQueried != nil {
		sessionLockQueried()
	}
	if err := row.Scan(&acquired); err != nil {
		discardSession(conn)
		pool.release(p)
		return nil, false, fmt.Errorf("storage: session lock: %w", err)
	}
	if !acquired {
		_ = conn.Close()
		pool.release(p)
		return nil, false, nil
	}
	pool.pin(key, conn)
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			pool.unpin(key, conn)
			unlockCtx, cancel := context.WithTimeout(context.Background(), sessionUnlockTimeout)
			defer cancel()
			if _, err := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
				discardSession(conn)
			} else {
				_ = conn.Close()
			}
			pool.release(p)
		})
	}
	return unlock, true, nil
}

// SessionLockHeld implements SessionLockVerifier. It asks, on the pinned
// session that took key, whether that session still holds it (pg_locks for
// its own backend). A session the server ended, or one the connection lost,
// answers false: the work done under the lock must stop. A key this
// provider does not hold answers false.
func (p *postgresProvider) SessionLockHeld(ctx context.Context, key int64) (bool, error) {
	conn := p.sessionLocks.pinned(key)
	if conn == nil {
		return false, nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, sessionUnlockTimeout)
	defer cancel()
	// A bigint advisory key is stored as classid (high 32 bits) and objid
	// (low 32 bits) with objsubid 1.
	var held bool
	err := conn.QueryRowContext(checkCtx, `SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()
		  AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 1)`,
		int64(uint32(uint64(key)>>32)), int64(uint32(uint64(key)))).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("storage: session lock check: %w", err)
	}
	return held, nil
}
