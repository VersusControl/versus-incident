package storage_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestPostgresCanonicalServiceIdentityAdversarialWrites(t *testing.T) {
	provider := newTestPostgres(t)
	now := time.Now().UTC()
	fixtures := []struct {
		id      string
		service string
		content map[string]interface{}
		want    string
	}{
		{id: "top-exact", content: map[string]interface{}{"ServiceName": "checkout", "APP": "wrong"}, want: "checkout"},
		{id: "top-folded", content: map[string]interface{}{"Service_Name": "orders", "APP": "wrong"}, want: "orders"},
		{id: "nested", content: map[string]interface{}{"labels": map[string]interface{}{"App": "wrong", "Service_Name": "billing"}}, want: "billing"},
		{id: "cloudwatch-service", content: cloudWatchDimensions(map[string]interface{}{"Name": "InstanceId", "Value": "i-abc"}, map[string]interface{}{"Name": "ServiceName", "Value": "payments"}), want: "payments"},
		{id: "cloudwatch-target", content: cloudWatchDimensions(map[string]interface{}{"Name": "LoadBalancer", "Value": "app/lb/1"}, map[string]interface{}{"Name": "TargetGroup", "Value": "tg/checkout/2"}), want: "tg/checkout/2"},
		{id: "explicit-whitespace", service: "  ", content: map[string]interface{}{"service": "ignored"}, want: "  "},
	}
	for _, fixture := range fixtures {
		record := &storage.IncidentRecord{ID: fixture.id, OrgID: "canonical", Service: fixture.service, Content: fixture.content, CreatedAt: now}
		if err := provider.SaveIncident(record); err != nil {
			t.Fatalf("SaveIncident(%s): %v", fixture.id, err)
		}
		if record.Service != fixture.want {
			t.Fatalf("SaveIncident(%s) canonical service = %q, want %q", fixture.id, record.Service, fixture.want)
		}
		got, err := provider.GetIncident(fixture.id)
		if err != nil {
			t.Fatalf("GetIncident(%s): %v", fixture.id, err)
		}
		if got.Service != fixture.want || got.ServiceLabel() != fixture.want {
			t.Fatalf("GetIncident(%s) service/label = %q/%q, want %q", fixture.id, got.Service, got.ServiceLabel(), fixture.want)
		}
		analysis := &storage.AnalysisRecord{ID: "analysis-" + fixture.id, OrgID: "canonical", IncidentID: fixture.id, RequestedAt: now}
		if err := provider.SaveAnalysis(analysis); err != nil {
			t.Fatalf("SaveAnalysis(%s): %v", fixture.id, err)
		}
		if strings.TrimSpace(fixture.want) == "" {
			continue
		}
		analyses, total, err := provider.(storage.ScopedAnalysisSearchPager).SearchAnalysesPageForScope(
			tenancy.NewOrgScope("canonical"), storage.AnalysisSearchOptions{Service: fixture.want, Limit: 10},
		)
		if err != nil {
			t.Fatalf("SearchAnalysesPageForScope(%s): %v", fixture.id, err)
		}
		if total != 1 || len(analyses) != 1 || analyses[0].ID != analysis.ID {
			t.Fatalf("analysis service filter %q = %#v total=%d, want %s", fixture.want, analyses, total, analysis.ID)
		}
	}
}

func TestPostgresAnalysisServiceIdentityIsCaseSensitive(t *testing.T) {
	provider := newTestPostgres(t)
	now := time.Now().UTC()
	for _, service := range []string{"Checkout", "checkout"} {
		incidentID := "incident-" + service
		if err := provider.SaveIncident(&storage.IncidentRecord{ID: incidentID, OrgID: "canonical", Service: service, CreatedAt: now}); err != nil {
			t.Fatalf("SaveIncident(%s): %v", service, err)
		}
		if err := provider.SaveAnalysis(&storage.AnalysisRecord{ID: "analysis-" + service, OrgID: "canonical", IncidentID: incidentID, RequestedAt: now}); err != nil {
			t.Fatalf("SaveAnalysis(%s): %v", service, err)
		}
	}

	analyses, total, err := provider.(storage.ScopedAnalysisSearchPager).SearchAnalysesPageForScope(
		tenancy.NewOrgScope("canonical"), storage.AnalysisSearchOptions{Service: "Checkout", Limit: 10},
	)
	if err != nil {
		t.Fatalf("SearchAnalysesPageForScope: %v", err)
	}
	if total != 1 || len(analyses) != 1 || analyses[0].ID != "analysis-Checkout" {
		t.Fatalf("Checkout analyses = %#v total=%d, want exact-case analysis", analyses, total)
	}
}

func TestPostgresCanonicalServiceQueriesUseIndexAtScale(t *testing.T) {
	provider := newTestPostgres(t)
	db := provider.(storage.SQLAccessor).DB()
	if _, err := db.Exec(`
		INSERT INTO vs_incidents (id, created_at, org_id, service, content)
		SELECT 'scale-' || value, NOW() - (value || ' seconds')::interval,
		       'scale', 'svc-' || (value % 100), '{}'::jsonb
		FROM generate_series(1, 200000) AS value`); err != nil {
		t.Fatalf("seed 200k incidents: %v", err)
	}
	if _, err := db.Exec(`ANALYZE vs_incidents`); err != nil {
		t.Fatalf("analyze incidents: %v", err)
	}

	countPlan := explainPlan(t, db, `SELECT count(*) FROM vs_incidents WHERE org_id = 'scale' AND service = 'svc-7' AND created_at >= NOW() - interval '30 days'`)
	listPlan := explainPlan(t, db, `SELECT id, created_at FROM vs_incidents WHERE org_id = 'scale' AND service = 'svc-7' AND created_at >= NOW() - interval '30 days' ORDER BY created_at DESC LIMIT 10`)
	for name, plan := range map[string]string{"count": countPlan, "list": listPlan} {
		t.Logf("%s service plan:\n%s", name, plan)
		if !strings.Contains(plan, "idx_incidents_org_service_created_at") || strings.Contains(plan, "Seq Scan") {
			t.Fatalf("%s service query did not use canonical service index:\n%s", name, plan)
		}
	}
}

func cloudWatchDimensions(dimensions ...map[string]interface{}) map[string]interface{} {
	items := make([]interface{}, len(dimensions))
	for index, dimension := range dimensions {
		items[index] = dimension
	}
	return map[string]interface{}{"Trigger": map[string]interface{}{"Dimensions": items}}
}

func testPostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
	}
	return dsn
}

func assertIndexExists(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var exists bool
	if err := db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil || !exists {
		t.Fatalf("index %s exists = %v, err=%v", name, exists, err)
	}
}

func explainPlan(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN (ANALYZE, BUFFERS, FORMAT TEXT) " + query)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan explain plan: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain plan rows: %v", err)
	}
	return strings.Join(lines, "\n")
}
