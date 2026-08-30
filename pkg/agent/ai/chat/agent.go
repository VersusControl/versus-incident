package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	MaxToolIterations    = 8
	DefaultToolTimeout   = 20 * time.Second
	MaxOutputBytes       = 8192
	MaxModelContextTurns = 20
	stagnationLimit      = 3
)

type Options struct {
	HTTPClient   *http.Client
	BaseURL      string
	Timeout      time.Duration
	AuthKeyFunc  func(context.Context) (string, bool)
	Runtime      einowrap.RuntimeAI
	ChatModel    model.ToolCallingChatModel
	ToolTimeout  time.Duration
	ToolProvider func() ([]core.Tool, error)
	SeedProvider func() ([]core.Tool, error)
}

type Agent struct {
	cfg          config.AgentAIConfig
	runner       *adk.Runner
	holder       *einowrap.Holder[*adk.Runner]
	tools        []core.Tool
	toolDisplays map[string]string
	toolTimeout  time.Duration
	toolProvider func() ([]core.Tool, error)
	seedProvider func() ([]core.Tool, error)
}

type historyContextKey struct{}

func withHistory(ctx context.Context, turns []Turn) context.Context {
	return context.WithValue(ctx, historyContextKey{}, append([]Turn(nil), turns...))
}

func New(ctx context.Context, cfg config.AgentAIConfig, tools []core.Tool, opts Options) (*Agent, error) {
	toolTimeout := opts.ToolTimeout
	if toolTimeout == 0 {
		toolTimeout = DefaultToolTimeout
	}
	filtered := make([]core.Tool, 0, len(tools))
	displays := make(map[string]string, len(tools))
	names := make(map[string]struct{}, len(tools))
	for _, value := range tools {
		if value == nil || value.Name() == "" {
			continue
		}
		if _, exists := names[value.Name()]; exists {
			return nil, fmt.Errorf("duplicate tool name %q", value.Name())
		}
		names[value.Name()] = struct{}{}
		filtered = append(filtered, value)
		displays[value.Name()] = core.ToolDisplayName(value)
	}
	agent := &Agent{cfg: cfg, tools: filtered, toolDisplays: displays, toolTimeout: toolTimeout, toolProvider: opts.ToolProvider, seedProvider: opts.SeedProvider}
	build := func(ctx context.Context, chatModel model.ToolCallingChatModel) (*adk.Runner, error) {
		return agent.buildRunner(ctx, chatModel)
	}
	if opts.ChatModel != nil {
		runner, err := build(ctx, opts.ChatModel)
		if err != nil {
			return nil, err
		}
		agent.runner = runner
		return agent, nil
	}
	agent.holder = einowrap.NewModelHolder(cfg, einowrap.Options{
		HTTPClient: opts.HTTPClient, BaseURL: opts.BaseURL, Timeout: opts.Timeout, AuthKeyFunc: opts.AuthKeyFunc,
	}, opts.Runtime, func(ctx context.Context, resolved config.AgentAIConfig, modelOptions einowrap.Options) (*adk.Runner, error) {
		chatModel, err := einowrap.NewToolCallingChatModel(ctx, resolved, modelOptions)
		if err != nil {
			return nil, err
		}
		return build(ctx, chatModel)
	})
	if _, err := agent.holder.Get(ctx); err != nil {
		return nil, err
	}
	return agent, nil
}

func (agent *Agent) buildRunner(ctx context.Context, chatModel model.ToolCallingChatModel) (*adk.Runner, error) {
	tools := agent.tools
	if agent.toolProvider != nil {
		var err error
		tools, err = agent.toolProvider()
		if err != nil {
			return nil, fmt.Errorf("chat: load tools: %w", err)
		}
	}
	einoTools := make([]tool.BaseTool, 0, len(tools))
	for _, value := range tools {
		adapted, err := einowrap.NewTool(guardedTool{Tool: value}, agent.toolTimeout, MaxToolPayloadBytes)
		if err != nil {
			return nil, err
		}
		einoTools = append(einoTools, adapted)
	}
	chatAgent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
		Name:          "chat",
		Description:   "Read-only DevOps and SRE assistant for connected systems",
		Instruction:   SystemPrompt(),
		Model:         chatModel,
		MaxIterations: MaxToolIterations,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: einoTools,
			UnknownToolsHandler: func(_ context.Context, name, _ string) (string, error) {
				return fmt.Sprintf(`{"available":false,"reason":"tool %q is unavailable"}`, capString(name, 128)), nil
			},
			ExecuteSequentially: true,
		}},
		Handlers: []adk.ChatModelAgentMiddleware{&turnMiddleware{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{}}},
	})
	if err != nil {
		return nil, fmt.Errorf("chat: build ADK agent: %w", err)
	}
	return adk.NewRunner(ctx, adk.RunnerConfig{Agent: chatAgent, EnableStreaming: true}), nil
}

func (agent *Agent) Name() string          { return "chat" }
func (agent *Agent) Kind() core.AITaskKind { return core.AITaskChat }

