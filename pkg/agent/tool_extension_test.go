package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type extensionTool string

func (tool extensionTool) Name() string          { return string(tool) }
func (extensionTool) Description() string        { return "test" }
func (extensionTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool extensionTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	return &core.ToolResult{Tool: tool.Name(), Found: true}, nil
}

func TestToolContributorsAreOrderedScopedAndRejectCollisions(t *testing.T) {
	RegisterToolContributor("z-test", func(scope tenancy.OrgScope, _ []config.AgentSourceConfig, _ core.Scrubber) ([]core.Tool, []error) {
		if scope.Write != "acme" {
			t.Fatalf("scope = %+v", scope)
		}
		return []core.Tool{extensionTool("shared")}, nil
	})
	RegisterToolContributor("a-test", func(tenancy.OrgScope, []config.AgentSourceConfig, core.Scrubber) ([]core.Tool, []error) {
		return []core.Tool{extensionTool("shared"), extensionTool("first")}, nil
	})
	t.Cleanup(func() { RegisterToolContributor("z-test", nil); RegisterToolContributor("a-test", nil) })
	tools, errs := contributedTools(tenancy.NewOrgScope("acme"), nil, nil)
	if len(tools) != 2 || tools[0].Name() != "shared" || tools[1].Name() != "first" {
		t.Fatalf("tools = %v", tools)
	}
	if len(errs) != 1 {
		t.Fatalf("errors = %v", errs)
	}
}
