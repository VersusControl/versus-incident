package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// openAIChatURL is the standard OpenAI chat/completions endpoint.
// Hard-coded so detect mode just works once the operator sets an API
// key — no extra base_url plumbing.
const openAIChatURL = "https://api.openai.com/v1/chat/completions"

// OpenAI is an AISRE backed by OpenAI's /chat/completions endpoint.
// One call per AgentResult; no streaming, no tool use.
type OpenAI struct {
	cfg        config.AgentAIConfig
	httpClient *http.Client

	// SampleFn extracts the sample lines passed to the model from an
	// AgentResult. Defaults to "first up to 3 messages". Exposed so
	// the worker can swap in a redaction-aware extractor without the
	// AI package depending on pkg/agent.
	SampleFn func(core.AgentResult) []string
}

// NewOpenAI constructs the analyzer. httpClient may be nil — a sane
// default (30s timeout, no proxy) is used.
func NewOpenAI(cfg config.AgentAIConfig, client *http.Client) *OpenAI {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &OpenAI{
		cfg:        cfg,
		httpClient: client,
		SampleFn:   defaultSampleFn,
	}
}

// Name implements core.AISRE.
func (o *OpenAI) Name() string { return "openai" }

// Analyze sends the result to the configured chat endpoint and parses
// the structured AIFinding from the response. The returned
// AICallResult also carries the user prompt sent, the raw model
// response, the wall-clock duration, and the model id — used by the
// detect log to render an audit trail in the UI.
func (o *OpenAI) Analyze(ctx context.Context, r core.AgentResult) (*core.AICallResult, error) {
	if o == nil {
		return nil, fmt.Errorf("ai: nil analyzer")
	}
	if o.cfg.Model == "" {
		return nil, fmt.Errorf("ai: model is empty")
	}

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

	samples := o.SampleFn(r)
	system, user := BuildPrompt(r, source, service, samples)

	body := chatRequest{
		Model:       o.cfg.Model,
		Temperature: floatOr(o.cfg.Temperature, 0.2),
		MaxTokens:   intOr(o.cfg.MaxTokens, 512),
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: &responseFormat{Type: "json_object"},
	}

	start := time.Now()
	raw, err := o.do(ctx, body)
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}

	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("ai: decode response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("ai: response had no choices")
	}
	content := strings.TrimSpace(resp.Choices[0].Message.Content)
	if content == "" {
		return nil, fmt.Errorf("ai: empty content from model")
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
		Model:       o.cfg.Model,
	}, nil
}

func (o *OpenAI) do(ctx context.Context, body chatRequest) ([]byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ai: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIChatURL, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ai: http: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ai: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ai: http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	return data, nil
}

// -----------------------------------------------------------------------------
// wire types
// -----------------------------------------------------------------------------

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

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

func floatOr(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func intOr(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
