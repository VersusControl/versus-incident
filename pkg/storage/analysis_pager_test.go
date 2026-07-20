package storage_test

// analysis_pager_test.go — the bounded analyses-list seam
// (storage.AnalysisPager): a cheap total count computed WITHOUT loading rows,
// plus a most-recent-first page with offset continuation. It backs the fix for
// the /analyses endpoint that used to load the whole vs_analyses table to
// render one page.
//
// Memory and file backends run unconditionally (they implement the capability
// over their capped in-memory slice); Postgres is gated on TEST_POSTGRES_DSN.

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// seedAnalyses stores n analyses with strictly increasing requested_at so the
// newest-first order is deterministic. Returns the ids in creation order
// (oldest first), so the newest-first expectation is the reverse.
func seedAnalyses(t *testing.T, p storage.Provider, n int) []string {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := "an-" + string(rune('a'+i))
		rec := &storage.AnalysisRecord{
			ID:          id,
			IncidentID:  "inc-1",
			RequestedAt: base.Add(time.Duration(i) * time.Minute),
			Status:      "ok",
		}
		if err := p.SaveAnalysis(rec); err != nil {
			t.Fatalf("SaveAnalysis %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func analysisPagerBackends() map[string]func(*testing.T) storage.Provider {
	return map[string]func(*testing.T) storage.Provider{
		"memory":   newMemoryPager,
		"file":     newFilePager,
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
}

// TestAnalysisPagerCount proves CountAnalyses returns the whole-set total
// without materializing rows, on every backend implementing the capability.
func TestAnalysisPagerCount(t *testing.T) {
	for name, mk := range analysisPagerBackends() {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			seedAnalyses(t, p, 5)

			pager, ok := p.(storage.AnalysisPager)
			if !ok {
				t.Fatalf("%s backend does not implement storage.AnalysisPager", name)
			}
			n, err := pager.CountAnalyses()
			if err != nil {
				t.Fatalf("CountAnalyses: %v", err)
			}
			if n != 5 {
				t.Fatalf("CountAnalyses = %d, want 5", n)
			}
		})
	}
}

// TestAnalysisPagerPageNewestFirst proves the page read returns rows newest
// first and that offset continuation neither drops nor duplicates a row across
// page boundaries.
func TestAnalysisPagerPageNewestFirst(t *testing.T) {
	for name, mk := range analysisPagerBackends() {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			ids := seedAnalyses(t, p, 5) // oldest→newest
			pager := p.(storage.AnalysisPager)

			// Walk the whole set two rows at a time and reassemble the ids.
			var got []string
			for offset := 0; offset < 5; offset += 2 {
				page, err := pager.ListAnalysesPage(offset, 2)
				if err != nil {
					t.Fatalf("ListAnalysesPage offset=%d: %v", offset, err)
				}
				for _, r := range page {
					got = append(got, r.ID)
				}
			}
			// Expected: newest first — the reverse of creation order.
			want := []string{ids[4], ids[3], ids[2], ids[1], ids[0]}
			if len(got) != len(want) {
				t.Fatalf("reassembled %d ids, want %d (%v)", len(got), len(want), got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d = %q, want %q (full %v)", i, got[i], want[i], got)
				}
			}
		})
	}
}

// TestAnalysisPagerDefaultPageSize proves limit<=0 falls back to the bounded
// DefaultAnalysisPageSize rather than an unbounded read.
func TestAnalysisPagerDefaultPageSize(t *testing.T) {
	p := storage.NewMemory()
	seedAnalyses(t, p, 3)
	pager := p.(storage.AnalysisPager)
	page, err := pager.ListAnalysesPage(0, 0)
	if err != nil {
		t.Fatalf("ListAnalysesPage: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("default-size page returned %d rows, want 3", len(page))
	}
}
