package analyze

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

const (
	defaultMaxToolIterations = 3
	maxToolOutputBytes       = 4096
)

// Agent is the analyze-kind AIAgent. It binds the resolved per-task
// config, a tool-calling Eino chat model with the read-only tool
// catalog already attached, and an in-memory tool registry the agent
// dispatches to when the model issues a tool_call.
//
// The struct deliberately has NO Emitter / Notifier field. The
// import-graph guard test in agent_test.go asserts this so future
// edits cannot silently turn analyze into a notification path.
type Agent struct {
	cfg     config.AgentAIConfig
	chat    model.ToolCallingChatModel
	tools   map[string]core.AnalyzeTool
	maxIter int
}

// Options is the constructor-side bag for test plumbing.
type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	Timeout    time.Duration

	// ChatModel overrides the Eino chat model. When non-nil the agent
	// skips dialing OpenAI; tests pass a fake. The fake MUST already
	// have tools bound (the constructor will not call WithTools on it).
	ChatModel model.ToolCallingChatModel
}

// New constructs an analyze Agent. cfg must already be resolved for
// the analyze task (see config.AgentAIConfig.Resolve). Every tool in
// the supplied list is registered with the agent.
func New(ctx context.Context, cfg config.AgentAIConfig, tools []core.AnalyzeTool, opts Options) (*Agent, error) {
	reg := map[string]core.AnalyzeTool{}
	for _, t := range tools {
		if t == nil || t.Name() == "" {
			continue
		}
		reg[t.Name()] = t
	}

	infos, err := buildToolInfos(reg)
	if err != nil {
		return nil, err
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
		bound, err := base.WithTools(infos)
		if err != nil {
			return nil, fmt.Errorf("analyze: WithTools: %w", err)
		}
		chat = bound
	}

	return &Agent{
		cfg:     cfg,
		chat:    chat,
		tools:   reg,
		maxIter: defaultMaxToolIterations,
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

	traces := make([]core.ToolCallTrace, 0)
	start := time.Now()
	rawFinal := ""

	for iter := 0; iter <= a.maxIter; iter++ {
		out, err := a.chat.Generate(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("analyze: chat (iter %d): %w", iter, err)
		}
		if out == nil {
			return nil, fmt.Errorf("analyze: empty response (iter %d)", iter)
		}

		// If the model produced tool calls, dispatch them and loop.
		if len(out.ToolCalls) > 0 {
			if iter == a.maxIter {
				// Budget exhausted: force a final pass by appending a
				// system nudge and breaking out on next turn.
				messages = append(messages, out)
				messages = append(messages, schema.SystemMessage(
					"Tool iteration budget exhausted. Reply with the final JSON finding now."))
				continue
			}
			messages = append(messages, out)
			for _, tc := range out.ToolCalls {
				trace := a.invokeTool(ctx, tc)
				traces = append(traces, trace)
				// Build the tool message regardless of error — the model
				// needs a reply per tool_call_id or it will refuse next turn.
				content := trace.Output
				if trace.Error != "" {
					content = fmt.Sprintf(`{"error":%q}`, trace.Error)
				}
				messages = append(messages, schema.ToolMessage(content, tc.ID))
			}
			continue
		}

		// No tool calls → this is the final assistant message.
		rawFinal = strings.TrimSpace(out.Content)
		break
	}

	durationMs := time.Since(start).Milliseconds()
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

func (a *Agent) invokeTool(ctx context.Context, tc schema.ToolCall) core.ToolCallTrace {
	t := core.ToolCallTrace{
		Name: tc.Function.Name,
		Args: tc.Function.Arguments,
	}
	tool, ok := a.tools[tc.Function.Name]
	if !ok {
		t.Error = fmt.Sprintf("unknown tool %q", tc.Function.Name)
		return t
	}
	start := time.Now()
	result, err := tool.Invoke(ctx, json.RawMessage(tc.Function.Arguments))
	t.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		t.Error = err.Error()
		return t
	}
	b, mErr := json.Marshal(result)
	if mErr != nil {
		t.Error = fmt.Sprintf("marshal tool output: %v", mErr)
		return t
	}
	if len(b) > maxToolOutputBytes {
		b = append(b[:maxToolOutputBytes], []byte(`..."truncated"`)...)
	}
	t.Output = string(b)
	return t
}

// buildToolInfos converts the registry into Eino ToolInfos. Each
// AnalyzeTool's ArgsSchema is treated as JSON Schema 2020-12.
func buildToolInfos(reg map[string]core.AnalyzeTool) ([]*schema.ToolInfo, error) {
	out := make([]*schema.ToolInfo, 0, len(reg))
	for name, t := range reg {
		info := &schema.ToolInfo{
			Name: name,
			Desc: t.Description(),
		}
		argsSchema := t.ArgsSchema()
		if len(argsSchema) > 0 {
			raw, err := json.Marshal(argsSchema)
			if err != nil {
				return nil, fmt.Errorf("analyze: marshal schema for tool %q: %w", name, err)
			}
			js := &jsonschema.Schema{}
			if err := json.Unmarshal(raw, js); err != nil {
				return nil, fmt.Errorf("analyze: parse schema for tool %q: %w", name, err)
			}
			info.ParamsOneOf = schema.NewParamsOneOfByJSONSchema(js)
		}
		out = append(out, info)
	}
	return out, nil
}
