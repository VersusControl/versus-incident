// Package tools holds the read-only tool catalog exposed to the
// analyze-kind AI agent. Every tool in this package MUST be safely
// read-only: no Save*, Update*, Delete*, http POST/PUT, on-call
// trigger, or notification dispatch. The import-graph guard test in
// pkg/agent/ai/analyze enforces that this package does not transitively
// depend on services.CreateIncident or any provider in pkg/common.
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

// PatternCatalog is the read-only slice of *agent.Catalog that the
// analyze tools depend on. Declaring it as a local interface (instead
// of importing pkg/agent) keeps the import graph one-directional:
// pkg/agent imports this package via the factory, not the reverse.
type PatternCatalog interface {
	Get(id string) *PatternView
	All() []*PatternView
	AllServices() map[string]ServiceInfo
}

// PatternView mirrors agent.Pattern with the fields the analyze tools
// surface. agent.Catalog implements the interface via a thin adapter
// in the factory (see pkg/agent/ai/analyze/tools/adapter.go on the
// agent side — implemented as a private wrapper).
type PatternView struct {
	ID        string
	Template  string
	Source    string
	Service   string
	RuleName  string
	Verdict   string
	Tags      []string
	Count     int
	Baseline  float64
	FirstSeen time.Time
	LastSeen  time.Time
}

// ServiceInfo mirrors agent.ServiceInfo for the tools layer.
type ServiceInfo struct {
	FirstSeen time.Time
}

// RecentIncidents lists incidents from storage within a time window,
// optionally filtered by service. The agent uses it to spot bursts /
// recurring incidents on the same service.
type RecentIncidents struct {
	Store storage.Provider
}

// Name implements core.AnalyzeTool.
func (RecentIncidents) Name() string { return "recent_incidents" }

// Description implements core.AnalyzeTool.
func (RecentIncidents) Description() string {
	return "List incidents from the local store within the last N minutes, optionally filtered by service. Returns id, title, service, severity, resolved, created_at."
}

// ArgsSchema implements core.AnalyzeTool.
func (RecentIncidents) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"window_minutes": map[string]any{
				"type":        "integer",
				"description": "Look back this many minutes from now. Default 60, max 1440.",
			},
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name to filter by (exact match).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Cap the number of incidents returned. Default 20, max 100.",
			},
		},
	}
}

type recentIncidentsArgs struct {
	WindowMinutes int    `json:"window_minutes"`
	Service       string `json:"service"`
	Limit         int    `json:"limit"`
}

type recentIncidentItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title,omitempty"`
	Service   string    `json:"service,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Resolved  bool      `json:"resolved"`
	CreatedAt time.Time `json:"created_at"`
}

// Invoke implements core.AnalyzeTool.
func (r RecentIncidents) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if r.Store == nil {
		return nil, fmt.Errorf("recent_incidents: storage not configured")
	}
	var a recentIncidentsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("recent_incidents: parse args: %w", err)
		}
	}
	if a.WindowMinutes <= 0 {
		a.WindowMinutes = 60
	}
	if a.WindowMinutes > 1440 {
		a.WindowMinutes = 1440
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 100 {
		a.Limit = 100
	}

	all, err := r.Store.ListIncidents(0)
	if err != nil {
		return nil, fmt.Errorf("recent_incidents: list: %w", err)
	}
	cutoff := time.Now().UTC().Add(-time.Duration(a.WindowMinutes) * time.Minute)
	out := make([]recentIncidentItem, 0)
	for _, rec := range all {
		if rec.CreatedAt.Before(cutoff) {
			continue
		}
		if a.Service != "" && !strings.EqualFold(rec.Service, a.Service) {
			continue
		}
		out = append(out, recentIncidentItem{
			ID:        rec.ID,
			Title:     rec.Title,
			Service:   rec.Service,
			Resolved:  rec.Resolved,
			CreatedAt: rec.CreatedAt,
		})
		if len(out) >= a.Limit {
			break
		}
	}
	return &core.ToolResult{
		Tool:  RecentIncidents{}.Name(),
		Found: true,
		Data: map[string]any{
			"count":          len(out),
			"window_minutes": a.WindowMinutes,
			"service":        a.Service,
			"incidents":      out,
		},
	}, nil
}

// PatternHistory looks up the agent catalog by pattern id and returns
// the curated metadata (template, counts, baseline, verdict, tags).
type PatternHistory struct {
	Catalog PatternCatalog
}

// Name implements core.AnalyzeTool.
func (PatternHistory) Name() string { return "pattern_history" }

// Description implements core.AnalyzeTool.
func (PatternHistory) Description() string {
	return "Look up a learned log-pattern by id and return its template, observed counts, EWMA baseline, service, verdict, and operator tags."
}

// ArgsSchema implements core.AnalyzeTool.
func (PatternHistory) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern_id": map[string]any{
				"type":        "string",
				"description": "Required. The pattern id from a prior incident or finding.",
			},
		},
		"required": []string{"pattern_id"},
	}
}

type patternHistoryArgs struct {
	PatternID string `json:"pattern_id"`
}

// Invoke implements core.AnalyzeTool.
func (p PatternHistory) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if p.Catalog == nil {
		return nil, fmt.Errorf("pattern_history: catalog not configured")
	}
	var a patternHistoryArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("pattern_history: parse args: %w", err)
		}
	}
	if a.PatternID == "" {
		return nil, fmt.Errorf("pattern_history: pattern_id is required")
	}
	pat := p.Catalog.Get(a.PatternID)
	if pat == nil {
		return &core.ToolResult{
			Tool:  PatternHistory{}.Name(),
			Found: false,
			Data:  map[string]any{"pattern_id": a.PatternID},
		}, nil
	}
	return &core.ToolResult{
		Tool:  PatternHistory{}.Name(),
		Found: true,
		Data: map[string]any{
			"pattern_id": pat.ID,
			"template":   pat.Template,
			"source":     pat.Source,
			"service":    pat.Service,
			"rule_name":  pat.RuleName,
			"verdict":    pat.Verdict,
			"tags":       pat.Tags,
			"count":      pat.Count,
			"baseline":   pat.Baseline,
			"first_seen": pat.FirstSeen,
			"last_seen":  pat.LastSeen,
		},
	}, nil
}

// DescribeService returns the catalog-known summary for a service: how
// long it has been observed and how many patterns are attributed to it.
type DescribeService struct {
	Catalog PatternCatalog
}

// Name implements core.AnalyzeTool.
func (DescribeService) Name() string { return "describe_service" }

// Description implements core.AnalyzeTool.
func (DescribeService) Description() string {
	return "Summarize what the agent knows about a service: first-seen timestamp and the top learned patterns attributed to it."
}

// ArgsSchema implements core.AnalyzeTool.
func (DescribeService) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"service": map[string]any{
				"type":        "string",
				"description": "Required. The service name to look up.",
			},
			"top_patterns": map[string]any{
				"type":        "integer",
				"description": "How many top patterns to include. Default 5, max 20.",
			},
		},
		"required": []string{"service"},
	}
}

type describeServiceArgs struct {
	Service     string `json:"service"`
	TopPatterns int    `json:"top_patterns"`
}

type describePatternEntry struct {
	ID       string  `json:"id"`
	Template string  `json:"template"`
	Count    int     `json:"count"`
	Baseline float64 `json:"baseline"`
	Verdict  string  `json:"verdict,omitempty"`
}

// Invoke implements core.AnalyzeTool.
func (d DescribeService) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if d.Catalog == nil {
		return nil, fmt.Errorf("describe_service: catalog not configured")
	}
	var a describeServiceArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("describe_service: parse args: %w", err)
		}
	}
	if a.Service == "" {
		return nil, fmt.Errorf("describe_service: service is required")
	}
	if a.TopPatterns <= 0 {
		a.TopPatterns = 5
	}
	if a.TopPatterns > 20 {
		a.TopPatterns = 20
	}

	res := &core.ToolResult{
		Tool: DescribeService{}.Name(),
		Data: map[string]any{"service": a.Service},
	}
	services := d.Catalog.AllServices()
	if info, ok := services[a.Service]; ok {
		res.Found = true
		res.Data["first_seen"] = info.FirstSeen
	}

	matches := make([]describePatternEntry, 0)
	for _, p := range d.Catalog.All() {
		if !strings.EqualFold(p.Service, a.Service) {
			continue
		}
		matches = append(matches, describePatternEntry{
			ID:       p.ID,
			Template: p.Template,
			Count:    p.Count,
			Baseline: p.Baseline,
			Verdict:  p.Verdict,
		})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Count > matches[j].Count
	})
	if len(matches) > a.TopPatterns {
		matches = matches[:a.TopPatterns]
	}
	res.Data["pattern_count"] = len(matches)
	res.Data["top_patterns"] = matches
	return res, nil
}

// Default returns the production tool set wired to the given storage
// and catalog. Callers can also assemble a custom set by constructing
// the individual tool structs.
func Default(store storage.Provider, cat PatternCatalog) []core.AnalyzeTool {
	out := make([]core.AnalyzeTool, 0, 3)
	if store != nil {
		out = append(out, RecentIncidents{Store: store})
	}
	if cat != nil {
		out = append(out, PatternHistory{Catalog: cat})
		out = append(out, DescribeService{Catalog: cat})
	}
	return out
}
