package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

type countingTool struct{ calls int }

type blockingSeedTool struct{ countingTool }

func (tool *blockingSeedTool) Name() string { return "get_system_overview" }
func (tool *blockingSeedTool) Invoke(ctx context.Context, _ json.RawMessage) (*core.ToolResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type captureChatObserver struct{ events []core.ChatEvent }

func (observer *captureChatObserver) OnChatEvent(event core.ChatEvent) {
	observer.events = append(observer.events, event)
}

func (tool *countingTool) Name() string               { return "counting" }
func (tool *countingTool) Description() string        { return "counts calls" }
func (tool *countingTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool *countingTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	tool.calls++
	return &core.ToolResult{Tool: tool.Name(), Found: true, Data: map[string]any{"value": 1}}, nil
}

func TestNewRejectsDuplicateToolNames(t *testing.T) {
	_, err := New(context.Background(), config.AgentAIConfig{}, []core.Tool{&countingTool{}, &countingTool{}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "duplicate tool name") {
		t.Fatalf("New error = %v, want duplicate tool name", err)
	}
}

func TestGuardedToolCachesDuplicateCallsAndBreaksStagnation(t *testing.T) {
	value := &countingTool{}
	guard := newTurnGuard()
	ctx := context.WithValue(context.Background(), turnGuardContextKey{}, guard)
	wrapped := guardedTool{Tool: value}
	for range stagnationLimit + 1 {
		if _, err := wrapped.Invoke(ctx, json.RawMessage(`{"q":"same"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if value.calls != 1 {
		t.Fatalf("underlying calls = %d, want 1", value.calls)
	}
	state := &adk.ChatModelAgentState{ToolInfos: []*schema.ToolInfo{{Name: "counting"}}}
	middleware := &turnMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}
	_, state, err := middleware.BeforeModelRewriteState(ctx, state, &adk.ModelContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ToolInfos) != 0 {
		t.Fatal("stagnation breaker left tools available")
	}
}

func TestHistoryCompactionIsVisible(t *testing.T) {
	turns := make([]Turn, MaxModelContextTurns+3)
	for index := range turns {
		turns[index] = Turn{Role: TurnUser, Content: "message"}
	}
	ctx := withHistory(context.Background(), turns)
	messages, compacted := historyMessages(ctx)
	if len(messages) != MaxModelContextTurns {
		t.Fatalf("messages = %d, want %d", len(messages), MaxModelContextTurns)
	}
	if compacted != 3 {
		t.Fatalf("compacted = %d, want 3", compacted)
	}
}

func TestAgentSeedBoundsEachTool(t *testing.T) {
	agent := &Agent{tools: []core.Tool{&blockingSeedTool{}}, toolTimeout: 10 * time.Millisecond}
	started := time.Now()
	traces := agent.Seed(context.Background())
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("seed tool exceeded timeout: %s", elapsed)
	}
	if len(traces) != 1 || traces[0].Error == "" {
		t.Fatalf("seed traces = %+v", traces)
	}
}

func TestAgentSeedUsesCurrentToolProvider(t *testing.T) {
	tool := &blockingSeedTool{}
	agent := &Agent{
		tools:        []core.Tool{tool},
		toolTimeout:  time.Second,
		toolProvider: func() ([]core.Tool, error) { return nil, nil },
	}
	if traces := agent.Seed(context.Background()); len(traces) != 0 {
		t.Fatalf("seed traces = %+v, want none", traces)
	}
	if tool.calls != 0 {
		t.Fatalf("disabled seed tool calls = %d, want 0", tool.calls)
	}
}

func TestAgentSeedPrefersFreshSeedProvider(t *testing.T) {
	tool := &blockingSeedTool{}
	agent := &Agent{
		tools:        []core.Tool{tool},
		toolTimeout:  time.Second,
		toolProvider: func() ([]core.Tool, error) { return []core.Tool{tool}, nil },
		seedProvider: func() ([]core.Tool, error) { return nil, nil },
	}
	if traces := agent.Seed(context.Background()); len(traces) != 0 {
		t.Fatalf("seed traces = %+v, want none", traces)
	}
	if tool.calls != 0 {
		t.Fatalf("stale holder tool calls = %d, want 0", tool.calls)
	}
}

func TestConsumeMessageDeltasConcatenateToFinalMarkdown(t *testing.T) {
	reader, writer := schema.Pipe[*schema.Message](1)
	chunks := []string{"Hello", " world", "\n\n", "next line"}
	go func() {
		defer writer.Close()
		for _, content := range chunks {
			if writer.Send(&schema.Message{Role: schema.Assistant, Content: content}, nil) {
				return
			}
		}
	}()
	var deltas strings.Builder
	message, err := consumeMessage(context.Background(), &adk.MessageVariant{
		Role: schema.Assistant, IsStreaming: true, MessageStream: reader,
	}, func(delta string) {
		deltas.WriteString(capRawString(delta, MaxOutputBytes))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(chunks, "")
	if deltas.String() != want || message == nil || message.Content != want {
		t.Fatalf("deltas=%q final=%q want=%q", deltas.String(), message.Content, want)
	}
}

func TestTakePendingCallCorrelatesRepeatedToolsOutOfOrder(t *testing.T) {
	pending := map[string][]schema.ToolCall{
		"query_metrics": {
			{ID: "first", Function: schema.FunctionCall{Name: "query_metrics", Arguments: `{"query":"first"}`}},
			{ID: "second", Function: schema.FunctionCall{Name: "query_metrics", Arguments: `{"query":"second"}`}},
		},
	}

	second, ok := takePendingCall(pending, "query_metrics", "second")
	if !ok || second.ID != "second" {
		t.Fatalf("second result matched %+v, ok=%t", second, ok)
	}
	first, ok := takePendingCall(pending, "query_metrics", "first")
	if !ok || first.ID != "first" || len(pending["query_metrics"]) != 0 {
		t.Fatalf("first result matched %+v, ok=%t, remaining=%d", first, ok, len(pending["query_metrics"]))
	}
}