func (agent *Agent) currentRunner(ctx context.Context) (*adk.Runner, error) {
	if agent.runner != nil {
		return agent.runner, nil
	}
	return agent.holder.Get(ctx)
}

func (agent *Agent) RunChatTurn(ctx context.Context, task core.ChatTask) (*core.ChatTurnResult, error) {
	if agent == nil {
		return nil, fmt.Errorf("chat: nil agent")
	}
	if task.Kind() != core.AITaskChat || strings.TrimSpace(task.Message) == "" {
		return nil, fmt.Errorf("chat: invalid task")
	}
	runner, err := agent.currentRunner(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat: model unavailable")
	}
	runCtx := context.WithValue(ctx, turnGuardContextKey{}, newTurnGuard())
	var sequence int64
	messages, compacted := historyMessages(runCtx)
	messages = append(messages, schema.UserMessage(buildUserPrompt(task)))

	core.EmitChatEvent(ctx, core.ChatEvent{Seq: atomic.AddInt64(&sequence, 1), Kind: core.ChatEventRunStarted})
	if compacted > 0 {
		core.EmitChatEvent(ctx, core.ChatEvent{Seq: atomic.AddInt64(&sequence, 1), Kind: core.ChatEventCompacted, Output: fmt.Sprintf("%d older turns omitted from model context", compacted)})
	}
	started := time.Now()
	result := &core.ChatTurnResult{Model: agent.cfg.Model}
	var pending = map[string][]schema.ToolCall{}
	iterator := runner.Run(runCtx, messages)
	for event, ok := iterator.Next(); ok; event, ok = iterator.Next() {
		if event == nil {
			continue
		}
		if event.Err != nil {
			result.DurationMs = time.Since(started).Milliseconds()
			if errors.Is(event.Err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
				return result, context.Canceled
			}
			if errors.Is(event.Err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return result, fmt.Errorf("chat: run timed out: %w", context.DeadlineExceeded)
			}
			return result, fmt.Errorf("chat: ADK run failed")
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		variant := event.Output.MessageOutput
		onDelta := func(string) {}
		if variant.Role == schema.Assistant {
			onDelta = func(delta string) {
				core.EmitChatEvent(ctx, core.ChatEvent{Seq: atomic.AddInt64(&sequence, 1), Kind: core.ChatEventModelDelta, Delta: capRawString(delta, MaxOutputBytes)})
			}
		}
		message, messageErr := consumeMessage(ctx, variant, onDelta)
		if messageErr != nil {
			return result, safeRunError(ctx, messageErr)
		}
		if message == nil {
			continue
		}
		if variant.Role == schema.Assistant {
			if len(message.ToolCalls) > 0 {
				for _, call := range message.ToolCalls {
					pending[call.Function.Name] = append(pending[call.Function.Name], call)
					core.EmitChatEvent(ctx, core.ChatEvent{Seq: atomic.AddInt64(&sequence, 1), Kind: core.ChatEventToolStarted, Tool: call.Function.Name, CallID: call.ID, ToolDisplay: agent.toolDisplays[call.Function.Name], Args: capString(call.Function.Arguments, MaxToolPayloadBytes)})
				}
			} else if strings.TrimSpace(message.Content) != "" {
				result.Markdown = capString(message.Content, MaxOutputBytes)
			}
		}
		if variant.Role == schema.Tool {
			name := variant.ToolName
			trace := core.ToolCallTrace{CallID: message.ToolCallID, Name: name, Output: capString(message.Content, MaxToolPayloadBytes)}
			if call, ok := takePendingCall(pending, name, message.ToolCallID); ok {
				trace.CallID = call.ID
				trace.Args = capString(call.Function.Arguments, MaxToolPayloadBytes)
			}
			result.ToolCalls = append(result.ToolCalls, trace)
			locator := fmt.Sprintf("tool-call-%d", len(result.ToolCalls))
			if trace.CallID != "" {
				locator = "tool-call-" + trace.CallID
			}
			result.Citations = append(result.Citations, core.ChatCitation{Tool: name, Label: agent.toolDisplays[name], Locator: locator})
			core.EmitChatEvent(ctx, core.ChatEvent{Seq: atomic.AddInt64(&sequence, 1), Kind: core.ChatEventToolFinished, Tool: name, CallID: trace.CallID, ToolDisplay: agent.toolDisplays[name], Args: trace.Args, Output: trace.Output})
		}
	}
	result.DurationMs = time.Since(started).Milliseconds()
	if ctx.Err() != nil {
		return result, safeRunError(ctx, ctx.Err())
	}
	if result.Markdown == "" {
		return result, fmt.Errorf("chat: model returned no answer")
	}
	return result, nil
}

func takePendingCall(pending map[string][]schema.ToolCall, name, callID string) (schema.ToolCall, bool) {
	calls := pending[name]
	if len(calls) == 0 {
		return schema.ToolCall{}, false
	}
	matched := len(calls) - 1
	if callID != "" {
		for index := range calls {
			if calls[index].ID == callID {
				matched = index
				break
			}
		}
	}
	call := calls[matched]
	pending[name] = append(calls[:matched], calls[matched+1:]...)
	return call, true
}

// Seed collects the available discovery evidence once for a new session.
func (agent *Agent) Seed(ctx context.Context) []core.ToolCallTrace {
	wanted := map[string]bool{"get_system_overview": true, "list_services": true}
	traces := make([]core.ToolCallTrace, 0, 2)
	tools := agent.tools
	provider := agent.seedProvider
	if provider == nil {
		provider = agent.toolProvider
	}
	if provider != nil {
		var err error
		tools, err = provider()
		if err != nil {
			return []core.ToolCallTrace{{Error: "unavailable: tool settings unavailable"}}
		}
	}
	for _, value := range tools {
		if !wanted[value.Name()] {
			continue
		}
		started := time.Now()
		toolCtx, cancel := context.WithTimeout(ctx, agent.toolTimeout)
		result, err := guardedTool{Tool: value}.Invoke(toolCtx, json.RawMessage(`{}`))
		cancel()
		trace := core.ToolCallTrace{Name: value.Name(), DurationMs: time.Since(started).Milliseconds()}
		if err != nil {
			code, message := core.ClassifyToolError(err)
			trace.Error = string(code) + ": " + message
		} else if encoded, marshalErr := json.Marshal(result); marshalErr == nil {
			trace.Output = capString(string(encoded), MaxToolPayloadBytes)
		}
		traces = append(traces, trace)
	}
	return traces
}

func consumeMessage(ctx context.Context, variant *adk.MessageVariant, onDelta func(string)) (*schema.Message, error) {
	if !variant.IsStreaming {
		return variant.Message, nil
	}
	defer variant.MessageStream.Close()
	chunks := make([]*schema.Message, 0, 8)
	for {
		chunk, err := variant.MessageStream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if chunk != nil {
			chunks = append(chunks, chunk)
			if chunk.Content != "" {
				onDelta(chunk.Content)
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	return schema.ConcatMessages(chunks)
}

func safeRunError(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("chat: run timed out: %w", context.DeadlineExceeded)
	}
	return fmt.Errorf("chat: model stream failed")
}

func historyMessages(ctx context.Context) ([]*schema.Message, int) {
	turns, _ := ctx.Value(historyContextKey{}).([]Turn)
	compacted := 0
	if len(turns) > MaxModelContextTurns {
		compacted = len(turns) - MaxModelContextTurns
		turns = turns[compacted:]
	}
	messages := make([]*schema.Message, 0, len(turns))
	for _, turn := range turns {
		switch turn.Role {
		case TurnUser:
			messages = append(messages, schema.UserMessage(capString(turn.Content, MaxMessageBytes)))
		case TurnAssistant:
			messages = append(messages, schema.AssistantMessage(capString(turn.Content, MaxOutputBytes), nil))
		case TurnCompaction:
			messages = append(messages, schema.SystemMessage(capString(turn.Content, 512)))
		}
	}
	return messages, compacted
}

type turnGuardContextKey struct{}

type turnGuard struct {
	mu       sync.Mutex
	cache    map[string]string
	evidence map[string]struct{}
	stagnant int
}

func newTurnGuard() *turnGuard {
	return &turnGuard{cache: map[string]string{}, evidence: map[string]struct{}{}}
}

type guardedTool struct{ core.Tool }

func (value guardedTool) Invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	guard, _ := ctx.Value(turnGuardContextKey{}).(*turnGuard)
	if guard == nil {
		return value.Tool.Invoke(ctx, args)
	}
	key := value.Name() + "\x00" + string(args)
	guard.mu.Lock()
	if cached, ok := guard.cache[key]; ok {
		guard.stagnant++
		guard.mu.Unlock()
		var result core.ToolResult
		if err := jsonUnmarshal([]byte(cached), &result); err == nil {
			return &result, nil
		}
		return &core.ToolResult{Tool: value.Name(), Found: false, Data: map[string]any{"available": false, "reason": "repeated evidence; stop querying"}}, nil
	}
	guard.mu.Unlock()
	result, err := value.Tool.Invoke(ctx, args)
	if err != nil {
		return nil, err
	}
	encoded, _ := jsonMarshal(result)
	hash := sha256.Sum256(encoded)
	evidenceKey := hex.EncodeToString(hash[:])
	guard.mu.Lock()
	guard.cache[key] = string(encoded)
	if _, seen := guard.evidence[evidenceKey]; seen {
		guard.stagnant++
	} else {
		guard.evidence[evidenceKey] = struct{}{}
		guard.stagnant = 0
	}
	guard.mu.Unlock()
	return result, nil
}

type turnMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
}

func (middleware *turnMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, modelContext *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	guard, _ := ctx.Value(turnGuardContextKey{}).(*turnGuard)
	if guard != nil {
		guard.mu.Lock()
		stagnant := guard.stagnant >= stagnationLimit
		guard.mu.Unlock()
		if stagnant {
			state.ToolInfos = nil
			state.DeferredToolInfos = nil
		}
	}
	return ctx, state, nil
}

var jsonMarshal = func(value any) ([]byte, error) { return json.Marshal(value) }
var jsonUnmarshal = func(data []byte, value any) error { return json.Unmarshal(data, value) }
