package tools

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestRecentIncidentsOrgScope(t *testing.T) {
	now := time.Now().UTC()
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "default-1", Service: "api", CreatedAt: now.Add(-time.Minute)},
		&storage.IncidentRecord{ID: "acme-1", OrgID: "acme", Service: "api", CreatedAt: now.Add(-time.Minute)},
	)

	for _, tc := range []struct {
		name  string
		scope tenancy.OrgScope
		want  string
	}{
		{name: "default", want: "default-1"},
		{name: "explicit", scope: tenancy.NewOrgScope("acme"), want: "acme-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := (RecentIncidents{Store: store, Scope: tc.scope}).Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{}))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			incidents := result.Data["incidents"].([]recentIncidentItem)
			if len(incidents) != 1 || incidents[0].ID != tc.want {
				t.Fatalf("incidents = %+v, want only %q", incidents, tc.want)
			}
		})
	}
}

func TestRecentIncidentsUnknownOrgIsEmpty(t *testing.T) {
	store := newStoreWithIncidents(t, &storage.IncidentRecord{ID: "default-1", CreatedAt: time.Now().UTC()})
	result, err := (RecentIncidents{Store: store, Scope: tenancy.NewOrgScope("unknown")}).Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Data["count"] != 0 {
		t.Fatalf("count = %v, want 0", result.Data["count"])
	}
	if result.Found {
		t.Fatal("Found = true, want false for an empty result")
	}
}

func TestRecentIncidentsOrgScopeUnionExcludesForeign(t *testing.T) {
	now := time.Now().UTC()
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "legacy", CreatedAt: now},
		&storage.IncidentRecord{ID: "licensed", OrgID: "licensed", CreatedAt: now},
		&storage.IncidentRecord{ID: "foreign", OrgID: "foreign", CreatedAt: now},
	)
	result, err := (RecentIncidents{
		Store: store,
		Scope: tenancy.NewOrgScope("licensed", storage.DefaultOrgID),
	}).Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	incidents := result.Data["incidents"].([]recentIncidentItem)
	if len(incidents) != 2 {
		t.Fatalf("incidents = %+v, want licensed and default only", incidents)
	}
	seen := map[string]bool{}
	for _, incident := range incidents {
		seen[incident.ID] = true
	}
	if !seen["legacy"] || !seen["licensed"] || seen["foreign"] {
		t.Fatalf("incident ids = %v, want legacy+licensed without foreign", seen)
	}
}
