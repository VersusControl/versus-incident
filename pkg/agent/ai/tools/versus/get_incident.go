package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const describeIncidentAnalysisLimit = 3

// GetIncident returns bounded lifecycle and assignment metadata for one incident.
type GetIncident struct {
	Store    storage.Provider
	Scope    tenancy.OrgScope
	Redactor LineRedactor
}

func (GetIncident) Name() string        { return "get_incident" }
func (GetIncident) DisplayName() string { return "Inspecting incident" }
func (GetIncident) Description() string {
	return "Describe one scoped incident's status, lifecycle timestamps, assignment IDs, source, origin, and up to three linked analysis summaries. Raw payloads are never returned, and missing resolver identity is reported as unknown."
}
func (GetIncident) ArgsSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"incident_id"},
		"properties": map[string]any{
			"incident_id": map[string]any{"type": "string", "description": "Exact incident id."},
		},
	}
}

type describeIncidentArgs struct {
	IncidentID string `json:"incident_id"`
}

func (tool GetIncident) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if tool.Store == nil {
		return core.UnavailableToolResult(tool.Name(), "incident storage not configured"), nil
	}
	var parsed describeIncidentArgs
	if err := json.Unmarshal(args, &parsed); err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
	}
	parsed.IncidentID = strings.TrimSpace(parsed.IncidentID)
	if parsed.IncidentID == "" {
		err := errors.New("incident_id is required")
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	record, err := tool.Store.GetIncident(parsed.IncidentID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return &core.ToolResult{Tool: tool.Name(), Found: false, Data: map[string]any{"incident_id": parsed.IncidentID}}, nil
		}
		return nil, fmt.Errorf("get_incident: get: %w", err)
	}
	if !scopeContainsOrg(tool.Scope, record.OrgID) {
		return &core.ToolResult{Tool: tool.Name(), Found: false, Data: map[string]any{"incident_id": parsed.IncidentID}}, nil
	}

	analyses, analysesAvailable, analysesTruncated, err := incidentAnalysisSummaries(tool.Store, tool.Scope, parsed.IncidentID, tool.Redactor)
	if err != nil {
		return nil, fmt.Errorf("get_incident: analyses: %w", err)
	}
	status := "open"
	if record.Resolved {
		status = "resolved"
	} else if record.AckedAt != nil {
		status = "acknowledged"
	}
	data := map[string]any{
		"incident_id":            record.ID,
		"title":                  scrubModelText(record.Title, tool.Redactor),
		"service":                record.ServiceLabel(),
		"status":                 status,
		"resolved":               record.Resolved,
		"created_at":             record.CreatedAt,
		"source":                 record.Source,
		"origin":                 record.EffectiveOrigin(),
		"assigned_team_id":       record.AssignedTeamID,
		"assigned_member_ids":    append([]string(nil), record.AssignedMemberIDs...),
		"resolved_by_known":      false,
		"resolved_by_reason":     "resolver actor is not persisted on incident records",
		"analyses_available":     analysesAvailable,
		"analyses":               analyses,
		"analyses_truncated":     analysesTruncated,
		"analysis_summary_limit": describeIncidentAnalysisLimit,
	}
	if record.AckedAt != nil {
		data["acked_at"] = record.AckedAt
	}
	if record.ResolvedAt != nil {
		data["resolved_at"] = record.ResolvedAt
	}
	return &core.ToolResult{Tool: tool.Name(), Found: true, Data: data}, nil
}

func scopeContainsOrg(scope tenancy.OrgScope, orgID string) bool {
	orgID = storage.NormalizeOrgID(orgID)
	for _, allowed := range scope.Normalized().OrgIDs() {
		if allowed == orgID {
			return true
		}
	}
	return false
}

func incidentAnalysisSummaries(store storage.Provider, scope tenancy.OrgScope, incidentID string, redactor LineRedactor) ([]analysisItem, bool, bool, error) {
	pager, ok := store.(storage.ScopedAnalysisSearchPager)
	var records []*storage.AnalysisRecord
	truncated := false
	if ok {
		var total int
		var err error
		records, total, err = pager.SearchAnalysesPageForScope(scope.Normalized(), storage.AnalysisSearchOptions{IncidentID: incidentID, Limit: describeIncidentAnalysisLimit})
		if err != nil {
			return nil, true, false, err
		}
		truncated = total > len(records)
	} else {
		page, err := store.ListAnalysesByIncident(incidentID, discoveryFallbackLimit+1)
		if err != nil {
			return nil, true, false, err
		}
		truncated = len(page) > discoveryFallbackLimit
		for _, record := range page[:min(len(page), discoveryFallbackLimit)] {
			if !scopeContainsOrg(scope, record.OrgID) {
				continue
			}
			if len(records) == describeIncidentAnalysisLimit {
				truncated = true
				continue
			}
			records = append(records, record)
		}
	}
	items := make([]analysisItem, 0, len(records))
	for _, record := range records {
		items = append(items, analysisItem{ID: record.ID, IncidentID: record.IncidentID, RequestedAt: record.RequestedAt.Format(timeFormat), Model: record.Model, Status: record.Status, Finding: compactFinding(record.Finding, redactor)})
	}
	return items, true, truncated, nil
}
