package analyze

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// errTool is a read-only AnalyzeTool that always fails, used to assert
// the ReAct loop surfaces tool errors to the model rather than aborting.
type errTool struct {
	name    string
	invoked int
}

func (e *errTool) Name() string        { return e.name }
func (e *errTool) Description() string { return "always errors" }
func (e *errTool) ArgsSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (e *errTool) Invoke(_ context.Context, _ json.RawMessage) (*core.ToolResult, error) {
	e.invoked++
	return nil, errBoom
}

var errBoom = fmt.Errorf("boom from tool")

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

// TestAgent_ConcurrentToolCalls exercises the Eino ReAct loop with two
// tool calls in a single turn and parallel_tools enabled: both tools
// must run (concurrent compose.ToolsNode fan-out) and both must be
// recorded in the callback-built audit trace before the final finding
// is parsed.
func TestAgent_ConcurrentToolCalls(t *testing.T) {
	a := &stubTool{name: "tool_a"}
	b := &stubTool{name: "tool_b"}
	finalJSON := `{"title":"t","summary":"s","next_steps":["x"]}`

	twoCalls := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c-a", Type: "function", Function: schema.FunctionCall{Name: "tool_a", Arguments: `{"q":"a"}`}},
		{ID: "c-b", Type: "function", Function: schema.FunctionCall{Name: "tool_b", Arguments: `{"q":"b"}`}},
	})
	fake := &fakeChat{turns: []*schema.Message{twoCalls, schema.AssistantMessage(finalJSON, nil)}}

	agent, err := New(context.Background(),
		config.AgentAIConfig{Model: "fake", APIKey: "x"},
		[]core.AnalyzeTool{a, b},
		Options{ChatModel: fake, ParallelTools: true},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := agent.Run(context.Background(), core.AnalyzeTask{
		Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-2"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if a.invoked != 1 || b.invoked != 1 {
		t.Fatalf("tool invocations: a=%d b=%d, want 1/1", a.invoked, b.invoked)
	}
	if len(res.ToolCalls) != 2 {
		t.Fatalf("trace len = %d, want 2", len(res.ToolCalls))
	}
	got := map[string]bool{}
	for _, tc := range res.ToolCalls {
		got[tc.Name] = true
		if tc.Output == "" {
			t.Fatalf("trace %q has empty output", tc.Name)
		}
	}
	if !got["tool_a"] || !got["tool_b"] {
		t.Fatalf("trace missing a tool: %+v", res.ToolCalls)
	}
	if res.Finding == nil || res.Finding.Title != "t" {
		t.Fatalf("finding parsed wrong: %+v", res.Finding)
	}
}

// TestAgent_ToolErrorDoesNotAbort asserts a failing tool is surfaced to
// the model as a structured error instead of aborting the ReAct graph,
// so the model can still produce a final finding.
func TestAgent_ToolErrorDoesNotAbort(t *testing.T) {
	boom := &errTool{name: "boom"}
	finalJSON := `{"title":"recovered","summary":"s","next_steps":["x"]}`

	callMsg := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c-1", Type: "function", Function: schema.FunctionCall{Name: "boom", Arguments: `{}`}},
	})
	fake := &fakeChat{turns: []*schema.Message{callMsg, schema.AssistantMessage(finalJSON, nil)}}

	agent := newAgentWithFake(t, fake, []core.AnalyzeTool{boom}, 3)
	res, err := agent.Run(context.Background(), core.AnalyzeTask{
		Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-3"},
	})
	if err != nil {
		t.Fatalf("Run returned error, want graceful recovery: %v", err)
	}
	if res.Finding == nil || res.Finding.Title != "recovered" {
		t.Fatalf("finding parsed wrong: %+v", res.Finding)
	}
	if len(res.ToolCalls) != 1 || !strings.Contains(res.ToolCalls[0].Output, "error") {
		t.Fatalf("expected tool error surfaced in trace output: %+v", res.ToolCalls)
	}
}

// blockTool blocks until its context is cancelled or it is released,
// used to assert the per-tool timeout cancels a slow dispatch.
type blockTool struct {
	name    string
	release chan struct{}
	invoked int32
}

