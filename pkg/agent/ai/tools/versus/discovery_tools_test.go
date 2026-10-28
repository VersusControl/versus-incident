package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type discoveryCatalog struct {
	patterns         []*PatternView
	services         []ServiceRow
	patternPageReads int
}

func (catalog *discoveryCatalog) PatternsPage(opts CatalogPageOptions) ([]*PatternView, int, error) {
	catalog.patternPageReads++
	var matches []*PatternView
	for _, pattern := range catalog.patterns {
		if opts.Service != "" && pattern.Service != opts.Service {
			continue
		}
		query := strings.ToLower(strings.TrimSpace(opts.Search))
		if query != "" && !strings.Contains(strings.ToLower(pattern.ID+" "+pattern.Template+" "+pattern.Service), query) {
			continue
		}
		matches = append(matches, pattern)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Count > matches[j].Count })
	return pagePatternViews(matches, opts.Offset, opts.Limit), len(matches), nil
}

func (catalog *discoveryCatalog) ServicesPage(opts CatalogPageOptions) ([]ServiceRow, int, error) {
	var matches []ServiceRow
	query := strings.ToLower(strings.TrimSpace(opts.Search))
	for _, service := range catalog.services {
		if query == "" || strings.Contains(strings.ToLower(service.Name), query) {
			matches = append(matches, service)
		}
	}
	return pageServiceRows(matches, opts.Offset, opts.Limit), len(matches), nil
}

func pagePatternViews(values []*PatternView, offset, limit int) []*PatternView {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = discoveryDefaultLimit
	}
	if offset >= len(values) {
		return nil
	}
	end := offset + limit
	if end > len(values) {
		end = len(values)
	}
	return values[offset:end]
}

func pageServiceRows(values []ServiceRow, offset, limit int) []ServiceRow {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = discoveryDefaultLimit
	}
	if offset >= len(values) {
		return nil
	}
	end := offset + limit
	if end > len(values) {
		end = len(values)
	}
	return values[offset:end]
}

type discoveryStore struct {
	storage.Provider
	incidents         []*storage.IncidentRecord
	analyses          []*storage.AnalysisRecord
	serviceRangeReads int
	analysisPageReads int
}

func newDiscoveryStore(incidents []*storage.IncidentRecord, analyses []*storage.AnalysisRecord) *discoveryStore {
	return &discoveryStore{Provider: storage.NewMemory(), incidents: incidents, analyses: analyses}
}

func orgAllowed(scope tenancy.OrgScope, orgID string) bool {
	orgID = storage.NormalizeOrgID(orgID)
	for _, allowed := range scope.Normalized().OrgIDs() {
		if orgID == allowed {
			return true
		}
	}
	return false
}

