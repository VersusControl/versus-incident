package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// RelatedLogs pulls a raw-log slice from the configured signal sources
// around the incident window so the analyze agent can inspect the
// surrounding context. Every returned line is scrubbed through the same
// redactor used before any AI call, so secrets never reach the model.
type RelatedLogs struct {
	Reader   SignalReader
	Redactor LineRedactor
	Services ServiceExtractor
}

const (
	relatedLogsDefaultWindow = 15
	relatedLogsMaxWindow     = 1440
	relatedLogsDefaultLimit  = 50
	relatedLogsMaxLimit      = 200
)

// Name implements core.AnalyzeTool.
func (RelatedLogs) Name() string { return "get_related_logs" }

// Description implements core.AnalyzeTool.
func (RelatedLogs) Description() string {
	return "Pull a redacted slice of raw logs from a configured signal source around the incident window. Optionally filter by source name or service. Returns timestamp, severity, source, and message per line."
}

// ArgsSchema implements core.AnalyzeTool.
func (RelatedLogs) ArgsSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"source": map[string]any{
				"type":        "string",
				"description": "Optional source name (e.g. \"file:noisy-app\"). When omitted, every configured source is queried.",
			},
			"service": map[string]any{
				"type":        "string",
				"description": "Optional service name to filter lines by (best-effort match against fields and source).",
			},
			"window_minutes": map[string]any{
				"type":        "integer",
				"description": "Look back this many minutes from now. Default 15, max 1440.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Cap the number of log lines returned. Default 50, max 200.",
			},
		},
	}
}

type relatedLogsArgs struct {
	Source        string `json:"source"`
	Service       string `json:"service"`
	WindowMinutes int    `json:"window_minutes"`
	Limit         int    `json:"limit"`
}

type relatedLogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Severity  string    `json:"severity,omitempty"`
	Source    string    `json:"source"`
	Message   string    `json:"message"`
}

// Invoke implements core.AnalyzeTool.
func (rl RelatedLogs) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if rl.Reader == nil {
		return nil, fmt.Errorf("get_related_logs: no signal sources configured")
	}
	var a relatedLogsArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, fmt.Errorf("get_related_logs: parse args: %w", err)
		}
	}
	if a.WindowMinutes <= 0 {
		a.WindowMinutes = relatedLogsDefaultWindow
	}
	if a.WindowMinutes > relatedLogsMaxWindow {
		a.WindowMinutes = relatedLogsMaxWindow
	}
	if a.Limit <= 0 {
		a.Limit = relatedLogsDefaultLimit
	}
	if a.Limit > relatedLogsMaxLimit {
		a.Limit = relatedLogsMaxLimit
	}

	now := time.Now().UTC()
	since := now.Add(-time.Duration(a.WindowMinutes) * time.Minute)

	// Resolve which sources to query.
	var targets []string
	if a.Source != "" {
		targets = []string{a.Source}
	} else {
		targets = rl.Reader.Sources()
	}

	out := make([]relatedLogLine, 0, a.Limit)
	for _, name := range targets {
		sigs, err := rl.Reader.Pull(ctx, name, since)
		if err != nil {
			// A single bad source must not sink the whole call when the
			// model asked for "all sources"; surface the error only when
			// the model explicitly targeted that source.
			if a.Source != "" {
				return nil, fmt.Errorf("get_related_logs: pull %q: %w", name, err)
			}
			continue
		}
		for _, s := range sigs {
			if s.Timestamp.Before(since) {
				continue
			}
			msg := rl.scrub(s.Message)
			if a.Service != "" && !rl.signalMatchesService(s, msg, a.Service) {
				continue
			}
			out = append(out, relatedLogLine{
				Timestamp: s.Timestamp,
				Severity:  s.Severity,
				Source:    s.Source,
				Message:   msg,
			})
		}
	}

	// Newest first, then cap.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	if len(out) > a.Limit {
		out = out[:a.Limit]
	}

	return &core.ToolResult{
		Tool:  RelatedLogs{}.Name(),
		Found: len(out) > 0,
		Data: map[string]any{
			"count":          len(out),
			"window_minutes": a.WindowMinutes,
			"source":         a.Source,
			"service":        a.Service,
			"logs":           out,
		},
	}, nil
}

func (rl RelatedLogs) scrub(s string) string {
	if rl.Redactor == nil {
		return s
	}
	return rl.Redactor.Scrub(s)
}

// signalMatchesService is a best-effort filter. It first tries the same
// `agent.service_patterns` extraction the worker uses (so the filter is
// consistent with how signals are attributed to services), then falls
// back to the common structured service keys, and finally to a substring
// match against the source name. Kept loose on purpose — the model uses
// this to narrow noise, not as an exact join. msg is the post-redaction
// message, matching the worker's redact-then-extract order.
func (rl RelatedLogs) signalMatchesService(s core.Signal, msg, service string) bool {
	if rl.Services != nil {
		if extracted := rl.Services.Extract(msg); extracted != "" {
			return strings.EqualFold(extracted, service)
		}
	}
	for _, key := range []string{"service", "service.name", "svc", "app", "component"} {
		if v, ok := s.Fields[key]; ok {
			if str, ok := v.(string); ok && strings.EqualFold(str, service) {
				return true
			}
		}
	}
	return strings.Contains(strings.ToLower(s.Source), strings.ToLower(service))
}
