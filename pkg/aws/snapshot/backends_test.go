package snapshot

import (
	"os"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/storage"
	_ "github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver"
)

// inventoryProvider is a provider with the inventory tables.
type inventoryProvider interface {
	storage.Provider
	storage.AWSInventoryStore
}

// openBackend opens the provider the running test uses; eachBackend sets it.
var openBackend func(t *testing.T) inventoryProvider

// eachBackend runs fn on memory, file (SQLite) and, when TEST_POSTGRES_DSN
// names an isolated database, Postgres. The Postgres inventory tables are
// emptied before each run, so every run starts from no data.
func eachBackend(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	backends := []struct {
		name string
		open func(t *testing.T) inventoryProvider
	}{
		{"memory", func(*testing.T) inventoryProvider { return storage.NewMemory().(inventoryProvider) }},
		{"file", func(t *testing.T) inventoryProvider {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			return provider.(inventoryProvider)
		}},
		{"postgres", func(t *testing.T) inventoryProvider {
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("TEST_POSTGRES_DSN not set")
			}
			provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			if _, err := provider.(storage.SQLAccessor).DB().Exec(`TRUNCATE aws_account_epochs, aws_sync_units, aws_unit_generations, aws_resources,
 aws_health, aws_security_flags, aws_changes, aws_change_coverage`); err != nil {
				t.Fatal(err)
			}
			return provider.(inventoryProvider)
		}},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			previous := openBackend
			openBackend = backend.open
			defer func() { openBackend = previous }()
			fn(t)
		})
	}
}
