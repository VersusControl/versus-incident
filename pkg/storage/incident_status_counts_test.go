package storage_test

// incident_status_counts_test.go — the per-origin × per-status count seam
// (storage.IncidentPager.CountIncidentsByStatus and the search variant). This
// is the storage half of the fix that made the server the single source of
// truth for every displayed count: a cheap COUNT/FILTER breakdown of open /
// acked / resolved per origin, computed WITHOUT loading rows, that must agree
// with the raw stored resolved/acked_at/origin columns even past one page.
//
// Memory and file backends run unconditionally over their capped in-memory
// slice; Postgres is gated on TEST_POSTGRES_DSN.

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

type statusSpec struct {
	origin   string // explicit Origin ("" = legacy, derived from source)
	source   string
	resolved bool
	acked    bool
	want     string // expected EffectiveOrigin
}

// seedStatusRecords saves a deterministic mix of origins and statuses,
// including legacy empty-origin rows (which must classify as webhook). The
// spread of resolved/acked/open across both origins is what lets the test
// prove the breakdown reads the real stored status columns rather than a tally
// of whatever page happened to load.
func seedStatusRecords(t *testing.T, p storage.Provider) []statusSpec {
	t.Helper()
	base := time.Now().UTC().Add(-2 * time.Hour)
	specs := []statusSpec{
		// ai_detect: 3 open, 2 acked, 1 resolved
		{storage.OriginAIDetect, "agent", false, false, storage.OriginAIDetect},
		{storage.OriginAIDetect, "agent", false, false, storage.OriginAIDetect},
		{"", "agent:detect", false, false, storage.OriginAIDetect}, // legacy → ai_detect
		{storage.OriginAIDetect, "agent", false, true, storage.OriginAIDetect},
		{storage.OriginAIDetect, "agent", false, true, storage.OriginAIDetect},
		{storage.OriginAIDetect, "agent", true, false, storage.OriginAIDetect},
		// webhook: 2 open, 1 acked, 3 resolved (incl. legacy empty-origin)
		{storage.OriginWebhook, "webhook", false, false, storage.OriginWebhook},
		{"", "", false, false, storage.OriginWebhook}, // legacy empty → webhook
		{storage.OriginWebhook, "sns", false, true, storage.OriginWebhook},
		{storage.OriginWebhook, "webhook", true, false, storage.OriginWebhook},
		{"", "sqs", true, false, storage.OriginWebhook}, // legacy inbound → webhook
		{storage.OriginWebhook, "webhook", true, false, storage.OriginWebhook},
	}
	for i, s := range specs {
		rec := &storage.IncidentRecord{
			ID:        string(rune('a' + i)),
			Title:     "incident",
			Origin:    s.origin,
			Source:    s.source,
			Resolved:  s.resolved,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if s.acked {
			ackedAt := rec.CreatedAt.Add(time.Second)
			rec.AckedAt = &ackedAt
		}
		if s.resolved {
			resolvedAt := rec.CreatedAt.Add(2 * time.Second)
			rec.ResolvedAt = &resolvedAt
		}
		if got := rec.EffectiveOrigin(); got != s.want {
			t.Fatalf("spec %d: EffectiveOrigin = %q, want %q", i, got, s.want)
		}
		if err := p.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %d: %v", i, err)
		}
	}
	return specs
}

// TestIncidentCountsByStatus proves the per-origin × per-status breakdown
// matches the seeded truth on every backend, and holds the invariants the UI
// relies on: open+acked+resolved == total for each origin, and
// ai_detect+webhook == total for each status.
func TestIncidentCountsByStatus(t *testing.T) {
	backends := map[string]func(*testing.T) storage.Provider{
		"memory":   newMemoryPager,
		"file":     newFilePager,
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			seedStatusRecords(t, p)

			pager, ok := p.(storage.IncidentPager)
			if !ok {
				t.Fatalf("%s backend does not implement storage.IncidentPager", name)
			}
			got, err := pager.CountIncidentsByStatus()
			if err != nil {
				t.Fatalf("CountIncidentsByStatus: %v", err)
			}

			// Expected from the seed truth table.
			want := storage.IncidentStatusCounts{
				AIDetect: storage.StatusCounts{Open: 3, Acked: 2, Resolved: 1, Total: 6},
				Webhook:  storage.StatusCounts{Open: 2, Acked: 1, Resolved: 3, Total: 6},
				Total:    storage.StatusCounts{Open: 5, Acked: 3, Resolved: 4, Total: 12},
			}
			if got != want {
				t.Fatalf("CountIncidentsByStatus =\n%+v\nwant\n%+v", got, want)
			}

			assertStatusInvariants(t, got)
		})
	}
}

