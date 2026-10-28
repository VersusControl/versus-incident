package tools

import (
	"context"
	"encoding/json"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// GetPattern looks up the agent catalog by pattern id and returns
// the curated metadata (template, counts, baseline, verdict, tags).
type GetPattern struct {
	Catalog  PatternCatalog
	Redactor LineRedactor
}

// Name implements core.AnalyzeTool.
func (GetPattern) Name() string        { return "get_pattern" }
func (GetPattern) DisplayName() string { return "Inspecting pattern" }

// Description implements core.AnalyzeTool.
func (GetPattern) Description() string {
	return "Inspect one learned log pattern, including history, verdict, readiness, effective threshold, reason availability, provenance, and operator tags."
}

// ArgsSchema implements core.AnalyzeTool.
func (GetPattern) ArgsSchema() map[string]any {
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
func (p GetPattern) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if p.Catalog == nil {
		return core.UnavailableToolResult(GetPattern{}.Name(), "Versus catalog not configured"), nil
	}
	var a patternHistoryArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	if a.PatternID == "" {
		return nil, core.NewToolError(core.ToolErrorInvalidArguments, "pattern_id is required", nil)
	}
	pat := p.Catalog.Get(a.PatternID)
	if pat == nil {
		return &core.ToolResult{
			Tool:  GetPattern{}.Name(),
			Found: false,
			Data:  map[string]any{"pattern_id": a.PatternID},
		}, nil
	}
	data := map[string]any{
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
	}
	ready := pat.AutoPromoteAfter > 0 && pat.Count >= pat.AutoPromoteAfter
	data["ready"] = ready
	data["effective_auto_promote_after"] = pat.AutoPromoteAfter
	known := pat.Verdict == "known"
	if known {
		data["reason_known"] = false
		data["reason"] = "stored provenance does not identify whether the known verdict was operator-curated or automatic"
		data["provenance"] = "unknown"
	} else {
		data["reason_known"] = true
		data["reason"] = "the stored verdict is not known"
		data["provenance"] = "not_applicable"
	}
	// Include the latest few REDACTED example log lines (bounded to keep the
	// analyze token budget sane). Omitted entirely when the pattern has none.
	if p.Redactor != nil {
		if samples := latestSamples(pat.Samples, patternHistorySampleLimit); len(samples) > 0 {
			for index := range samples {
				samples[index] = p.Redactor.Scrub(samples[index])
			}
			data["samples"] = samples
		}
	}
	data["samples_total"] = len(pat.Samples)
	data["samples_truncated"] = len(pat.Samples) > patternHistorySampleLimit
	return &core.ToolResult{
		Tool:  GetPattern{}.Name(),
		Found: true,
		Data:  data,
	}, nil
}
