package settings

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

type backend struct {
	name     string
	provider func(t *testing.T) storage.Provider
	// removes is true when the backend can drop a key outright, so a delete
	// must leave no row behind rather than an empty tombstone.
	removes bool
}

// tablesProvider is a provider with the AWS inventory and FinOps tables.
type tablesProvider interface {
	storage.Provider
	storage.AWSInventoryStore
	storage.FinOpsStore
}

// notNullBlobs stands in for a backend whose blob column is NOT NULL and
// which offers no exact-key delete: a nil payload is rejected the way the
// Postgres table rejects it. Its inventory tables are the wrapped ones.
type notNullBlobs struct{ tablesProvider }

func (p notNullBlobs) WriteBlob(name string, data []byte) error {
	if data == nil {
		return errors.New("null value in column \"data\" violates not-null constraint")
	}
	return p.tablesProvider.WriteBlob(name, data)
}

// notNullLifecycleBlobs adds the Lifecycle delete the Postgres backend
// offers, without the BlobDeleter capability, matching its shape.
type notNullLifecycleBlobs struct {
	notNullBlobs
	lifecycle storage.Lifecycle
}

func (p notNullLifecycleBlobs) PurgeOlderThan(domain string, cutoff time.Time) (int, error) {
	return p.lifecycle.PurgeOlderThan(domain, cutoff)
}

func (p notNullLifecycleBlobs) DeleteByID(domain, id string) error {
	return p.lifecycle.DeleteByID(domain, id)
}

func deleteBackends() []backend {
	return []backend{
		{"memory", func(*testing.T) storage.Provider { return storage.NewMemory() }, true},
		{"file", func(t *testing.T) storage.Provider {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			return provider
		}, false},
		{"not-null-without-delete", func(*testing.T) storage.Provider {
			return notNullBlobs{storage.NewMemory().(tablesProvider)}
		}, false},
		{"not-null-with-lifecycle-delete", func(*testing.T) storage.Provider {
			memory := storage.NewMemory()
			return notNullLifecycleBlobs{notNullBlobs{memory.(tablesProvider)}, memory.(storage.Lifecycle)}
		}, true},
		{"postgres", func(t *testing.T) storage.Provider {
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("TEST_POSTGRES_DSN not set")
			}
			provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			return provider
		}, true},
	}
}

// inventorylessProvider hides a provider's typed stores, standing in for a
// deployment whose inventory tables are missing.
type inventorylessProvider struct{ storage.Provider }