// TestIncidentCountsByStatusMatchesRawFilterPostgres cross-checks the
// COUNT/FILTER breakdown against the raw stored columns on a real Postgres:
// each returned status count must equal a plain SELECT count(*) FILTER over
// vs_incidents. This is the direct check that the "resolved says 0 here but
// 776 there" discrepancy was a client-side tally artifact, not bad columns —
// the server count and the raw column truth agree. Also seeds past one page so
// the count reflects rows the list endpoint never loads.
func TestIncidentCountsByStatusMatchesRawFilterPostgres(t *testing.T) {
	p := newTestPostgres(t) // skips when TEST_POSTGRES_DSN is unset
	accessor, ok := p.(storage.SQLAccessor)
	if !ok {
		t.Fatal("postgres backend must implement storage.SQLAccessor")
	}
	db := accessor.DB()

	// Seed well past a single page so a bounded page can never see the whole
	// set. Deterministic status/origin split via modular arithmetic:
	//   origin  = ai_detect when g%2==0 else webhook
	//   resolved when g%3==0; else acked when g%5==0; else open
	const n = 2500
	if _, err := db.Exec(`
		INSERT INTO vs_incidents (id, created_at, origin, resolved, acked_at, resolved_at, title)
		SELECT
			'st-' || g,
			now() - (g || ' seconds')::interval,
			CASE WHEN g % 2 = 0 THEN 'ai_detect' ELSE 'webhook' END,
			(g % 3 = 0),
			CASE WHEN g % 3 <> 0 AND g % 5 = 0 THEN now() ELSE NULL END,
			CASE WHEN g % 3 = 0 THEN now() ELSE NULL END,
			'incident ' || g
		FROM generate_series(1, $1) AS g`, n); err != nil {
		t.Fatalf("bulk seed: %v", err)
	}

	pager := p.(storage.IncidentPager)
	got, err := pager.CountIncidentsByStatus()
	if err != nil {
		t.Fatalf("CountIncidentsByStatus: %v", err)
	}

	// Raw truth straight from the stored columns — the FILTER breakdown must
	// match it exactly.
	rawTotal := func(where string) int {
		t.Helper()
		var c int
		if err := db.QueryRow(`SELECT count(*) FROM vs_incidents WHERE ` + where).Scan(&c); err != nil {
			t.Fatalf("raw count (%s): %v", where, err)
		}
		return c
	}

	checks := []struct {
		name  string
		got   int
		where string
	}{
		{"total open", got.Total.Open, "resolved = false AND acked_at IS NULL"},
		{"total acked", got.Total.Acked, "resolved = false AND acked_at IS NOT NULL"},
		{"total resolved", got.Total.Resolved, "resolved = true"},
		{"total all", got.Total.Total, "true"},
		{"ai open", got.AIDetect.Open, "origin = 'ai_detect' AND resolved = false AND acked_at IS NULL"},
		{"ai acked", got.AIDetect.Acked, "origin = 'ai_detect' AND resolved = false AND acked_at IS NOT NULL"},
		{"ai resolved", got.AIDetect.Resolved, "origin = 'ai_detect' AND resolved = true"},
		{"ai all", got.AIDetect.Total, "origin = 'ai_detect'"},
		{"webhook open", got.Webhook.Open, "origin <> 'ai_detect' AND resolved = false AND acked_at IS NULL"},
		{"webhook acked", got.Webhook.Acked, "origin <> 'ai_detect' AND resolved = false AND acked_at IS NOT NULL"},
		{"webhook resolved", got.Webhook.Resolved, "origin <> 'ai_detect' AND resolved = true"},
		{"webhook all", got.Webhook.Total, "origin <> 'ai_detect'"},
	}
	for _, c := range checks {
		if raw := rawTotal(c.where); c.got != raw {
			t.Errorf("%s: count = %d, raw SELECT count(*) FILTER = %d", c.name, c.got, raw)
		}
	}

	assertStatusInvariants(t, got)
}

// assertStatusInvariants checks the two reconciliation rules every surface
// depends on: statuses sum to the origin total, and origins sum to the status
// total.
func assertStatusInvariants(t *testing.T, c storage.IncidentStatusCounts) {
	t.Helper()
	for _, o := range []struct {
		name string
		s    storage.StatusCounts
	}{
		{"ai_detect", c.AIDetect},
		{"webhook", c.Webhook},
		{"total", c.Total},
	} {
		if o.s.Open+o.s.Acked+o.s.Resolved != o.s.Total {
			t.Errorf("%s: open+acked+resolved (%d+%d+%d) != total %d",
				o.name, o.s.Open, o.s.Acked, o.s.Resolved, o.s.Total)
		}
	}
	for _, s := range []struct {
		name             string
		ai, webhook, tot int
	}{
		{"open", c.AIDetect.Open, c.Webhook.Open, c.Total.Open},
		{"acked", c.AIDetect.Acked, c.Webhook.Acked, c.Total.Acked},
		{"resolved", c.AIDetect.Resolved, c.Webhook.Resolved, c.Total.Resolved},
		{"all", c.AIDetect.Total, c.Webhook.Total, c.Total.Total},
	} {
		if s.ai+s.webhook != s.tot {
			t.Errorf("%s: ai+webhook (%d+%d) != total %d", s.name, s.ai, s.webhook, s.tot)
		}
	}
}
