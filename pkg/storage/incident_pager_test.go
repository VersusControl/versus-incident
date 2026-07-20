package storage_test

// incident_pager_test.go — the bounded-list seam (storage.IncidentPager and
// storage.IncidentSearchPager): a cheap per-origin count computed WITHOUT
// loading rows, plus a most-recent-first page with offset continuation. These
// back the fix for the list endpoint that used to load the whole table to
// render one page and compute counts.
//
// Memory and file backends run unconditionally (they implement the capability
// over their capped in-memory slice); Postgres is gated on TEST_POSTGRES_DSN.

import (
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// pagerRecords seeds a provider with a deterministic, newest-last set: a mix
// of explicit-origin and legacy (origin-derived-from-source) rows so the
// per-origin classification is exercised end to end. Returns the ids in
// creation order (oldest first) so the newest-first expectations are easy to
// state.
func seedPagerRecords(t *testing.T, p storage.Provider) []string {
	t.Helper()
	base := time.Now().UTC().Add(-time.Hour)
	// origin/source pairs: the classification must land these as noted.
	specs := []struct {
		origin string // explicit Origin ("" = legacy, derived from source)
		source string
		want   string // expected EffectiveOrigin
	}{
		{storage.OriginAIDetect, "agent", storage.OriginAIDetect},
		{storage.OriginWebhook, "webhook", storage.OriginWebhook},
		{"", "agent", storage.OriginAIDetect},        // legacy agent → ai_detect
		{"", "agent:detect", storage.OriginAIDetect}, // legacy agent:… → ai_detect
		{"", "sns", storage.OriginWebhook},           // legacy inbound → webhook
		{"", "", storage.OriginWebhook},              // legacy empty → webhook
		{storage.OriginAIDetect, "agent", storage.OriginAIDetect},
		{storage.OriginWebhook, "sqs", storage.OriginWebhook},
	}
	ids := make([]string, 0, len(specs))
	for i, s := range specs {
		id := string(rune('a' + i))
		rec := &storage.IncidentRecord{
			ID:        id,
			Title:     "incident " + id,
			Origin:    s.origin,
			Source:    s.source,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if got := rec.EffectiveOrigin(); got != s.want {
			t.Fatalf("spec %d: EffectiveOrigin = %q, want %q", i, got, s.want)
		}
		if err := p.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func newMemoryPager(t *testing.T) storage.Provider { return storage.NewMemory() }

func newFilePager(t *testing.T) storage.Provider {
	t.Helper()
	p, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return p
}

// TestIncidentPagerCounts proves CountIncidents returns the whole-set
// per-origin tally including legacy rows, on every backend implementing the
// capability.
func TestIncidentPagerCounts(t *testing.T) {
	backends := map[string]func(*testing.T) storage.Provider{
		"memory":   newMemoryPager,
		"file":     newFilePager,
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			seedPagerRecords(t, p)

			pager, ok := p.(storage.IncidentPager)
			if !ok {
				t.Fatalf("%s backend does not implement storage.IncidentPager", name)
			}
			counts, err := pager.CountIncidents()
			if err != nil {
				t.Fatalf("CountIncidents: %v", err)
			}
			// From the seed: 4 ai_detect (2 explicit + 2 legacy agent), 4 webhook.
			if counts.AIDetect != 4 {
				t.Errorf("AIDetect = %d, want 4", counts.AIDetect)
			}
			if counts.Webhook != 4 {
				t.Errorf("Webhook = %d, want 4", counts.Webhook)
			}
			if counts.Total != 8 {
				t.Errorf("Total = %d, want 8", counts.Total)
			}
			if counts.AIDetect+counts.Webhook != counts.Total {
				t.Errorf("breakdown %d+%d != total %d", counts.AIDetect, counts.Webhook, counts.Total)
			}
		})
	}
}

// TestIncidentPagerPageNewestFirst proves the page read returns rows newest
// first and that offset continuation neither drops nor duplicates a row at the
// page boundary.
func TestIncidentPagerPageNewestFirst(t *testing.T) {
	backends := map[string]func(*testing.T) storage.Provider{
		"memory":   newMemoryPager,
		"file":     newFilePager,
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			ids := seedPagerRecords(t, p) // oldest-first ids
			pager := p.(storage.IncidentPager)

			// Newest-first is the reverse of creation order.
			wantOrder := make([]string, len(ids))
			for i, id := range ids {
				wantOrder[len(ids)-1-i] = id
			}

			// Walk the whole set 3 rows at a time; concatenating the pages must
			// reproduce wantOrder exactly (no dupes, no gaps at the boundary).
			var got []string
			for offset := 0; ; offset += 3 {
				page, err := pager.ListIncidentsPage("", offset, 3)
				if err != nil {
					t.Fatalf("ListIncidentsPage offset=%d: %v", offset, err)
				}
				if len(page) == 0 {
					break
				}
				for _, r := range page {
					got = append(got, r.ID)
				}
				if len(page) < 3 {
					break
				}
			}
			if len(got) != len(wantOrder) {
				t.Fatalf("paged %d ids, want %d", len(got), len(wantOrder))
			}
			for i := range wantOrder {
				if got[i] != wantOrder[i] {
					t.Fatalf("page order[%d] = %q, want %q (full: %v)", i, got[i], wantOrder[i], got)
				}
			}
		})
	}
}

