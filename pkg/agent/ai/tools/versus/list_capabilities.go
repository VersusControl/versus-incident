package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type ListCapabilities struct{ Capabilities []CapabilityStatus }

func (ListCapabilities) Name() string        { return "list_capabilities" }
func (ListCapabilities) DisplayName() string { return "Listing capabilities" }
func (ListCapabilities) Description() string {
	return "Discover whether system capabilities are configured, licensed, available, or unknown, with observations and safe setup actions. Results may be filtered by name or group."
}
func (ListCapabilities) ArgsSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"name":  map[string]any{"type": "string", "description": "Optional case-insensitive capability name filter."},
		"group": map[string]any{"type": "string", "description": "Optional case-insensitive capability group filter."},
	}}
}
func (tool ListCapabilities) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	var parsed struct {
		Name  string `json:"name"`
		Group string `json:"group"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsed); err != nil {
			return nil, core.NewToolError(core.ToolErrorInvalidArguments, "arguments must be valid JSON", err)
		}
	}
	items := make([]CapabilityStatus, 0, len(tool.Capabilities))
	for _, capability := range tool.Capabilities {
		if parsed.Name != "" && !strings.EqualFold(capability.Name, strings.TrimSpace(parsed.Name)) {
			continue
		}
		if parsed.Group != "" && !strings.EqualFold(capability.Group, strings.TrimSpace(parsed.Group)) {
			continue
		}
		items = append(items, capability)
	}
	return &core.ToolResult{Tool: tool.Name(), Found: len(items) > 0, Data: map[string]any{"capabilities": items}}, nil
}
