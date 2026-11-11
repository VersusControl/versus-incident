package agent

import (
	"slices"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent/act"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	awstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/aws"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// TestAnalyzeAndChatGetAWSReadToolsAndNoActTools: with the AWS connector
// on, both Analyze and chat receive get_aws_connection (it only reads
// stored state), Analyze never receives an act tool, and chat's act tool
// stays behind its own Kubernetes actions capability.
func TestAnalyzeAndChatGetAWSReadToolsAndNoActTools(t *testing.T) {
	service := settings.NewService(settings.NewStore(storage.NewMemory(), tenancy.DefaultOrgScope()))
	ConfigureAWS(func() *settings.Service { return service })
	t.Cleanup(func() { ConfigureAWS(nil) })
	runtime := awsRuntimeTools()
	if !slices.Equal(names(runtime), awstools.ToolNames()) {
		t.Fatalf("runtime AWS tools %v", names(runtime))
	}
	view, err := aitools.NewManager(storage.NewMemory()).LoadToolsets(tenancy.DefaultOrgScope())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := aitools.Snapshot{Capabilities: map[string]aitools.DependencyStatus{
		"aws_connector": {Configured: true, Constructed: true, Healthy: true, Name: "AWS connector"}}}

	analyze, err := view.Filter(aitools.AgentAnalyze, runtime, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names(analyze), []string{"get_aws_connection"}) {
		t.Fatalf("Analyze AWS tools %v", names(analyze))
	}
	chatRuntime := chatRuntimeToolCatalog(runtime, &act.Service{})
	chat, err := view.Filter(aitools.AgentChat, chatRuntime, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(chat), "get_aws_connection") {
		t.Fatalf("chat AWS tools %v", names(chat))
	}
	for _, tools := range [][]string{names(analyze), names(chat)} {
		if slices.Contains(tools, "propose_action") {
			t.Fatalf("an act tool reached an agent without the actions capability: %v", tools)
		}
	}
	if slices.Contains(names(runtime), "propose_action") {
		t.Fatal("the shared runtime tools Analyze receives hold an act tool")
	}
	off, err := view.Filter(aitools.AgentAnalyze, runtime, aitools.Snapshot{})
	if err != nil || len(off) != 0 {
		t.Fatalf("without the AWS connector Analyze gets no AWS tool: %v", names(off))
	}
}
