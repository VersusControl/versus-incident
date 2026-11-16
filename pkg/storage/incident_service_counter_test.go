package storage_test

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func testIncidentServiceCounter(t *testing.T, provider storage.Provider) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, rec := range []*storage.IncidentRecord{
		{ID: "old", OrgID: "org-a", Service: "checkout", CreatedAt: now.Add(-48 * time.Hour)},
		// Insert newest before older so insertion order cannot masquerade as time order.
		{ID: "a-newer", OrgID: "org-a", Service: "checkout", CreatedAt: now.Add(-time.Minute), Content: map[string]any{"labels": map[string]any{"severity": "critical"}}},
		{ID: "a-older", OrgID: "org-a", CreatedAt: now.Add(-2 * time.Hour), Content: map[string]any{"service": "checkout", "severity": "warning"}},
		{ID: "a-middle", OrgID: "org-a", Service: "checkout", CreatedAt: now.Add(-time.Hour), Content: map[string]any{"Trigger": map[string]any{"Dimensions": []any{map[string]any{"Name": "Severity", "Value": "medium"}}}}},
		{ID: "other-org", OrgID: "org-b", Service: "checkout", CreatedAt: now.Add(-time.Hour)},
		{ID: "other-service", OrgID: "org-a", Service: "billing", CreatedAt: now.Add(-30 * time.Minute)},
		{ID: "different-case", OrgID: "org-a", Service: "Checkout", CreatedAt: now.Add(-20 * time.Minute)},
	} {
		if err := provider.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident(%s): %v", rec.ID, err)
		}
	}

	counter, ok := provider.(storage.IncidentServiceCounter)
	if !ok {
		t.Fatal("memory backend does not implement IncidentServiceCounter")
	}
	count, severities, err := counter.CountIncidentsByServiceSince("org-a", "checkout", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountIncidentsByServiceSince: %v", err)
	}
	if count != 3 || severities["warning"] != 1 || severities["critical"] != 1 || severities["medium"] != 1 {
		t.Fatalf("count/severities = %d/%v, want 3 with warning+critical+medium", count, severities)
	}

	recent, err := counter.ListIncidentsByServiceSince("org-a", "checkout", now.Add(-24*time.Hour), 3)
	if err != nil {
		t.Fatalf("ListIncidentsByServiceSince: %v", err)
	}
	if len(recent) != 3 || recent[0].ID != "a-newer" || recent[1].ID != "a-middle" || recent[2].ID != "a-older" {
		t.Fatalf("recent = %#v, want newest org-a checkout incidents by CreatedAt", recent)
	}
	caseCount, _, err := counter.CountIncidentsByServiceSince("org-a", "Checkout", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("CountIncidentsByServiceSince mixed case: %v", err)
	}
	if caseCount != 1 {
		t.Fatalf("Checkout count = %d, want 1 exact-case match", caseCount)
	}
}

// testIncidentServiceCounterAlertmanagerSeverity counts severities for
// Alertmanager bodies whose labels sit only on the alerts, plus malformed
// shapes that must count as "no severity" instead of failing the query.
func testIncidentServiceCounterAlertmanagerSeverity(t *testing.T, provider storage.Provider) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	labels := func(severity string) map[string]any {
		return map[string]any{"labels": map[string]any{"severity": severity}}
	}
	for i, content := range []map[string]any{
		{"status": "firing", "alerts": []any{labels("critical"), labels("info")}},
		{"commonLabels": map[string]any{"env": "prod"}, "alerts": []any{labels("high")}},
		{"commonLabels": map[string]any{"env": "prod"}, "alerts": []any{labels("warning"), labels("info")}},
		{"alerts": []any{"x"}},
		{"alerts": "text"},
		{"alerts": []any{map[string]any{"labels": "x"}}},
		{"alerts": map[string]any{"labels": map[string]any{"severity": "critical"}}},
		{"labels": "x"},
		{"Trigger": "x"},
	} {
		rec := &storage.IncidentRecord{ID: "am-" + string(rune('a'+i)), OrgID: "org-a", Service: "alerts", CreatedAt: now.Add(-time.Duration(i+1) * time.Minute), Content: content}
		if err := provider.SaveIncident(rec); err != nil {
			t.Fatalf("SaveIncident(%s): %v", rec.ID, err)
		}
	}
	count, severities, err := provider.(storage.IncidentServiceCounter).CountIncidentsByServiceSince("org-a", "alerts", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CountIncidentsByServiceSince: %v", err)
	}
	if count != 9 || severities["critical"] != 1 || severities["high"] != 1 || severities[""] != 7 || len(severities) != 3 {
		t.Fatalf("count/severities = %d/%v, want 9 with critical=1 high=1 and 7 without severity", count, severities)
	}
}

func TestIncidentServiceCounterParity(t *testing.T) {
	backends := map[string]func(*testing.T) storage.Provider{
		"memory": func(*testing.T) storage.Provider { return storage.NewMemory() },
		"file": func(t *testing.T) storage.Provider {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatalf("NewFile: %v", err)
			}
			return provider
		},
		"postgres": func(t *testing.T) storage.Provider { return newTestPostgres(t) },
	}
	for name, makeProvider := range backends {
		t.Run(name, func(t *testing.T) {
			testIncidentServiceCounter(t, makeProvider(t))
			testIncidentServiceCounterAlertmanagerSeverity(t, makeProvider(t))
		})
	}
}
