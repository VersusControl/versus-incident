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

func newClientHittingServer(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	return srv.Client()
}

func newOpenAIForTest(t *testing.T, srv *httptest.Server, attempts int) *OpenAI {
	t.Helper()
	o := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k"},
		newClientHittingServer(t, srv),
		attempts,
		1*time.Millisecond, // initial backoff
		2*time.Millisecond, // max backoff
	)
	// Make sleep & jitter deterministic + cheap.
	o.sleep = func(time.Duration) {}
	o.rng = nil
	return o
}

// rewrite the request URL so the analyzer hits our httptest server
// instead of api.openai.com. We do this by overriding the transport.
type rewritingTransport struct {
	dst http.RoundTripper
	url string
}

func (r *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Replace scheme+host with the test server's.
	new, err := http.NewRequestWithContext(req.Context(), req.Method, r.url, req.Body)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Header {
		new.Header[k] = v
	}
	return r.dst.RoundTrip(new)
}

func withHandler(t *testing.T, handler http.HandlerFunc) (*OpenAI, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	o := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k"},
		&http.Client{
			Transport: &rewritingTransport{dst: http.DefaultTransport, url: srv.URL},
			Timeout:   2 * time.Second,
		},
		3,
		1*time.Millisecond,
		2*time.Millisecond,
	)
	o.sleep = func(time.Duration) {}
	o.rng = nil
	return o, srv
}

func goodResponse() string {
	// Minimum JSON for ParseFinding — title/severity present, JSON
	// well-formed (the body is a JSON object containing the fields
	// ParseFinding looks for).
	return `{"choices":[{"message":{"content":"{\"verdict\":\"unknown\",\"title\":\"t\",\"severity\":\"warn\",\"category\":\"infra\",\"confidence\":0.5,\"summary\":\"s\",\"suggested_actions\":[]}"}}]}`
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

	o := NewOpenAIWithRetry(
		config.AgentAIConfig{Model: "test-model", APIKey: "k"},
		&http.Client{
			Transport: &rewritingTransport{dst: http.DefaultTransport, url: srv.URL},
			Timeout:   2 * time.Second,
		},
		1, 0, 0,
	)

	if _, err := o.Analyze(context.Background(), sampleResult()); err == nil {
		t.Fatal("expected error")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls=%d want 1", calls)
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
