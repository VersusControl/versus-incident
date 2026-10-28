package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// GetService returns the catalog-known summary for a service: how
// long it has been observed and how many patterns are attributed to it.
type GetService struct {
	Catalog     PatternCatalog
	Redactor    LineRedactor
	Reliability ServiceReliabilityProvider
	Scope       tenancy.OrgScope
}

// Name implements core.AnalyzeTool.
func (GetService) Name() string        { return "get_service" }
func (GetService) DisplayName() string { return "Inspecting service" }

// Description implements core.AnalyzeTool.
func (GetService) Description() string {
	return "Inspect a service's catalog facts, learned patterns, and optional provider-neutral reliability state including SLIs and SLOs."
}

// ArgsSchema implements core.AnalyzeTool.
func (GetService) ArgsSchema() map[string]any {
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
	ID               string  `json:"id"`
	Template         string  `json:"template"`
	Count            int     `json:"count"`
	Baseline         float64 `json:"baseline"`
	Verdict          string  `json:"verdict,omitempty"`
	SamplesTotal     int     `json:"samples_total"`
	SamplesTruncated bool    `json:"samples_truncated"`
	// Sample is the latest REDACTED example log line for the pattern (only the
	// most recent one, to save tokens in the compact per-service listing).
	Sample string `json:"sample,omitempty"`
}

// Invoke implements core.AnalyzeTool.
func (d GetService) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if d.Catalog == nil && d.Reliability == nil {
		return core.UnavailableToolResult(GetService{}.Name(), "Versus catalog not configured"), nil
	}
	var a describeServiceArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	if a.Service == "" {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "service is required", nil)
	}
	if a.TopPatterns <= 0 {
		a.TopPatterns = 5
	}
	if a.TopPatterns > 20 {
		a.TopPatterns = 20
	}

	res := &core.ToolResult{
		Tool: GetService{}.Name(),
		Data: map[string]any{"service": a.Service},
	}

	matches := make([]describePatternEntry, 0)
	if d.Catalog != nil {
		services := d.Catalog.AllServices()
		if info, ok := services[a.Service]; ok {
			res.Found = true
			res.Data["first_seen"] = info.FirstSeen
		}
	}
	var patterns []*PatternView
	if d.Catalog != nil {
		patterns = d.Catalog.All()
	}
	for _, p := range patterns {
		if p.Service != a.Service {
			continue
		}
		entry := describePatternEntry{
			ID:               p.ID,
			Template:         p.Template,
			Count:            p.Count,
			Baseline:         p.Baseline,
			Verdict:          p.Verdict,
			SamplesTotal:     len(p.Samples),
			SamplesTruncated: len(p.Samples) > describeServiceSampleLimit,
		}
		if d.Redactor != nil {
			if s := latestSamples(p.Samples, describeServiceSampleLimit); len(s) > 0 {
				entry.Sample = d.Redactor.Scrub(s[0])
			}
		}
		matches = append(matches, entry)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Count > matches[j].Count
	})
	patternsTotal := len(matches)
	if len(matches) > a.TopPatterns {
		matches = matches[:a.TopPatterns]
	}
	res.Data["pattern_count"] = len(matches)
	res.Data["patterns_total"] = patternsTotal
	res.Data["truncated"] = patternsTotal > len(matches)
	res.Data["top_patterns"] = matches
	if d.Reliability == nil {
		res.Data["reliability"] = ServiceReliabilitySnapshot{
			Service: a.Service, Configured: false, Licensed: CapabilityStatusUnknown,
			Available: CapabilityStatusFalse, Reason: "service reliability provider is not configured",
			SetupAction: "Configure a service reliability provider for this deployment.",
		}
		return res, nil
	}
	reliability, err := d.Reliability.ServiceReliability(ctx, d.Scope.Normalized(), a.Service)
	if err != nil {
		return nil, fmt.Errorf("get_service: reliability provider: %w", err)
	}
	if reliability.Service == "" {
		reliability.Service = a.Service
	}
	reliability.Service = scrubModelText(reliability.Service, d.Redactor)
	reliability.Source = scrubModelText(reliability.Source, d.Redactor)
	reliability.Observation = scrubModelText(reliability.Observation, d.Redactor)
	reliability.Reason = scrubModelText(reliability.Reason, d.Redactor)
	reliability.SetupAction = scrubModelText(reliability.SetupAction, d.Redactor)
	if len(reliability.SLIs) > reliabilityItemLimit {
		reliability.SLIs = reliability.SLIs[:reliabilityItemLimit]
	}
	for index := range reliability.SLIs {
		reliability.SLIs[index].Name = scrubModelText(reliability.SLIs[index].Name, d.Redactor)
		reliability.SLIs[index].Kind = scrubModelText(reliability.SLIs[index].Kind, d.Redactor)
		reliability.SLIs[index].Description = scrubModelText(reliability.SLIs[index].Description, d.Redactor)
		reliability.SLIs[index].Unit = scrubModelText(reliability.SLIs[index].Unit, d.Redactor)
	}
	if len(reliability.SLOs) > reliabilityItemLimit {
		reliability.SLOs = reliability.SLOs[:reliabilityItemLimit]
	}
	for index := range reliability.SLOs {
		reliability.SLOs[index].Name = scrubModelText(reliability.SLOs[index].Name, d.Redactor)
		reliability.SLOs[index].SLI = scrubModelText(reliability.SLOs[index].SLI, d.Redactor)
	}
	res.Data["reliability"] = reliability
	res.Found = res.Found || reliability.Available == CapabilityStatusTrue || len(reliability.SLIs) > 0 || len(reliability.SLOs) > 0
	return res, nil
}
