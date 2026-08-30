package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type ListAnalyses struct {
	Store    storage.Provider
	Scope    tenancy.OrgScope
	Redactor LineRedactor
}

func (ListAnalyses) Name() string        { return "list_analyses" }
func (ListAnalyses) DisplayName() string { return "Listing prior analyses" }
func (ListAnalyses) Description() string {
	return "List bounded, scoped prior analyses filtered by exact incident, exact service, or full-record query. Service filtering requires storage support because analysis records do not carry service directly; no unbounded history loads are used."
}
func (ListAnalyses) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"incident_id": map[string]any{"type": "string", "description": "Optional exact incident id."}, "service": map[string]any{"type": "string", "description": "Optional exact service name."}, "query": map[string]any{"type": "string", "description": "Optional case-insensitive analysis-record query."}, "limit": map[string]any{"type": "integer", "description": "Default 20, max 20."},
		"offset": map[string]any{"type": "integer", "description": "Matches to skip. Default 0, max 500."},
	}}
}

type analysisHistoryArgs struct {
	IncidentID string `json:"incident_id"`
	Service    string `json:"service"`
	Query      string `json:"query"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}
type analysisItem struct {
	ID          string                  `json:"id"`
	IncidentID  string                  `json:"incident_id"`
	RequestedAt string                  `json:"requested_at"`
	Model       string                  `json:"model,omitempty"`
	Status      string                  `json:"status"`
	Finding     *compactAnalysisFinding `json:"finding,omitempty"`
}

type compactAnalysisFinding struct {
	Title               string                     `json:"title,omitempty"`
	Summary             string                     `json:"summary,omitempty"`
	Severity            string                     `json:"severity,omitempty"`
	Category            string                     `json:"category,omitempty"`
	Confidence          float64                    `json:"confidence,omitempty"`
	RootCauseHypotheses []core.RootCauseHypothesis `json:"root_cause_hypotheses,omitempty"`
	Evidence            []core.EvidenceItem        `json:"evidence,omitempty"`
	RelatedPatternIDs   []string                   `json:"related_pattern_ids,omitempty"`
	NextSteps           []string                   `json:"next_steps,omitempty"`
}

func (tool ListAnalyses) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if tool.Store == nil {
		return core.UnavailableToolResult(tool.Name(), "analysis storage not configured"), nil
	}
	var parsed analysisHistoryArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	if parsed.Service != "" && strings.TrimSpace(parsed.Service) == "" {
		err := fmt.Errorf("service must not be blank")
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	limit := parsed.Limit
	if limit <= 0 || limit > discoveryDefaultLimit {
		limit = discoveryDefaultLimit
	}
	offset, err := validateOffset(parsed.Offset)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	scope := tool.Scope.Normalized()
	var records []*storage.AnalysisRecord
	total := 0
	exact := false
	truncated := false
	if pager, ok := tool.Store.(storage.ScopedAnalysisSearchPager); ok {
		records, total, err = pager.SearchAnalysesPageForScope(scope, storage.AnalysisSearchOptions{IncidentID: parsed.IncidentID, Service: parsed.Service, Query: parsed.Query, Offset: offset, Limit: limit})
		exact = true
		truncated = offset+len(records) < total
	} else {
		if parsed.Service != "" {
			return core.UnavailableToolResult(tool.Name(), "service-filtered analysis history not supported by storage backend"), nil
		}
		pager, ok := tool.Store.(storage.ScopedAnalysisPager)
		if !ok {
			return core.UnavailableToolResult(tool.Name(), "scoped analysis paging not supported by storage backend"), nil
		}
		var page []*storage.AnalysisRecord
		page, err = pager.ListAnalysesPageForScope(scope, 0, discoveryFallbackLimit+1)
		if err == nil {
			truncated = len(page) > discoveryFallbackLimit
			if truncated {
				page = page[:discoveryFallbackLimit]
			}
			matched := 0
			for _, record := range page {
				if parsed.IncidentID != "" && record.IncidentID != parsed.IncidentID {
					continue
				}
				if parsed.Query != "" {
					raw, _ := json.Marshal(record)
					if !strings.Contains(strings.ToLower(string(raw)), strings.ToLower(parsed.Query)) {
						continue
					}
				}
				total++
				if matched >= offset && len(records) < limit {
					records = append(records, record)
				}
				matched++
			}
			exact = !truncated
			truncated = truncated || offset+len(records) < total
		}
	}
	if err != nil {
		return nil, fmt.Errorf("list_analyses: search: %w", err)
	}
	items := make([]analysisItem, 0, len(records))
	for _, record := range records {
		items = append(items, analysisItem{ID: record.ID, IncidentID: record.IncidentID, RequestedAt: record.RequestedAt.Format(timeFormat), Model: record.Model, Status: record.Status, Finding: compactFinding(record.Finding, tool.Redactor)})
	}
	data := map[string]any{"incident_id": parsed.IncidentID, "service": parsed.Service, "query": parsed.Query, "count": len(items), "limit": limit, "offset": offset, "truncated": truncated, "has_more": truncated, "total_exact": exact, "analyses": items}
	if exact {
		data["total"] = total
	}
	return &core.ToolResult{Tool: tool.Name(), Found: len(items) > 0, Data: data}, nil
}

func compactFinding(finding *core.AIFinding, redactor LineRedactor) *compactAnalysisFinding {
	if finding == nil {
		return nil
	}
	compact := &compactAnalysisFinding{Severity: finding.Severity, Category: finding.Category, Confidence: finding.Confidence}
	if redactor == nil {
		return compact
	}
	scrub := func(value string, limit int) string {
		value = redactor.Scrub(value)
		runes := []rune(value)
		if len(runes) > limit {
			return string(runes[:limit])
		}
		return value
	}
	compact.Title = scrub(finding.Title, 256)
	compact.Summary = scrub(finding.Summary, 512)
	for _, hypothesis := range finding.RootCauseHypotheses[:min(len(finding.RootCauseHypotheses), 5)] {
		hypothesis.Hypothesis = scrub(hypothesis.Hypothesis, 256)
		hypothesis.Rationale = scrub(hypothesis.Rationale, 512)
		compact.RootCauseHypotheses = append(compact.RootCauseHypotheses, hypothesis)
	}
	for _, evidence := range finding.Evidence[:min(len(finding.Evidence), 5)] {
		evidence.Source = scrub(evidence.Source, 128)
		evidence.Summary = scrub(evidence.Summary, 256)
		evidence.Detail = scrub(evidence.Detail, 512)
		compact.Evidence = append(compact.Evidence, evidence)
	}
	for _, patternID := range finding.RelatedPatternIDs[:min(len(finding.RelatedPatternIDs), 10)] {
		compact.RelatedPatternIDs = append(compact.RelatedPatternIDs, scrub(patternID, 128))
	}
	for _, nextStep := range finding.NextSteps[:min(len(finding.NextSteps), 5)] {
		compact.NextSteps = append(compact.NextSteps, scrub(nextStep, 256))
	}
	return compact
}
