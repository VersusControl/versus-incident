package core

import (
	"context"
	"encoding/json"
	"strings"
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

func TestToolResultAvailability(t *testing.T) {
	availableJSON, err := json.Marshal(ToolResult{Tool: "probe", Found: true})
	if err != nil {
		t.Fatalf("marshal available: %v", err)
	}
	if !strings.Contains(string(availableJSON), `"available":true`) {
		t.Fatalf("available result = %s", availableJSON)
	}

	unavailable := UnavailableToolResult("probe", "source not configured")
	if unavailable.IsAvailable() {
		t.Fatal("unavailable result reported available")
	}
	unavailableJSON, err := json.Marshal(unavailable)
	if err != nil {
		t.Fatalf("marshal unavailable: %v", err)
	}
	if !strings.Contains(string(unavailableJSON), `"available":false`) || !strings.Contains(string(unavailableJSON), `"reason":"source not configured"`) {
		t.Fatalf("unavailable result = %s", unavailableJSON)
	}
	var roundTrip ToolResult
	if err := json.Unmarshal(unavailableJSON, &roundTrip); err != nil {
		t.Fatalf("unmarshal unavailable: %v", err)
	}
	if roundTrip.IsAvailable() || roundTrip.Reason != "source not configured" {
		t.Fatalf("round-trip = %+v", roundTrip)
	}
}
