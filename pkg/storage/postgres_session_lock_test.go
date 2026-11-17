package storage_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestPostgresSessionLockIsHeldByOneReplica opens two providers on one
// database, as two replicas would: while one holds a key the other is
// refused without waiting, a different key is independent, and once the
// holder unlocks the other acquires it. A pool limited to one connection can
// still serve its own queries while it holds the lock.
func TestPostgresSessionLockIsHeldByOneReplica(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres session lock test")
	}
	open := func(maxOpen int) storage.SessionLocker {
		t.Helper()
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn, MaxOpenConns: maxOpen})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		locker, ok := provider.(storage.SessionLocker)
		if !ok {
			t.Fatalf("%T does not implement storage.SessionLocker", provider)
		}
		return locker
	}
	first, second := open(1), open(0)
	ctx := context.Background()
	key := time.Now().UnixNano()

	unlock, acquired, err := first.TryLockSession(ctx, key)
	if err != nil || !acquired {
		t.Fatalf("first lock acquired=%v err=%v", acquired, err)
	}
	blob := "session-lock/" + time.Now().Format(time.RFC3339Nano)
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	written := make(chan error, 1)
	go func() { written <- first.(storage.Provider).WriteBlob(blob, []byte("held")) }()
	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("the holder's own write failed: %v", err)
		}
	case <-writeCtx.Done():
		t.Fatal("the holder's own write waited for the connection the lock pinned")
	}

	if other, acquired, err := second.TryLockSession(ctx, key); err != nil || acquired {
		if other != nil {
			other()
		}
		t.Fatalf("a second replica took a held key: acquired=%v err=%v", acquired, err)
	}
	otherUnlock, acquired, err := second.TryLockSession(ctx, key+1)
	if err != nil || !acquired {
		t.Fatalf("a different key acquired=%v err=%v", acquired, err)
	}
	otherUnlock()

	unlock()
	unlock()
	again, acquired, err := second.TryLockSession(ctx, key)
	if err != nil || !acquired {
		t.Fatalf("the released key acquired=%v err=%v", acquired, err)
	}
	again()
}

// TestPostgresSessionLockCancelledDuringAcquireLeavesTheKeyFree: the caller's
// context is cancelled after the server ran pg_try_advisory_lock but before
// the answer was read. The server may hold the lock for that session, so the
// session must be closed, not pooled: another replica can take the key
// shortly after, and the pool is not left with a connection that keeps it.
func TestPostgresSessionLockCancelledDuringAcquireLeavesTheKeyFree(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres session lock test")
	}
	open := func() storage.SessionLocker {
		t.Helper()
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider.(storage.SessionLocker)
	}
	first, second := open(), open()
	key := time.Now().UnixNano()

	acquireCtx, cancel := context.WithCancel(context.Background())
	restore := storage.SetSessionLockQueried(func() {
		cancel()
		// database/sql closes the open answer once it sees the
		// cancellation; give it the moment it needs.
		time.Sleep(200 * time.Millisecond)
	})
	unlock, acquired, err := first.TryLockSession(acquireCtx, key)
	restore()
	if err == nil {
		if unlock != nil {
			unlock()
		}
		t.Skipf("the answer was read before the cancellation landed (acquired=%v); nothing to check", acquired)
	}
	if acquired || unlock != nil {
		t.Fatalf("a failed acquire reported acquired=%v unlock=%v", acquired, unlock != nil)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		other, acquired, err := second.TryLockSession(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if acquired {
			other()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the key stayed held after the acquire was cancelled: the session that took it went back to the pool")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The first replica's pool must not keep a session that holds the key
	// either: it can take the key itself once the other let it go.
	again, acquired, err := first.TryLockSession(context.Background(), key)
	if err != nil || !acquired {
		t.Fatalf("the first replica could not take the key again: acquired=%v err=%v", acquired, err)
	}
	again()
}

// TestPostgresSessionLockHeldSeesALostSession: the server ends the session
// that holds a key (as an idle-session timeout or a dropped connection
// would). The holder's check reports the key no longer held, so its work
// stops, and another replica can take the key.
func TestPostgresSessionLockHeldSeesALostSession(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres session lock test")
	}
	open := func() storage.Provider {
		t.Helper()
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	first, second := open(), open()
	ctx := context.Background()
	key := time.Now().UnixNano()

	unlock, acquired, err := first.(storage.SessionLocker).TryLockSession(ctx, key)
	if err != nil || !acquired {
		t.Fatalf("lock acquired=%v err=%v", acquired, err)
	}
	defer unlock()
	verifier := first.(storage.SessionLockVerifier)
	if held, err := verifier.SessionLockHeld(ctx, key); !held || err != nil {
		t.Fatalf("the holder's check on a held key: held=%v err=%v", held, err)
	}

	db := second.(storage.SQLAccessor).DB()
	var terminated int
	if err := db.QueryRowContext(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 1`,
		int64(uint32(uint64(key)>>32)), int64(uint32(uint64(key)))).Scan(&terminated); err != nil || terminated != 1 {
		t.Fatalf("end the holding session: %d ended, err %v", terminated, err)
	}
	if held, _ := verifier.SessionLockHeld(ctx, key); held {
		t.Fatal("the holder still reads the key held after its session ended")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		other, acquired, err := second.(storage.SessionLocker).TryLockSession(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if acquired {
			other()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the key stayed held after the holding session ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
