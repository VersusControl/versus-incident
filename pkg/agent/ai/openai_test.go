package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

func withHandler(t *testing.T, handler http.HandlerFunc) (*OpenAI, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Construct with provider=openai (the default URL), then override
	// chatURL to the httptest server. We can reach the unexported
	// field because the test lives in the same package; this avoids
	// adding a public seam just for tests.
	o, err := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k", Provider: "openai"},
		&http.Client{Timeout: 2 * time.Second},
		3,
		1*time.Millisecond,
		2*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	o.chatURL = srv.URL
	return o, srv
}

func goodResponse() string {
	// Minimum JSON for ParseFinding — title/severity present, JSON
	// well-formed (the body is a JSON object containing the fields
	// ParseFinding looks for).
	return `{"choices":[{"message":{"content":"{\"verdict\":\"unknown\",\"title\":\"t\",\"severity\":\"warn\",\"category\":\"infra\",\"confidence\":0.5,\"summary\":\"s\",\"suggested_actions\":[]}"}}]}`
}

// truncatedResponse returns a chat envelope whose content is a JSON
// object missing its closing brace — simulating Gemini-style
// truncation when the model hits max_tokens mid-output. The envelope
// itself is valid; ParseFinding will fail on the content.
func truncatedResponse() string {
	return `{"choices":[{"finish_reason":"length","message":{"content":"{\"title\":\"truncated mid output, no closing brace\",\"summary\":\"this response was cut o"}}]}`
}

func goodResponseWithFinishStop() string {
	return `{"choices":[{"finish_reason":"stop","message":{"content":"{\"verdict\":\"unknown\",\"title\":\"t\",\"severity\":\"warn\",\"category\":\"infra\",\"confidence\":0.5,\"summary\":\"s\",\"suggested_actions\":[]}"}}]}`
}

func contentFilterResponse() string {
	return `{"choices":[{"finish_reason":"content_filter","message":{"content":""}}]}`
}

func sampleResult() core.AgentResult {
	return core.AgentResult{
		Verdict:   core.VerdictUnknown,
		PatternID: "p1",
		Template:  "t",
		SampleSignals: []core.Signal{
			{Message: "boom"},
		},
		Frequency: 1,
	}
}

// TestOpenAI_TruncationAutoRetry verifies that a finish_reason="length"
// triggers exactly one auto-retry with doubled max_tokens. The first
// call returns a truncated JSON object; the retry returns the full
// object. Analyze should succeed using the retry's content.
func TestOpenAI_TruncationAutoRetry(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			// First call: model says it ran out of tokens.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(truncatedResponse()))
			return
		}
		// Retry: full response.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(goodResponseWithFinishStop()))
	})

	res, err := o.Analyze(context.Background(), sampleResult())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Finding == nil {
		t.Fatal("nil result/finding after auto-retry")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 calls (1 truncated + 1 retry), got %d", got)
	}
}

// TestOpenAI_TruncationRetryFailsWithClearError covers the case where
// the retry STILL returns truncated content. The error should
// explicitly mention max_tokens so operators know the fix.
func TestOpenAI_TruncationRetryFailsWithClearError(t *testing.T) {
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(truncatedResponse()))
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error when both attempts truncate")
	}
	if !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("error should mention truncation+max_tokens; got: %v", err)
	}
}

// TestOpenAI_ContentFilterErrorIsClear verifies that finish_reason=
// content_filter does NOT retry (more tokens won't unblock a safety
// filter) and surfaces a recognizable error.
func TestOpenAI_ContentFilterErrorIsClear(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(contentFilterResponse()))
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error on content_filter")
	}
	if !strings.Contains(err.Error(), "safety filter") {
		t.Fatalf("error should mention safety filter; got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("content_filter must not auto-retry; calls=%d want 1", got)
	}
}

// TestProviderURL covers the supported provider names + the empty
// default + an unknown value. The endpoints are operator-visible
// (any change is a behavior break) so we lock them down with an
// exact-match assertion.
func TestProviderURL(t *testing.T) {
	cases := []struct {
		provider string
		wantURL  string
		wantErr  bool
	}{
		{"", "https://api.openai.com/v1/chat/completions", false}, // default
		{"openai", "https://api.openai.com/v1/chat/completions", false},
		{"OpenAI", "https://api.openai.com/v1/chat/completions", false}, // case-insensitive
		{"gemini", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", false},
		{"  gemini  ", "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", false}, // whitespace-tolerant
		{"claude", "https://api.anthropic.com/v1/chat/completions", false},
		{"anthropic", "", true}, // common typo; explicit reject
		{"bedrock", "", true},
		{"openrouter", "", true},
	}
	for _, tc := range cases {
		got, err := ProviderURL(tc.provider)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("ProviderURL(%q): expected error, got url=%q", tc.provider, got)
		case !tc.wantErr && err != nil:
			t.Errorf("ProviderURL(%q): unexpected error: %v", tc.provider, err)
		case !tc.wantErr && got != tc.wantURL:
			t.Errorf("ProviderURL(%q): got %q, want %q", tc.provider, got, tc.wantURL)
		}
	}
}

