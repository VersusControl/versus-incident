package cost

import (
	"errors"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// notNullBlobs rejects a nil payload the way a NOT NULL blob column does,
// and offers no exact-key delete.
type notNullBlobs struct{ storage.Provider }

func (p notNullBlobs) WriteBlob(name string, data []byte) error {
	if data == nil {
		return errors.New("null value in column data violates not-null constraint")
	}
	return p.Provider.WriteBlob(name, data)
}

// notNullLifecycleBlobs adds the lifecycle delete a database backend
// offers, without BlobDeleter.
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

// TestRemoveDeletesOrLeavesAnEmptyTombstone: removing a budget deletes the
// key where the backend can, else leaves an empty (never nil) blob that
// readers treat as absent, on every backend shape.
func TestRemoveDeletesOrLeavesAnEmptyTombstone(t *testing.T) {
	backends := []struct {
		name     string
		provider func(t *testing.T) storage.Provider
		removes  bool
	}{
		{"memory", func(*testing.T) storage.Provider { return storage.NewMemory() }, true},
		{"file", func(t *testing.T) storage.Provider {
			p, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			return p
		}, false},
		{"not-null-without-delete", func(*testing.T) storage.Provider { return notNullBlobs{storage.NewMemory()} }, false},
		{"not-null-with-lifecycle-delete", func(*testing.T) storage.Provider {
			m := storage.NewMemory()
			return notNullLifecycleBlobs{notNullBlobs{m}, m.(storage.Lifecycle)}
		}, true},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			p := b.provider(t)
			store := NewStore(p, tenancy.DefaultOrgScope())
			one := finops.Budget{ID: "b1", Name: "Total", Scope: finops.ScopeDeployment, Monthly: finops.USD(10), Source: finops.BudgetFromUI}
			two := finops.Budget{ID: "b2", Name: "Team", Scope: finops.ScopeTeam, Value: "checkout", Monthly: finops.USD(5), Source: finops.BudgetFromUI}
			if err := store.PutBudgets(ctx, []finops.Budget{one, two}); err != nil {
				t.Fatal(err)
			}
			if err := store.PutBudgets(ctx, []finops.Budget{two}); err != nil {
				t.Fatalf("remove: %v", err)
			}
			if bs, err := store.Budgets(ctx); err != nil || len(bs) != 1 || bs[0].ID != "b2" {
				t.Fatalf("budgets %+v %v", bs, err)
			}
			raw, err := p.ReadBlob("aws/default/finops/budgets/b1")
			if err != nil {
				t.Fatal(err)
			}
			if b.removes && raw != nil {
				t.Fatalf("a backend that deletes keeps a row: %q", raw)
			}
			if len(raw) != 0 {
				t.Fatalf("removed budget still has data: %q", raw)
			}
		})
	}
}
