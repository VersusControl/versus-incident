package analyze

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	utilcb "github.com/cloudwego/eino/utils/callbacks"
	"github.com/eino-contrib/jsonschema"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	defaultMaxToolIterations = 8
	maxToolOutputBytes       = 8192
	// defaultToolTimeout caps a single tool dispatch when the operator
	// does not configure analyze.tool_timeout. A slow tool surfaces as a
	// tool error in the audit trace instead of consuming the whole
	// analysis budget.
	defaultToolTimeout = 20 * time.Second
)

// Agent is the analyze-kind AIAgent. The investigation loop, tool
// fan-out, and per-call audit all run on Eino's pre-built ReAct agent
// (flow/agent/react). The struct binds the resolved per-task config,
// the ReAct agent (which already owns the tool-calling chat model plus
// a compose.ToolsNode — sequential by default, concurrent when
// analyze.parallel_tools is set), and an in-memory registry of the
// read-only tools (kept for introspection / allow-list assertions).
//
// The struct deliberately has NO Emitter / Notifier / Sender /
// Dispatcher field. The import-graph guard test in agent_test.go
// asserts this so future edits cannot silently turn analyze into a
// notification path.
type Agent struct {
	cfg     config.AgentAIConfig
	agent   *react.Agent
	tools   map[string]core.AnalyzeTool
	maxIter int
}

// Options is the constructor-side bag for test plumbing.
type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	Timeout    time.Duration

	// ChatModel overrides the Eino tool-calling chat model. When
	// non-nil the agent skips dialing OpenAI; tests pass a fake. The
	// ReAct agent binds tools onto it via WithTools.
	ChatModel model.ToolCallingChatModel

	// ToolTimeout caps a single tool dispatch. Zero applies the built-in
	// defaultToolTimeout; a negative value disables the per-tool cap.
	ToolTimeout time.Duration

	// ParallelTools runs multiple tool calls emitted in one model turn
	// concurrently. False (the default) dispatches them sequentially.
	ParallelTools bool
}

// New constructs an analyze Agent. cfg must already be resolved for
// the analyze task (see config.AgentAIConfig.Resolve). Every tool in
// the supplied list is registered with the agent.
func New(ctx context.Context, cfg config.AgentAIConfig, tools []core.AnalyzeTool, opts Options) (*Agent, error) {
	toolTimeout := opts.ToolTimeout
	if toolTimeout == 0 {
		toolTimeout = defaultToolTimeout
	}

	reg := map[string]core.AnalyzeTool{}
	einoTools := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		if t == nil || t.Name() == "" {
			continue
		}
		reg[t.Name()] = t
		et, err := newEinoTool(t, toolTimeout)
		if err != nil {
			return nil, err
		}
		einoTools = append(einoTools, et)
	}

	chat := opts.ChatModel
	if chat == nil {
		base, err := einowrap.NewToolCallingChatModel(ctx, cfg, einowrap.Options{
			HTTPClient: opts.HTTPClient,
			BaseURL:    opts.BaseURL,
			Timeout:    opts.Timeout,
		})
		if err != nil {
			return nil, err
		}
		chat = base
	}

	maxIter := defaultMaxToolIterations

	// MaxStep bounds the ReAct graph's pregel super-steps. The graph
	// alternates chat -> tools -> chat ...; N chat calls interleaved
	// with N-1 tool rounds take 2N-1 steps. Allowing maxIter tool
	// rounds plus a final answer call (N = maxIter+1) gives the bound
	// below — the framework-native equivalent of the old maxIter loop
	// plus its "budget exhausted" guard.
	reactAgent, err := react.NewAgent(ctx, &react.AgentConfig{
		ToolCallingModel: chat,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: einoTools,
			// Repair malformed tool calls from small models instead of
			// aborting the turn: a hallucinated tool name is answered
			// with a structured error the model can recover from.
			UnknownToolsHandler: func(_ context.Context, name, _ string) (string, error) {
				return fmt.Sprintf(`{"error":"unknown tool %q"}`, name), nil
			},
			// Tool dispatch is sequential by default; opts.ParallelTools
			// flips ExecuteSequentially off so multiple tool calls in one
			// turn fan out concurrently. The audit trace stays ordered
			// either way (seq-stamped at OnStart, stable-sorted).
			ExecuteSequentially: !opts.ParallelTools,
		},
		MaxStep: 2*maxIter + 1,
	})
	if err != nil {
		return nil, fmt.Errorf("analyze: build react agent: %w", err)
	}

	return &Agent{
		cfg:     cfg,
		agent:   reactAgent,
		tools:   reg,
		maxIter: maxIter,
	}, nil
}

