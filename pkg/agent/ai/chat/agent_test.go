package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

func TestModelResponseDiagnosticClassifiesWithoutLeakingProviderBody(t *testing.T) {
	const checkLogs = " Check the Versus server logs for the full provider error."
	tests := []struct {
		name    string
		failure string
		want    string
	}{
		{name: "authentication", failure: "status 401 invalid_api_key sk-secret", want: `Claude authentication failed for model "claude-sonnet-5"; verify the configured API key.` + checkLogs},
		{name: "model", failure: "status 404 model_not_found sk-secret", want: `Claude model "claude-sonnet-5" was not found or is unavailable to this account.` + checkLogs},
		{name: "quota", failure: "status 429 rate_limit sk-secret", want: `Claude rate limit or quota was reached for model "claude-sonnet-5"; retry later or check provider limits.` + checkLogs},
		{name: "temperature", failure: "status 400 invalid_request_error: `temperature` is deprecated for this model", want: `Claude model "claude-sonnet-5" rejected the configured temperature; set AGENT_AI_TEMPERATURE=-1 to omit it, restart Versus, and retry.` + checkLogs},
		{name: "message ordering", failure: `status 400 {"type":"error","error":{"type":"invalid_request_error","message":"messages.1: role 'system' must precede an 'assistant' message or end the array"}}`, want: `Claude rejected the generated chat history for model "claude-sonnet-5": messages.1: role 'system' must precede an 'assistant' message or end the array.` + checkLogs},
		{name: "spaced message ordering", failure: `status 400 prefix {"type": "error", "error": {"type": "invalid_request_error", "message": "messages.2: roles must alternate"}} trailing`, want: `Claude rejected the generated chat history for model "claude-sonnet-5": messages.2: roles must alternate.` + checkLogs},
		{name: "unsafe validation body", failure: `status 400 {"type":"error","error":{"type":"invalid_request_error","message":"messages.1: leaked sk-secret"}}`, want: `Claude rejected the request for model "claude-sonnet-5" as invalid; verify model compatibility and token limits.` + checkLogs},
		{name: "unknown", failure: "provider exploded with sk-secret", want: `Claude could not produce a response with model "claude-sonnet-5"; verify provider configuration and model access.` + checkLogs},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := newModelResponseError("claude", "claude-sonnet-5", errors.New(test.failure))
			var modelErr *modelResponseError
			if !errors.Is(err, errModelResponseUnavailable) || !errors.As(err, &modelErr) {
				t.Fatalf("error = %v, want typed model response error", err)
			}
			if modelErr.diagnostic != test.want {
				t.Fatalf("diagnostic = %q, want %q", modelErr.diagnostic, test.want)
			}
			if strings.Contains(modelErr.diagnostic, "sk-secret") {
				t.Fatalf("diagnostic leaked provider response: %q", modelErr.diagnostic)
			}
		})
	}
}

func TestModelResponseDiagnosticLogsOriginalProviderError(t *testing.T) {
	var output bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(originalWriter) })

	failure := "HTTP 400 Bad Request: `temperature` is Deprecated for This Model"
	_ = modelResponseDiagnostic("claude", "claude-sonnet-5", failure)
	if !strings.Contains(output.String(), failure) {
		t.Fatalf("log output = %q, want original provider error %q", output.String(), failure)
	}
}

type countingTool struct{ calls int }

type namedTool struct{ name string }

func (tool namedTool) Name() string               { return tool.name }
func (tool namedTool) Description() string        { return tool.name }
func (tool namedTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool namedTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	return &core.ToolResult{Tool: tool.name, Found: true}, nil
}

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

type recordingChatModel struct {
	messages []*schema.Message
}

func (chatModel *recordingChatModel) Generate(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	chatModel.messages = append([]*schema.Message(nil), messages...)
	return schema.AssistantMessage("Healthy", nil), nil
}

func (chatModel *recordingChatModel) Stream(_ context.Context, messages []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chatModel.messages = append([]*schema.Message(nil), messages...)
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer writer.Close()
		writer.Send(schema.AssistantMessage("Healthy", nil), nil)
	}()
	return reader, nil
}

