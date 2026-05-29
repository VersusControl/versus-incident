package analyze

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// fakeChat is a deterministic stand-in for an Eino tool-calling chat
// model. It emits scripted replies on successive Generate calls. Tests
// own the script: tool_call turns are mixed with a final assistant
// message holding the AIFinding JSON.
type fakeChat struct {
	turns []*schema.Message
	idx   int
	tools []*schema.ToolInfo

	lastMessages []*schema.Message
}

func (f *fakeChat) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	f.lastMessages = append(f.lastMessages, in[len(in)-1])
	if f.idx >= len(f.turns) {
		return f.turns[len(f.turns)-1], nil
	}
	m := f.turns[f.idx]
	f.idx++
	return m, nil
}

func (f *fakeChat) Stream(_ context.Context, _ []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return nil, nil
}

func (f *fakeChat) WithTools(t []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	f.tools = t
	return f, nil
}

// stubTool is a read-only AnalyzeTool used to validate dispatch.
type stubTool struct {
	name    string
	invoked int
	last    json.RawMessage
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub" }
func (s *stubTool) ArgsSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
	}
}
func (s *stubTool) Invoke(_ context.Context, args json.RawMessage) (*core.ToolResult, error) {
	s.invoked++
	s.last = args
	return &core.ToolResult{
		Tool:  s.name,
		Found: true,
		Data:  map[string]any{"echo": string(args)},
	}, nil
}

func newAgentWithFake(t *testing.T, fake *fakeChat, tools []core.AnalyzeTool, maxIter int) *Agent {
	t.Helper()
	a, err := New(context.Background(),
		config.AgentAIConfig{Model: "fake", APIKey: "x"},
		tools,
		Options{ChatModel: fake},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if maxIter > 0 {
		a.maxIter = maxIter
	}
	return a
}

// TestAgent_NoEmitterField is the static guard from E4-T5. The struct
// must never grow an Emitter / Notifier / Sender field — analyze is
// read-only by contract.
func TestAgent_NoEmitterField(t *testing.T) {
	tp := reflect.TypeOf(Agent{})
	for i := 0; i < tp.NumField(); i++ {
		name := strings.ToLower(tp.Field(i).Name)
		switch name {
		case "emitter", "notifier", "sender", "dispatcher":
			t.Fatalf("Agent has forbidden field %q; analyze must stay read-only", tp.Field(i).Name)
		}
	}
}

func TestAgent_RejectsNonAnalyzeTask(t *testing.T) {
	a := newAgentWithFake(t, &fakeChat{turns: []*schema.Message{schema.AssistantMessage("{}", nil)}}, nil, 1)
	if _, err := a.Run(context.Background(), core.DetectTask{}); err == nil {
		t.Fatalf("expected error for DetectTask")
	}
}

func TestAgent_RunToolLoop(t *testing.T) {
	stub := &stubTool{name: "echo_tool"}
	finalJSON := `{"title":"t","summary":"s","severity":"medium","confidence":0.7,
		"next_steps":["check disk"],"evidence":[{"source":"echo_tool","summary":"ran"}]}`

	toolCallMsg := schema.AssistantMessage("", []schema.ToolCall{{
		ID:   "call-1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "echo_tool",
			Arguments: `{"q":"disk"}`,
		},
	}})
	finalMsg := schema.AssistantMessage(finalJSON, nil)

	fake := &fakeChat{turns: []*schema.Message{toolCallMsg, finalMsg}}
	a := newAgentWithFake(t, fake, []core.AnalyzeTool{stub}, 3)

	res, err := a.Run(context.Background(), core.AnalyzeTask{
		Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-1", Service: "svc"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stub.invoked != 1 {
		t.Fatalf("tool invoked %d times, want 1", stub.invoked)
	}
	if res.Finding == nil {
		t.Fatalf("nil finding")
	}
	if res.Finding.Title != "t" || res.Finding.Severity != "medium" {
		t.Fatalf("finding parsed wrong: %+v", res.Finding)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "echo_tool" {
		t.Fatalf("tool trace = %+v", res.ToolCalls)
	}
}

func TestAgent_FilterAllowList(t *testing.T) {
	stub1 := &stubTool{name: "keep_me"}
	stub2 := &stubTool{name: "drop_me"}
	fake := &fakeChat{turns: []*schema.Message{schema.AssistantMessage(`{"title":"a","summary":"b","next_steps":["x"]}`, nil)}}

	a, err := New(context.Background(),
		config.AgentAIConfig{Model: "fake"},
		[]core.AnalyzeTool{stub1, stub2},
		Options{ChatModel: fake},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := a.tools["keep_me"]; !ok {
		t.Fatalf("keep_me not registered")
	}
	if _, ok := a.tools["drop_me"]; !ok {
		t.Fatalf("drop_me not registered")
	}
}