// Name implements core.AIAgent.
func (a *Agent) Name() string { return "analyze" }

// Kind implements core.AIAgent.
func (a *Agent) Kind() core.AITaskKind { return core.AITaskAnalyze }

// Run implements core.AIAgent. Rejects any non-AnalyzeTask.
func (a *Agent) Run(ctx context.Context, task core.AITask) (*core.AICallResult, error) {
	if a == nil {
		return nil, fmt.Errorf("analyze: nil agent")
	}
	if task == nil {
		return nil, fmt.Errorf("analyze: nil task")
	}
	at, ok := task.(core.AnalyzeTask)
	if !ok {
		return nil, fmt.Errorf("analyze: expected AnalyzeTask, got %s", task.Kind())
	}
	return a.run(ctx, at.Snapshot)
}

func (a *Agent) run(ctx context.Context, snap core.AnalyzeIncidentSnapshot) (*core.AICallResult, error) {
	user := BuildUserPrompt(snap)
	messages := []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(user),
	}

	// The audit trace is built from Eino tool callbacks rather than
	// hand instrumentation, so it captures every tool the framework
	// dispatches (including concurrent calls in one turn).
	collector := &traceCollector{}
	handler := utilcb.NewHandlerHelper().Tool(collector.toolHandler()).Handler()

	start := time.Now()
	out, err := a.agent.Generate(ctx, messages, agent.WithComposeOptions(compose.WithCallbacks(handler)))
	durationMs := time.Since(start).Milliseconds()
	traces := collector.ordered()

	if err != nil {
		return &core.AICallResult{
			UserPrompt: user,
			DurationMs: durationMs,
			Model:      a.cfg.Model,
			ToolCalls:  traces,
		}, fmt.Errorf("analyze: react agent: %w", err)
	}
	if out == nil {
		return &core.AICallResult{
			UserPrompt: user,
			DurationMs: durationMs,
			Model:      a.cfg.Model,
			ToolCalls:  traces,
		}, fmt.Errorf("analyze: react agent returned no message")
	}

	rawFinal := strings.TrimSpace(out.Content)
	if rawFinal == "" {
		return &core.AICallResult{
			UserPrompt: user,
			DurationMs: durationMs,
			Model:      a.cfg.Model,
			ToolCalls:  traces,
		}, fmt.Errorf("analyze: model never produced a final message")
	}

	finding, err := ParseFinding(rawFinal)
	if err != nil {
		return &core.AICallResult{
			UserPrompt:  user,
			RawResponse: rawFinal,
			DurationMs:  durationMs,
			Model:       a.cfg.Model,
			ToolCalls:   traces,
		}, err
	}

	return &core.AICallResult{
		Finding:     finding,
		UserPrompt:  user,
		RawResponse: rawFinal,
		DurationMs:  durationMs,
		Model:       a.cfg.Model,
		ToolCalls:   traces,
	}, nil
}

// traceCollector turns Eino tool callbacks into ordered
// core.ToolCallTrace entries. Tools may run concurrently, so the slice
// is mutex-guarded and reassembled in start order (a monotonically
// increasing sequence stamped at OnStart) before it lands in the
// core.AICallResult.
type traceCollector struct {
	mu    sync.Mutex
	seq   int64
	items []*traceItem
}

type traceItem struct {
	seq   int64
	start time.Time
	trace core.ToolCallTrace
}

type traceCtxKey struct{}

