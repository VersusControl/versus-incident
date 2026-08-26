package storage_test

// incident_window_counts_test.go — the windowed count seam
// (storage.IncidentWindowCounter). Counts used to describe the whole table,
// so the number on screen only ever grew and stopped describing current load.
// The window bounds every count surface to a recent span, and this proves the
// bound is applied in the STORE (not by trimming a page after the fact) and
// that all three backends agree on the same answer.
//
// Memory and file backends run unconditionally; Postgres is gated on
// TEST_POSTGRES_DSN.

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// seedWindowRecords saves two cohorts either side of the window edge: three
// recent rows (1h old) and three stale ones (30d old). The stale cohort is
// deliberately given a DIFFERENT status mix from the recent one, so a backend
// that ignores `since` cannot accidentally produce the right numbers.
func seedWindowRecords(t *testing.T, p storage.Provider) {
	t.Helper()
	now := time.Now().UTC()
	type spec struct {
		age      time.Duration
		resolved bool
		acked    bool
	}
	specs := []spec{
		// recent: 2 open, 1 acked
		{time.Hour, false, false},
		{2 * time.Hour, false, false},
		{3 * time.Hour, false, true},
		// stale: 1 open, 2 resolved
		{30 * 24 * time.Hour, false, false},
		{31 * 24 * time.Hour, true, false},
		{32 * 24 * time.Hour, true, false},
	}
	for i, s := range specs {
		rec := &storage.IncidentRecord{
			ID:        string(rune('a' + i)),
			Title:     "incident",
			Origin:    storage.OriginWebhook,
			Source:    "webhook",
			Resolved:  s.resolved,
			CreatedAt: now.Add(-s.age),
		}
		if s.acked {
			ackedAt := rec.CreatedAt.Add(time.Second)
			rec.AckedAt = &ackedAt
		}
		if s.resolved {
			resolvedAt := rec.CreatedAt.Add(2 * time.Second)
			rec.ResolvedAt = &resolvedAt
		}
		if err := p.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %d: %v", i, err)
		}
	}
}

// TestCountIncidentsByStatusSince proves the window excludes rows older than
// the cutoff on every backend, and that a zero cutoff means unbounded (the
// "all time" setting) rather than "nothing is recent enough".
func TestCountIncidentsByStatusSince(t *testing.T) {
	backends := map[string]func(*testing.T) storage.Provider{
		"memory":   newMemoryPager,
		"file":     newFilePager,
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			seedWindowRecords(t, p)

			counter, ok := p.(storage.IncidentWindowCounter)
			if !ok {
				t.Fatalf("%s backend does not implement storage.IncidentWindowCounter", name)
			}

			// 7-day window: only the recent cohort.
			since := time.Now().UTC().Add(-7 * 24 * time.Hour)
			got, err := counter.CountIncidentsByStatusSince(since)
			if err != nil {
				t.Fatalf("CountIncidentsByStatusSince: %v", err)
			}
			want := storage.IncidentStatusCounts{
				Webhook: storage.StatusCounts{Open: 2, Acked: 1, Total: 3},
				Total:   storage.StatusCounts{Open: 2, Acked: 1, Total: 3},
			}
			if got != want {
				t.Fatalf("windowed counts =\n%+v\nwant\n%+v", got, want)
			}
			assertStatusInvariants(t, got)

			// Zero cutoff: unbounded, so both cohorts are counted. This is the
			// "all time" setting and must not be read as an empty window.
			all, err := counter.CountIncidentsByStatusSince(time.Time{})
			if err != nil {
				t.Fatalf("CountIncidentsByStatusSince(zero): %v", err)
			}
			wantAll := storage.IncidentStatusCounts{
				Webhook: storage.StatusCounts{Open: 3, Acked: 1, Resolved: 2, Total: 6},
				Total:   storage.StatusCounts{Open: 3, Acked: 1, Resolved: 2, Total: 6},
			}
			if all != wantAll {
				t.Fatalf("unbounded counts =\n%+v\nwant\n%+v", all, wantAll)
			}
			assertStatusInvariants(t, all)

			// A cutoff in the future matches nothing — the window is a real
			// filter, not a no-op that always returns the whole set.
			none, err := counter.CountIncidentsByStatusSince(time.Now().UTC().Add(time.Hour))
			if err != nil {
				t.Fatalf("CountIncidentsByStatusSince(future): %v", err)
			}
			if none.Total.Total != 0 {
				t.Fatalf("future cutoff counted %d rows, want 0", none.Total.Total)
			}
		})
	}
}