func (store *discoveryStore) scopedIncidents(scope tenancy.OrgScope) []*storage.IncidentRecord {
	var out []*storage.IncidentRecord
	for _, record := range store.incidents {
		if orgAllowed(scope, record.OrgID) {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (store *discoveryStore) CountIncidentsByStatusSinceForScope(scope tenancy.OrgScope, since time.Time) (storage.IncidentStatusCounts, error) {
	return storage.StatusCountsSince(store.scopedIncidents(scope), since), nil
}

func (store *discoveryStore) CountIncidentsForScope(scope tenancy.OrgScope) (storage.IncidentCounts, error) {
	counts := storage.StatusCountsOf(store.scopedIncidents(scope))
	return storage.IncidentCounts{AIDetect: counts.AIDetect.Open + counts.AIDetect.Acked, Webhook: counts.Webhook.Open + counts.Webhook.Acked, Total: counts.Total.Open + counts.Total.Acked}, nil
}

func (store *discoveryStore) CountIncidentsByStatusForScope(scope tenancy.OrgScope) (storage.IncidentStatusCounts, error) {
	return storage.StatusCountsOf(store.scopedIncidents(scope)), nil
}

func (store *discoveryStore) ListIncidentsPageForScope(scope tenancy.OrgScope, _ string, offset, limit int) ([]*storage.IncidentRecord, error) {
	values := store.scopedIncidents(scope)
	if offset >= len(values) {
		return nil, nil
	}
	end := offset + limit
	if end > len(values) {
		end = len(values)
	}
	return values[offset:end], nil
}

func (store *discoveryStore) CountIncidentsByServicesSinceForScope(scope tenancy.OrgScope, services []string, since time.Time) (map[string]int, error) {
	return store.CountIncidentsByServicesInRangeForScope(scope, services, since, time.Time{})
}

func (store *discoveryStore) CountIncidentsByServicesInRangeForScope(scope tenancy.OrgScope, services []string, start, end time.Time) (map[string]int, error) {
	store.serviceRangeReads++
	out := make(map[string]int, len(services))
	wanted := make(map[string]bool, len(services))
	for _, service := range services {
		wanted[service] = true
	}
	for _, record := range store.scopedIncidents(scope) {
		if !record.CreatedAt.Before(start) && (end.IsZero() || record.CreatedAt.Before(end)) && wanted[record.ServiceLabel()] {
			out[record.ServiceLabel()]++
		}
	}
	return out, nil
}

type fallbackDiscoveryStore struct {
	storage.Provider
	incidents []*storage.IncidentRecord
	reads     int
}

type overviewRangeStore struct {
	*discoveryStore
	reads int
	limit int
}

func (store *overviewRangeStore) ListIncidentsInRangeForScope(scope tenancy.OrgScope, start, end time.Time, limit int) ([]*storage.IncidentRecord, error) {
	store.reads++
	store.limit = limit
	var records []*storage.IncidentRecord
	for _, record := range store.scopedIncidents(scope) {
		if !record.CreatedAt.Before(start) && (end.IsZero() || record.CreatedAt.Before(end)) {
			records = append(records, record)
		}
	}
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}

func (store *fallbackDiscoveryStore) ListIncidents(limit int) ([]*storage.IncidentRecord, error) {
	store.reads++
	if limit > 0 && len(store.incidents) > limit {
		return store.incidents[:limit], nil
	}
	return store.incidents, nil
}

func (store *discoveryStore) SearchIncidentsPageForScope(scope tenancy.OrgScope, query, _ string, offset, limit int) ([]*storage.IncidentRecord, error) {
	matches := store.matchingIncidents(scope, query)
	if offset >= len(matches) {
		return nil, nil
	}
	end := offset + limit
	if end > len(matches) {
		end = len(matches)
	}
	return matches[offset:end], nil
}

func (store *discoveryStore) CountIncidentsMatchingForScope(scope tenancy.OrgScope, query string) (storage.IncidentCounts, error) {
	counts := storage.StatusCountsOf(store.matchingIncidents(scope, query))
	return storage.IncidentCounts{Total: counts.Total.Open + counts.Total.Acked}, nil
}

func (store *discoveryStore) CountIncidentsMatchingByStatusForScope(scope tenancy.OrgScope, query string) (storage.IncidentStatusCounts, error) {
	return storage.StatusCountsOf(store.matchingIncidents(scope, query)), nil
}

func (store *discoveryStore) matchingIncidents(scope tenancy.OrgScope, query string) []*storage.IncidentRecord {
	query = strings.ToLower(query)
	var out []*storage.IncidentRecord
	for _, record := range store.scopedIncidents(scope) {
		if query == "" || strings.Contains(strings.ToLower(record.Title+" "+record.ServiceLabel()+" "+record.Source), query) {
			out = append(out, record)
		}
	}
	return out
}

func (store *discoveryStore) SearchAnalysesPageForScope(scope tenancy.OrgScope, opts storage.AnalysisSearchOptions) ([]*storage.AnalysisRecord, int, error) {
	store.analysisPageReads++
	incidents := make(map[string]*storage.IncidentRecord)
	for _, incident := range store.scopedIncidents(scope) {
		incidents[incident.ID] = incident
	}
	var matches []*storage.AnalysisRecord
	for _, record := range store.analyses {
		if !orgAllowed(scope, record.OrgID) || opts.IncidentID != "" && record.IncidentID != opts.IncidentID {
			continue
		}
		if opts.Service != "" {
			incident := incidents[record.IncidentID]
			if incident == nil || incident.ServiceLabel() != opts.Service {
				continue
			}
		}
		raw, _ := json.Marshal(record)
		if opts.Query != "" && !strings.Contains(strings.ToLower(string(raw)), strings.ToLower(opts.Query)) {
			continue
		}
		matches = append(matches, record)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].RequestedAt.After(matches[j].RequestedAt) })
	total := len(matches)
	if len(matches) > opts.Limit {
		matches = matches[:opts.Limit]
	}
	return matches, total, nil
}

type staticHealth struct{ snapshot DetectionHealthSnapshot }

func (health staticHealth) DetectionHealth(tenancy.OrgScope) DetectionHealthSnapshot {
	return health.snapshot
}

type secretRedactor struct{}

func (secretRedactor) Scrub(value string) string {
	return strings.ReplaceAll(value, "secret", "[redacted]")
}

func TestDiscoveryToolsUnavailable(t *testing.T) {
	tools := []core.Tool{GetSystemOverview{}, ListServices{}, GetDetectionHealth{}, SearchIncidents{}, ListPatterns{}, ListAnalyses{}}
	for _, tool := range tools {
		t.Run(tool.Name(), func(t *testing.T) {
			result, err := tool.Invoke(context.Background(), nil)
			assertUnavailable(t, result, err)
		})
	}
}

func TestDiscoveryToolsRejectMalformedArgs(t *testing.T) {
	catalog := &discoveryCatalog{}
	store := newDiscoveryStore(nil, nil)
	health := staticHealth{}
	tools := []core.Tool{
		GetSystemOverview{Store: store, Catalog: catalog}, ListServices{Store: store, Catalog: catalog},
		GetDetectionHealth{Reader: health}, SearchIncidents{Store: store},
		ListPatterns{Catalog: catalog}, ListAnalyses{Store: store},
	}
	for _, tool := range tools {
		t.Run(tool.Name(), func(t *testing.T) {
			_, err := tool.Invoke(context.Background(), json.RawMessage(`{"limit":`))
			if err == nil {
				t.Fatal("Invoke error = nil, want malformed argument error")
			}
			code, message := core.ClassifyToolError(err)
			if code != core.ToolErrorInvalidArguments || message != "arguments must be valid JSON" {
				t.Fatalf("classification = %q, %q, want invalid_arguments and actionable message", code, message)
			}
		})
	}
}

func TestDiscoveryToolsRejectBlankServiceWithoutReadingData(t *testing.T) {
	now := time.Now().UTC()
	catalog := &discoveryCatalog{patterns: []*PatternView{{ID: "unfiltered-pattern", Service: "api"}}}
	store := newDiscoveryStore(
		[]*storage.IncidentRecord{{ID: "unfiltered-incident", Service: "api", CreatedAt: now}},
		[]*storage.AnalysisRecord{{ID: "unfiltered-analysis", IncidentID: "unfiltered-incident", RequestedAt: now}},
	)
	tests := []struct {
		name  string
		tool  core.Tool
		reads func() int
	}{
		{name: "list_patterns", tool: ListPatterns{Catalog: catalog}, reads: func() int { return catalog.patternPageReads }},
		{name: "list_analyses", tool: ListAnalyses{Store: store}, reads: func() int { return store.analysisPageReads }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.tool.Invoke(context.Background(), json.RawMessage(`{"service":" \t "}`))
			if result != nil {
				t.Fatalf("result = %+v, want nil", result)
			}
			code, message := core.ClassifyToolError(err)
			if code != core.ToolErrorInvalidArguments || message != "service must not be blank" {
				t.Fatalf("classification = %q, %q, want invalid_arguments and fixed safe message", code, message)
			}
			if reads := test.reads(); reads != 0 {
				t.Fatalf("backend reads = %d, want 0", reads)
			}
		})
	}
}

