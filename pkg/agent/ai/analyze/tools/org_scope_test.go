package tools

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestRecentIncidents_OrgScoped proves the tool reads ONE org's incidents.
// storage.Provider.ListIncidents takes no org parameter, so an unscoped tool
// handed a store holding more than one org's rows would feed another tenant's
// incidents into this org's analysis.
func TestRecentIncidents_OrgScoped(t *testing.T) {
	now := time.Now().UTC()
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "default-1", Service: "api", CreatedAt: now.Add(-time.Minute)},
		&storage.IncidentRecord{ID: "acme-1", OrgID: "acme", Service: "api", CreatedAt: now.Add(-time.Minute)},
		&storage.IncidentRecord{ID: "globex-1", OrgID: "globex", Service: "api", CreatedAt: now.Add(-time.Minute)},
	)

	tests := []struct {
		name  string
		orgID string
		want  string
	}{
		{name: "unset org reads the default tenant only", orgID: "", want: "default-1"},
		{name: "explicit default org", orgID: storage.DefaultOrgID, want: "default-1"},
		{name: "explicit other org", orgID: "acme", want: "acme-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := RecentIncidents{Store: store, OrgID: tc.orgID}
			res, err := tool.Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{}))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			incidents := res.Data["incidents"].([]recentIncidentItem)
			if len(incidents) != 1 || incidents[0].ID != tc.want {
				t.Fatalf("incidents = %+v, want only %q", incidents, tc.want)
			}
		})
	}
}

// TestRecentIncidents_UnknownOrgReadsNothing proves the scoping is a filter, not
// a hint: an org with no rows gets an empty result rather than the whole store.
func TestRecentIncidents_UnknownOrgReadsNothing(t *testing.T) {
	now := time.Now().UTC()
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "default-1", Service: "api", CreatedAt: now.Add(-time.Minute)},
		&storage.IncidentRecord{ID: "acme-1", OrgID: "acme", Service: "api", CreatedAt: now.Add(-time.Minute)},
	)
	tool := RecentIncidents{Store: store, OrgID: "nobody"}

	res, err := tool.Invoke(context.Background(), mustArgs(t, recentIncidentsArgs{}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := res.Data["count"]; got != 0 {
		t.Fatalf("count = %v, want 0 for an org with no incidents", got)
	}
}

// TestDescribeDependencies_OrgScoped proves the has_recent_incident annotation
// is org-scoped too: another tenant's incident must not flag this tenant's
// neighbour as firing.
func TestDescribeDependencies_OrgScoped(t *testing.T) {
	now := time.Now().UTC()
	graph := NewDependencyGraph(map[string][]string{"web": {"api"}})
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "acme-api", OrgID: "acme", Service: "api", CreatedAt: now.Add(-time.Minute)},
	)

	firing := func(t *testing.T, orgID string) bool {
		t.Helper()
		tool := DescribeDependencies{Graph: graph, Store: store, OrgID: orgID}
		res, err := tool.Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{Service: "web"}))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		upstream := res.Data["upstream"].([]dependencyNeighbour)
		if len(upstream) != 1 || upstream[0].Service != "api" {
			t.Fatalf("upstream = %+v, want the single api neighbour", upstream)
		}
		return upstream[0].HasRecentIncident
	}

	if firing(t, "") {
		t.Error("the default org saw acme's incident flag its neighbour as firing")
	}
	if !firing(t, "acme") {
		t.Error("acme did not see its own incident flag its neighbour as firing")
	}
}
