package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type SearchIncidents struct {
	Store    storage.Provider
	Scope    tenancy.OrgScope
	Redactor LineRedactor
}

func (SearchIncidents) Name() string        { return "search_incidents" }
func (SearchIncidents) DisplayName() string { return "Searching incident history" }
func (SearchIncidents) Description() string {
	return "Search scoped incident history using the backend full-text capability. Use list_services first for service-name discovery. Returns available:false when search is unsupported and never substitutes an unbounded substring scan."
}
func (SearchIncidents) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"query":  map[string]any{"type": "string", "description": "Full-text query. Empty returns the newest scoped incidents."},
		"limit":  map[string]any{"type": "integer", "description": "Default 20, max 100."},
		"offset": map[string]any{"type": "integer", "description": "Matches to skip. Default 0, max 500."},
	}}
}

type searchIncidentsArgs struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}
type incidentSearchItem struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Service   string `json:"service,omitempty"`
	Source    string `json:"source,omitempty"`
	Resolved  bool   `json:"resolved"`
	CreatedAt string `json:"created_at"`
}

func (tool SearchIncidents) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if tool.Store == nil {
		return core.UnavailableToolResult(tool.Name(), "incident storage not configured"), nil
	}
	var parsed searchIncidentsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	limit := clampLimit(parsed.Limit)
	offset, err := validateOffset(parsed.Offset)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	scope := tool.Scope.Normalized()
	var records []*storage.IncidentRecord
	total := 0
	exact := false
	truncated := false
	if pager, ok := tool.Store.(storage.ScopedIncidentSearchPager); ok {
		records, err = pager.SearchIncidentsPageForScope(scope, parsed.Query, "", offset, limit)
		if err == nil {
			var counts storage.IncidentStatusCounts
			counts, err = pager.CountIncidentsMatchingByStatusForScope(scope, parsed.Query)
			total = counts.Total.Total
			exact = true
			truncated = offset+len(records) < total
		}
	} else if searcher, ok := tool.Store.(storage.ScopedSearcher); ok {
		records, err = searcher.SearchIncidentsForScope(scope, parsed.Query, offset+limit+1)
		truncated = len(records) > offset+limit
		if offset >= len(records) {
			records = nil
		} else {
			records = records[offset:]
			if len(records) > limit {
				records = records[:limit]
			}
		}
	} else {
		return core.UnavailableToolResult(tool.Name(), "incident search not supported by storage backend"), nil
	}
	if err != nil {
		return nil, fmt.Errorf("search_incidents: search: %w", err)
	}
	items := make([]incidentSearchItem, 0, len(records))
	for _, record := range records {
		items = append(items, incidentSearchItem{ID: record.ID, Title: scrubModelText(record.Title, tool.Redactor), Service: record.ServiceLabel(), Source: record.Source, Resolved: record.Resolved, CreatedAt: record.CreatedAt.Format(timeFormat)})
	}
	data := map[string]any{"count": len(items), "limit": limit, "offset": offset, "truncated": truncated, "has_more": truncated, "incidents": items, "total_exact": exact}
	if exact {
		data["total"] = total
	}
	return &core.ToolResult{Tool: tool.Name(), Found: len(items) > 0, Data: data}, nil
}

const timeFormat = "2006-01-02T15:04:05Z07:00"
