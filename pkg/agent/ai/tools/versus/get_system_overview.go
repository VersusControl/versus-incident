package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// GetSystemOverview summarizes the local evidence available to the agent.
type GetSystemOverview struct {
	Store   storage.Provider
	Scope   tenancy.OrgScope
	Catalog PagedPatternCatalog
	Health  DetectionHealthReader
}

func (GetSystemOverview) Name() string        { return "get_system_overview" }
func (GetSystemOverview) DisplayName() string { return "Inspecting system overview" }
func (GetSystemOverview) Description() string {
	return "Start discovery here to answer what Versus can see. Returns exact scoped incident status counts for the requested time range when supported, known and recently active services, learned-pattern total, and passive detection-source health. Never treats a dark category as healthy."
}
func (GetSystemOverview) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "properties": aitools.AddTimeRangeProperties(map[string]any{}, 1440, 10080)}
}

type overviewArgs struct{ aitools.TimeRangeArgs }

func (tool GetSystemOverview) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if tool.Store == nil && tool.Catalog == nil && tool.Health == nil {
		return core.UnavailableToolResult(tool.Name(), "incident storage, Versus catalog, and detection health are not configured"), nil
	}
	var parsed overviewArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	timeRange, err := aitools.ResolveTimeRange(parsed.TimeRangeArgs, time.Now(), 1440, 10080)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "time range is invalid", err)
	}
	scope := tool.Scope.Normalized()
	var counts storage.IncidentStatusCounts
	countsExact := false
	var windowRecords []*storage.IncidentRecord
	windowTruncated := false
	windowReadAttempted := false
	if counter, ok := tool.Store.(storage.ScopedIncidentWindowCounter); ok && parsed.End == "" {
		counts, err = counter.CountIncidentsByStatusSinceForScope(scope, timeRange.Start)
		countsExact = true
	} else if lister, ok := tool.Store.(storage.ScopedRangeLister); ok {
		windowReadAttempted = true
		windowRecords, err = lister.ListIncidentsInRangeForScope(scope, timeRange.Start, timeRange.End, discoveryFallbackLimit+1)
		if err == nil {
			windowTruncated = len(windowRecords) > discoveryFallbackLimit
			if windowTruncated {
				windowRecords = windowRecords[:discoveryFallbackLimit]
			} else {
				counts = storage.StatusCountsOf(windowRecords)
				countsExact = true
			}
		}
	} else if tool.Store != nil {
		windowReadAttempted = true
		windowRecords, windowTruncated, err = boundedIncidentsForScope(tool.Store, scope, discoveryFallbackLimit)
		if err == nil {
			windowed := make([]*storage.IncidentRecord, 0, len(windowRecords))
			for _, record := range windowRecords {
				if !record.CreatedAt.Before(timeRange.Start) && record.CreatedAt.Before(timeRange.End) {
					windowed = append(windowed, record)
				}
			}
			windowRecords = windowed
			counts = storage.StatusCountsOf(windowed)
			countsExact = !windowTruncated
		}
	}
	if err != nil {
		return nil, fmt.Errorf("get_system_overview: count incidents: %w", err)
	}
	var services []ServiceRow
	servicesTotal := 0
	patternsTotal := 0
	if tool.Catalog != nil {
		services, servicesTotal, err = tool.Catalog.ServicesPage(CatalogPageOptions{Limit: discoveryMaxLimit})
		if err != nil {
			return nil, fmt.Errorf("get_system_overview: list services: %w", err)
		}
		_, patternsTotal, err = tool.Catalog.PatternsPage(CatalogPageOptions{Limit: 1})
		if err != nil {
			return nil, fmt.Errorf("get_system_overview: count patterns: %w", err)
		}
	}
	activeSet := map[string]struct{}{}
	if tool.Store != nil && !windowReadAttempted {
		windowReadAttempted = true
		if lister, ok := tool.Store.(storage.ScopedRangeLister); ok {
			windowRecords, err = lister.ListIncidentsInRangeForScope(scope, timeRange.Start, timeRange.End, discoveryFallbackLimit+1)
			if err == nil {
				windowTruncated = len(windowRecords) > discoveryFallbackLimit
				if windowTruncated {
					windowRecords = windowRecords[:discoveryFallbackLimit]
				}
			}
		} else {
			windowRecords, windowTruncated, err = boundedIncidentsForScope(tool.Store, scope, discoveryFallbackLimit)
		}
		if err != nil {
			return nil, fmt.Errorf("get_system_overview: list active services: %w", err)
		}
	}
	for _, record := range windowRecords {
		if service := record.ServiceLabel(); service != "" && !record.CreatedAt.Before(timeRange.Start) && record.CreatedAt.Before(timeRange.End) {
			activeSet[service] = struct{}{}
		}
	}
	active := make([]string, 0, len(activeSet))
	for service := range activeSet {
		active = append(active, service)
	}
	sort.Strings(active)
	known := make([]string, 0, len(services))
	for _, service := range services {
		known = append(known, service.Name)
	}
	data := map[string]any{
		"start": timeRange.Start.Format(time.RFC3339), "end": timeRange.End.Format(time.RFC3339),
		"time_range_minutes": timeRange.Minutes, "incident_counts": counts,
		"incident_data_available": tool.Store != nil, "incident_counts_exact": countsExact, "known_services": known,
		"known_services_total": servicesTotal, "known_services_truncated": servicesTotal > len(known),
		"active_services": active, "active_services_count": len(active),
		"active_services_truncated": windowTruncated, "has_more": windowTruncated,
		"catalog_available": tool.Catalog != nil, "patterns_learned_total": patternsTotal,
	}
	if !windowTruncated {
		data["active_services_total"] = len(active)
	}
	healthFound := false
	if tool.Health != nil {
		snapshot := tool.Health.DetectionHealth(tool.Scope)
		data["detection_health"] = snapshot
		healthFound = len(snapshot.Sources) > 0 || len(snapshot.Categories) > 0 || snapshot.Observation != ""
	} else {
		data["detection_health"] = DetectionHealthSnapshot{Sources: []SourceHealth{}, Categories: unknownCategoryHealth(), Observation: "unknown"}
	}
	return &core.ToolResult{Tool: tool.Name(), Found: counts.Total.Total > 0 || servicesTotal > 0 || patternsTotal > 0 || healthFound, Data: data}, nil
}
