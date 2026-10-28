package tools

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestDescribeDependenciesOrgScope(t *testing.T) {
	store := newStoreWithIncidents(t, &storage.IncidentRecord{
		ID: "acme-api", OrgID: "acme", Service: "api", CreatedAt: time.Now().UTC().Add(-time.Minute),
	})
	graph := NewDependencyGraph(map[string][]string{"web": {"api"}})

	firing := func(t *testing.T, scope tenancy.OrgScope) bool {
		t.Helper()
		result, err := (DescribeDependencies{Graph: graph, Store: store, Scope: scope}).Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{Service: "web"}))
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		upstream := result.Data["upstream"].([]dependencyNeighbour)
		if len(upstream) != 1 {
			t.Fatalf("upstream = %+v, want one neighbor", upstream)
		}
		return upstream[0].HasRecentIncident
	}

	if firing(t, tenancy.OrgScope{}) {
		t.Error("default org saw acme incident")
	}
	if !firing(t, tenancy.NewOrgScope("acme")) {
		t.Error("acme did not see its incident")
	}
}

func TestDescribeDependenciesOrgScopeUnionExcludesForeign(t *testing.T) {
	now := time.Now().UTC()
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "legacy-api", Service: "api", CreatedAt: now},
		&storage.IncidentRecord{ID: "licensed-db", OrgID: "licensed", Service: "database", CreatedAt: now},
		&storage.IncidentRecord{ID: "foreign-cache", OrgID: "foreign", Service: "cache", CreatedAt: now},
	)
	graph := NewDependencyGraph(map[string][]string{"web": {"api", "database", "cache"}})
	result, err := (DescribeDependencies{
		Graph: graph,
		Store: store,
		Scope: tenancy.NewOrgScope("licensed", storage.DefaultOrgID),
	}).Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{Service: "web"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	upstream := result.Data["upstream"].([]dependencyNeighbour)
	flags := make(map[string]bool, len(upstream))
	for _, neighbour := range upstream {
		flags[neighbour.Service] = neighbour.HasRecentIncident
	}
	if !flags["api"] || !flags["database"] || flags["cache"] {
		t.Fatalf("incident flags = %v, want api+database true and foreign cache false", flags)
	}
}
