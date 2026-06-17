package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_ServesRegisteredMetrics(t *testing.T) {
	IncidentsTotal.WithLabelValues("sent").Inc()

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	if !strings.Contains(out, `versus_incidents_total{status="sent"}`) {
		t.Errorf("expected versus_incidents_total in exposition output")
	}
	// Default collectors should be present too (free observability).
	if !strings.Contains(out, "go_goroutines") {
		t.Errorf("expected Go runtime metrics from the Go collector")
	}
}

func TestRegisterAgentPatternsGauge(t *testing.T) {
	RegisterAgentPatternsGauge(func() float64 { return 7 })

	srv := httptest.NewServer(Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "versus_agent_patterns 7") {
		t.Errorf("expected versus_agent_patterns gauge = 7 in exposition output")
	}
}
