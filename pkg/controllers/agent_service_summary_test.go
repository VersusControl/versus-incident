package controllers

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// newContentServiceSummaryController seeds a store with incidents for
// "checkout" in every shape the intake sees: the durable Service column, a
// webhook payload whose service lives only in content, an Alertmanager
// notification with service+severity only in labels, and a CloudWatch alarm
// with the service only in Trigger.Dimensions. One unrelated incident guards
// against over-counting.
func newContentServiceSummaryController(t *testing.T) (*AgentController, []*storage.IncidentRecord) {
	t.Helper()
	loadServiceDetailConfig(t, "30m")

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("checkout")

	now := time.Now().UTC()
	recs := []*storage.IncidentRecord{
		{ID: "i-column", Service: "checkout", Title: "column", CreatedAt: now.Add(-1 * time.Hour),
			Content: map[string]any{"severity": "critical"}},
		{ID: "i-content", Title: "content only", CreatedAt: now.Add(-2 * time.Hour),
			Content: map[string]any{"service": "checkout", "severity": "high"}},
		{ID: "i-alertmanager", Title: "alertmanager", CreatedAt: now.Add(-3 * time.Hour),
			Content: map[string]any{"labels": map[string]any{"service": "checkout", "severity": "warning"}}},
		{ID: "i-cloudwatch", Title: "cloudwatch", CreatedAt: now.Add(-4 * time.Hour),
			Content: map[string]any{
				"AlarmName": "cpu-high",
				"Trigger": map[string]any{
					"Namespace": "AWS/ECS",
					"Dimensions": []any{
						map[string]any{"name": "ClusterName", "value": "prod"},
						map[string]any{"name": "ServiceName", "value": "checkout"},
					},
				},
				"severity": "critical",
			}},
		{ID: "i-other", Service: "billing", Title: "unrelated", CreatedAt: now.Add(-1 * time.Hour),
			Content: map[string]any{"severity": "low"}},
	}
	store := storage.NewMemory()
	for _, r := range recs {
		if err := store.SaveIncident(r); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	services.SetStorage(store)
	t.Cleanup(func() { services.SetStorage(nil) })

	return NewAgentController(cat, nil, nil, nil, nil, false), recs
}

// TestServiceIncidentSummary_MatchesIncidentsList is the reported bug: the
// service detail counted the raw Service column while the incidents list
// derives the label from content, so a webhook incident whose service only
// lives in the payload showed up in the list but not in the count.
func TestServiceIncidentSummary_MatchesIncidentsList(t *testing.T) {
	ctrl, recs := newContentServiceSummaryController(t)

	listCount := 0
	for _, r := range recs {
		if services.ServiceLabel(r) == "checkout" {
			listCount++
		}
	}
	if listCount != 4 {
		t.Fatalf("list count = %d, want 4 (test fixture drifted)", listCount)
	}

	summary := ctrl.serviceIncidentSummary("checkout", storage.DefaultOrgID)
	got, ok := summary["count"].(int)
	if !ok {
		t.Fatalf("count is %T, want int", summary["count"])
	}
	if got != listCount {
		t.Fatalf("service detail count = %d, incidents list count = %d", got, listCount)
	}
}

func TestServiceIncidentSummary_ServiceCapabilityAvoidsWholeTableStarvation(t *testing.T) {
	loadServiceDetailConfig(t, "30m")
	store := storage.NewMemory()
	now := time.Now().UTC()
	if err := store.SaveIncident(&storage.IncidentRecord{
		ID: "target", OrgID: "org-a", Service: "checkout", Title: "target",
		CreatedAt: now.Add(-2 * time.Hour), Content: map[string]any{"severity": "high"},
	}); err != nil {
		t.Fatalf("SaveIncident(target): %v", err)
	}
	for i := 0; i < serviceIncidentScanLimit+1; i++ {
		if err := store.SaveIncident(&storage.IncidentRecord{
			ID: "unrelated-" + string(rune(i)), OrgID: "org-a", Service: "billing",
			CreatedAt: now.Add(-time.Hour).Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("SaveIncident(unrelated %d): %v", i, err)
		}
	}
	services.SetStorage(store)
	t.Cleanup(func() { services.SetStorage(nil) })

	summary := (&AgentController{}).serviceIncidentSummary("checkout", "org-a")
	if got, _ := summary["count"].(int); got != 1 {
		t.Fatalf("count = %d, want 1 beyond newest-%d unrelated rows", got, serviceIncidentScanLimit)
	}
	recent, _ := summary["recent"].([]fiber.Map)
	if len(recent) != 1 || recent[0]["id"] != "target" {
		t.Fatalf("recent = %#v, want target incident", recent)
	}
}

func TestServiceIncidentSummary_UsesConfiguredCountWindow(t *testing.T) {
	store := storage.NewMemory()
	now := time.Now().UTC()
	for _, rec := range []*storage.IncidentRecord{
		{ID: "recent", Service: "checkout", CreatedAt: now.Add(-time.Hour)},
		{ID: "older", Service: "checkout", CreatedAt: now.Add(-48 * time.Hour)},
	} {
		if err := store.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident(%s): %v", rec.ID, err)
		}
	}
	services.SetStorage(store)
	t.Cleanup(func() { services.SetStorage(nil) })

	for _, tc := range []struct {
		window string
		want   int
	}{
		{window: agent.CountWindow24h, want: 1},
		{window: agent.CountWindowAll, want: 2},
	} {
		if err := agent.SaveCountSettings(store, agent.CountSettings{Window: tc.window}); err != nil {
			t.Fatalf("SaveCountSettings(%s): %v", tc.window, err)
		}
		summary := (&AgentController{}).serviceIncidentSummary("checkout", storage.DefaultOrgID)
		if got := summary["count_window"]; got != tc.window {
			t.Errorf("window %s: count_window = %v", tc.window, got)
		}
		if got, _ := summary["count"].(int); got != tc.want {
			t.Errorf("window %s: count = %d, want %d", tc.window, got, tc.want)
		}
	}
}

// TestServiceIncidentSummary_SeverityHistogramReadsNestedShapes guards the
// second half of the same bug: the histogram read only the top-level severity
// key, so Alertmanager and CloudWatch incidents landed in "unknown". The
// buckets are the report's fixed bands, so a synonym like "warning" is counted
// under "medium" rather than opening a bucket of its own.
func TestServiceIncidentSummary_SeverityHistogramReadsNestedShapes(t *testing.T) {
	ctrl, _ := newContentServiceSummaryController(t)

	summary := ctrl.serviceIncidentSummary("checkout", storage.DefaultOrgID)
	sev, ok := summary["severities"].(fiber.Map)
	if !ok {
		t.Fatalf("severities is %T, want a map", summary["severities"])
	}
	if _, unknown := sev["unknown"]; unknown {
		t.Fatalf("severity histogram has an unknown bucket: %v", sev)
	}
	want := map[string]int{"critical": 2, "high": 1, "medium": 1}
	for band, n := range want {
		if got, _ := sev[band].(int); got != n {
			t.Fatalf("severities[%q] = %v, want %d (histogram %v)", band, sev[band], n, sev)
		}
	}
}

// TestServiceIncidentSummary_SeverityHistogramKeysAreBanded proves the
// histogram keys come from the fixed band set, not from the payload. The raw
// severity is attacker-supplied free text, so keying the response on it both
// fragmented the counts and let a stream of distinct labels grow the response
// by one key each.
func TestServiceIncidentSummary_SeverityHistogramKeysAreBanded(t *testing.T) {
	loadServiceDetailConfig(t, "30m")

	cat, err := agent.LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	cat.RegisterService("checkout")

	now := time.Now().UTC()
	store := storage.NewMemory()
	for i, sev := range []string{"sev1", "P1", "CRITICAL", "wat-is-this", "🔥", ""} {
		rec := &storage.IncidentRecord{
			ID: "i-" + sev + string(rune('a'+i)), Service: "checkout",
			Title: "seeded", CreatedAt: now.Add(-time.Duration(i+1) * time.Minute),
			Content: map[string]any{"severity": sev},
		}
		if err := store.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	services.SetStorage(store)
	t.Cleanup(func() { services.SetStorage(nil) })

	ctrl := NewAgentController(cat, nil, nil, nil, nil, false)
	summary := ctrl.serviceIncidentSummary("checkout", storage.DefaultOrgID)
	sev, ok := summary["severities"].(fiber.Map)
	if !ok {
		t.Fatalf("severities is %T, want a map", summary["severities"])
	}

	bands := map[string]bool{"critical": true, "high": true, "medium": true, "low": true, "unknown": true}
	for k := range sev {
		if !bands[k] {
			t.Fatalf("histogram key %q is not a band: %v", k, sev)
		}
	}
	if got, _ := sev["critical"].(int); got != 3 {
		t.Fatalf("severities[critical] = %v, want 3 (sev1/P1/CRITICAL all band critical): %v", sev["critical"], sev)
	}
	if got, _ := sev["unknown"].(int); got != 3 {
		t.Fatalf("severities[unknown] = %v, want 3 (unrecognised labels collapse): %v", sev["unknown"], sev)
	}

	// Every recent row reports its band too, so the list and the histogram
	// never disagree about one incident's severity.
	recent, ok := summary["recent"].([]fiber.Map)
	if !ok {
		t.Fatalf("recent is %T, want []fiber.Map", summary["recent"])
	}
	for _, r := range recent {
		if s, _ := r["severity"].(string); !bands[s] {
			t.Fatalf("recent severity %q is not a band: %v", s, r)
		}
	}
}
