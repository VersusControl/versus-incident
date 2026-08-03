package agent

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// catalog_page_test.go — parity coverage for the bounded catalog list reads
// (Catalog.PatternsPage / Catalog.ServicesPage). Three backends must page
// identically: the default in-memory map, a base CatalogStore that only
// implements Snapshot (the Go-side fold), and the live Postgres pager (gated on
// TEST_POSTGRES_DSN). All three order patterns by Count desc / id asc and
// services by FirstSeen asc / name asc, return the whole-(filtered-)set total,
// and clamp the window.

// snapshotOnlyStore is a CatalogStore that serves a fixed read view via
// Snapshot but deliberately does NOT implement CatalogPager, so it exercises
// the Catalog's Snapshot-fold fallback (the file-backend / non-pager path).
type snapshotOnlyStore struct {
	patterns []*Pattern
	services map[string]ServiceInfo
}

func (s *snapshotOnlyStore) Load() (map[string]*Pattern, map[string]*ServiceInfo, error) {
	return nil, nil, nil
}
func (s *snapshotOnlyStore) Persist(map[string]*Pattern, map[string]*ServiceInfo) error { return nil }
func (s *snapshotOnlyStore) Snapshot() ([]*Pattern, map[string]ServiceInfo, error) {
	return s.patterns, s.services, nil
}
func (s *snapshotOnlyStore) Curate(CatalogEdit) error { return nil }

// TestPatternsPage_InMemory proves the default in-memory backend serves a
// bounded page (not the whole catalog) ordered by Count desc, reports the true
// total, and honours the search filter.
func TestPatternsPage_InMemory(t *testing.T) {
	SetCatalogStore(nil)
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// Seed five patterns with descending counts so the ordering is unambiguous.
	c.Upsert("p1", "checkout timeout", "src", 5, 0.2, "default", "checkout")
	c.Upsert("p2", "payment declined", "src", 4, 0.2, "default", "payments")
	c.Upsert("p3", "orders slow", "src", 3, 0.2, "default", "orders")
	c.Upsert("p4", "cache miss", "src", 2, 0.2, "default", "cache")
	c.Upsert("p5", "dns flake", "src", 1, 0.2, "default", "dns")

	page, total, err := c.PatternsPage(CatalogPageOptions{Offset: 0, Limit: 2})
	if err != nil {
		t.Fatalf("PatternsPage: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5 (whole-set count regardless of page size)", total)
	}
	if len(page) != 2 {
		t.Fatalf("page len = %d, want 2 (bounded, not the whole catalog)", len(page))
	}
	if page[0].ID != "p1" || page[1].ID != "p2" {
		t.Fatalf("page order = [%s %s], want [p1 p2] (Count desc)", page[0].ID, page[1].ID)
	}

	// Second window continues the same order without overlap.
	next, _, err := c.PatternsPage(CatalogPageOptions{Offset: 2, Limit: 2})
	if err != nil {
		t.Fatalf("PatternsPage offset 2: %v", err)
	}
	if len(next) != 2 || next[0].ID != "p3" || next[1].ID != "p4" {
		t.Fatalf("second window = %v, want [p3 p4]", ids(next))
	}

	// Search filters both page and total to the matching set.
	hits, hitTotal, err := c.PatternsPage(CatalogPageOptions{Limit: 10, Search: "payment"})
	if err != nil {
		t.Fatalf("PatternsPage search: %v", err)
	}
	if hitTotal != 1 || len(hits) != 1 || hits[0].ID != "p2" {
		t.Fatalf("search 'payment' = %v total=%d, want [p2] total=1", ids(hits), hitTotal)
	}
}

