package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type ListPatterns struct {
	Catalog  PagedPatternCatalog
	Redactor LineRedactor
}

func (ListPatterns) Name() string        { return "list_patterns" }
func (ListPatterns) DisplayName() string { return "Listing learned patterns" }
func (ListPatterns) Description() string {
	return "List learned patterns by service or query with exact filtered totals, verdicts, tags, baselines, and bounded redacted samples. Resolve uncertain service names with list_services first."
}
func (ListPatterns) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"service": map[string]any{"type": "string", "description": "Optional exact service name."}, "query": map[string]any{"type": "string", "description": "Case-insensitive template, id, or service substring."}, "limit": map[string]any{"type": "integer", "description": "Default 20, max 100."},
		"offset": map[string]any{"type": "integer", "description": "Matches to skip. Default 0, max 500."},
	}}
}

type describePatternsArgs struct {
	Service string `json:"service"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
	Offset  int    `json:"offset"`
}
type patternItem struct {
	ID               string   `json:"id"`
	Template         string   `json:"template"`
	Service          string   `json:"service,omitempty"`
	Count            int      `json:"count"`
	Baseline         float64  `json:"baseline"`
	Verdict          string   `json:"verdict,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Samples          []string `json:"samples,omitempty"`
	SamplesTotal     int      `json:"samples_total"`
	SamplesTruncated bool     `json:"samples_truncated"`
}

func (tool ListPatterns) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if tool.Catalog == nil {
		return core.UnavailableToolResult(tool.Name(), "Versus catalog not configured"), nil
	}
	var parsed describePatternsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	if parsed.Service != "" && strings.TrimSpace(parsed.Service) == "" {
		err := fmt.Errorf("service must not be blank")
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	limit := clampLimit(parsed.Limit)
	offset, err := validateOffset(parsed.Offset)
	if err != nil {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, err.Error(), err)
	}
	page, total, err := tool.Catalog.PatternsPage(CatalogPageOptions{Offset: offset, Limit: limit, Search: parsed.Query, Service: parsed.Service})
	if err != nil {
		return nil, fmt.Errorf("list_patterns: catalog page: %w", err)
	}
	items := make([]patternItem, 0, len(page))
	for _, pattern := range page {
		var samples []string
		if tool.Redactor != nil {
			samples = latestSamples(pattern.Samples, patternHistorySampleLimit)
			for index := range samples {
				samples[index] = tool.Redactor.Scrub(samples[index])
			}
		}
		items = append(items, patternItem{ID: pattern.ID, Template: pattern.Template, Service: pattern.Service, Count: pattern.Count, Baseline: pattern.Baseline, Verdict: pattern.Verdict, Tags: append([]string(nil), pattern.Tags...), Samples: samples, SamplesTotal: len(pattern.Samples), SamplesTruncated: len(pattern.Samples) > len(samples)})
	}
	hasMore := offset+len(items) < total
	return &core.ToolResult{Tool: tool.Name(), Found: len(items) > 0, Data: map[string]any{"service": parsed.Service, "query": parsed.Query, "count": len(items), "total": total, "limit": limit, "offset": offset, "truncated": hasMore, "has_more": hasMore, "patterns": items}}, nil
}
