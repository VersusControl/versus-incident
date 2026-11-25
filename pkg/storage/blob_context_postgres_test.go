package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// postgresHangGuard is how long a test waits for an event on the database
// before it calls the wait a hang. A passing test never comes near it.
const postgresHangGuard = 60 * time.Second

func openContextBlobPostgres(t *testing.T) (storage.Provider, string) {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres context blob tests")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider, dsn
}

// While their context is live, the context blob operations on Postgres read,
// write and swap exactly as the plain ones do, directly and through the
// package helpers that pick them.
func TestPostgresContextBlobOperationsMatchThePlainOnes(t *testing.T) {
	provider, _ := openContextBlobPostgres(t)
	store, ok := provider.(storage.ContextBlobStore)
	if !ok {
		t.Fatalf("postgres provider %T does not implement storage.ContextBlobStore", provider)
	}
	cas, ok := provider.(storage.ContextBlobCAS)
	if !ok {
		t.Fatalf("postgres provider %T does not implement storage.ContextBlobCAS", provider)
	}
	name := fmt.Sprintf("context-blob-%d/round-trip", time.Now().UnixNano())
	t.Cleanup(func() { deleteContextBlobs(provider, name) })
	ctx := t.Context()

	if data, err := store.ReadBlobContext(ctx, name); err != nil || data != nil {
		t.Fatalf("missing blob read = %q, %v; want nil, nil", data, err)
	}
	if err := store.WriteBlobContext(ctx, name, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if data, err := provider.ReadBlob(name); err != nil || string(data) != "first" {
		t.Fatalf("plain read after context write = %q, %v", data, err)
	}
	if err := storage.WriteBlobContext(ctx, provider, name, []byte("second")); err != nil {
		t.Fatal(err)
	}
	if data, err := storage.ReadBlobContext(ctx, provider, name); err != nil || string(data) != "second" {
		t.Fatalf("helper read = %q, %v; want second", data, err)
	}

	for _, step := range []struct {
		what                  string
		expected, replacement []byte
		want                  bool
		after                 []byte
	}{
		{"create over an existing blob", nil, []byte("other"), false, []byte("second")},
		{"stale compare", []byte("first"), []byte("other"), false, []byte("second")},
		{"matching compare", []byte("second"), []byte("third"), true, []byte("third")},
		{"stale delete", []byte("second"), nil, false, []byte("third")},
		{"matching delete", []byte("third"), nil, true, nil},
		{"create when absent", nil, []byte("fourth"), true, []byte("fourth")},
	} {
		swapped, err := cas.CompareAndSwapBlobContext(ctx, name, step.expected, step.replacement)
		if err != nil || swapped != step.want {
			t.Fatalf("%s: swapped=%v err=%v; want %v", step.what, swapped, err, step.want)
		}
		if data, err := provider.ReadBlob(name); err != nil || string(data) != string(step.after) || (data == nil) != (step.after == nil) {
			t.Fatalf("%s: blob is %q (%v); want %q", step.what, data, err, step.after)
		}
	}
	if swapped, err := storage.CompareAndSwapBlobContext(ctx, provider.(storage.BlobCAS), name, []byte("fourth"), []byte("fifth")); err != nil || !swapped {
		t.Fatalf("helper swap: swapped=%v err=%v", swapped, err)
	}
}

// A context blob operation stops when its context ends: at once when the
// context has already ended, and while Postgres holds the statement behind a
// lock, without waiting for the lock. An operation that stopped changes
// nothing.
func TestPostgresContextBlobOperationsStopWithTheirContext(t *testing.T) {
	provider, dsn := openContextBlobPostgres(t)
	store := provider.(storage.ContextBlobStore)
	cas := provider.(storage.ContextBlobCAS)
	name := fmt.Sprintf("context-blob-%d/cancel", time.Now().UnixNano())
	t.Cleanup(func() { deleteContextBlobs(provider, name) })
	if err := provider.WriteBlob(name, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		what string
		run  func(context.Context) error
	}{
		{"read", func(ctx context.Context) error { _, err := store.ReadBlobContext(ctx, name); return err }},
		{"write", func(ctx context.Context) error { return store.WriteBlobContext(ctx, name, []byte("written")) }},
		{"swap", func(ctx context.Context) error {
			_, err := cas.CompareAndSwapBlobContext(ctx, name, []byte("kept"), []byte("swapped"))
			return err
		}},
		{"create", func(ctx context.Context) error {
			_, err := cas.CompareAndSwapBlobContext(ctx, name+"/absent", nil, []byte("created"))
			return err
		}},
		{"delete", func(ctx context.Context) error {
			_, err := cas.CompareAndSwapBlobContext(ctx, name, []byte("kept"), nil)
			return err
		}},
	}
	unchanged := func(t *testing.T, what string) {
		t.Helper()
		if data, err := provider.ReadBlob(name); err != nil || string(data) != "kept" {
			t.Fatalf("%s that stopped changed the blob to %q (%v)", what, data, err)
		}
		if data, err := provider.ReadBlob(name + "/absent"); err != nil || data != nil {
			t.Fatalf("%s that stopped created a blob %q (%v)", what, data, err)
		}
	}

	t.Run("context already ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for _, operation := range operations {
			if err := operation.run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s with an ended context returned %v; want context.Canceled", operation.what, err)
			}
			unchanged(t, operation.what)
		}
	})

	t.Run("statement held by a lock", func(t *testing.T) {
		locker, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer locker.Close()
		for _, operation := range operations {
			// An exclusive table lock makes Postgres hold every statement on
			// the blob table, reads included, as a database that stopped
			// answering does.
			lock, err := locker.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lock.ExecContext(t.Context(), `LOCK TABLE vs_blobs IN ACCESS EXCLUSIVE MODE`); err != nil {
				_ = lock.Rollback()
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() { result <- operation.run(ctx) }()
			waitForLockWaiter(t, locker, operation.what)
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					_ = lock.Rollback()
					t.Fatalf("%s held by a lock returned %v once its context ended; want context.Canceled", operation.what, err)
				}
			case <-time.After(postgresHangGuard):
				_ = lock.Rollback()
				t.Fatalf("%s held by a lock did not stop when its context ended", operation.what)
			}
			if err := lock.Rollback(); err != nil {
				t.Fatal(err)
			}
			unchanged(t, operation.what)
		}
	})
}

// waitForLockWaiter waits until a statement on this database is waiting for a
// lock on the blob table, the event that shows the operation reached Postgres
// and is held there.
func waitForLockWaiter(t *testing.T, db *sql.DB, what string) {
	t.Helper()
	deadline := time.Now().Add(postgresHangGuard)
	for pause := time.Millisecond; ; pause = min(2*pause, 10*time.Millisecond) {
		var waiting int
		err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%vs_blobs%'`).Scan(&waiting)
		if err != nil {
			t.Fatalf("read lock waiters: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("hang guard (%s) reached: the %s never waited on the blob table lock", postgresHangGuard, what)
		}
		time.Sleep(pause) // testlint:allow poll pause of the lock-waiter wait; pg_stat_activity has no notification
	}
}

// deleteContextBlobs removes what a context blob test wrote.
func deleteContextBlobs(provider storage.Provider, name string) {
	deleter, ok := provider.(storage.BlobDeleter)
	if !ok {
		return
	}
	_ = deleter.DeleteBlob(name)
	_ = deleter.DeleteBlob(name + "/absent")
}