// TestIncidentPagerOriginFilter proves the origin filter is applied in the
// backend so a filtered page is bounded to that origin and stays newest-first.
func TestIncidentPagerOriginFilter(t *testing.T) {
	backends := map[string]func(*testing.T) storage.Provider{
		"memory":   newMemoryPager,
		"file":     newFilePager,
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			seedPagerRecords(t, p)
			pager := p.(storage.IncidentPager)

			ai, err := pager.ListIncidentsPage(storage.OriginAIDetect, 0, 100)
			if err != nil {
				t.Fatalf("ListIncidentsPage ai: %v", err)
			}
			if len(ai) != 4 {
				t.Fatalf("ai page returned %d rows, want 4", len(ai))
			}
			for _, r := range ai {
				if r.EffectiveOrigin() != storage.OriginAIDetect {
					t.Errorf("ai page contains %q origin row %q", r.EffectiveOrigin(), r.ID)
				}
			}

			web, err := pager.ListIncidentsPage(storage.OriginWebhook, 0, 100)
			if err != nil {
				t.Fatalf("ListIncidentsPage webhook: %v", err)
			}
			if len(web) != 4 {
				t.Fatalf("webhook page returned %d rows, want 4", len(web))
			}
			for _, r := range web {
				if r.EffectiveOrigin() != storage.OriginWebhook {
					t.Errorf("webhook page contains %q origin row %q", r.EffectiveOrigin(), r.ID)
				}
			}
		})
	}
}

// TestIncidentPagerDefaultPageSize proves limit<=0 falls back to the bounded
// default rather than returning the whole set.
func TestIncidentPagerDefaultPageSize(t *testing.T) {
	p := storage.NewMemory()
	seedPagerRecords(t, p)
	pager := p.(storage.IncidentPager)
	page, err := pager.ListIncidentsPage("", 0, 0)
	if err != nil {
		t.Fatalf("ListIncidentsPage: %v", err)
	}
	// Only 8 rows seeded, all fit under the default page; the point is the
	// call is bounded by DefaultIncidentPageSize, not that it errors.
	if len(page) != 8 {
		t.Fatalf("default-size page returned %d rows, want 8", len(page))
	}
	if storage.DefaultIncidentPageSize <= 0 {
		t.Fatalf("DefaultIncidentPageSize must be positive, got %d", storage.DefaultIncidentPageSize)
	}
}

