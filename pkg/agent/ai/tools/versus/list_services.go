package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type ListServices struct {
	Store   storage.Provider
	Scope   tenancy.OrgScope
	Catalog PagedPatternCatalog
}

func (ListServices) Name() string        { return "list_services" }
func (ListServices) DisplayName() string { return "Resolving known services" }
func (ListServices) Description() string {
	return "Resolve service names before service-specific queries. Returns a stable catalog page with first_seen and scoped incident counts. Search is a case-insensitive name substring; total is the exact post-search catalog total."
}
func (ListServices) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "properties": aitools.AddTimeRangeProperties(map[string]any{
		"search": map[string]any{"type": "string", "description": "Optional case-insensitive service-name substring."},
		"limit":  map[string]any{"type": "integer", "description": "Default 20, max 100."},
		"offset": map[string]any{"type": "integer", "description": "Rows to skip. Default 0, max 500."},
	}, 10080, 43200)}
}

type listServicesArgs struct {
	aitools.TimeRangeArgs
	Search string `json:"search"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}
type serviceItem struct {
	Name          string    `json:"name"`
	FirstSeen     time.Time `json:"first_seen"`
	IncidentCount int       `json:"incident_count"`
}

func (tool ListServices) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if tool.Catalog == nil {
		return core.UnavailableToolResult(tool.Name(), "Versus catalog is required"), nil
	}
	var parsed listServicesArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	rangeValue, err := aitools.ResolveTimeRange(parsed.TimeRangeArgs, time.Now(), 10080, 43200)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "time range is invalid", err)
	}
	limit := clampLimit(parsed.Limit)
	offset, err := validateOffset(parsed.Offset)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	rows, total, err := tool.Catalog.ServicesPage(CatalogPageOptions{Offset: offset, Limit: limit, Search: parsed.Search})
	if err != nil {
		return nil, fmt.Errorf("list_services: catalog page: %w", err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	counts := map[string]int{}
	countsExact := false
	if tool.Store != nil {
		if reader, ok := tool.Store.(storage.ScopedIncidentServiceRangeSummaryReader); ok {
			counts, err = reader.CountIncidentsByServicesInRangeForScope(tool.Scope.Normalized(), names, rangeValue.Start, rangeValue.End)
			countsExact = true
		} else {
			var records []*storage.IncidentRecord
			var truncated bool
			records, truncated, err = boundedIncidentsForScope(tool.Store, tool.Scope, discoveryFallbackLimit)
			if err == nil {
				requested := map[string]string{}
				for _, name := range names {
					requested[name] = name
				}
				for _, record := range records {
					if record.CreatedAt.Before(rangeValue.Start) || !record.CreatedAt.Before(rangeValue.End) {
						continue
					}
					if name, ok := requested[record.ServiceLabel()]; ok {
						counts[name]++
					}
				}
				countsExact = !truncated
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("list_services: incident counts: %w", err)
	}
	items := make([]serviceItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, serviceItem{Name: row.Name, FirstSeen: row.Info.FirstSeen, IncidentCount: counts[row.Name]})
	}
	return &core.ToolResult{Tool: tool.Name(), Found: len(items) > 0, Data: map[string]any{
		"search": parsed.Search, "count": len(items), "total": total, "limit": limit, "offset": offset,
		"truncated": offset+len(items) < total, "has_more": offset+len(items) < total, "incident_counts_available": tool.Store != nil, "incident_counts_exact": countsExact,
		"start": rangeValue.Start.Format(time.RFC3339), "end": rangeValue.End.Format(time.RFC3339), "services": items,
	}}, nil
}
