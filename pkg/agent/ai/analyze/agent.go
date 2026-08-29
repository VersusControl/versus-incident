package analyze

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	cfg config.AgentAIConfig
	// agent is the ReAct agent used when a fixed ChatModel override was
	// supplied (tests). It is nil in production, where holder owns the
	// (re)build instead.
	agent *react.Agent
	// holder lazily (re)builds the ReAct agent — tool-calling chat model
	// plus the bound tool node — when the effective provider (or model id /
	// runtime state) changes, so an operator's runtime provider switch is
	// picked up on the next Run without a restart. Nil when a fixed
	// ChatModel override was supplied.
	holder       *einowrap.Holder[*react.Agent]
	tools        map[string]core.Tool
	toolDisplays map[string]string
	maxIter      int
}

// Options is the constructor-side bag for test plumbing.
type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	Timeout    time.Duration

	// AuthKeyFunc is an OPTIONAL per-request Authorization override passed
	// straight to the chat model's transport. Nil (the OSS default) leaves
	// the YAML-keyed header untouched.
	AuthKeyFunc func(ctx context.Context) (key string, ok bool)

	// Runtime folds optional runtime overrides (provider / enabled / key
	// state) into the model holder's rebuild signature. The zero value (the
	// OSS default) pins the configured provider and builds the agent once.
	// Ignored when ChatModel is set (a fixed override is never rebuilt).
	Runtime einowrap.RuntimeAI

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
func New(ctx context.Context, cfg config.AgentAIConfig, tools []core.Tool, opts Options) (*Agent, error) {
	toolTimeout := opts.ToolTimeout
	if toolTimeout == 0 {
		toolTimeout = defaultToolTimeout
	}

	reg := map[string]core.Tool{}
	displays := map[string]string{}
	einoTools := make([]tool.BaseTool, 0, len(tools))
	for _, t := range tools {
		if t == nil || t.Name() == "" {
			continue
		}
		reg[t.Name()] = t
		displays[t.Name()] = core.ToolDisplayName(t)
		et, err := einowrap.NewTool(t, toolTimeout, maxToolOutputBytes)
		if err != nil {
			return nil, err
		}
		einoTools = append(einoTools, et)
	}

	chat := opts.ChatModel

	maxIter := defaultMaxToolIterations

	// buildReactAgent wraps a tool-calling chat model in the ReAct graph with
	// the bound tool node. It is the per-(re)build unit: the holder calls it
	// each time the effective model signature changes.
	buildReactAgent := func(ctx context.Context, chat model.ToolCallingChatModel) (*react.Agent, error) {
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
		return reactAgent, nil
	}

	a := &Agent{
		cfg:          cfg,
		tools:        reg,
		toolDisplays: displays,
		maxIter:      maxIter,
	}

	// A fixed ChatModel override (tests) is never rebuilt: build the ReAct
	// agent once around it. Otherwise route construction through a holder so
	// a runtime provider change rebuilds the tool-calling model + ReAct graph
	// on the next Run without a restart.
	if chat != nil {
		reactAgent, err := buildReactAgent(ctx, chat)
		if err != nil {
			return nil, err
		}
		a.agent = reactAgent
		return a, nil
	}

	a.holder = einowrap.NewModelHolder(cfg, einowrap.Options{
		HTTPClient:  opts.HTTPClient,
		BaseURL:     opts.BaseURL,
		Timeout:     opts.Timeout,
		AuthKeyFunc: opts.AuthKeyFunc,
	}, opts.Runtime, func(ctx context.Context, c config.AgentAIConfig, o einowrap.Options) (*react.Agent, error) {
		base, err := einowrap.NewToolCallingChatModel(ctx, c, o)
		if err != nil {
			return nil, err
		}
		return buildReactAgent(ctx, base)
	})
	// Build once up front so a bad config (empty model, explicitly-set
	// unknown provider) still fails fast at construction.
	if _, err := a.holder.Get(ctx); err != nil {
		return nil, err
	}
	return a, nil
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

// reactAgent returns the ReAct agent for the current effective signature.
// When a fixed ChatModel override was supplied (tests) it returns the
// statically-built agent; otherwise it consults the holder, which rebuilds
// the tool-calling model + graph only when the signature changed.
func (a *Agent) reactAgent(ctx context.Context) (*react.Agent, error) {
	if a.agent != nil {
		return a.agent, nil
	}
	return a.holder.Get(ctx)
}

func (a *Agent) run(ctx context.Context, snap core.AnalyzeIncidentSnapshot) (*core.AICallResult, error) {
	user := BuildUserPrompt(snap)
	messages := []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(user),
	}

	// Resolve the ReAct agent for the current effective signature; the
	// holder rebuilds the tool-calling model + graph only when the provider
	// / model / runtime state changed (nil holder ⇒ fixed test override).
	reactAgent, err := a.reactAgent(ctx)
	if err != nil {
		return &core.AICallResult{UserPrompt: user, Model: a.cfg.Model}, fmt.Errorf("analyze: build react agent: %w", err)
	}

	// The audit trace is built from Eino tool callbacks rather than
	// hand instrumentation, so it captures every tool the framework
	// dispatches (including concurrent calls in one turn).
	collector := &traceCollector{obsCtx: ctx, toolDisplays: a.toolDisplays}
	handler := utilcb.NewHandlerHelper().
		ChatModel(collector.chatModelHandler()).
		Tool(collector.toolHandler()).
		Handler()

	core.EmitAnalyzeEvent(ctx, core.AnalyzeEvent{Kind: core.AnalyzeEventRunStarted})

	start := time.Now()
	callOpts := agent.WithComposeOptions(compose.WithCallbacks(handler))
	var out *schema.Message
	// Stream only when someone is watching. An unwatched run has nothing to
	// gain from incremental delivery, and Generate stays the simpler path.
	if core.AnalyzeObserverFrom(ctx) != nil {
		out, err = streamFinalMessage(ctx, reactAgent, messages, collector, callOpts)
	} else {
		out, err = reactAgent.Generate(ctx, messages, callOpts)
	}
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

// streamFinalMessage runs the ReAct loop in streaming mode and reassembles
// the assistant's answer from the graph's output chunks.
//
// The per-chunk events the viewer sees are NOT emitted here: they come from
// the chat model callback, which sees every model turn rather than only the
// final one and — being a single goroutine per turn — emits each turn's
// deltas and its model_finished in a fixed order. Emitting from both places
// would race and split the transcript.
//
// Only Content is carried over: the reassembled message feeds ParseFinding
// and the persisted RawResponse, both of which read Content alone.
func streamFinalMessage(
	ctx context.Context,
	reactAgent *react.Agent,
	messages []*schema.Message,
	collector *traceCollector,
	opts ...agent.AgentOption,
) (*schema.Message, error) {
	sr, err := reactAgent.Stream(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	// Runs after sr.Close(): the run must not be reported finished while a
	// model turn is still emitting events, or the client sees the terminal
	// event and stops reading before the transcript is complete.
	defer collector.waitDrains()
	defer sr.Close()

	var sb strings.Builder
	for {
		chunk, recvErr := sr.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		if chunk == nil || chunk.Content == "" {
			continue
		}
		sb.WriteString(chunk.Content)
	}
	return &schema.Message{Role: schema.Assistant, Content: sb.String()}, nil
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
	// turn counts model round-trips so the UI can group each batch of tool
	// calls under the model turn that asked for them.
	turn int64
	// obsCtx carries the run's AnalyzeObserver. Eino hands each callback its
	// own ctx, so the observer is held here rather than read from those.
	obsCtx       context.Context
	toolDisplays map[string]string
	// drains tracks the goroutines reading each streamed model turn.
	drains sync.WaitGroup
}

type modelTurnCtxKey struct{}

// nextSeq continues the tool sequence for the run-level terminal events, so
// the stream stays strictly ordered end to end.
func (c *traceCollector) nextSeq() int64 {
	return atomic.AddInt64(&c.seq, 1)
}

func (c *traceCollector) emit(ev core.AnalyzeEvent) {
	if c.obsCtx == nil {
		return
	}
	core.EmitAnalyzeEvent(c.obsCtx, ev)
}

// waitDrains blocks until every streamed model turn has been fully read and
// its events emitted.
func (c *traceCollector) waitDrains() { c.drains.Wait() }

// chatModelHandler streams the model's own turns. Without it the viewer sees
// tool calls appear from nowhere with dead air in between, which is most of
// the wall-clock time of a run.
//
// Only the assistant's content is forwarded, capped by the same capOutput used
// for tool output. The final turn's content is what gets persisted as
// RawResponse, so this exposes nothing the stored record does not already
// hold; intermediate turns are usually empty (tool calls only).
func (c *traceCollector) chatModelHandler() *utilcb.ModelCallbackHandler {
	return &utilcb.ModelCallbackHandler{
		OnStart: func(ctx context.Context, _ *callbacks.RunInfo, _ *model.CallbackInput) context.Context {
			turn := atomic.AddInt64(&c.turn, 1)
			c.emit(core.AnalyzeEvent{
				Seq:  c.nextSeq(),
				Kind: core.AnalyzeEventModelStarted,
				Turn: int(turn),
			})
			return context.WithValue(ctx, modelTurnCtxKey{}, turn)
		},
		OnEnd: func(ctx context.Context, _ *callbacks.RunInfo, output *model.CallbackOutput) context.Context {
			turn, _ := ctx.Value(modelTurnCtxKey{}).(int64)
			ev := core.AnalyzeEvent{
				Seq:  c.nextSeq(),
				Kind: core.AnalyzeEventModelFinished,
				Turn: int(turn),
			}
			if output != nil && output.Message != nil {
				ev.Output = capOutput(strings.TrimSpace(output.Message.Content))
			}
			c.emit(ev)
			return ctx
		},
		// A streamed model turn ends through this timing instead of OnEnd, so
		// without it a streamed run would never report a finished turn and the
		// viewer's cursor would spin forever. Eino hands every handler its own
		// copy of the stream, so draining it here cannot starve the graph.
		OnEndWithStreamOutput: func(ctx context.Context, _ *callbacks.RunInfo, output *schema.StreamReader[*model.CallbackOutput]) context.Context {
			turn, _ := ctx.Value(modelTurnCtxKey{}).(int64)
			c.drains.Add(1)
			go c.drainModelStream(output, turn)
			return ctx
		},
		OnError: func(ctx context.Context, _ *callbacks.RunInfo, err error) context.Context {
			turn, _ := ctx.Value(modelTurnCtxKey{}).(int64)
			ev := core.AnalyzeEvent{
				Seq:  c.nextSeq(),
				Kind: core.AnalyzeEventModelFinished,
				Turn: int(turn),
			}
			if err != nil {
				ev.Error = err.Error()
			}
			c.emit(ev)
			return ctx
		},
	}
}

// drainModelStream reads one model turn's output copy to completion, emitting
// a delta per non-empty chunk and a single model_finished when the turn ends.
// Both come from this one goroutine, so a turn's events are always ordered
// deltas-then-finished.
func (c *traceCollector) drainModelStream(output *schema.StreamReader[*model.CallbackOutput], turn int64) {
	defer c.drains.Done()
	defer output.Close()

	var sb strings.Builder
	var streamErr error
	for {
		chunk, recvErr := output.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			streamErr = recvErr
			break
		}
		if chunk == nil || chunk.Message == nil || chunk.Message.Content == "" {
			continue
		}
		sb.WriteString(chunk.Message.Content)
		c.emit(core.AnalyzeEvent{
			Seq:    c.nextSeq(),
			Kind:   core.AnalyzeEventModelDelta,
			Turn:   int(turn),
			Output: capOutput(chunk.Message.Content),
		})
	}

	ev := core.AnalyzeEvent{
		Seq:    c.nextSeq(),
		Kind:   core.AnalyzeEventModelFinished,
		Turn:   int(turn),
		Output: capOutput(strings.TrimSpace(sb.String())),
	}
	if streamErr != nil {
		ev.Error = streamErr.Error()
	}
	c.emit(ev)
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
			c.emit(core.AnalyzeEvent{
				Seq:         it.seq,
				Kind:        core.AnalyzeEventToolStarted,
				Turn:        int(atomic.LoadInt64(&c.turn)),
				Tool:        it.trace.Name,
				ToolDisplay: c.toolDisplays[it.trace.Name],
				Args:        it.trace.Args,
			})
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
			c.emit(core.AnalyzeEvent{
				Seq:         it.seq,
				Kind:        core.AnalyzeEventToolFinished,
				Tool:        it.trace.Name,
				ToolDisplay: c.toolDisplays[it.trace.Name],
				Output:      it.trace.Output,
				DurationMs:  it.trace.DurationMs,
			})
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
			c.emit(core.AnalyzeEvent{
				Seq:         it.seq,
				Kind:        core.AnalyzeEventToolError,
				Tool:        it.trace.Name,
				ToolDisplay: c.toolDisplays[it.trace.Name],
				DurationMs:  it.trace.DurationMs,
				Error:       it.trace.Error,
			})
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