func (chatModel *recordingChatModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return chatModel, nil
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

func TestAgentResolvesKubernetesAuthorizationForEveryTurn(t *testing.T) {
	for _, permissions := range [][]bool{{true, false, true}, {false, true, false}} {
		agent := &Agent{tools: []core.Tool{
			namedTool{name: "counting"},
			namedTool{name: "get_cluster_overview"},
		}}
		for index, allowed := range permissions {
			ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{
				Authenticated: true,
				Permissions: map[core.Permission]bool{
					core.PermissionInfrastructureView: allowed,
				},
			})
			available, err := agent.availableTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"counting"}
			if allowed {
				want = append(want, "get_cluster_overview")
			}
			got := make([]string, 0, len(available))
			for _, tool := range available {
				got = append(got, tool.Name())
			}
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("permissions=%v turn=%d tools=%v want=%v", permissions, index, got, want)
			}
		}
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

func TestHistoryCompactionDoesNotInjectSystemMessages(t *testing.T) {
	turns := []Turn{
		{Role: TurnAssistant, Content: "Earlier answer"},
		{Role: TurnCompaction, Content: `{"kind":"session_discovery"}`},
		{Role: TurnUser, Content: "What is unhealthy?"},
	}
	messages, _ := historyMessages(withHistory(context.Background(), turns))
	if len(messages) != len(turns) {
		t.Fatalf("messages = %d, want %d", len(messages), len(turns))
	}
	for index, message := range messages {
		if message.Role == schema.System {
			t.Fatalf("message %d has system role in replayed history", index)
		}
	}
	if messages[1].Role != schema.User || messages[1].Content != turns[1].Content {
		t.Fatalf("compaction message = %#v, want bounded user context", messages[1])
	}
}

func TestAgentSendsOnlyLeadingSystemMessageAfterCompaction(t *testing.T) {
	chatModel := &recordingChatModel{}
	agent, err := New(context.Background(), config.AgentAIConfig{Model: "test-model"}, nil, Options{ChatModel: chatModel})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withHistory(context.Background(), []Turn{
		{Role: TurnAssistant, Content: "Earlier answer"},
		{Role: TurnCompaction, Content: `{"kind":"session_discovery"}`},
	})
	if _, err := agent.RunChatTurn(ctx, core.ChatTask{Message: "What is unhealthy?"}); err != nil {
		t.Fatal(err)
	}
	if len(chatModel.messages) != 2 || chatModel.messages[0].Role != schema.System {
		t.Fatalf("model messages = %#v, want leading system instruction and history", chatModel.messages)
	}
	for index, message := range chatModel.messages[1:] {
		if message.Role == schema.System {
			t.Fatalf("model message %d has system role after conversation started", index+1)
		}
	}
	if chatModel.messages[1].Role != schema.User || !strings.Contains(chatModel.messages[1].Content, "session_discovery") || !strings.Contains(chatModel.messages[1].Content, "What is unhealthy?") {
		t.Fatalf("normalized user context = %#v", chatModel.messages[1])
	}
}

func TestNormalizeHistoryMessagesAlternatesRoles(t *testing.T) {
	messages := normalizeHistoryMessages([]*schema.Message{
		schema.AssistantMessage("orphaned answer", nil),
		schema.UserMessage("retained context"),
		schema.UserMessage("current question"),
		schema.AssistantMessage("first answer", nil),
		schema.AssistantMessage("continued answer", nil),
	})
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want two alternating messages", messages)
	}
	if messages[0].Role != schema.User || messages[1].Role != schema.Assistant {
		t.Fatalf("roles = %s, %s", messages[0].Role, messages[1].Role)
	}
	if !strings.Contains(messages[0].Content, "retained context") || !strings.Contains(messages[0].Content, "current question") {
		t.Fatalf("merged user content = %q", messages[0].Content)
	}
	if !strings.Contains(messages[1].Content, "first answer") || !strings.Contains(messages[1].Content, "continued answer") {
		t.Fatalf("merged assistant content = %q", messages[1].Content)
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
