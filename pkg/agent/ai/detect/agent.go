// Package detect contains the detect-kind AI agent: cheap, tool-free,
// single-call classification of unknown / spiking log patterns. It is
// the production AIAgent the worker consumes; the router also exposes
// it to admin endpoints under task kind core.AITaskDetect.
//
// Detect MUST stay tool-free. Tool wiring is reserved for the
// analyze-kind agent — letting detect call tools would turn every
// noisy log line into a multi-step LLM workflow.
package detect

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// Agent is the detect-kind AIAgent. It binds the resolved per-task
// config, the Eino chat model, and a sample extractor.
type Agent struct {
	cfg config.AgentAIConfig
	// chat is the holder that lazily (re)builds the tool-free Eino base
	// chat model. We type its artifact as BaseChatModel (not
	// ToolCallingChatModel) so the compiler — and the structural guard in
	// agent_test.go — rejects any future BindTools / WithTools wiring on the
	// detect path. The holder rebuilds the model when the effective provider
	// (or model id / runtime state) changes, so an operator's runtime
	// provider switch is picked up on the next Run without a restart.
	chat *einowrap.Holder[model.BaseChatModel]

	// SampleFn picks the lines forwarded to the model from an
	// AgentResult. Defaults to "first up to 3 non-empty messages".
	// Exposed so callers can swap in a redaction-aware extractor.
	SampleFn func(core.AgentResult) []string
}

// Options is the constructor-side bag for test plumbing. Production
// callers pass a zero Options{} and let the agent dial OpenAI directly.
type Options struct {
	// HTTPClient overrides the default *http.Client. Optional.
	HTTPClient *http.Client
	// BaseURL overrides the chat/completions endpoint. Tests use this
	// to point at an httptest server; production leaves it empty.
	BaseURL string
	// Timeout caps each chat call. Defaults to 30s.
	Timeout time.Duration
	// AuthKeyFunc is an OPTIONAL per-request Authorization override passed
	// straight to the chat model's transport. Nil (the OSS default) leaves
	// the YAML-keyed header untouched.
	AuthKeyFunc func(ctx context.Context) (key string, ok bool)

	// Runtime folds optional runtime overrides (provider / enabled / key
	// state) into the model holder's rebuild signature. The zero value (the
	// OSS default) pins the configured provider and builds the model once.
	Runtime einowrap.RuntimeAI
}

// New constructs a detect Agent. cfg must already be resolved for the
// detect task (see config.AgentAIConfig.Resolve).
func New(ctx context.Context, cfg config.AgentAIConfig, opts Options) (*Agent, error) {
	holder := einowrap.NewChatModelHolder(cfg, einowrap.Options{
		HTTPClient:  opts.HTTPClient,
		BaseURL:     opts.BaseURL,
		Timeout:     opts.Timeout,
		AuthKeyFunc: opts.AuthKeyFunc,
	}, opts.Runtime)
	// Build once up front so a bad config (empty model, explicitly-set
	// unknown provider) still fails fast at construction, exactly as the
	// pre-holder wiring did.
	if _, err := holder.Get(ctx); err != nil {
		return nil, err
	}
	return &Agent{
		cfg:      cfg,
		chat:     holder,
		SampleFn: defaultSampleFn,
	}, nil
}

// Name implements core.AIAgent.
func (a *Agent) Name() string { return "detect" }

// Kind implements core.AIAgent.
func (a *Agent) Kind() core.AITaskKind { return core.AITaskDetect }

// ChatModel exposes the underlying Eino model. Only used by the
// tool-free guard test in agent_test.go. It returns the currently-built
// model from the holder (built eagerly in New); it never triggers a rebuild.
func (a *Agent) ChatModel() model.BaseChatModel { return a.chat.Current() }

// Run implements core.AIAgent. It rejects any task that is not a
// DetectTask; the router enforces kind routing but we double-check.
func (a *Agent) Run(ctx context.Context, task core.AITask) (*core.AICallResult, error) {
	if a == nil {
		return nil, fmt.Errorf("detect: nil agent")
	}
	if task == nil {
		return nil, fmt.Errorf("detect: nil task")
	}
	dt, ok := task.(core.DetectTask)
	if !ok {
		return nil, fmt.Errorf("detect: expected DetectTask, got %s", task.Kind())
	}
	return a.analyze(ctx, dt.Result)
}

func (a *Agent) analyze(ctx context.Context, r core.AgentResult) (*core.AICallResult, error) {
	source := ""
	service := ""
	if len(r.SampleSignals) > 0 {
		source = r.SampleSignals[0].Source
		if v, ok := r.SampleSignals[0].Fields["service"]; ok {
			if s, ok := v.(string); ok {
				service = s
			}
		}
	}

	samples := a.SampleFn(r)
	system, user := BuildPrompt(r, source, service, samples)

	// Resolve the model for the current effective signature; the holder
	// rebuilds it only when the provider / model / runtime state changed,
	// so a steady tick reuses the cached model at no cost.
	chat, err := a.chat.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect: build chat model: %w", err)
	}

	start := time.Now()
	out, err := chat.Generate(ctx, []*schema.Message{
		schema.SystemMessage(system),
		schema.UserMessage(user),
	})
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("detect: chat: %w", err)
	}
	if out == nil {
		return nil, fmt.Errorf("detect: empty response")
	}

	content := strings.TrimSpace(out.Content)
	if content == "" {
		return nil, fmt.Errorf("detect: empty content from model")
	}

	finding, err := ParseFinding(content)
	if err != nil {
		return nil, err
	}
	if finding.SampleIDs == nil && r.PatternID != "" {
		finding.SampleIDs = []string{r.PatternID}
	}

	return &core.AICallResult{
		Finding:     finding,
		UserPrompt:  user,
		RawResponse: content,
		DurationMs:  durationMs,
		Model:       a.cfg.Model,
	}, nil
}

func defaultSampleFn(r core.AgentResult) []string {
	out := make([]string, 0, 3)
	for _, s := range r.SampleSignals {
		if s.Message == "" {
			continue
		}
		out = append(out, s.Message)
		if len(out) == 3 {
			break
		}
	}
	return out
}
