package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestGetIncidentReturnsScopedLifecycleAssignmentAndUnknownResolver(t *testing.T) {
	store := storage.NewMemory()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	acked := now.Add(time.Minute)
	resolved := now.Add(2 * time.Minute)
	record := &storage.IncidentRecord{ID: "inc-1", OrgID: "licensed", Title: "checkout errors", Service: "checkout", Source: "agent", Origin: storage.OriginAIDetect, Resolved: true, CreatedAt: now, AckedAt: &acked, ResolvedAt: &resolved, AssignedTeamID: "team-sre", AssignedMemberIDs: []string{"user-1", "user-2"}, Content: map[string]any{"token": "must-not-leak"}}
	if err := store.SaveIncident(record); err != nil {
		t.Fatal(err)
	}

	result, err := (GetIncident{Store: store, Scope: tenancy.NewOrgScope("licensed", "default")}).Invoke(context.Background(), json.RawMessage(`{"incident_id":"inc-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Data["status"] != "resolved" || result.Data["assigned_team_id"] != "team-sre" {
		t.Fatalf("unexpected result: %+v", result)
	}
	members := result.Data["assigned_member_ids"].([]string)
	if len(members) != 2 || members[0] != "user-1" || members[1] != "user-2" {
		t.Fatalf("assigned members = %v", members)
	}
	if result.Data["resolved_by_known"] != false || result.Data["resolved_by_reason"] == "" {
		t.Fatalf("resolver uncertainty missing: %+v", result.Data)
	}
	encoded, _ := json.Marshal(result.Data)
	if string(encoded) == "" || contains(string(encoded), "must-not-leak") {
		t.Fatalf("raw content leaked: %s", encoded)
	}
}

func TestGetIncidentBoundsScopesAndRedactsLinkedAnalyses(t *testing.T) {
	store := storage.NewMemory()
	if err := store.SaveIncident(&storage.IncidentRecord{ID: "inc-analyses", OrgID: "license", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		orgID := "license"
		if index == 4 {
			orgID = "foreign"
		}
		if err := store.SaveAnalysis(&storage.AnalysisRecord{ID: string(rune('a' + index)), OrgID: orgID, IncidentID: "inc-analyses", RequestedAt: time.Now().Add(time.Duration(index) * time.Second), Status: "ok", Finding: &core.AIFinding{Title: "secret title", Summary: "secret summary", NextSteps: []string{"secret next"}}}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := (GetIncident{Store: store, Scope: tenancy.NewOrgScope("license", "default"), Redactor: secretRedactor{}}).Invoke(context.Background(), json.RawMessage(`{"incident_id":"inc-analyses"}`))
	if err != nil {
		t.Fatal(err)
	}
	analyses := result.Data["analyses"].([]analysisItem)
	if len(analyses) != describeIncidentAnalysisLimit || result.Data["analyses_truncated"] != true {
		t.Fatalf("analyses=%+v truncated=%v", analyses, result.Data["analyses_truncated"])
	}
	encoded, _ := json.Marshal(analyses)
	if contains(string(encoded), "secret") {
		t.Fatalf("analysis text was not redacted: %s", encoded)
	}
}

func TestGetIncidentHidesForeignOrg(t *testing.T) {
	store := storage.NewMemory()
	if err := store.SaveIncident(&storage.IncidentRecord{ID: "foreign", OrgID: "other", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	result, err := (GetIncident{Store: store, Scope: tenancy.NewOrgScope("licensed", "default")}).Invoke(context.Background(), json.RawMessage(`{"incident_id":"foreign"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Found {
		t.Fatalf("foreign incident exposed: %+v", result)
	}
}

func contains(value, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if value[index:index+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
