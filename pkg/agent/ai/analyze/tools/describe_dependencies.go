package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// DependencyGraph is the read-only service-dependency graph that powers
// the describe_dependencies tool. It is built from the operator-authored
// `depends_on` (upstream) edges; the reverse `depended_on_by`
// (downstream) edges are derived automatically. The graph is immutable
// after construction.
type DependencyGraph struct {
	// upstream[s] = services s depends on.
	upstream map[string][]string
	// downstream[s] = services that depend on s.
	downstream map[string][]string
	// known is the set of every service that appears as a node anywhere
	// in the graph (either as a key or as a neighbour).
	known map[string]bool
}

// NewDependencyGraph builds a DependencyGraph from per-service upstream
// edges. The map key is a service name; the value is the list of
// services it depends on. Self-edges and duplicate neighbours are
// dropped; the reverse edges are derived. A nil/empty input yields an
// empty graph (every lookup is then a miss).
func NewDependencyGraph(dependsOn map[string][]string) *DependencyGraph {
	g := &DependencyGraph{
		upstream:   make(map[string][]string),
		downstream: make(map[string][]string),
		known:      make(map[string]bool),
	}
	for svc, deps := range dependsOn {
		if svc == "" {
			continue
		}
		g.known[svc] = true
		seen := make(map[string]bool)
		for _, dep := range deps {
			if dep == "" || dep == svc || seen[dep] {
				continue
			}
			seen[dep] = true
			g.known[dep] = true
			g.upstream[svc] = append(g.upstream[svc], dep)
			g.downstream[dep] = append(g.downstream[dep], svc)
		}
	}
	for _, list := range g.upstream {
		sort.Strings(list)
	}
	for _, list := range g.downstream {
		sort.Strings(list)
	}
	return g
}

// Len reports how many service nodes the graph knows about.
func (g *DependencyGraph) Len() int {
	if g == nil {
		return 0
	}
	return len(g.known)
}

// DescribeDependencies surfaces the upstream and downstream neighbours of
// a service from the operator-authored dependency graph, each annotated
// with whether that neighbour also has a recent incident. The agent uses
// it to reason about cascading failures.
type DescribeDependencies struct {
	Graph *DependencyGraph
	// Store is optional. When nil, neighbours are returned without the
	// has_recent_incident annotation.
	Store storage.Provider
}

// Name implements core.AnalyzeTool.
func (DescribeDependencies) Name() string { return "describe_dependencies" }

// Description implements core.AnalyzeTool.
func (DescribeDependencies) Description() string {
	return "Look up a service's upstream (depends_on) and downstream (depended_on_by) neighbours from the dependency graph, each flagged with whether it also has a recent incident. Useful for reasoning about cascading failures."
}

// ArgsSchema implements core.AnalyzeTool.
func (DescribeDependencies) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"service": map[string]any{
				"type":        "string",
				"description": "Required. The service to look up in the dependency graph.",
			},
			"window_minutes": map[string]any{
				"type":        "integer",
				"description": "Look back this many minutes when flagging neighbours with recent incidents. Default 60, max 1440.",
			},
		},
		"required": []string{"service"},
	}
}

type describeDependenciesArgs struct {
	Service       string `json:"service"`
	WindowMinutes int    `json:"window_minutes"`
}

type dependencyNeighbour struct {
	Service           string `json:"service"`
	HasRecentIncident bool   `json:"has_recent_incident"`
}

// Invoke implements core.AnalyzeTool.
func (d DescribeDependencies) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	var a describeDependenciesArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("describe_dependencies: parse args: %w", err)
		}
	}
	if a.Service == "" {
		return nil, fmt.Errorf("describe_dependencies: service is required")
	}
	if a.WindowMinutes <= 0 {
		a.WindowMinutes = 60
	}
	if a.WindowMinutes > 1440 {
		a.WindowMinutes = 1440
	}

	res := &core.ToolResult{
		Tool: DescribeDependencies{}.Name(),
		Data: map[string]any{
			"service":        a.Service,
			"window_minutes": a.WindowMinutes,
		},
	}

	if d.Graph == nil || !d.Graph.known[a.Service] {
		// Unknown to the graph: a clean miss, not an error, so the model
		// can move on without burning a retry.
		res.Found = false
		res.Data["upstream"] = []dependencyNeighbour{}
		res.Data["downstream"] = []dependencyNeighbour{}
		return res, nil
	}

	firing := d.servicesWithRecentIncident(a.WindowMinutes)
	res.Found = true
	res.Data["upstream"] = annotate(d.Graph.upstream[a.Service], firing)
	res.Data["downstream"] = annotate(d.Graph.downstream[a.Service], firing)
	return res, nil
}

// servicesWithRecentIncident returns the set of service names that have
// at least one incident newer than the cutoff. A nil store yields a nil
// set (annotations default to false).
func (d DescribeDependencies) servicesWithRecentIncident(windowMinutes int) map[string]bool {
	if d.Store == nil {
		return nil
	}
	all, err := d.Store.ListIncidents(0)
	if err != nil {
		// Annotation is best-effort; a store error degrades to no flags
		// rather than failing the whole lookup.
		return nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowMinutes) * time.Minute)
	set := make(map[string]bool)
	for _, rec := range all {
		if rec.Service == "" || rec.CreatedAt.Before(cutoff) {
			continue
		}
		set[strings.ToLower(rec.Service)] = true
	}
	return set
}

func annotate(neighbours []string, firing map[string]bool) []dependencyNeighbour {
	out := make([]dependencyNeighbour, 0, len(neighbours))
	for _, n := range neighbours {
		out = append(out, dependencyNeighbour{
			Service:           n,
			HasRecentIncident: firing[strings.ToLower(n)],
		})
	}
	return out
}