func TestDiscoveryToolsEmpty(t *testing.T) {
	catalog := &discoveryCatalog{}
	store := newDiscoveryStore(nil, nil)
	health := staticHealth{}
	tools := []core.Tool{
		GetSystemOverview{Store: store, Catalog: catalog, Health: health},
		ListServices{Store: store, Catalog: catalog},
		GetDetectionHealth{Reader: health},
		SearchIncidents{Store: store},
		ListPatterns{Catalog: catalog},
		ListAnalyses{Store: store},
	}
	for _, tool := range tools {
		t.Run(tool.Name(), func(t *testing.T) {
			result, err := tool.Invoke(context.Background(), nil)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if !result.IsAvailable() || result.Found {
				t.Fatalf("available=%v found=%v, want available empty result", result.IsAvailable(), result.Found)
			}
		})
	}
}

func TestDiscoveryToolsPreserveAvailablePartialBlocks(t *testing.T) {
	catalog := &discoveryCatalog{services: []ServiceRow{{Name: "api"}}}
	services, err := (ListServices{Catalog: catalog}).Invoke(context.Background(), nil)
	if err != nil || !services.IsAvailable() || !services.Found || services.Data["incident_counts_available"] != false {
		t.Fatalf("catalog-only services = %+v, err=%v", services, err)
	}
	health := staticHealth{snapshot: DetectionHealthSnapshot{Sources: []SourceHealth{{Name: "logs", Kind: "logs", Configured: true}}}}
	overview, err := (GetSystemOverview{Health: health}).Invoke(context.Background(), nil)
	if err != nil || !overview.IsAvailable() || !overview.Found || overview.Data["incident_data_available"] != false || overview.Data["catalog_available"] != false {
		t.Fatalf("health-only overview = %+v, err=%v", overview, err)
	}
}

