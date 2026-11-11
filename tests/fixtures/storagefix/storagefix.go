// Package storagefix opens the storage backend a behaviour test runs on, so
// one suite can run unchanged on memory, file (SQLite) and Postgres storage.
//
// TEST_STORAGE_BACKEND selects it: "memory" (the default), "file", or
// "postgres". Postgres needs TEST_POSTGRES_DSN (an isolated database, as
// `harness.sh pgtest` provides); every provider gets its own schema there,
// so tests sharing the database never see each other's rows.
package storagefix

import (
	"database/sql"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	// File mode keeps its tables in SQLite; a host registers the driver.
	_ "github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver"
)

// EnvBackend names the storage backend.
const EnvBackend = "TEST_STORAGE_BACKEND"

// Backend is the selected backend name.
func Backend() string {
	switch b := os.Getenv(EnvBackend); b {
	case "file", "postgres":
		return b
	default:
		return "memory"
	}
}

var schemas atomic.Int64

// Provider opens a fresh provider on the selected backend, closed when the
// test ends.
func Provider(t testing.TB) storage.Provider {
	t.Helper()
	switch Backend() {
	case "file":
		p, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Fatalf("%s=postgres needs TEST_POSTGRES_DSN (an isolated database)", EnvBackend)
		}
		schema := "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.FormatInt(schemas.Add(1), 10)
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close()
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatal(err)
		}
		p, err := storage.NewPostgres(storage.PostgresOptions{DSN: withSearchPath(t, dsn, schema)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = p.Close()
			if db, err := sql.Open("pgx", dsn); err == nil {
				_, _ = db.Exec("DROP SCHEMA " + schema + " CASCADE")
				_ = db.Close()
			}
		})
		return p
	default:
		p := storage.NewMemory()
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
}

// withSearchPath points every connection of the DSN at the schema.
func withSearchPath(t testing.TB, dsn, schema string) string {
	t.Helper()
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

// PostgresReplicas opens n providers on one fresh Postgres schema, as n
// server processes sharing a database. It skips the test unless the
// Postgres backend is selected.
func PostgresReplicas(t testing.TB, n int) []storage.Provider {
	t.Helper()
	if Backend() != "postgres" {
		t.Skip("replicas share a Postgres database; " + EnvBackend + "=postgres not set")
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Fatalf("%s=postgres needs TEST_POSTGRES_DSN (an isolated database)", EnvBackend)
	}
	schema := "r" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.FormatInt(schemas.Add(1), 10)
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	providers := make([]storage.Provider, 0, n)
	t.Cleanup(func() {
		for _, p := range providers {
			_ = p.Close()
		}
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	for i := 0; i < n; i++ {
		p, err := storage.NewPostgres(storage.PostgresOptions{DSN: withSearchPath(t, dsn, schema)})
		if err != nil {
			t.Fatal(err)
		}
		providers = append(providers, p)
	}
	return providers
}