func (c *traceCollector) toolHandler() *utilcb.ToolCallbackHandler {
	return &utilcb.ToolCallbackHandler{
		OnStart: func(ctx context.Context, info *callbacks.RunInfo, input *tool.CallbackInput) context.Context {
			it := &traceItem{
				seq:   atomic.AddInt64(&c.seq, 1),
				start: time.Now(),
			}
			if info != nil {
				it.trace.Name = info.Name
			}
			if input != nil {
				it.trace.Args = input.ArgumentsInJSON
			}
			c.mu.Lock()
			c.items = append(c.items, it)
			c.mu.Unlock()
			return context.WithValue(ctx, traceCtxKey{}, it)
		},
		OnEnd: func(ctx context.Context, _ *callbacks.RunInfo, output *tool.CallbackOutput) context.Context {
			it, _ := ctx.Value(traceCtxKey{}).(*traceItem)
			if it == nil {
				return ctx
			}
			it.trace.DurationMs = time.Since(it.start).Milliseconds()
			if output != nil {
				it.trace.Output = capOutput(output.Response)
			}
			return ctx
		},
		OnError: func(ctx context.Context, _ *callbacks.RunInfo, err error) context.Context {
			it, _ := ctx.Value(traceCtxKey{}).(*traceItem)
			if it == nil {
				return ctx
			}
			it.trace.DurationMs = time.Since(it.start).Milliseconds()
			if err != nil {
				it.trace.Error = err.Error()
			}
			return ctx
		},
	}
}

func (c *traceCollector) ordered() []core.ToolCallTrace {
	c.mu.Lock()
	defer c.mu.Unlock()
	sort.SliceStable(c.items, func(i, j int) bool { return c.items[i].seq < c.items[j].seq })
	out := make([]core.ToolCallTrace, 0, len(c.items))
	for _, it := range c.items {
		out = append(out, it.trace)
	}
	return out
}

func capOutput(s string) string {
	if len(s) > maxToolOutputBytes {
		return s[:maxToolOutputBytes] + `..."truncated"`
	}
	return s
}

// einoTool adapts a read-only core.AnalyzeTool onto Eino's
// tool.InvokableTool so the ReAct agent's ToolsNode can dispatch it.
// The single core.ToolResult envelope is preserved: Invoke still
// returns core.ToolResult, which this adapter JSON-marshals (capped at
// maxToolOutputBytes) into the string the model consumes.
type einoTool struct {
	impl    core.AnalyzeTool
	info    *schema.ToolInfo
	timeout time.Duration
}

func newEinoTool(t core.AnalyzeTool, timeout time.Duration) (*einoTool, error) {
	info := &schema.ToolInfo{
		Name: t.Name(),
		Desc: t.Description(),
	}
	argsSchema := t.ArgsSchema()
	if len(argsSchema) > 0 {
		raw, err := json.Marshal(argsSchema)
		if err != nil {
			return nil, fmt.Errorf("analyze: marshal schema for tool %q: %w", t.Name(), err)
		}
		js := &jsonschema.Schema{}
		if err := json.Unmarshal(raw, js); err != nil {
			return nil, fmt.Errorf("analyze: parse schema for tool %q: %w", t.Name(), err)
		}
		info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(js)
	}
	return &einoTool{impl: t, info: info, timeout: timeout}, nil
}

// Info implements tool.BaseTool.
func (e *einoTool) Info(_ context.Context) (*schema.ToolInfo, error) { return e.info, nil }

// InvokableRun implements tool.InvokableTool. Tool errors (including a
// per-tool timeout) are surfaced to the model as a structured error
// message instead of aborting the ReAct graph, mirroring the prior
// loop's resilience (the model can recover or pick another tool on the
// next turn).
func (e *einoTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	result, err := e.invoke(ctx, json.RawMessage(argumentsInJSON))
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error()), nil
	}
	b, mErr := json.Marshal(result)
	if mErr != nil {
		return fmt.Sprintf(`{"error":%q}`, "marshal tool output: "+mErr.Error()), nil
	}
	return capOutput(string(b)), nil
}

// invoke runs the wrapped tool under the per-tool timeout. A
// non-positive timeout disables the cap and the tool runs directly.
// Otherwise the tool runs against a derived deadline context; if it
// outruns the deadline (even when the tool ignores cancellation) the
// dispatch returns a timeout error promptly while the goroutine drains
// into a buffered channel, so no goroutine is leaked indefinitely.
func (e *einoTool) invoke(ctx context.Context, args json.RawMessage) (*core.ToolResult, error) {
	if e.timeout <= 0 {
		return e.impl.Invoke(ctx, args)
	}

	tctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	type result struct {
		out *core.ToolResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := e.impl.Invoke(tctx, args)
		done <- result{out: out, err: err}
	}()

	select {
	case <-tctx.Done():
		return nil, fmt.Errorf("tool %q timed out after %s", e.impl.Name(), e.timeout)
	case r := <-done:
		return r.out, r.err
	}
}
