package agent

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// The OSS agent build never imports enterprise, so the metric/trace kinds are
// unregistered here. Register them once through the same OSS seam the
// enterprise module uses, so these tests can exercise the metrics/traces path.
func init() {
	signalsources.RegisterKind("prometheus", signalsources.KindMetrics)
	signalsources.RegisterKind("traces", signalsources.KindTraces)
}

// newKindWorker builds a Worker over the given sources with the STOCK logs
// default pattern ((?i).*error.*), so we can prove that a logs source keeps the
// error-only filter while a metrics/traces source learns all. The optional
// regex config carries any top-level per-kind override under test.
func newKindWorker(t *testing.T, regex config.AgentRegexConfig, sources []config.AgentSourceConfig) *Worker {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	logs, errs := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: "(?i).*error.*"})
	if len(errs) > 0 {
		t.Fatalf("NewRegexMatcher: %v", errs)
	}
	w, err := NewWorker(WorkerOptions{
		Cfg:     config.AgentConfig{Sources: sources, Regex: regex},
		Matcher: logs,
		Miner:   NewMiner(0.4, 4, 100),
		Catalog: cat,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// TestMatcherForSource_ByKind proves the per-source-KIND regex default: a logs
// source resolves the stock logs matcher (error-only), while metrics/traces
// sources resolve the built-in match-all matcher — so a metric/trace message
// with no "error" in it is NOT dropped on the log-brain path, with the stock
// config.yaml and zero per-source config.
func TestMatcherForSource_ByKind(t *testing.T) {
	w := newKindWorker(t, config.AgentRegexConfig{}, []config.AgentSourceConfig{
		{Name: "applogs", Type: "loki", Enable: true},
		{Name: "promsrc", Type: "prometheus", Enable: true},
		{Name: "tracesrc", Type: "traces", Enable: true},
	})

	// Logs source → the stock logs matcher (same pointer), error-only.
	logsM := w.matcherForSource("applogs")
	if logsM != w.matcher {
		t.Errorf("logs source did not resolve the logs matcher")
	}
	if logsM.Match("metric ec2-i-0abcd1234/5xx = 5.35").Matched() {
		t.Errorf("logs matcher should NOT match a non-error metric message")
	}
	if !logsM.Match("connection error: refused").Matched() {
		t.Errorf("logs matcher should still match an error message")
	}

	// Metrics + traces sources → the match-all matcher (same pointer), and it
	// matches a metric/trace message that carries no "error".
	for _, name := range []string{"promsrc", "tracesrc"} {
		m := w.matcherForSource(name)
		if m != w.matchAll {
			t.Errorf("%s: did not resolve the match-all matcher", name)
		}
		if !m.Match("metric ec2-i-0abcd1234/5xx = 5.35").Matched() {
			t.Errorf("%s: match-all matcher should match a non-error metric message", name)
		}
	}
}

// TestMatcherForSource_PerKindOverride proves precedence #1: the OPTIONAL
// top-level per-kind override (agent.regex.metrics / agent.regex.traces) wins
// over the learn-all default and applies to every source of that kind, while a
// kind with no override still learns all.
func TestMatcherForSource_PerKindOverride(t *testing.T) {
	w := newKindWorker(t, config.AgentRegexConfig{Metrics: "(?i)5xx"}, []config.AgentSourceConfig{
		{Name: "promsrc", Type: "prometheus", Enable: true},
		{Name: "tracesrc", Type: "traces", Enable: true},
	})

	// metrics override is set → metrics source resolves a dedicated matcher
	// that filters non-matching metric messages.
	m := w.matcherForSource("promsrc")
	if m == w.matchAll || m == w.matcher {
		t.Errorf("metrics override should resolve a dedicated per-kind matcher")
	}
	if !m.Match("metric 5xx spike").Matched() {
		t.Errorf("override matcher should match its pattern")
	}
	if m.Match("metric latency = 12ms").Matched() {
		t.Errorf("override matcher should NOT match outside its pattern")
	}

	// traces override is NOT set → traces source still learns all.
	if tm := w.matcherForSource("tracesrc"); tm != w.matchAll {
		t.Errorf("traces with no override should resolve the match-all matcher")
	}
}

// TestKindOf_AgreesWithLogBrainSeam is the OSS drift guard: the kind registered
// for each built-in log type must equal the log brain's Kind() seam.
func TestKindOf_AgreesWithLogBrainSeam(t *testing.T) {
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	var lb core.SignalLearner = newLogBrain("x", NewMiner(0.4, 4, 100), cat, nil, nil, 0.2, config.AgentCatalogConfig{}, nil, 0)
	for _, typ := range []string{"elasticsearch", "file", "loki", "cloudwatchlogs", "graylog", "splunk"} {
		if got := string(signalsources.KindOf(typ)); got != lb.Kind() {
			t.Errorf("KindOf(%q) = %q, log brain Kind() = %q (drift)", typ, got, lb.Kind())
		}
	}
}