func (b *blockTool) Name() string               { return b.name }
func (b *blockTool) Description() string        { return "blocks" }
func (b *blockTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (b *blockTool) Invoke(ctx context.Context, _ json.RawMessage) (*core.ToolResult, error) {
	atomic.AddInt32(&b.invoked, 1)
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return &core.ToolResult{Tool: b.name, Found: true}, nil
}

// TestAgent_ToolTimeout asserts a tool that outruns the per-tool
// timeout is abandoned, its timeout surfaces as a structured tool error
// in the audit trace, and the ReAct graph still recovers a finding from
// the model's final message.
func TestAgent_ToolTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow := &blockTool{name: "slow_tool", release: release}

	callMsg := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c-1", Type: "function", Function: schema.FunctionCall{Name: "slow_tool", Arguments: `{}`}},
	})
	finalJSON := `{"title":"recovered","summary":"s","next_steps":["x"]}`
	fake := &fakeChat{turns: []*schema.Message{callMsg, schema.AssistantMessage(finalJSON, nil)}}

	a, err := New(context.Background(),
		config.AgentAIConfig{Model: "fake", APIKey: "x"},
		[]core.AnalyzeTool{slow},
		Options{ChatModel: fake, ToolTimeout: 50 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	res, err := a.Run(context.Background(), core.AnalyzeTask{
		Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-timeout"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run returned error, want graceful recovery: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("run took %s, expected the tool to be abandoned quickly", elapsed)
	}
	if atomic.LoadInt32(&slow.invoked) != 1 {
		t.Fatalf("tool invoked %d times, want 1", slow.invoked)
	}
	if len(res.ToolCalls) != 1 || !strings.Contains(res.ToolCalls[0].Output, "timed out") {
		t.Fatalf("expected timeout surfaced in trace output: %+v", res.ToolCalls)
	}
	if res.Finding == nil || res.Finding.Title != "recovered" {
		t.Fatalf("finding parsed wrong: %+v", res.Finding)
	}
}

// concProbe is shared state across two probeTool instances. Each tool
// records the peak number of simultaneously-active invocations so a
// test can distinguish concurrent fan-out (maxActive == 2) from
// sequential dispatch (maxActive == 1).
type concProbe struct {
	active    int32
	maxActive int32
	want      int32
	wait      time.Duration
	proceed   chan struct{}
	once      sync.Once
}

type probeTool struct {
	name  string
	probe *concProbe
}

func (p *probeTool) Name() string               { return p.name }
func (p *probeTool) Description() string        { return "probe" }
func (p *probeTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (p *probeTool) Invoke(ctx context.Context, _ json.RawMessage) (*core.ToolResult, error) {
	c := p.probe
	n := atomic.AddInt32(&c.active, 1)
	for {
		m := atomic.LoadInt32(&c.maxActive)
		if n <= m || atomic.CompareAndSwapInt32(&c.maxActive, m, n) {
			break
		}
	}
	if n >= c.want {
		c.once.Do(func() { close(c.proceed) })
	}
	select {
	case <-c.proceed:
	case <-time.After(c.wait):
	case <-ctx.Done():
	}
	atomic.AddInt32(&c.active, -1)
	return &core.ToolResult{Tool: p.name, Found: true}, nil
}

func runProbeAgent(t *testing.T, parallel bool, wait time.Duration) int32 {
	t.Helper()
	probe := &concProbe{want: 2, wait: wait, proceed: make(chan struct{})}
	a := &probeTool{name: "probe_a", probe: probe}
	b := &probeTool{name: "probe_b", probe: probe}

	twoCalls := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "p-a", Type: "function", Function: schema.FunctionCall{Name: "probe_a", Arguments: `{}`}},
		{ID: "p-b", Type: "function", Function: schema.FunctionCall{Name: "probe_b", Arguments: `{}`}},
	})
	finalJSON := `{"title":"t","summary":"s","next_steps":["x"]}`
	fake := &fakeChat{turns: []*schema.Message{twoCalls, schema.AssistantMessage(finalJSON, nil)}}

	agent, err := New(context.Background(),
		config.AgentAIConfig{Model: "fake", APIKey: "x"},
		[]core.AnalyzeTool{a, b},
		Options{ChatModel: fake, ParallelTools: parallel},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := agent.Run(context.Background(), core.AnalyzeTask{
		Snapshot: core.AnalyzeIncidentSnapshot{IncidentID: "i-probe"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return atomic.LoadInt32(&probe.maxActive)
}

// TestAgent_ParallelTools asserts the parallel_tools knob controls
// whether two tool calls in one turn overlap. With it enabled both
// tools are in flight simultaneously (peak 2); with it disabled (the
// default) dispatch is sequential (peak 1).
func TestAgent_ParallelTools(t *testing.T) {
	if got := runProbeAgent(t, true, 2*time.Second); got != 2 {
		t.Fatalf("parallel: maxActive = %d, want 2", got)
	}
	if got := runProbeAgent(t, false, 60*time.Millisecond); got != 1 {
		t.Fatalf("sequential: maxActive = %d, want 1", got)
	}
}

// TestAgent_DefaultToolTimeout asserts newEinoTool carries the built-in
// default timeout that New applies when Options.ToolTimeout is zero, so
// production always has a per-tool cap even when analyze.tool_timeout is
// unset.
func TestAgent_DefaultToolTimeout(t *testing.T) {
	stub := &stubTool{name: "echo"}
	tool, err := newEinoTool(stub, defaultToolTimeout)
	if err != nil {
		t.Fatalf("newEinoTool: %v", err)
	}
	if tool.timeout != defaultToolTimeout {
		t.Fatalf("default tool timeout = %s, want %s", tool.timeout, defaultToolTimeout)
	}
}
