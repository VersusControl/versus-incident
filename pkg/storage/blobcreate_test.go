package storage_test

// blobcreate_test.go — covers the optional storage.BlobCreator capability
// (X9-T11): the atomic create-if-absent blob primitive that elects ONE
// writer across instances sharing a store (the substrate for generate-once
// secrets under HA / multi-instance).
//
// The race semantics are exercised against the memory backend (the test
// substrate) and the file backend (in-process O_EXCL is atomic on one node).
// A Postgres-backed run is gated on TEST_POSTGRES_DSN exactly like the rest
// of the parity suite, so the standard CI loop (no live Postgres) stays
// green while a real database, when present, proves the
// INSERT … ON CONFLICT DO NOTHING + read-after-write path.

import (
	"fmt"
	"sync"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// asCreator type-asserts the optional capability, failing the test when the
// backend does not implement it (memory/file/postgres must).
func asCreator(t *testing.T, p storage.Provider) storage.BlobCreator {
	t.Helper()
	bc, ok := p.(storage.BlobCreator)
	if !ok {
		t.Fatalf("backend %T must implement storage.BlobCreator", p)
	}
	return bc
}

// runCreateIfAbsent exercises the single-threaded contract: first create
// wins, a second create is a no-op that leaves the first writer's bytes, and
// create over a key already set by WriteBlob is a no-op read.
func runCreateIfAbsent(t *testing.T, p storage.Provider) {
	t.Helper()
	bc := asCreator(t, p)

	const key = "enterprise/bootstrap"
	first := []byte(`{"session_key":"AAA","master_key":"BBB"}`)

	// First create on an absent key writes and reports written==true.
	written, err := bc.CreateBlobIfAbsent(key, first)
	if err != nil {
		t.Fatalf("CreateBlobIfAbsent(first): %v", err)
	}
	if !written {
		t.Fatal("first create should report written==true")
	}
	got, err := p.ReadBlob(key)
	if err != nil {
		t.Fatalf("ReadBlob after first create: %v", err)
	}
	if string(got) != string(first) {
		t.Fatalf("ReadBlob = %q, want %q", got, first)
	}

	// A second create with DIFFERENT bytes must not overwrite — it reports
	// written==false and the stored bytes remain the first writer's.
	second := []byte(`{"session_key":"ZZZ","master_key":"YYY"}`)
	written, err = bc.CreateBlobIfAbsent(key, second)
	if err != nil {
		t.Fatalf("CreateBlobIfAbsent(second): %v", err)
	}
	if written {
		t.Fatal("second create should report written==false (loser of the race)")
	}
	got, err = p.ReadBlob(key)
	if err != nil {
		t.Fatalf("ReadBlob after second create: %v", err)
	}
	if string(got) != string(first) {
		t.Fatalf("second create clobbered the survivor: got %q, want %q", got, first)
	}

	// Create over a key already set by WriteBlob is a no-op read returning
	// the stored bytes.
	const wkey = "patterns"
	stored := []byte(`{"version":1}`)
	if err := p.WriteBlob(wkey, stored); err != nil {
		t.Fatalf("WriteBlob(%s): %v", wkey, err)
	}
	written, err = bc.CreateBlobIfAbsent(wkey, []byte(`{"version":99}`))
	if err != nil {
		t.Fatalf("CreateBlobIfAbsent over existing WriteBlob key: %v", err)
	}
	if written {
		t.Fatal("create over an existing WriteBlob key should report written==false")
	}
	got, err = p.ReadBlob(wkey)
	if err != nil {
		t.Fatalf("ReadBlob after create over existing: %v", err)
	}
	if string(got) != string(stored) {
		t.Fatalf("create clobbered a WriteBlob value: got %q, want %q", got, stored)
	}
}

// runCreateRace fires N concurrent creators at one key, each with distinct
// bytes, and asserts exactly one observes written==true and every caller can
// read the single surviving value (the winner's bytes). Run with -race this
// also flags any unsynchronized access in a backend.
func runCreateRace(t *testing.T, p storage.Provider) {
	t.Helper()
	bc := asCreator(t, p)

	const (
		key = "enterprise/bootstrap"
		n   = 16
	)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		winners  int
		winData  []byte
		firstErr error
	)
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := []byte(fmt.Sprintf("writer-%02d", i))
			<-start // line every goroutine up so they race
			written, err := bc.CreateBlobIfAbsent(key, payload)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if written {
				winners++
				winData = payload
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("a concurrent CreateBlobIfAbsent failed: %v", firstErr)
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winner, got %d", winners)
	}

	// Every caller — winner and losers alike — reads the one surviving value.
	got, err := p.ReadBlob(key)
	if err != nil {
		t.Fatalf("ReadBlob after race: %v", err)
	}
	if string(got) != string(winData) {
		t.Fatalf("survivor mismatch: ReadBlob = %q, want winner's bytes %q", got, winData)
	}
}

// ---------------------------------------------------------------------------
// Memory backend
// ---------------------------------------------------------------------------

func TestMemoryCreateIfAbsent(t *testing.T) {
	runCreateIfAbsent(t, storage.NewMemory())
}

func TestMemoryCreateRace(t *testing.T) {
	runCreateRace(t, storage.NewMemory())
}

// ---------------------------------------------------------------------------
// File backend (single-node O_EXCL)
// ---------------------------------------------------------------------------

func TestFileCreateIfAbsent(t *testing.T) {
	p, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()
	runCreateIfAbsent(t, p)
}

func TestFileCreateRace(t *testing.T) {
	p, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer p.Close()
	runCreateRace(t, p)
}

// ---------------------------------------------------------------------------
// Postgres backend (gated on TEST_POSTGRES_DSN, like the parity suite)
// ---------------------------------------------------------------------------

func TestPostgresCreateIfAbsent(t *testing.T) {
	runCreateIfAbsent(t, newTestPostgres(t))
}

func TestPostgresCreateRace(t *testing.T) {
	runCreateRace(t, newTestPostgres(t))
}
