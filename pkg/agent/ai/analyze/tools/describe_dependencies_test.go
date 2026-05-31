package tools

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestDescribeDependencies_Metadata(t *testing.T) {
	tool := DescribeDependencies{}
	if got := tool.Name(); got != "describe_dependencies" {
		t.Errorf("Name() = %q, want describe_dependencies", got)
	}
	if tool.Description() == "" {
		t.Error("Description() is empty")
	}
	schema := tool.ArgsSchema()
	req, ok := schema["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "service" {
		t.Errorf("ArgsSchema required = %v, want [service]", schema["required"])
	}
}

func TestNewDependencyGraph_DerivesReverseEdges(t *testing.T) {
	g := NewDependencyGraph(map[string][]string{
		"web":    {"api"},
		"api":    {"database", "cache", "database"}, // duplicate dropped
		"worker": {"database", "worker"},            // self-edge dropped
		"":       {"ignored"},                       // empty key dropped
	})

	if g.Len() != 5 {
		t.Errorf("Len() = %d, want 5 (web, api, worker, database, cache)", g.Len())
	}
	// Upstream edges sorted and de-duplicated.
	if got := g.upstream["api"]; len(got) != 2 || got[0] != "cache" || got[1] != "database" {
		t.Errorf("upstream[api] = %v, want [cache database]", got)
	}
	// Self-edge for worker dropped.
	if got := g.upstream["worker"]; len(got) != 1 || got[0] != "database" {
		t.Errorf("upstream[worker] = %v, want [database]", got)
	}
	// Reverse edges derived: database is depended on by api + worker.
	if got := g.downstream["database"]; len(got) != 2 || got[0] != "api" || got[1] != "worker" {
		t.Errorf("downstream[database] = %v, want [api worker]", got)
	}
	if got := g.downstream["api"]; len(got) != 1 || got[0] != "web" {
		t.Errorf("downstream[api] = %v, want [web]", got)
	}
}

func TestDescribeDependencies_MissingService(t *testing.T) {
	tool := DescribeDependencies{Graph: NewDependencyGraph(map[string][]string{"web": {"api"}})}
	if _, err := tool.Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{})); err == nil {
		t.Fatal("expected error when service is empty")
	}
}

func TestDescribeDependencies_BadArgs(t *testing.T) {
	tool := DescribeDependencies{Graph: NewDependencyGraph(nil)}
	if _, err := tool.Invoke(context.Background(), []byte("{bad")); err == nil {
		t.Fatal("expected error on malformed args")
	}
}

func TestDescribeDependencies_UnknownService(t *testing.T) {
	tool := DescribeDependencies{Graph: NewDependencyGraph(map[string][]string{"web": {"api"}})}
	res, err := tool.Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{Service: "ghost"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Found {
		t.Error("Found = true, want false for service unknown to graph")
	}
	if up := res.Data["upstream"].([]dependencyNeighbour); len(up) != 0 {
		t.Errorf("upstream = %v, want empty", up)
	}
}

func TestDescribeDependencies_NilGraph(t *testing.T) {
	tool := DescribeDependencies{}
	res, err := tool.Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{Service: "api"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Found {
		t.Error("Found = true, want false when graph is nil")
	}
}

func TestDescribeDependencies_TraversalAndAnnotation(t *testing.T) {
	now := time.Now().UTC()
	// api depends on database + cache; web depends on api.
	graph := NewDependencyGraph(map[string][]string{
		"web": {"api"},
		"api": {"database", "cache"},
	})
	// database has a recent incident; web (downstream) does too; cache does not.
	store := newStoreWithIncidents(t,
		&storage.IncidentRecord{ID: "i1", Service: "DataBase", CreatedAt: now.Add(-5 * time.Minute)},
		&storage.IncidentRecord{ID: "i2", Service: "web", CreatedAt: now.Add(-10 * time.Minute)},
		&storage.IncidentRecord{ID: "i3", Service: "cache", CreatedAt: now.Add(-3 * time.Hour)}, // outside window
	)
	tool := DescribeDependencies{Graph: graph, Store: store}

	res, err := tool.Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{Service: "api"}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Found {
		t.Fatal("Found = false, want true for known service")
	}

	up := res.Data["upstream"].([]dependencyNeighbour)
	// Sorted: cache, database.
	if len(up) != 2 || up[0].Service != "cache" || up[1].Service != "database" {
		t.Fatalf("upstream = %v, want [cache database]", up)
	}
	if up[0].HasRecentIncident {
		t.Error("cache flagged with recent incident, want false (incident is outside window)")
	}
	if !up[1].HasRecentIncident {
		t.Error("database not flagged, want true (case-insensitive match on 'DataBase')")
	}

	down := res.Data["downstream"].([]dependencyNeighbour)
	if len(down) != 1 || down[0].Service != "web" {
		t.Fatalf("downstream = %v, want [web]", down)
	}
	if !down[0].HasRecentIncident {
		t.Error("web not flagged, want true")
	}
}

func TestDescribeDependencies_WindowClampAndNilStore(t *testing.T) {
	graph := NewDependencyGraph(map[string][]string{"api": {"database"}})
	tool := DescribeDependencies{Graph: graph} // nil store

	res, err := tool.Invoke(context.Background(), mustArgs(t, describeDependenciesArgs{Service: "api", WindowMinutes: 99999}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Data["window_minutes"] != 1440 {
		t.Errorf("window_minutes = %v, want 1440 (clamped)", res.Data["window_minutes"])
	}
	up := res.Data["upstream"].([]dependencyNeighbour)
	if len(up) != 1 || up[0].HasRecentIncident {
		t.Errorf("upstream = %v, want database with no annotation (nil store)", up)
	}
}