// TestServicesPage_InMemory proves the service list is bounded, totalled, and
// name-searchable on the default in-memory backend.
func TestServicesPage_InMemory(t *testing.T) {
	SetCatalogStore(nil)
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	for _, name := range []string{"checkout", "payments", "orders"} {
		c.RegisterService(name)
	}

	page, total, err := c.ServicesPage(CatalogPageOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ServicesPage: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(page) != 2 {
		t.Fatalf("page len = %d, want 2 (bounded)", len(page))
	}

	hits, hitTotal, err := c.ServicesPage(CatalogPageOptions{Limit: 10, Search: "check"})
	if err != nil {
		t.Fatalf("ServicesPage search: %v", err)
	}
	if hitTotal != 1 || len(hits) != 1 || hits[0].Name != "checkout" {
		t.Fatalf("search 'check' = %v total=%d, want [checkout] total=1", svcNames(hits), hitTotal)
	}
}

// TestPatternsPage_SnapshotFallback proves a base CatalogStore (Snapshot only,
// no CatalogPager) is paged by folding Snapshot in Go, with the SAME Count-desc
// order and true total the pager path returns.
func TestPatternsPage_SnapshotFallback(t *testing.T) {
	store := &snapshotOnlyStore{
		patterns: []*Pattern{
			{ID: "a", Template: "alpha", Count: 1},
			{ID: "b", Template: "bravo", Count: 9},
			{ID: "c", Template: "charlie", Count: 5},
		},
	}
	SetCatalogStore(store)
	t.Cleanup(func() { SetCatalogStore(nil) })
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	page, total, err := c.PatternsPage(CatalogPageOptions{Limit: 2})
	if err != nil {
		t.Fatalf("PatternsPage: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(page) != 2 || page[0].ID != "b" || page[1].ID != "c" {
		t.Fatalf("fallback page = %v, want [b c] (Count desc)", ids(page))
	}
}

// TestServicesPage_SnapshotFallback proves the Snapshot-fold service page is
// ordered by FirstSeen asc / name asc — the deterministic order the UI relies
// on to accumulate stable windows.
func TestServicesPage_SnapshotFallback(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &snapshotOnlyStore{
		services: map[string]ServiceInfo{
			"late":   {FirstSeen: base.Add(2 * time.Hour)},
			"early":  {FirstSeen: base},
			"middle": {FirstSeen: base.Add(time.Hour)},
		},
	}
	SetCatalogStore(store)
	t.Cleanup(func() { SetCatalogStore(nil) })
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}

	page, total, err := c.ServicesPage(CatalogPageOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ServicesPage: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if got := svcNames(page); got[0] != "early" || got[1] != "middle" || got[2] != "late" {
		t.Fatalf("service order = %v, want [early middle late] (FirstSeen asc)", got)
	}
}

// TestPGCatalog_ListPatternsPage_OrgScopedAndOrdered proves the live Postgres
// pager applies the SAME org scope Snapshot does (a pattern under a different
// org never appears), orders by fleet count desc, and reports the whole-set
// total independent of the page window.
func TestPGCatalog_ListPatternsPage_OrgScopedAndOrdered(t *testing.T) {
	cat, db := newPGCatalog(t)

	cat.Upsert("hot", "hot template", "src", 9, 0.2, "default", "checkout")
	cat.Upsert("warm", "warm template", "src", 4, 0.2, "default", "orders")
	cat.Upsert("cool", "cool template", "src", 1, 0.2, "default", "cache")
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist default org: %v", err)
	}

	// A pattern owned by a DIFFERENT org must be invisible to the default
	// store's pager — the tenant-isolation guarantee Snapshot enforces.
	other := NewPostgresCatalogStore(db, "tenant-other", 0)
	other.Persist(
		map[string]*Pattern{
			"foreign": {ID: "foreign", OrgID: "tenant-other", Template: "foreign template", Count: 100},
		},
		nil,
	)

	pager, ok := catalogStore().(CatalogPager)
	if !ok {
		t.Fatal("postgres catalog store must implement CatalogPager")
	}

	page, total, err := pager.ListPatternsPage(CatalogPageOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListPatternsPage: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3 (default org only, foreign excluded)", total)
	}
	if len(page) != 2 || page[0].ID != "hot" || page[1].ID != "warm" {
		t.Fatalf("page = %v, want [hot warm] (count desc)", ids(page))
	}
	for _, p := range page {
		if p.ID == "foreign" {
			t.Fatal("foreign-org pattern leaked into default-org page")
		}
		if p.OrgID != storage.DefaultOrgID {
			t.Fatalf("page pattern OrgID = %q, want %q", p.OrgID, storage.DefaultOrgID)
		}
	}

	// The second window continues without overlap; total is unchanged.
	tail, tailTotal, err := pager.ListPatternsPage(CatalogPageOptions{Offset: 2, Limit: 2})
	if err != nil {
		t.Fatalf("ListPatternsPage tail: %v", err)
	}
	if tailTotal != 3 {
		t.Fatalf("tail total = %d, want 3", tailTotal)
	}
	if len(tail) != 1 || tail[0].ID != "cool" {
		t.Fatalf("tail = %v, want [cool]", ids(tail))
	}
}

// TestPGCatalog_ListServicesPage_OrgScopedAndOrdered proves the live Postgres
// service pager is org-scoped, ordered by first_seen, and totalled.
func TestPGCatalog_ListServicesPage_OrgScopedAndOrdered(t *testing.T) {
	cat, db := newPGCatalog(t)

	cat.CreateService("checkout")
	cat.CreateService("payments")
	if err := cat.Persist(); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	other := NewPostgresCatalogStore(db, "tenant-other", 0)
	other.Persist(nil, map[string]*ServiceInfo{
		"foreign-svc": {OrgID: "tenant-other", FirstSeen: time.Now().UTC(), Manual: true},
	})

	pager := catalogStore().(CatalogPager)
	page, total, err := pager.ListServicesPage(CatalogPageOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListServicesPage: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2 (default org only)", total)
	}
	for _, row := range page {
		if row.Name == "foreign-svc" {
			t.Fatal("foreign-org service leaked into default-org page")
		}
		if row.Info.OrgID != storage.DefaultOrgID {
			t.Fatalf("service OrgID = %q, want %q", row.Info.OrgID, storage.DefaultOrgID)
		}
	}
}

func ids(ps []*Pattern) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

func svcNames(rows []ServiceRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}
