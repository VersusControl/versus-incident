package agent

import (
	"testing"

	versustools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/versus"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestBuildAnalyzeToolsPreservesBaseOrder(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	tools := buildAnalyzeTools(store, tenancy.DefaultOrgScope(), newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil)
	want := []string{"recent_incidents", "pattern_history", "describe_service"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	for index, name := range want {
		if tools[index].Name() != name {
			t.Fatalf("tool[%d] = %q, want %q", index, tools[index].Name(), name)
		}
	}
}

func TestBuildAnalyzeToolsThreadsOrgScope(t *testing.T) {
	catalog, store := newBuildCatalog(t)
	scope := tenancy.NewOrgScope("licensed", "default")
	tools := buildAnalyzeTools(store, scope, newCatalogAdapter(catalog), nil, nil, nil, nil, nil, nil, nil, nil, nil)
	recent, ok := tools[0].(versustools.RecentIncidents)
	if !ok {
		t.Fatalf("tool[0] = %T, want RecentIncidents", tools[0])
	}
	if got := recent.Scope.OrgIDs(); len(got) != 2 || got[0] != "licensed" || got[1] != "default" {
		t.Fatalf("recent scope = %v, want [licensed default]", got)
	}
}
