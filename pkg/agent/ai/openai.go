package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
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

	// Retry parameters. Zero values mean "no retry beyond the first
	// attempt" — set by NewOpenAIWithRetry.
	maxAttempts    int
	initialBackoff time.Duration
	maxBackoff     time.Duration

	// sleep is the function used to wait between retries. Exposed for
	// tests so they don't have to wait real wall-clock time.
	sleep func(time.Duration)
	// rng generates jitter; injectable so tests can pin it.
	rng *rand.Rand
}

// NewOpenAI constructs the analyzer. httpClient may be nil — a sane
// default (30s timeout, no proxy) is used. No retry — backwards
// compatible.
func NewOpenAI(cfg config.AgentAIConfig, client *http.Client) *OpenAI {
	return NewOpenAIWithRetry(cfg, client, 1, 0, 0)
}

// NewOpenAIWithRetry constructs the analyzer with explicit retry
// parameters. maxAttempts <= 1 disables retry. Each retry sleeps
// `min(maxBackoff, initialBackoff * 2^(attempt-1))` plus up to 25%
// jitter. Retries fire on HTTP 429, any 5xx, or any network/transport
// error. 4xx other than 429 surface immediately (the request is
// fundamentally broken — retrying won't help).
func NewOpenAIWithRetry(cfg config.AgentAIConfig, client *http.Client, maxAttempts int, initialBackoff, maxBackoff time.Duration) *OpenAI {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &OpenAI{
		cfg:            cfg,
		httpClient:     client,
		SampleFn:       defaultSampleFn,
		maxAttempts:    maxAttempts,
		initialBackoff: initialBackoff,
		maxBackoff:     maxBackoff,
		sleep:          time.Sleep,
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
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

	attempts := o.maxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		// Ctx already cancelled — give up immediately so callers see
		// the underlying error, not a "retry exhausted" wrapper.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("ai: http: %w", err)
		}

		data, status, err := o.doOnce(ctx, buf)
		if err == nil {
			return data, nil
		}
		lastErr = err

		// Don't retry on non-retryable HTTP statuses (4xx other than
		// 429). The first attempt either succeeds or we surface the
		// error so the operator can fix config (bad API key, bad
		// model name, etc.).
		if !isRetryable(status, err) || attempt == attempts {
			return nil, lastErr
		}
		// Compute exponential backoff with jitter.
		backoff := o.initialBackoff
		for i := 1; i < attempt; i++ {
			backoff *= 2
			if o.maxBackoff > 0 && backoff > o.maxBackoff {
				backoff = o.maxBackoff
				break
			}
		}
		if o.rng != nil && backoff > 0 {
			jitter := time.Duration(o.rng.Int63n(int64(backoff / 4)))
			backoff += jitter
		}
		o.sleep(backoff)
	}
	return nil, lastErr
}

// doOnce performs a single HTTP attempt. Returns the response body
// (nil on error), the HTTP status code (0 when no response was
// received), and any error. The status code lets the caller tell
// retryable failures (429, 5xx) from non-retryable (4xx other).
func (o *OpenAI) doOnce(ctx context.Context, payload []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIChatURL, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, fmt.Errorf("ai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	}

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("ai: http: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("ai: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("ai: http %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	return data, resp.StatusCode, nil
}

// isRetryable reports whether a failed attempt is worth retrying.
// status==0 means the request never got a response (network/transport
// failure) — usually transient. status==429 (rate limit) and 5xx
// (server-side) are also transient. Anything else (400, 401, 403, 404,
// 422…) is a client error that won't fix itself.
func isRetryable(status int, err error) bool {
	if status == 0 {
		return err != nil
	}
	if status == http.StatusTooManyRequests {
		return true
	}
	if status >= 500 && status < 600 {
		return true
	}
	return false
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
