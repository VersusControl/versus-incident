package tools

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
)

func hasTool(tools []core.AnalyzeTool, name string) bool {
	for _, t := range tools {
		if t.Name() == name {
			return true
		}
	}
	return false
}

// TestDefault_FindRunbookRegistration verifies find_runbook is wired
// only when BOTH the embedder and the runbook searcher are present, so a
// community install with no embeddings configured never registers it.
func TestDefault_FindRunbookRegistration(t *testing.T) {
	emb := &recordingEmbedder{}
	idx := &fakeSearcher{}

	cases := []struct {
		name     string
		embedder core.Embedder
		runbooks RunbookSearcher
		want     bool
	}{
		{"both present", emb, idx, true},
		{"embedder only", emb, nil, false},
		{"searcher only", nil, idx, false},
		{"neither", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := Default(nil, nil, nil, nil, nil, nil, nil, tc.embedder, tc.runbooks, nil, nil)
			if got := hasTool(tools, "find_runbook"); got != tc.want {
				t.Errorf("find_runbook registered = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDefault_MetricTraceRegistration verifies query_metrics and
// query_traces are wired only when their respective readers are present,
// so a community install without a metric/trace backend never registers
// them.
func TestDefault_MetricTraceRegistration(t *testing.T) {
	metrics := &fakeMetricReader{}
	traces := &fakeTraceReader{}

	none := Default(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if hasTool(none, "query_metrics") || hasTool(none, "query_traces") {
		t.Fatal("metric/trace tools registered with nil readers")
	}

	both := Default(nil, nil, nil, nil, nil, nil, nil, nil, nil, metrics, traces)
	if !hasTool(both, "query_metrics") {
		t.Error("query_metrics not registered with a reader")
	}
	if !hasTool(both, "query_traces") {
		t.Error("query_traces not registered with a reader")
	}

	metricsOnly := Default(nil, nil, nil, nil, nil, nil, nil, nil, nil, metrics, nil)
	if !hasTool(metricsOnly, "query_metrics") || hasTool(metricsOnly, "query_traces") {
		t.Error("metrics-only registration wrong")
	}
}
