package tools

import (
	"context"
	"encoding/json"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type GetDetectionHealth struct {
	Reader DetectionHealthReader
	Scope  tenancy.OrgScope
}

func (GetDetectionHealth) Name() string        { return "get_detection_health" }
func (GetDetectionHealth) DisplayName() string { return "Inspecting detection health" }
func (GetDetectionHealth) Description() string {
	return "Check passive source wiring before ruling out a cause. Reports sources built from configuration and dark categories. Reachability remains unknown unless runtime state provides an observation. Never calls Pull."
}
func (GetDetectionHealth) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (tool GetDetectionHealth) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if len(args) > 0 {
		var parsed map[string]any
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	if tool.Reader == nil {
		result := core.UnavailableToolResult(tool.Name(), "detection health state not configured")
		result.Data = map[string]any{"sources": []SourceHealth{}, "source_count": 0, "categories": unknownCategoryHealth(), "observation": "unknown"}
		return result, nil
	}
	snapshot := tool.Reader.DetectionHealth(tool.Scope)
	return &core.ToolResult{Tool: tool.Name(), Found: len(snapshot.Sources) > 0, Data: map[string]any{
		"sources": snapshot.Sources, "source_count": len(snapshot.Sources), "categories": snapshot.Categories, "observation": snapshot.Observation,
	}}, nil
}

func unknownCategoryHealth() []CategoryHealth {
	return []CategoryHealth{{Kind: "logs", Dark: true}, {Kind: "metrics", Dark: true}, {Kind: "traces", Dark: true}}
}
