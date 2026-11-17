package agent

import (
	"slices"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	finopstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/finops"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func names(tools []core.Tool) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

// TestAnalyzeGetsFinOpsReadToolsAndNoActTools: the factory hands Analyze
// the shared runtime tools (read only) and adds the act tool to chat only.
// With cost data on, Analyze receives exactly the FinOps read tools.
func TestAnalyzeGetsFinOpsReadToolsAndNoActTools(t *testing.T) {
	ConfigureFinOps(func() finops.Reader { return finops.NewService(nil, nil, finops.Config{}) })
	t.Cleanup(func() { ConfigureFinOps(nil) })
	runtime := finopsRuntimeTools()
	view, err := aitools.NewManager(storage.NewMemory()).LoadToolsets(tenancy.DefaultOrgScope())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := aitools.Snapshot{Capabilities: map[string]aitools.DependencyStatus{
		"cost_data": {Configured: true, Constructed: true, Healthy: true, Name: "Cloud cost data"}}}
	analyze, err := view.Filter(aitools.AgentAnalyze, runtime, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(analyze), finopstools.ToolNames()) {
		t.Fatalf("Analyze FinOps tools %v, want %v", names(analyze), finopstools.ToolNames())
	}
	chat := chatRuntimeToolCatalog(runtime, &act.Service{})
	if !slices.Contains(names(chat), "propose_action") {
		t.Fatal("the act tool is added to chat")
	}
	if slices.Contains(names(runtime), "propose_action") {
		t.Fatal("the shared runtime tools Analyze receives hold no act tool")
	}
	off, err := view.Filter(aitools.AgentAnalyze, runtime, aitools.Snapshot{})
	if err != nil || len(off) != 0 {
		t.Fatalf("without cost data Analyze gets no FinOps tool: %v", names(off))
	}
}