func TestGetSystemOverviewReportsScopedCountsCatalogAndDarkCategories(t *testing.T) {
	now := time.Now().UTC()
	store := newDiscoveryStore([]*storage.IncidentRecord{
		{ID: "default", Service: "api", CreatedAt: now.Add(-time.Minute)},
		{ID: "licensed", OrgID: "licensed", Service: "worker", Resolved: true, CreatedAt: now.Add(-2 * time.Minute)},
		{ID: "foreign", OrgID: "foreign", Service: "secret", CreatedAt: now.Add(-time.Minute)},
	}, nil)
	catalog := &discoveryCatalog{
		services: []ServiceRow{{Name: "api"}, {Name: "worker"}},
		patterns: []*PatternView{{ID: "p1"}, {ID: "p2"}},
	}
	health := staticHealth{snapshot: DetectionHealthSnapshot{Categories: []CategoryHealth{{Kind: "logs", Configured: true}, {Kind: "metrics", Dark: true}}, Observation: "unknown"}}
	result, err := (GetSystemOverview{Store: store, Scope: tenancy.NewOrgScope("licensed", "default"), Catalog: catalog, Health: health}).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	counts := result.Data["incident_counts"].(storage.IncidentStatusCounts)
	if counts.Total.Total != 2 || counts.Total.Resolved != 1 {
		t.Fatalf("counts = %+v, want two scoped incidents with one resolved", counts)
	}
	active := result.Data["active_services"].([]string)
	if fmt.Sprint(active) != "[api worker]" {
		t.Fatalf("active services = %v, want [api worker]", active)
	}
	if result.Data["patterns_learned_total"] != 2 || result.Data["known_services_total"] != 2 {
		t.Fatalf("catalog totals = patterns %v services %v", result.Data["patterns_learned_total"], result.Data["known_services_total"])
	}
}

