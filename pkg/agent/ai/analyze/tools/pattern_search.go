package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// PatternSearch browses the agent catalog with filtering and ordering.
// It complements PatternHistory (which requires the exact pattern id)
// by letting the model find candidate patterns from a template
// substring, a verdict, a service, or a rule_name. Useful when the
// model wants to ask "are there other patterns on this service that
// fired recently?" without already knowing their ids.
type PatternSearch struct {
	Catalog PatternCatalog
}

// Name implements core.AnalyzeTool.
func (PatternSearch) Name() string { return "pattern_search" }

// Description implements core.AnalyzeTool.
func (PatternSearch) Description() string {
	return "Search the learned log-pattern catalog. Filter by template substring (case-insensitive), service, verdict, or rule_name; results ordered by count or last_seen. Use it to find candidate patterns when you do not already know an id; for the full record of one known id use pattern_history."
}

// Recognised values for ArgsSchema enums. Kept private so tests can
// reuse them without exporting from the package surface.
var (
	patternSearchVerdicts = []string{"known", "unknown", "spike"}
	patternSearchOrders   = []string{"count_desc", "last_seen_desc", "first_seen_desc"}
)

// ArgsSchema implements core.AnalyzeTool.
func (PatternSearch) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Optional case-insensitive substring matched against the pattern template.",
			},
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name to filter by (exact, case-insensitive).",
			},
			"verdict": map[string]any{
				"type":        "string",
				"enum":        anySliceFromStrings(patternSearchVerdicts),
				"description": "Optional verdict filter: known | unknown | spike.",
			},
			"rule_name": map[string]any{
				"type":        "string",
				"description": "Optional named-rule filter (e.g. oom, panic, 5xx-burst), exact match.",
			},
			"order_by": map[string]any{
				"type":        "string",
				"enum":        anySliceFromStrings(patternSearchOrders),
				"description": "Result ordering. Default count_desc.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Cap the number of patterns returned. Default 20, max 100.",
			},
		},
	}
}

// anySliceFromStrings widens a []string to []any so it can sit inside
// the loose map[string]any JSON-schema representation Eino accepts.
func anySliceFromStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

type patternSearchArgs struct {
	Query    string `json:"query"`
	Service  string `json:"service"`
	Verdict  string `json:"verdict"`
	RuleName string `json:"rule_name"`
	OrderBy  string `json:"order_by"`
	Limit    int    `json:"limit"`
}

type patternSearchItem struct {
	ID       string   `json:"id"`
	Template string   `json:"template"`
	Service  string   `json:"service,omitempty"`
	Source   string   `json:"source,omitempty"`
	RuleName string   `json:"rule_name,omitempty"`
	Verdict  string   `json:"verdict,omitempty"`
	Tags     []string `json:"tags,omitempty"`
	Count    int      `json:"count"`
	Baseline float64  `json:"baseline"`
	LastSeen string   `json:"last_seen"`
}

// Invoke implements core.AnalyzeTool.
func (p PatternSearch) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if p.Catalog == nil {
		return nil, fmt.Errorf("pattern_search: catalog not configured")
	}
	var a patternSearchArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("pattern_search: parse args: %w", err)
		}
	}

	// Defaults + caps. Order matters: clamp BEFORE filtering so the
	// limit is honoured against the full filtered set.
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 100 {
		a.Limit = 100
	}
	if a.OrderBy == "" {
		a.OrderBy = "count_desc"
	}
	if !containsString(patternSearchOrders, a.OrderBy) {
		return nil, fmt.Errorf("pattern_search: invalid order_by %q (want %s)", a.OrderBy, strings.Join(patternSearchOrders, " | "))
	}
	if a.Verdict != "" && !containsString(patternSearchVerdicts, strings.ToLower(a.Verdict)) {
		return nil, fmt.Errorf("pattern_search: invalid verdict %q (want %s)", a.Verdict, strings.Join(patternSearchVerdicts, " | "))
	}

	queryLower := strings.ToLower(a.Query)
	all := p.Catalog.All()
	filtered := make([]*PatternView, 0, len(all))
	for _, pat := range all {
		if pat == nil {
			continue
		}
		if queryLower != "" && !strings.Contains(strings.ToLower(pat.Template), queryLower) {
			continue
		}
		if a.Service != "" && !strings.EqualFold(pat.Service, a.Service) {
			continue
		}
		if a.Verdict != "" && !strings.EqualFold(pat.Verdict, a.Verdict) {
			continue
		}
		if a.RuleName != "" && pat.RuleName != a.RuleName {
			continue
		}
		filtered = append(filtered, pat)
	}

	sortPatternViews(filtered, a.OrderBy)

	totalMatched := len(filtered)
	if len(filtered) > a.Limit {
		filtered = filtered[:a.Limit]
	}

	out := make([]patternSearchItem, 0, len(filtered))
	for _, pat := range filtered {
		out = append(out, patternSearchItem{
			ID:       pat.ID,
			Template: pat.Template,
			Service:  pat.Service,
			Source:   pat.Source,
			RuleName: pat.RuleName,
			Verdict:  pat.Verdict,
			Tags:     pat.Tags,
			Count:    pat.Count,
			Baseline: pat.Baseline,
			LastSeen: pat.LastSeen.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	return &core.ToolResult{
		Tool:  PatternSearch{}.Name(),
		Found: true,
		Data: map[string]any{
			"count":         len(out),
			"total_matched": totalMatched,
			"truncated":     totalMatched > len(out),
			"order_by":      a.OrderBy,
			"query":         a.Query,
			"service":       a.Service,
			"verdict":       a.Verdict,
			"rule_name":     a.RuleName,
			"patterns":      out,
		},
	}, nil
}

// sortPatternViews orders the slice in-place by the operator's choice.
// All orderings break ties by id ascending so output is deterministic.
func sortPatternViews(views []*PatternView, orderBy string) {
	sort.SliceStable(views, func(i, j int) bool {
		a, b := views[i], views[j]
		switch orderBy {
		case "last_seen_desc":
			if !a.LastSeen.Equal(b.LastSeen) {
				return a.LastSeen.After(b.LastSeen)
			}
		case "first_seen_desc":
			if !a.FirstSeen.Equal(b.FirstSeen) {
				return a.FirstSeen.After(b.FirstSeen)
			}
		default: // count_desc
			if a.Count != b.Count {
				return a.Count > b.Count
			}
		}
		return a.ID < b.ID
	})
}

func containsString(in []string, v string) bool {
	for _, s := range in {
		if s == v {
			return true
		}
	}
	return false
}
