package storage_test

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestPostgresScopedCapabilitiesUnionAndExclude(t *testing.T) {
	provider := newTestPostgres(t)
	scope := tenancy.NewOrgScope("licensed", storage.DefaultOrgID)
	now := time.Now().UTC()
	for _, rec := range []*storage.IncidentRecord{
		{ID: "default-old", OrgID: storage.DefaultOrgID, Title: "shared outage", Service: "checkout", Origin: storage.OriginAIDetect, CreatedAt: now.Add(-3 * time.Minute)},
		{ID: "licensed-new", OrgID: "licensed", Title: "shared outage", Service: "checkout", Origin: storage.OriginWebhook, CreatedAt: now.Add(-time.Minute)},
		{ID: "foreign", OrgID: "foreign", Title: "shared outage", Service: "checkout", Origin: storage.OriginWebhook, CreatedAt: now},
	} {
		if err := provider.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident %q: %v", rec.ID, err)
		}
	}
	for _, rec := range []*storage.AnalysisRecord{
		{ID: "default-analysis", OrgID: storage.DefaultOrgID, IncidentID: "default-old", RequestedAt: now.Add(-2 * time.Minute), RawResponse: "shared finding"},
		{ID: "licensed-analysis", OrgID: "licensed", IncidentID: "licensed-new", RequestedAt: now.Add(-time.Minute), RawResponse: "shared finding"},
		{ID: "foreign-analysis", OrgID: "foreign", IncidentID: "foreign", RequestedAt: now, RawResponse: "shared finding"},
	} {
		if err := provider.SaveAnalysis(rec); err != nil {
			t.Fatalf("SaveAnalysis %q: %v", rec.ID, err)
		}
	}

	pager := provider.(storage.ScopedIncidentPager)
	counts, err := pager.CountIncidentsByStatusForScope(scope)
	if err != nil {
		t.Fatalf("CountIncidentsByStatusForScope: %v", err)
	}
	if counts.Total.Total != 2 || counts.AIDetect.Total != 1 || counts.Webhook.Total != 1 {
		t.Fatalf("scoped status counts = %#v, want total=2 ai=1 webhook=1", counts)
	}
	page, err := pager.ListIncidentsPageForScope(scope, "", 0, 1)
	if err != nil {
		t.Fatalf("ListIncidentsPageForScope: %v", err)
	}
	if len(page) != 1 || page[0].ID != "licensed-new" {
		t.Fatalf("first scoped page = %#v, want licensed-new", page)
	}
	next, err := pager.ListIncidentsPageForScope(scope, "", 1, 1)
	if err != nil {
		t.Fatalf("ListIncidentsPageForScope next: %v", err)
	}
	if len(next) != 1 || next[0].ID != "default-old" {
		t.Fatalf("second scoped page = %#v, want default-old", next)
	}

	searchPager := provider.(storage.ScopedIncidentSearchPager)
	searchCounts, err := searchPager.CountIncidentsMatchingByStatusForScope(scope, "shared")
	if err != nil {
		t.Fatalf("CountIncidentsMatchingByStatusForScope: %v", err)
	}
	if searchCounts.Total.Total != 2 {
		t.Fatalf("scoped search total = %d, want 2", searchCounts.Total.Total)
	}
	hits, err := searchPager.SearchIncidentsPageForScope(scope, "shared", "", 0, 10)
	if err != nil {
		t.Fatalf("SearchIncidentsPageForScope: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("scoped search returned %d rows, want 2", len(hits))
	}

	serviceCounter := provider.(storage.ScopedIncidentServiceCounter)
	serviceCount, _, err := serviceCounter.CountIncidentsByServiceSinceForScope(scope, "checkout", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountIncidentsByServiceSinceForScope: %v", err)
	}
	if serviceCount != 2 {
		t.Fatalf("scoped service count = %d, want 2", serviceCount)
	}

	rangeLister := provider.(storage.ScopedRangeLister)
	rangeRows, err := rangeLister.ListIncidentsInRangeForScope(
		scope, now.Add(-4*time.Minute), now.Add(-30*time.Second), 10,
	)
	if err != nil {
		t.Fatalf("ListIncidentsInRangeForScope: %v", err)
	}
	if len(rangeRows) != 2 || rangeRows[0].ID != "licensed-new" || rangeRows[1].ID != "default-old" {
		t.Fatalf("scoped range = %#v, want licensed-new then default-old", rangeRows)
	}
	for _, row := range rangeRows {
		if row.OrgID == "foreign" {
			t.Fatal("foreign incident leaked into scoped range")
		}
	}

	analysisPager := provider.(storage.ScopedAnalysisPager)
	analysisCount, err := analysisPager.CountAnalysesForScope(scope)
	if err != nil {
		t.Fatalf("CountAnalysesForScope: %v", err)
	}
	if analysisCount != 2 {
		t.Fatalf("scoped analysis count = %d, want 2", analysisCount)
	}
	analyses, err := analysisPager.ListAnalysesPageForScope(scope, 0, 10)
	if err != nil {
		t.Fatalf("ListAnalysesPageForScope: %v", err)
	}
	if len(analyses) != 2 || analyses[0].ID != "licensed-analysis" || analyses[1].ID != "default-analysis" {
		t.Fatalf("scoped analyses = %#v, want licensed then default", analyses)
	}

	searcher := provider.(storage.ScopedSearcher)
	analysisHits, err := searcher.SearchAnalysesForScope(scope, "shared finding", 10)
	if err != nil {
		t.Fatalf("SearchAnalysesForScope: %v", err)
	}
	if len(analysisHits) != 2 {
		t.Fatalf("scoped analysis search returned %d rows, want 2", len(analysisHits))
	}
}