// TestNewOpenAI_PropagatesProviderError ensures construction fails
// fast when the operator misconfigures the provider — otherwise the
// analyzer would 404 on every call instead of telling the operator
// at startup.
func TestNewOpenAI_PropagatesProviderError(t *testing.T) {
	if _, err := NewOpenAI(config.AgentAIConfig{Model: "m", APIKey: "k", Provider: "bogus"}, nil); err == nil {
		t.Fatal("expected error from NewOpenAI with unknown provider")
	}
}

// TestOpenAI_NameReflectsProvider verifies Name() returns the
// canonical lowercase provider name so the audit log and startup
// banner show what the analyzer is actually talking to.
func TestOpenAI_NameReflectsProvider(t *testing.T) {
	for _, in := range []string{"", "openai", "OpenAI"} {
		o, err := NewOpenAI(config.AgentAIConfig{Model: "m", APIKey: "k", Provider: in}, nil)
		if err != nil {
			t.Fatalf("construct(%q): %v", in, err)
		}
		if got := o.Name(); got != "openai" {
			t.Errorf("Name() for provider=%q: got %q, want openai", in, got)
		}
	}
	for _, p := range []string{"gemini", "claude"} {
		o, err := NewOpenAI(config.AgentAIConfig{Model: "m", APIKey: "k", Provider: p}, nil)
		if err != nil {
			t.Fatalf("construct(%q): %v", p, err)
		}
		if got := o.Name(); got != p {
			t.Errorf("Name() for provider=%q: got %q, want %q", p, got, p)
		}
	}
}

func TestOpenAI_RetriesOn429(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate"}`))
			return
		}
		_, _ = w.Write([]byte(goodResponse()))
	})

	res, err := o.Analyze(context.Background(), sampleResult())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.Finding == nil {
		t.Fatal("nil result/finding")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("calls=%d want 2", calls)
	}
}

func TestOpenAI_RetriesOn5xx(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(goodResponse()))
	})

	if _, err := o.Analyze(context.Background(), sampleResult()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("calls=%d want 3", calls)
	}
}

func TestOpenAI_NoRetryOn4xx(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err=%v want it to mention 401", err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls=%d want 1 (no retry on 4xx)", calls)
	}
}

func TestOpenAI_RetryExhausted(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	})

	_, err := o.Analyze(context.Background(), sampleResult())
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("calls=%d want 3 (maxAttempts)", calls)
	}
}

func TestOpenAI_NoRetryWhenAttemptsIs1(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	o, err := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k", Provider: "openai"},
		&http.Client{Timeout: 2 * time.Second},
		1, 0, 0,
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	o.chatURL = srv.URL

	if _, err := o.Analyze(context.Background(), sampleResult()); err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
}

// TestOpenAI_CancelDuringSleepReturnsFast verifies the ctx-aware
// retry sleep added in v1.4.1 hardening. Before this fix, an
// in-flight backoff blocked SIGTERM for up to MaxBackoff seconds.
// We use a server that always returns 503 and an unusually large
// backoff so the test would obviously hang without the ctx-aware
// sleep.
func TestOpenAI_CancelDuringSleepReturnsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	o, err := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k", Provider: "openai"},
		&http.Client{Timeout: 2 * time.Second},
		3,
		2*time.Second, // initial backoff — long enough that a non-ctx-aware sleep would hang
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	o.chatURL = srv.URL
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel ~50ms in, during the first retry sleep.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, analyzeErr := o.Analyze(ctx, sampleResult())
	elapsed := time.Since(start)
	if analyzeErr == nil {
		t.Fatal("expected error after cancel")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Analyze took %s — the retry sleep ignored ctx cancel", elapsed)
	}
}

func TestOpenAI_ContextCancelStopsRetry(t *testing.T) {
	var calls int32
	o, _ := withHandler(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	if _, err := o.Analyze(ctx, sampleResult()); err == nil {
		t.Fatal("expected error")
	}
	// One attempt may have raced — we just want to verify we didn't burn through all 3.
	if got := atomic.LoadInt32(&calls); got > 1 {
		t.Fatalf("calls=%d want <=1 (context already cancelled)", got)
	}
}