// TestDeleteAccountKeepsRecordWhenSnapshotStoreUnavailable proves a purge
// that cannot reach its store leaves the settings record in place, so the
// operator can retry, and that the retry removes everything once the store
// is back.
func TestDeleteAccountKeepsRecordWhenSnapshotStoreUnavailable(t *testing.T) {
	const accountID = "111122223333"
	org := "delete-retry"
	scope := tenancy.OrgScope{Write: org, Read: []string{org}}
	provider := storage.NewMemory()
	store := NewStore(provider, scope)
	if err := store.SaveAccount(org, AccountRecord{AccountID: accountID, DisplayName: "Prod", Regions: []string{"us-east-1"}, Source: SourceWizard, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	recoveredSnapshots := snapshot.New(provider, scope)
	unit := model.Unit{Account: accountID, Region: "us-east-1", Service: "ec2"}
	if _, err := recoveredSnapshots.Commit(unit, instanceCandidate()); err != nil {
		t.Fatal(err)
	}

	unavailable := NewService(store, WithConfig(collectingConfig(true)),
		WithSnapshots(snapshot.New(inventorylessProvider{provider}, scope)))
	_, err := unavailable.DeleteAccount(context.Background(), org, accountID)
	if !errors.Is(err, ErrNoStorage) {
		t.Fatalf("delete error = %v, want the classed storage error", err)
	}
	if accounts, err := store.ListAccounts(org); err != nil || len(accounts) != 1 {
		t.Fatalf("settings record did not survive the failed purge: %+v %v", accounts, err)
	}

	recovered := NewService(store, WithConfig(collectingConfig(true)), WithSnapshots(recoveredSnapshots))
	if _, err := recovered.DeleteAccount(context.Background(), org, accountID); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if accounts, err := store.ListAccounts(org); err != nil || len(accounts) != 0 {
		t.Fatalf("account still listed after the retry: %+v %v", accounts, err)
	}
	result, err := snapshot.New(provider, scope).Resources(context.Background(), snapshot.Query{})
	if err != nil || len(result.Resources) != 0 || len(result.Units) != 0 {
		t.Fatalf("inventory rows survived the retry: %+v %v", result, err)
	}
}

// TestDeleteAccountRemovesEverythingOnEveryBackend checks that deleting an
// account removes the account record, every inventory row and change, and
// the cost lines it paid for on every backend, and that the collectors stop
// listing it. Another payer's cost lines stay.
// A backend that can drop keys (memory, Postgres) leaves no row behind; one
// that cannot (the file store answers "unsupported" for these keys) leaves
// empty tombstones, which a NOT NULL blob column must still accept.
func TestDeleteAccountRemovesEverythingOnEveryBackend(t *testing.T) {
	const accountID = "111122223333"
	for _, item := range deleteBackends() {
		t.Run(item.name, func(t *testing.T) {
			provider := item.provider(t)
			org := "delete-" + strconv.FormatInt(time.Now().UnixNano(), 36)
			scope := tenancy.OrgScope{Write: org, Read: []string{org}}
			store := NewStore(provider, scope)
			snapshots := snapshot.New(provider, scope)
			costs, err := cost.New(provider, scope, cost.Options{})
			if err != nil {
				t.Fatal(err)
			}
			fake := fakeaws.New(t, "new-account-connect")
			service := NewService(store, WithConfig(collectingConfig(true)), WithHTTPClient(fake.HTTPClient()),
				WithBaseProvider(staticBase), WithLimiter(limit.NewManager(100)), WithSnapshots(snapshots), WithPayerRemover(costs))
			finops, ok := storage.FinOps(provider)
			if !ok {
				t.Fatal("provider has no FinOps tables")
			}
			const otherPayer = "999988887777"
			for _, payer := range []string{accountID, otherPayer} {
				line := storage.FinOpsCostLine{Day: "2026-01-01", Account: payer, Service: "compute", ServiceName: "Amazon EC2", RecordType: "Usage", NetAmortizedMicros: 10}
				if err := finops.ReplaceFinOpsCostDays(context.Background(), scope, payer, []string{"2026-01-01"}, []storage.FinOpsCostLine{line}); err != nil {
					t.Fatal(err)
				}
			}

			if err := store.SaveAccount(org, AccountRecord{AccountID: accountID, DisplayName: "Prod", Regions: []string{"us-east-1"}, Source: SourceWizard, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			unit := model.Unit{Account: accountID, Region: "us-east-1", Service: "ec2"}
			if _, err := snapshots.Commit(unit, instanceCandidate()); err != nil {
				t.Fatal(err)
			}
			if err := snapshots.AppendChanges(accountID, "us-east-1", []model.Change{{ID: "c1", Time: time.Now(), Account: accountID}}, nil, ""); err != nil {
				t.Fatal(err)
			}
			if len(service.CollectTargets(context.Background())) != 1 {
				t.Fatal("account not collected before delete")
			}

			if removal, err := service.DeleteAccount(context.Background(), org, accountID); err != nil || removal.PurgePending {
				t.Fatalf("delete = %+v %v; nothing holds the cost lock, so the purge runs now", removal, err)
			}
			if accounts, _ := store.ListAccounts(org); len(accounts) != 0 {
				t.Fatalf("account still listed: %+v", accounts)
			}
			if targets := service.CollectTargets(context.Background()); len(targets) != 0 {
				t.Fatalf("deleted account still collected: %+v", targets)
			}
			blobs, err := provider.ListBlobs("aws/" + org + "/")
			if err != nil {
				t.Fatal(err)
			}
			for _, blob := range blobs {
				if !strings.Contains(blob.Name, accountID) {
					continue
				}
				if len(blob.Data) > 0 {
					t.Fatalf("%s still holds data after delete", blob.Name)
				}
				if item.removes {
					t.Fatalf("%s left behind as a tombstone on a backend that can delete it", blob.Name)
				}
			}
			if lines, err := finops.FinOpsCostLines(context.Background(), scope, storage.FinOpsCostQuery{Payer: accountID}); err != nil || len(lines) != 0 {
				t.Fatalf("the deleted payer still has cost lines: %+v %v", lines, err)
			}
			if lines, _ := finops.FinOpsCostLines(context.Background(), scope, storage.FinOpsCostQuery{Account: accountID}); len(lines) != 0 {
				t.Fatalf("the deleted account still has cost lines: %+v", lines)
			}
			if lines, _ := finops.FinOpsCostLines(context.Background(), scope, storage.FinOpsCostQuery{Payer: otherPayer}); len(lines) != 1 {
				t.Fatalf("another payer's cost lines went too: %+v", lines)
			}
			reloaded := snapshot.New(provider, scope)
			if accounts, _ := reloaded.Accounts(); len(accounts) != 0 {
				t.Fatalf("a fresh store still sees %v", accounts)
			}
			result, _ := reloaded.Resources(context.Background(), snapshot.Query{})
			if len(result.Resources) != 0 || len(result.Units) != 0 {
				t.Fatalf("a fresh store still reads %+v", result)
			}
		})
	}
}

// costReplicaLockKey is the cost adapter's replica lock key ("VSCOSTR1").
// If it ever changes, the test below stops finding the lock held and fails.
const costReplicaLockKey int64 = 0x5653434F53545231

// TestDeleteAccountWhileAnotherReplicaHoldsTheCostLock: on a shared
// Postgres, another replica's cost work holds the cost lock. The delete
// answers within the five-second bound with the purge pending: the record
// is gone and the cost lines are left for the cost refresh. Once the lock is
// free, the purge deletes them.
func TestDeleteAccountWhileAnotherReplicaHoldsTheCostLock(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	open := func() storage.Provider {
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	provider, elsewhere := open(), open()
	const accountID = "111122223333"
	ctx := context.Background()
	org := "delete-held-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	scope := tenancy.OrgScope{Write: org, Read: []string{org}}
	store := NewStore(provider, scope)
	costs, err := cost.New(provider, scope, cost.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, WithConfig(collectingConfig(true)), WithPayerRemover(costs))
	finops, ok := storage.FinOps(provider)
	if !ok {
		t.Fatal("provider has no FinOps tables")
	}
	line := storage.FinOpsCostLine{Day: "2026-01-01", Account: accountID, Service: "compute", ServiceName: "Amazon EC2", RecordType: "Usage", NetAmortizedMicros: 10}
	if err := finops.ReplaceFinOpsCostDays(ctx, scope, accountID, []string{"2026-01-01"}, []storage.FinOpsCostLine{line}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAccount(org, AccountRecord{AccountID: accountID, DisplayName: "Prod", Regions: []string{"us-east-1"}, Source: SourceWizard, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	unlock, acquired, err := elsewhere.(storage.SessionLocker).TryLockSession(ctx, costReplicaLockKey)
	if err != nil || !acquired {
		t.Fatalf("hold the cost lock: acquired=%v err=%v", acquired, err)
	}
	defer unlock()

	start := time.Now()
	removal, err := service.DeleteAccount(ctx, org, accountID)
	elapsed := time.Since(start)
	if err != nil || !removal.PurgePending {
		t.Fatalf("delete = %+v %v, want success with the purge pending", removal, err)
	}
	if elapsed > 6*time.Second {
		t.Fatalf("the delete took %s; the bound is five seconds", elapsed)
	}
	if accounts, _ := store.ListAccounts(org); len(accounts) != 0 {
		t.Fatalf("account still listed: %+v", accounts)
	}
	if lines, _ := finops.FinOpsCostLines(ctx, scope, storage.FinOpsCostQuery{Payer: accountID}); len(lines) != 1 {
		t.Fatalf("the purge ran while another replica held the lock: %d lines", len(lines))
	}

	unlock()
	purged, err := costs.PurgePayer(ctx, accountID, time.Second)
	if err != nil || !purged {
		t.Fatalf("purge once the lock is free = %v %v", purged, err)
	}
	if lines, _ := finops.FinOpsCostLines(ctx, scope, storage.FinOpsCostQuery{Payer: accountID}); len(lines) != 0 {
		t.Fatalf("the deleted payer still has %d cost lines", len(lines))
	}
}