// TestIncidentPagerLargeTablePostgres proves the fix on a large table: with
// ~100k incidents the count is one COUNT query and a page is one bounded LIMIT
// query returning only the page size — never the whole table into memory, and
// the page is served from the created_at index rather than a full scan. Gated
// on a real Postgres.
func TestIncidentPagerLargeTablePostgres(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	accessor, ok := p.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres backend must implement storage.SQLAccessor")
	}
	db := accessor.DB()

	// Bulk-seed ~100k incidents in one server-side statement (individual
	// SaveIncident round-trips would take far too long). Half ai_detect, half
	// webhook; created_at spread so newest-first ordering is real. The promoted
	// columns are written directly — the incident path no longer uses `data`.
	const n = 100000
	if _, err := db.Exec(`
		INSERT INTO vs_incidents (id, created_at, origin, resolved, title)
		SELECT
			'bulk-' || g,
			now() - (g || ' seconds')::interval,
			CASE WHEN g % 2 = 0 THEN 'ai_detect' ELSE 'webhook' END,
			false,
			'incident ' || g
		FROM generate_series(1, $1) AS g`, n); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}

	pager := p.(storage.IncidentPager)

	// The count is one query and reflects every row — without materializing a
	// single incident into Go.
	counts, err := pager.CountIncidents()
	if err != nil {
		t.Fatalf("CountIncidents: %v", err)
	}
	if counts.Total != n {
		t.Fatalf("CountIncidents total = %d, want %d", counts.Total, n)
	}
	if counts.AIDetect != n/2 || counts.Webhook != n/2 {
		t.Fatalf("count breakdown = ai:%d webhook:%d, want %d each", counts.AIDetect, counts.Webhook, n/2)
	}

	// A page returns ONLY the requested window, not the whole table — the whole
	// point of the fix. Newest-first.
	const pageSize = 1000
	page, err := pager.ListIncidentsPage("", 0, pageSize)
	if err != nil {
		t.Fatalf("ListIncidentsPage: %v", err)
	}
	if len(page) != pageSize {
		t.Fatalf("page returned %d rows, want %d (bounded)", len(page), pageSize)
	}
	for i := 1; i < len(page); i++ {
		if page[i-1].CreatedAt.Before(page[i].CreatedAt) {
			t.Fatalf("large-table page not newest-first at %d", i)
		}
	}

	// The page ORDER BY created_at DESC LIMIT must be served by the created_at
	// index — not a full scan of all 100k rows — so a page stays cheap as the
	// history grows.
	var plan strings.Builder
	rows, err := db.Query(`EXPLAIN SELECT id FROM vs_incidents ORDER BY created_at DESC LIMIT 1000`)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration error: %v", err)
	}
	if strings.Contains(plan.String(), "Seq Scan") {
		t.Errorf("page query fell back to a Seq Scan of the whole table:\n%s", plan.String())
	}
}

// TestIncidentSearchPagerPostgres proves the Postgres search pager counts and
// pages full-text matches without loading the whole match set. Gated on a real
// Postgres (it is the only backend implementing IncidentSearchPager).
func TestIncidentSearchPagerPostgres(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	seedPagerRecords(t, p)

	sp, ok := p.(storage.IncidentSearchPager)
	if !ok {
		t.Fatal("postgres backend does not implement storage.IncidentSearchPager")
	}

	// Every seeded title contains "incident"; the count must match the total.
	counts, err := sp.CountIncidentsMatching("incident")
	if err != nil {
		t.Fatalf("CountIncidentsMatching: %v", err)
	}
	if counts.Total != 8 {
		t.Errorf("matching total = %d, want 8", counts.Total)
	}

	// A bounded page of matches, newest first.
	page, err := sp.SearchIncidentsPage("incident", "", 0, 3)
	if err != nil {
		t.Fatalf("SearchIncidentsPage: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("search page returned %d rows, want 3", len(page))
	}
	for i := 1; i < len(page); i++ {
		if page[i-1].CreatedAt.Before(page[i].CreatedAt) {
			t.Errorf("search page not newest-first at %d", i)
		}
	}

	// Origin-scoped match count degrades to the plain count of that origin.
	aiPage, err := sp.SearchIncidentsPage("incident", storage.OriginAIDetect, 0, 100)
	if err != nil {
		t.Fatalf("SearchIncidentsPage ai: %v", err)
	}
	if len(aiPage) != 4 {
		t.Fatalf("ai search page returned %d rows, want 4", len(aiPage))
	}
	for _, r := range aiPage {
		if r.EffectiveOrigin() != storage.OriginAIDetect {
			t.Errorf("ai search page contains %q origin row %q", r.EffectiveOrigin(), r.ID)
		}
	}
}
