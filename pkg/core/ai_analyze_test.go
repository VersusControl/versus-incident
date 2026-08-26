package core

import (
	"context"
	"encoding/json"
	"testing"
)

type displayTool struct {
	name    string
	display string
}

func (t displayTool) Name() string             { return t.name }
func (t displayTool) DisplayName() string      { return t.display }
func (displayTool) Description() string        { return "test" }
func (displayTool) ArgsSchema() map[string]any { return nil }
func (displayTool) Invoke(context.Context, json.RawMessage) (*ToolResult, error) {
	return nil, nil
}

type legacyTool struct{ name string }

func (t legacyTool) Name() string             { return t.name }
func (legacyTool) Description() string        { return "test" }
func (legacyTool) ArgsSchema() map[string]any { return nil }
func (legacyTool) Invoke(context.Context, json.RawMessage) (*ToolResult, error) {
	return nil, nil
}

type pointerTool struct{ displayTool }

func TestToolDisplayName(t *testing.T) {
	var nilPointer *pointerTool
	tests := []struct {
		name string
		tool AnalyzeTool
		want string
	}{
		{name: "explicit", tool: displayTool{name: "query_metrics", display: "Checking metrics"}, want: "Checking metrics"},
		{name: "trimmed explicit", tool: displayTool{name: "query_metrics", display: "  Checking metrics  "}, want: "Checking metrics"},
		{name: "empty display fallback", tool: displayTool{name: "query_metrics"}, want: "Query Metrics"},
		{name: "legacy tool with mixed separators", tool: legacyTool{name: "get-related_logs"}, want: "Get Related Logs"},
		{name: "unicode", tool: displayTool{name: "évaluer_metrics"}, want: "Évaluer Metrics"},
		{name: "empty name", tool: displayTool{}, want: ""},
		{name: "nil interface", tool: nil, want: ""},
		{name: "typed nil", tool: nilPointer, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToolDisplayName(tt.tool); got != tt.want {
				t.Fatalf("ToolDisplayName() = %q, want %q", got, tt.want)
			}
		})
	}
}
