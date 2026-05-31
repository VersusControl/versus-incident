package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// DescribeService returns the catalog-known summary for a service: how
// long it has been observed and how many patterns are attributed to it.
type DescribeService struct {
	Catalog PatternCatalog
}

// Name implements core.AnalyzeTool.
func (DescribeService) Name() string { return "describe_service" }

// Description implements core.AnalyzeTool.
func (DescribeService) Description() string {
	return "Summarize what the agent knows about a service: first-seen timestamp and the top learned patterns attributed to it."
}

// ArgsSchema implements core.AnalyzeTool.
func (DescribeService) ArgsSchema() map[string]any {
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
	ID       string  `json:"id"`
	Template string  `json:"template"`
	Count    int     `json:"count"`
	Baseline float64 `json:"baseline"`
	Verdict  string  `json:"verdict,omitempty"`
}

// Invoke implements core.AnalyzeTool.
func (d DescribeService) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if d.Catalog == nil {
		return nil, fmt.Errorf("describe_service: catalog not configured")
	}
	var a describeServiceArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("describe_service: parse args: %w", err)
		}
	}
	if a.Service == "" {
		return nil, fmt.Errorf("describe_service: service is required")
	}
	if a.TopPatterns <= 0 {
		a.TopPatterns = 5
	}
	if a.TopPatterns > 20 {
		a.TopPatterns = 20
	}

	res := &core.ToolResult{
		Tool: DescribeService{}.Name(),
		Data: map[string]any{"service": a.Service},
	}
	services := d.Catalog.AllServices()
	if info, ok := services[a.Service]; ok {
		res.Found = true
		res.Data["first_seen"] = info.FirstSeen
	}

	matches := make([]describePatternEntry, 0)
	for _, p := range d.Catalog.All() {
		if !strings.EqualFold(p.Service, a.Service) {
			continue
		}
		matches = append(matches, describePatternEntry{
			ID:       p.ID,
			Template: p.Template,
			Count:    p.Count,
			Baseline: p.Baseline,
			Verdict:  p.Verdict,
		})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Count > matches[j].Count
	})
	if len(matches) > a.TopPatterns {
		matches = matches[:a.TopPatterns]
	}
	res.Data["pattern_count"] = len(matches)
	res.Data["top_patterns"] = matches
	return res, nil
}
