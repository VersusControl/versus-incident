package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// RecentIncidents lists incidents from storage within a time window,
// optionally filtered by service. The agent uses it to spot bursts /
// recurring incidents on the same service.
type RecentIncidents struct {
	Store storage.Provider
	// Scope is the ordered organization read scope. Its zero value is the
	// default-only OSS view.
	Scope tenancy.OrgScope
}

// Name implements core.AnalyzeTool.
func (RecentIncidents) Name() string        { return "recent_incidents" }
func (RecentIncidents) DisplayName() string { return "Reviewing recent incidents" }

// Description implements core.AnalyzeTool.
func (RecentIncidents) Description() string {
	return "List incidents from the local store within an absolute or relative time range, optionally filtered by service. Returns id, title, service, severity, resolved, created_at."
}

// ArgsSchema implements core.AnalyzeTool.
func (RecentIncidents) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": aitools.AddTimeRangeProperties(map[string]any{
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name to filter by (exact match).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Cap the number of incidents returned. Default 20, max 100.",
			},
		}, 60, 1440),
	}
}

type recentIncidentsArgs struct {
	aitools.TimeRangeArgs
	Service string `json:"service"`
	Limit   int    `json:"limit"`
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
		return core.UnavailableToolResult(RecentIncidents{}.Name(), "incident storage not configured"), nil
	}
	var a recentIncidentsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("recent_incidents: parse args: %w", err)
		}
	}
	timeRange, err := aitools.ResolveTimeRange(a.TimeRangeArgs, time.Now(), 60, 1440)
	if err != nil {
		return nil, fmt.Errorf("recent_incidents: %w", err)
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 100 {
		a.Limit = 100
	}

	all, err := incidentsForScope(r.Store, r.Scope)
	if err != nil {
		return nil, fmt.Errorf("recent_incidents: list: %w", err)
	}
	out := make([]recentIncidentItem, 0, a.Limit)
	incidentsTotal := 0
	for _, rec := range all {
		if rec.CreatedAt.Before(timeRange.Start) || rec.CreatedAt.After(timeRange.End) {
			continue
		}
		service := rec.ServiceLabel()
		if a.Service != "" && !strings.EqualFold(service, a.Service) {
			continue
		}
		incidentsTotal++
		if len(out) >= a.Limit {
			continue
		}
		out = append(out, recentIncidentItem{
			ID:        rec.ID,
			Title:     rec.Title,
			Service:   service,
			Resolved:  rec.Resolved,
			CreatedAt: rec.CreatedAt,
		})
	}
	return &core.ToolResult{
		Tool:  RecentIncidents{}.Name(),
		Found: len(out) > 0,
		Data: map[string]any{
			"count":              len(out),
			"time_range_minutes": timeRange.Minutes,
			"start":              timeRange.Start.Format(time.RFC3339),
			"end":                timeRange.End.Format(time.RFC3339),
			"incidents_total":    incidentsTotal,
			"truncated":          incidentsTotal > len(out),
			"service":            a.Service,
			"incidents":          out,
		},
	}, nil
}
