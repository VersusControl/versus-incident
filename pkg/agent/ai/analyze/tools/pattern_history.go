package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// PatternHistory looks up the agent catalog by pattern id and returns
// the curated metadata (template, counts, baseline, verdict, tags).
type PatternHistory struct {
	Catalog PatternCatalog
}

// Name implements core.AnalyzeTool.
func (PatternHistory) Name() string { return "pattern_history" }

// Description implements core.AnalyzeTool.
func (PatternHistory) Description() string {
	return "Look up a learned log-pattern by id and return its template, observed counts, EWMA baseline, service, verdict, and operator tags."
}

// ArgsSchema implements core.AnalyzeTool.
func (PatternHistory) ArgsSchema() map[string]any {
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
func (p PatternHistory) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if p.Catalog == nil {
		return nil, fmt.Errorf("pattern_history: catalog not configured")
	}
	var a patternHistoryArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("pattern_history: parse args: %w", err)
		}
	}
	if a.PatternID == "" {
		return nil, fmt.Errorf("pattern_history: pattern_id is required")
	}
	pat := p.Catalog.Get(a.PatternID)
	if pat == nil {
		return &core.ToolResult{
			Tool:  PatternHistory{}.Name(),
			Found: false,
			Data:  map[string]any{"pattern_id": a.PatternID},
		}, nil
	}
	return &core.ToolResult{
		Tool:  PatternHistory{}.Name(),
		Found: true,
		Data: map[string]any{
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
		},
	}, nil
}
