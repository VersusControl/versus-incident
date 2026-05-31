package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

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
