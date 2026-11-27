package storage_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/incidentcounts"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/jackc/pgx/v5/pgconn"
)

// shortCountLimit replaces the five-second count limit so a test watches a
// count reach it without waiting the full limit.
const shortCountLimit = 300 * time.Millisecond

// countsOnIncidents reports how many statements on this database other
// than the caller's own are waiting for, or running against, vs_incidents.
func countsOnIncidents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		  AND state = 'active' AND query ILIKE '%FROM vs_incidents%'`).Scan(&n); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return n
}

// Every incident count and incident page read on Postgres ends inside the
// database when it reaches its limit: held behind a lock on vs_incidents,
// each returns its timeout, and no statement of it is left running in
// pg_stat_activity, so a stalled table cannot hold pool connections.
// Abandoning the query only in Go would leave the server still waiting.
func TestPostgresIncidentCountsStopInTheDatabase(t *testing.T) {
	provider := newTestPostgres(t)
	if !storage.SetIncidentCountTimeout(provider, shortCountLimit) || !storage.SetIncidentListTimeout(provider, shortCountLimit) {
		t.Fatalf("provider %T is not the Postgres provider", provider)
	}
	db, err := sql.Open("pgx", os.Getenv("TEST_POSTGRES_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	scope := tenancy.DefaultOrgScope()
	since := time.Now().Add(-7 * 24 * time.Hour)

	operations := []struct {
		what string
		run  func() error
		want error
	}{
		{"list page", func() error {
			_, err := storage.ListIncidentsPageContext(context.Background(), provider.(storage.IncidentPager), "", 0, 100)
			return err
		}, storage.ErrListTimeout},
		{"list page by origin", func() error {
			_, err := provider.(storage.IncidentPager).ListIncidentsPage(storage.OriginWebhook, 0, 100)
			return err
		}, storage.ErrListTimeout},
		{"search page", func() error {
			_, err := storage.SearchIncidentsPageContext(context.Background(), provider.(storage.IncidentSearchPager), "disk", "", 0, 100)
			return err
		}, storage.ErrListTimeout},
		{"window count", func() error {
			_, err := provider.(storage.ContextIncidentWindowCounter).CountIncidentsByStatusSinceContext(context.Background(), since)
			return err
		}, storage.ErrCountTimeout},
		{"unbounded status count", func() error {
			_, err := provider.(storage.IncidentPager).CountIncidentsByStatus()
			return err
		}, storage.ErrCountTimeout},
		{"open count", func() error {
			_, err := provider.(storage.IncidentPager).CountIncidents()
			return err
		}, storage.ErrCountTimeout},
		{"search count", func() error {
			_, err := provider.(storage.IncidentSearchPager).CountIncidentsMatchingByStatus("disk")
			return err
		}, storage.ErrCountTimeout},
		{"search open count", func() error {
			_, err := provider.(storage.IncidentSearchPager).CountIncidentsMatching("disk")
			return err
		}, storage.ErrCountTimeout},
		{"service count", func() error {
			_, _, err := provider.(storage.ScopedIncidentServiceCounter).CountIncidentsByServiceSinceForScope(scope, "api", since)
			return err
		}, storage.ErrCountTimeout},
		{"service summary count", func() error {
			_, err := provider.(storage.ScopedIncidentServiceRangeSummaryReader).CountIncidentsByServicesInRangeForScope(scope, []string{"api"}, since, time.Time{})
			return err
		}, storage.ErrCountTimeout},
	}
	for _, op := range operations {
		lock, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lock.ExecContext(t.Context(), `LOCK TABLE vs_incidents IN ACCESS EXCLUSIVE MODE`); err != nil {
			_ = lock.Rollback()
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { result <- op.run() }()
		err = storage.WaitFor(t, result, op.what+" held behind a table lock returns")
		if !errors.Is(err, op.want) {
			_ = lock.Rollback()
			t.Fatalf("%s held behind a lock returned %v; want %v", op.what, err, op.want)
		}
		// The database itself ended the statement (query_canceled), rather
		// than the client dropping the connection and leaving the cancel to
		// arrive later.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			_ = lock.Rollback()
			t.Fatalf("%s held behind a lock returned %v; want the database to cancel it (57014)", op.what, err)
		}
		if running := countsOnIncidents(t, db); running != 0 {
			_ = lock.Rollback()
			t.Fatalf("%s returned but %d statements on vs_incidents still run in the database", op.what, running)
		}
		if err := lock.Rollback(); err != nil {
			t.Fatal(err)
		}
	}

	// The same provider counts normally once the table is free: the timed-out
	// counts left no broken connection behind.
	if _, err := provider.(storage.IncidentPager).CountIncidentsByStatus(); err != nil {
		t.Fatalf("count after the lock was released: %v", err)
	}
}

// A count whose caller's context ends stops at once and is not reported as a
// timeout.
func TestPostgresWindowCountFollowsItsContext(t *testing.T) {
	provider := newTestPostgres(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := provider.(storage.ContextIncidentWindowCounter).CountIncidentsByStatusSinceContext(ctx, time.Time{})
	if !errors.Is(err, context.Canceled) || errors.Is(err, storage.ErrCountTimeout) {
		t.Fatalf("count with an ended context returned %v; want context.Canceled and no timeout", err)
	}
}

// Memory, file and Postgres give the same counts through the count cache,
// for a bounded window and for all time.
func TestIncidentCountCacheParity(t *testing.T) {
	backends := map[string]func(*testing.T) storage.Provider{
		"memory":   newMemoryPager,
		"file":     newFilePager,
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			seedWindowRecords(t, p)
			cache := incidentcounts.New(incidentcounts.Options{})
			for _, window := range []struct {
				name  string
				since time.Time
				want  storage.StatusCounts
			}{
				{"7d", since, storage.StatusCounts{Open: 2, Acked: 1, Total: 3}},
				{"all", time.Time{}, storage.StatusCounts{Open: 3, Acked: 1, Resolved: 2, Total: 6}},
			} {
				r := cache.Get(t.Context(), incidentcounts.Key{Scope: storage.DefaultOrgID, Window: window.name},
					func(ctx context.Context) (storage.IncidentStatusCounts, error) {
						return storage.CountIncidentsByStatusSinceContext(ctx, p, window.since)
					})
				if r.Unknown || r.Counts.Total != window.want || r.Counts.Webhook != window.want {
					t.Fatalf("%s window through the cache = %+v, want total and webhook %+v", window.name, r, window.want)
				}
			}
		})
	}
}