func TestListServicesCapsAndUsesBatchedScopedCounts(t *testing.T) {
	now := time.Now().UTC()
	services := make([]ServiceRow, 105)
	for index := range services {
		services[index] = ServiceRow{Name: fmt.Sprintf("service-%03d", index), Info: ServiceInfo{FirstSeen: now.Add(-time.Duration(index) * time.Hour)}}
	}
	store := newDiscoveryStore([]*storage.IncidentRecord{
		{ID: "visible", OrgID: "licensed", Service: "service-000", CreatedAt: now},
		{ID: "foreign", OrgID: "foreign", Service: "service-000", CreatedAt: now},
	}, nil)
	result, err := (ListServices{Store: store, Scope: tenancy.NewOrgScope("licensed"), Catalog: &discoveryCatalog{services: services}}).Invoke(context.Background(), mustArgs(t, listServicesArgs{Limit: 1000}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Data["count"] != 100 || result.Data["total"] != 105 || result.Data["truncated"] != true {
		t.Fatalf("page metadata = %+v", result.Data)
	}
	items := result.Data["services"].([]serviceItem)
	if items[0].IncidentCount != 1 || result.Data["incident_counts_exact"] != true {
		t.Fatalf("first service = %+v exact=%v", items[0], result.Data["incident_counts_exact"])
	}
	if store.serviceRangeReads != 1 {
		t.Fatalf("service summary reads = %d, want one batched read", store.serviceRangeReads)
	}
}

func TestSearchIncidentsRedactsTitlesAndOmitsQueryEcho(t *testing.T) {
	store := newDiscoveryStore([]*storage.IncidentRecord{{ID: "incident", Title: "secret title", CreatedAt: time.Now()}}, nil)
	result, err := (SearchIncidents{Store: store, Redactor: secretRedactor{}}).Invoke(context.Background(), json.RawMessage(`{"query":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	item := result.Data["incidents"].([]incidentSearchItem)[0]
	if strings.Contains(item.Title, "secret") {
		t.Fatalf("title was not redacted: %q", item.Title)
	}
	if _, exists := result.Data["query"]; exists {
		t.Fatal("query echo should not expand model output")
	}
}

func TestListServicesExactCountsHonorEndAndContentService(t *testing.T) {
	now := time.Now().UTC()
	store := newDiscoveryStore([]*storage.IncidentRecord{
		{ID: "inside", Content: map[string]interface{}{"service": "api"}, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "at-end", Service: "api", CreatedAt: now.Add(-time.Hour)},
	}, nil)
	result, err := (ListServices{Store: store, Catalog: &discoveryCatalog{services: []ServiceRow{{Name: "api"}}}}).Invoke(context.Background(), mustArgs(t, listServicesArgs{TimeRangeArgs: aitools.TimeRangeArgs{Start: now.Add(-3 * time.Hour).Format(time.RFC3339), End: now.Add(-time.Hour).Format(time.RFC3339)}}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	items := result.Data["services"].([]serviceItem)
	if items[0].IncidentCount != 1 || result.Data["incident_counts_exact"] != true {
		t.Fatalf("services = %+v exact=%v, want one exact in-range incident", items, result.Data["incident_counts_exact"])
	}
}

func TestGetSystemOverviewFallbackUsesFullBudgetAndOneRead(t *testing.T) {
	now := time.Now().UTC()
	incidents := make([]*storage.IncidentRecord, 150)
	for index := range incidents {
		incidents[index] = &storage.IncidentRecord{ID: fmt.Sprintf("incident-%03d", index), Service: fmt.Sprintf("service-%03d", index), CreatedAt: now.Add(-time.Minute)}
	}
	store := &fallbackDiscoveryStore{Provider: storage.NewMemory(), incidents: incidents}
	result, err := (GetSystemOverview{Store: store, Catalog: &discoveryCatalog{}}).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Data["active_services_count"] != 150 || result.Data["incident_counts"].(storage.IncidentStatusCounts).Total.Total != 150 {
		t.Fatalf("overview under-read fallback: %+v", result.Data)
	}
	if store.reads != 1 {
		t.Fatalf("ListIncidents reads = %d, want 1", store.reads)
	}
}

func TestGetSystemOverviewExactCountsUseOneBoundedEmptyRangeRead(t *testing.T) {
	store := &overviewRangeStore{discoveryStore: newDiscoveryStore(nil, nil)}
	result, err := (GetSystemOverview{Store: store, Catalog: &discoveryCatalog{}}).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if store.reads != 1 || store.limit != discoveryFallbackLimit+1 {
		t.Fatalf("range reads/limit = %d/%d, want 1/%d", store.reads, store.limit, discoveryFallbackLimit+1)
	}
	if result.Data["incident_counts_exact"] != true || result.Data["active_services_truncated"] != false || result.Data["has_more"] != false || result.Data["active_services_total"] != 0 {
		t.Fatalf("empty overview metadata = %+v", result.Data)
	}
}

func TestGetSystemOverviewActiveServicesReportsBoundedTruncation(t *testing.T) {
	now := time.Now().UTC()
	incidents := make([]*storage.IncidentRecord, discoveryFallbackLimit+1)
	for index := range incidents {
		incidents[index] = &storage.IncidentRecord{
			ID: fmt.Sprintf("incident-%03d", index), Service: fmt.Sprintf("service-%03d", index), CreatedAt: now.Add(-time.Minute),
		}
	}
	store := &overviewRangeStore{discoveryStore: newDiscoveryStore(incidents, nil)}
	result, err := (GetSystemOverview{Store: store, Catalog: &discoveryCatalog{}}).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	counts := result.Data["incident_counts"].(storage.IncidentStatusCounts)
	if counts.Total.Total != discoveryFallbackLimit+1 || result.Data["incident_counts_exact"] != true {
		t.Fatalf("exact counts = %+v exact=%v", counts, result.Data["incident_counts_exact"])
	}
	if result.Data["active_services_count"] != discoveryFallbackLimit || result.Data["active_services_truncated"] != true || result.Data["has_more"] != true {
		t.Fatalf("bounded active services metadata = %+v", result.Data)
	}
	if _, claimed := result.Data["active_services_total"]; claimed {
		t.Fatalf("bounded active service total must not be reported: %+v", result.Data)
	}
}

func TestSearchIncidentsExactScopedUnion(t *testing.T) {
	now := time.Now().UTC()
	store := newDiscoveryStore([]*storage.IncidentRecord{
		{ID: "default", Title: "shared outage", CreatedAt: now},
		{ID: "licensed", OrgID: "licensed", Title: "shared outage", CreatedAt: now},
		{ID: "foreign", OrgID: "foreign", Title: "shared outage", CreatedAt: now},
	}, nil)
	result, err := (SearchIncidents{Store: store, Scope: tenancy.NewOrgScope("licensed", "default")}).Invoke(context.Background(), mustArgs(t, searchIncidentsArgs{Query: "shared", Limit: 1}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Data["count"] != 1 || result.Data["total"] != 2 || result.Data["truncated"] != true || result.Data["total_exact"] != true {
		t.Fatalf("metadata = %+v", result.Data)
	}
}

func TestListPatternsExactCombinedFilterAndRedaction(t *testing.T) {
	catalog := &discoveryCatalog{patterns: []*PatternView{
		{ID: "p1", Template: "timeout one", Service: "api", Count: 5, Samples: []string{"old secret", "two secret", "three secret", "new secret"}},
		{ID: "p2", Template: "timeout two", Service: "worker", Count: 4},
		{ID: "p3", Template: "other", Service: "api", Count: 3},
	}}
	result, err := (ListPatterns{Catalog: catalog, Redactor: secretRedactor{}}).Invoke(context.Background(), mustArgs(t, describePatternsArgs{Service: "api", Query: "timeout", Limit: 1}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Data["total"] != 1 || result.Data["truncated"] != false {
		t.Fatalf("metadata = %+v", result.Data)
	}
	item := result.Data["patterns"].([]patternItem)[0]
	if item.SamplesTotal != 4 || !item.SamplesTruncated || len(item.Samples) != 3 {
		t.Fatalf("sample metadata = %+v", item)
	}
	for _, sample := range item.Samples {
		if strings.Contains(sample, "secret") {
			t.Fatalf("unredacted sample = %q", sample)
		}
	}
}

func TestListAnalysesFiltersServiceAndExcludesForeign(t *testing.T) {
	now := time.Now().UTC()
	store := newDiscoveryStore([]*storage.IncidentRecord{
		{ID: "visible-incident", OrgID: "licensed", Service: "api", CreatedAt: now},
		{ID: "foreign-incident", OrgID: "foreign", Service: "api", CreatedAt: now},
	}, []*storage.AnalysisRecord{
		{ID: "visible-analysis", OrgID: "licensed", IncidentID: "visible-incident", RequestedAt: now, Status: "ok", RawResponse: "shared conclusion"},
		{ID: "foreign-analysis", OrgID: "foreign", IncidentID: "foreign-incident", RequestedAt: now, Status: "ok", RawResponse: "shared conclusion"},
	})
	result, err := (ListAnalyses{Store: store, Scope: tenancy.NewOrgScope("licensed")}).Invoke(context.Background(), mustArgs(t, analysisHistoryArgs{Service: "api", Query: "shared"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	items := result.Data["analyses"].([]analysisItem)
	if len(items) != 1 || items[0].ID != "visible-analysis" || result.Data["total"] != 1 {
		t.Fatalf("analyses = %+v metadata=%+v", items, result.Data)
	}
}

func TestDiscoveryToolsPreserveExactServiceIdentity(t *testing.T) {
	now := time.Now().UTC()
	store := newDiscoveryStore([]*storage.IncidentRecord{
		{ID: "upper-incident", Service: "Checkout", CreatedAt: now.Add(-time.Minute)},
		{ID: "lower-incident", Service: "checkout", CreatedAt: now.Add(-time.Minute)},
	}, []*storage.AnalysisRecord{
		{ID: "upper-analysis", IncidentID: "upper-incident", RequestedAt: now},
		{ID: "lower-analysis", IncidentID: "lower-incident", RequestedAt: now},
	})
	catalog := &discoveryCatalog{
		services: []ServiceRow{{Name: "Checkout"}, {Name: "checkout"}},
		patterns: []*PatternView{{ID: "upper-pattern", Service: "Checkout"}, {ID: "lower-pattern", Service: "checkout"}},
	}

	servicesResult, err := (ListServices{Store: store, Catalog: catalog}).Invoke(context.Background(), mustArgs(t, listServicesArgs{}))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	services := servicesResult.Data["services"].([]serviceItem)
	if len(services) != 2 || services[0].Name != "Checkout" || services[0].IncidentCount != 1 || services[1].Name != "checkout" || services[1].IncidentCount != 1 {
		t.Fatalf("services = %+v, want two exact identities with one incident each", services)
	}

	patternsResult, err := (ListPatterns{Catalog: catalog}).Invoke(context.Background(), mustArgs(t, describePatternsArgs{Service: services[0].Name}))
	if err != nil {
		t.Fatalf("DescribePatterns: %v", err)
	}
	patterns := patternsResult.Data["patterns"].([]patternItem)
	if len(patterns) != 1 || patterns[0].ID != "upper-pattern" {
		t.Fatalf("patterns = %+v, want exact Checkout pattern", patterns)
	}

	analysesResult, err := (ListAnalyses{Store: store}).Invoke(context.Background(), mustArgs(t, analysisHistoryArgs{Service: services[0].Name}))
	if err != nil {
		t.Fatalf("AnalysisHistory: %v", err)
	}
	analyses := analysesResult.Data["analyses"].([]analysisItem)
	if len(analyses) != 1 || analyses[0].ID != "upper-analysis" {
		t.Fatalf("analyses = %+v, want exact Checkout analysis", analyses)
	}
}

func TestListAnalysesOmitsRawAndBoundsRedactedFinding(t *testing.T) {
	now := time.Now().UTC()
	evidence := make([]core.EvidenceItem, 8)
	for index := range evidence {
		evidence[index] = core.EvidenceItem{Source: "secret", Summary: "secret", Detail: strings.Repeat("secret", 200)}
	}
	store := newDiscoveryStore(nil, []*storage.AnalysisRecord{{
		ID: "analysis", IncidentID: "incident", RequestedAt: now, RawResponse: strings.Repeat("raw-secret", 1000),
		Finding: &core.AIFinding{Title: "secret title", Summary: strings.Repeat("secret", 200), Evidence: evidence},
	}})
	result, err := (ListAnalyses{Store: store, Redactor: secretRedactor{}}).Invoke(context.Background(), mustArgs(t, analysisHistoryArgs{Limit: 100}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Data["limit"] != discoveryDefaultLimit {
		t.Fatalf("limit = %v, want %d", result.Data["limit"], discoveryDefaultLimit)
	}
	item := result.Data["analyses"].([]analysisItem)[0]
	if len(item.Finding.Evidence) != 5 || len([]rune(item.Finding.Summary)) > 512 {
		t.Fatalf("finding not bounded: %+v", item.Finding)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "raw-secret") || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "raw_response") {
		t.Fatalf("analysis history leaked raw or unredacted content: %s", raw)
	}
}

func TestDiscoveryToolsRejectOffsetAboveMaximum(t *testing.T) {
	store := newDiscoveryStore(nil, nil)
	catalog := &discoveryCatalog{}
	tools := []core.Tool{
		ListServices{Store: store, Catalog: catalog},
		SearchIncidents{Store: store},
		ListPatterns{Catalog: catalog},
		ListAnalyses{Store: store},
	}
	for _, tool := range tools {
		t.Run(tool.Name(), func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"offset":501}`)); err == nil || !strings.Contains(err.Error(), "at most 500") {
				t.Fatalf("Invoke error = %v, want bounded offset error", err)
			}
		})
	}
}

func TestGetDetectionHealthUnavailableHasStableUnknownShape(t *testing.T) {
	result, err := (GetDetectionHealth{}).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsAvailable() || result.Data["observation"] != "unknown" || len(result.Data["sources"].([]SourceHealth)) != 0 || len(result.Data["categories"].([]CategoryHealth)) != 3 {
		t.Fatalf("unavailable health shape = %+v", result)
	}
	for _, category := range result.Data["categories"].([]CategoryHealth) {
		if category.Configured || !category.Dark {
			t.Fatalf("unknown category = %+v", category)
		}
	}
}

func TestGetDetectionHealthPreservesUnknownObservation(t *testing.T) {
	snapshot := DetectionHealthSnapshot{Sources: []SourceHealth{{Name: "logs", Kind: "logs", Configured: true, Observation: "unknown"}}, Observation: "unknown"}
	result, err := (GetDetectionHealth{Reader: staticHealth{snapshot: snapshot}}).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	sources := result.Data["sources"].([]SourceHealth)
	if len(sources) != 1 || sources[0].Observation != "unknown" || sources[0].LastSuccessfulPull != nil {
		t.Fatalf("sources = %+v", sources)
	}
}
