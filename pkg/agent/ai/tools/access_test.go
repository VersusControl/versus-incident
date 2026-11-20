package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func TestEveryCatalogToolHasAnAccessGroup(t *testing.T) {
	for _, metadata := range Catalog() {
		group := AccessGroupForName(metadata.Name)
		if group == "" {
			t.Fatalf("tool %q has no access group", metadata.Name)
		}
		if (group == core.ToolGroupFinOps) != (metadata.Group == GroupFinOps) {
			t.Fatalf("tool %q access group %q does not match catalog group %q", metadata.Name, group, metadata.Group)
		}
	}
}

type groupedTool struct{ name, group string }

func (tool groupedTool) Name() string               { return tool.name }
func (tool groupedTool) Description() string        { return tool.name }
func (tool groupedTool) ArgsSchema() map[string]any { return nil }
func (tool groupedTool) ToolGroup() string          { return tool.group }
func (tool groupedTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	return nil, nil
}

func TestCallerToolRestrictionUsesTheToolsOwnGroup(t *testing.T) {
	restricted := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, ToolGroups: []string{core.ToolGroupFinOps}})
	if !CallerMayUseTool(restricted, groupedTool{name: "extension_cost", group: core.ToolGroupFinOps}) {
		t.Fatal("a tool naming the allowed group was refused")
	}
	if CallerMayUseTool(restricted, groupedTool{name: "extension_other", group: "other"}) {
		t.Fatal("a tool naming another group was allowed")
	}
	if CallerMayUseTool(restricted, groupedTool{name: "extension_unnamed"}) {
		t.Fatal("a tool with no group was allowed for a restricted caller")
	}
	if !CallerMayUseTool(context.Background(), groupedTool{name: "extension_unnamed"}) {
		t.Fatal("a caller without a restriction was refused")
	}
}
