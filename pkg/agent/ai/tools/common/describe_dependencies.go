package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
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
	// Scope is the ordered organization read scope. Its zero value is the
	// default-only OSS view.
	Scope tenancy.OrgScope
}

// Name implements core.AnalyzeTool.
func (DescribeDependencies) Name() string        { return "describe_dependencies" }
func (DescribeDependencies) DisplayName() string { return "Checking dependencies" }

// Description implements core.AnalyzeTool.
func (DescribeDependencies) Description() string {
	return "Look up a service's upstream (depends_on) and downstream (depended_on_by) neighbours from the dependency graph, each flagged with whether it also has a recent incident. Useful for reasoning about cascading failures."
}

// ArgsSchema implements core.AnalyzeTool.
func (DescribeDependencies) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": aitools.AddTimeRangeProperties(map[string]any{
			"service": map[string]any{
				"type":        "string",
				"description": "Required. The service to look up in the dependency graph.",
			},
		}, 60, 1440),
		"required": []string{"service"},
	}
}

type describeDependenciesArgs struct {
	aitools.TimeRangeArgs
	Service string `json:"service"`
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
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	if a.Service == "" {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "service is required", nil)
	}
	timeRange, err := aitools.ResolveTimeRange(a.TimeRangeArgs, time.Now(), 60, 1440)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "time range is invalid", err)
	}
	if d.Graph == nil {
		return core.UnavailableToolResult(DescribeDependencies{}.Name(), "dependency graph not configured"), nil
	}

	res := &core.ToolResult{
		Tool: DescribeDependencies{}.Name(),
		Data: map[string]any{
			"service":            a.Service,
			"time_range_minutes": timeRange.Minutes,
			"start":              timeRange.Start.Format(time.RFC3339),
			"end":                timeRange.End.Format(time.RFC3339),
		},
	}

	if !d.Graph.known[a.Service] {
		// Unknown to the graph: a clean miss, not an error, so the model
		// can move on without burning a retry.
		res.Found = false
		res.Data["upstream"] = []dependencyNeighbour{}
		res.Data["downstream"] = []dependencyNeighbour{}
		return res, nil
	}

	firing := d.servicesWithRecentIncident(timeRange)
	res.Found = true
	res.Data["upstream"] = annotate(d.Graph.upstream[a.Service], firing)
	res.Data["downstream"] = annotate(d.Graph.downstream[a.Service], firing)
	return res, nil
}

// servicesWithRecentIncident returns the set of service names that have
// at least one incident newer than the cutoff. A nil store yields a nil
// set (annotations default to false).
func (d DescribeDependencies) servicesWithRecentIncident(timeRange aitools.TimeRange) map[string]bool {
	if d.Store == nil {
		return nil
	}
	all, err := incidentsForScope(d.Store, d.Scope)
	if err != nil {
		// Annotation is best-effort; a store error degrades to no flags
		// rather than failing the whole lookup.
		return nil
	}
	set := make(map[string]bool)
	for _, rec := range all {
		service := rec.ServiceLabel()
		if service == "" || rec.CreatedAt.Before(timeRange.Start) || rec.CreatedAt.After(timeRange.End) {
			continue
		}
		set[strings.ToLower(service)] = true
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
