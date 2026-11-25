package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// A save fenced by the lease holder is refused when it names another holder
// or when the lease has expired, and accepted for the current holder, on
// every store. The stale holder's save carries the lease's current bytes, so
// only the holder check can refuse it.
func TestEvidenceSaveFencedByLeaseHolderRefusesStaleAndExpiredHolders(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider
			switch backend {
			case "memory":
				provider = NewMemory()
			case "file":
				file, err := NewFile(FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = file.Close() })
				provider = file
			default:
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated Postgres required")
				}
				postgres, err := NewPostgres(PostgresOptions{DSN: dsn})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = postgres.Close() })
				provider = postgres
			}
			store := provider.(EvidenceStore)
			unique := fmt.Sprintf("lease-holder-%s-%d", backend, time.Now().UnixNano())
			scope := EvidenceScope{OrgID: "default", ClusterID: unique, ClusterUID: "physical"}
			guardKey := "kubernetes/inventory/" + unique
			lease := func(owner string, expires time.Time) []byte {
				raw, _ := json.Marshal(map[string]any{"Owner": owner, "Expires": expires})
				if err := provider.WriteBlob(guardKey, raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			commit := func(id, owner string, guard []byte) error {
				batch := evidenceTestBatch(id, "", at, true, true)
				batch.GuardKey, batch.GuardExpected, batch.GuardOwner = guardKey, guard, owner
				return store.CommitEvidence(t.Context(), scope, batch)
			}

			current := lease("holder", time.Now().Add(time.Hour))
			if err := commit("stale-holder", "stale", current); !errors.Is(err, ErrEvidenceConflict) {
				t.Errorf("a save fenced by another holder was not refused: %v", err)
			}
			expired := lease("holder", time.Now().Add(-time.Minute))
			if err := commit("expired-lease", "holder", expired); !errors.Is(err, ErrEvidenceConflict) {
				t.Errorf("a save fenced by an expired lease was not refused: %v", err)
			}
			current = lease("holder", time.Now().Add(time.Hour))
			if err := commit("current-holder", "holder", current); err != nil {
				t.Errorf("the current holder's save was refused: %v", err)
			}
		})
	}
}

// The holder check reads the database clock when it runs, not when the save
// transaction began: a save may wait on a partition lock before it reaches
// the check, and a lease that ran out during that wait no longer fences it.
func TestEvidenceSaveGuardRefusesALeaseThatRanOutAfterTheSaveBegan(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	postgres := provider.(*postgresProvider)
	guardKey := fmt.Sprintf("kubernetes/inventory/lease-clock-%d", time.Now().UnixNano())
	tx, err := postgres.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var began time.Time
	if err := tx.QueryRowContext(t.Context(), `SELECT now()`).Scan(&began); err != nil {
		t.Fatal(err)
	}
	// The lease runs out at a database time read after the transaction
	// began, so it was still held at the transaction's start and has run out
	// by the time the check runs.
	var expires time.Time
	if err := postgres.db.QueryRowContext(t.Context(), `SELECT clock_timestamp()`).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if !expires.After(began) {
		t.Fatalf("database clock %s did not move past the transaction start %s", expires, began)
	}
	raw, _ := json.Marshal(map[string]any{"Owner": "holder", "Expires": expires})
	if err := provider.WriteBlob(guardKey, raw); err != nil {
		t.Fatal(err)
	}
	err = evidenceSaveGuard(t.Context(), tx, EvidenceBatch{GuardKey: guardKey, GuardOwner: "holder"})
	if !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("a save was fenced by a lease that ran out after it began: %v", err)
	}
}
