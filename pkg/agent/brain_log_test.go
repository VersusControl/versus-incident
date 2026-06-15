package agent

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func newLogBrainForTest(t *testing.T, cat config.AgentCatalogConfig) (*logBrain, *Catalog) {
	t.Helper()
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	m, errs := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	if len(errs) > 0 {
		t.Fatalf("NewRegexMatcher: %v", errs)
	}
	svc, errs := NewServiceMatcher([]string{`service=(\w+)`})
	if len(errs) > 0 {
		t.Fatalf("NewServiceMatcher: %v", errs)
	}
	return newLogBrain("es:test", NewMiner(0.4, 4, 100), c, m, svc, 0.2, cat), c
}

func TestLogBrain_Kind(t *testing.T) {
	b, _ := newLogBrainForTest(t, config.AgentCatalogConfig{})
	if b.Kind() != "logs" {
		t.Fatalf("Kind = %q, want logs", b.Kind())
	}
}

func TestLogBrain_GroupClustersAndExtractsService(t *testing.T) {
	b, _ := newLogBrainForTest(t, config.AgentCatalogConfig{})
	signals := []core.Signal{
		{Message: "service=api request failed id=1"},
		{Message: "service=api request failed id=2"},
		{Message: "service=web cache miss key=x"},
		{Message: ""}, // dropped: empty message
	}
	obs, err := b.Group(context.Background(), signals)
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("expected 2 observations, got %d: %+v", len(obs), obs)
	}

	byService := map[string]core.Observation{}
	total := 0
	for _, o := range obs {
		byService[o.Service] = o
		total += o.Frequency
	}
	if total != 3 {
		t.Fatalf("total frequency = %d, want 3 (one empty message dropped)", total)
	}
	api, ok := byService["api"]
	if !ok {
		t.Fatal("no observation attributed to service=api")
	}
	if api.Frequency != 2 || api.Value != 2 || len(api.Samples) != 2 {
		t.Errorf("api obs = freq:%d value:%v samples:%d, want 2/2/2", api.Frequency, api.Value, len(api.Samples))
	}
	if !api.IsNew {
		t.Error("first sighting of the api pattern should be IsNew")
	}
	if byService["web"].Frequency != 1 {
		t.Errorf("web frequency = %d, want 1", byService["web"].Frequency)
	}
}

func TestLogBrain_GroupRespectsRegexFilter(t *testing.T) {
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	// Only lines containing ERROR are interesting.
	m, errs := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: "ERROR"})
	if len(errs) > 0 {
		t.Fatalf("NewRegexMatcher: %v", errs)
	}
	svc, _ := NewServiceMatcher(nil)
	b := newLogBrain("es:test", NewMiner(0.4, 4, 100), c, m, svc, 0.2, config.AgentCatalogConfig{})

	obs, err := b.Group(context.Background(), []core.Signal{
		{Message: "ERROR boom happened code=1"},
		{Message: "INFO all good"}, // filtered out
		{Message: "ERROR boom happened code=2"},
	})
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation (INFO filtered), got %d", len(obs))
	}
	if obs[0].Frequency != 2 {
		t.Errorf("frequency = %d, want 2", obs[0].Frequency)
	}
	if obs[0].Service != "_unknown" {
		t.Errorf("service = %q, want _unknown (no patterns configured)", obs[0].Service)
	}
}

func TestLogBrain_ExpectedAlwaysConfident(t *testing.T) {
	b, c := newLogBrainForTest(t, config.AgentCatalogConfig{})

	mean, std, ok := b.Expected(context.Background(), "missing", time.Now())
	if mean != 0 || std != 0 || !ok {
		t.Fatalf("Expected(missing) = (%v,%v,%v), want (0,0,true)", mean, std, ok)
	}

	c.Upsert("p1", "tmpl", "es:test", 7, 0.2, "default", "api")
	mean, _, ok = b.Expected(context.Background(), "p1", time.Now())
	if !ok {
		t.Fatal("Expected must always report confident for logs")
	}
	if mean <= 0 {
		t.Fatalf("Expected baseline after upsert = %v, want > 0", mean)
	}
}

func TestLogBrain_LearnUpserts(t *testing.T) {
	b, c := newLogBrainForTest(t, config.AgentCatalogConfig{})
	obs := []core.Observation{{
		Key:       "p1",
		Service:   "api",
		Signal:    "tmpl",
		Frequency: 5,
		Samples:   []core.Signal{{Message: "service=api boom"}},
	}}
	if err := b.Learn(context.Background(), obs); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	p := c.Get("p1")
	if p == nil {
		t.Fatal("pattern was not upserted")
	}
	if p.Count != 5 {
		t.Errorf("count = %d, want 5", p.Count)
	}
	if p.Service != "api" {
		t.Errorf("service = %q, want api", p.Service)
	}
	if p.Template != "tmpl" {
		t.Errorf("template = %q, want tmpl", p.Template)
	}
	if p.RuleName != "default" {
		t.Errorf("rule = %q, want default (re-derived from Samples[0])", p.RuleName)
	}
}

// classifyOnce reproduces the worker's per-observation ordering exactly:
// snapshot Expected (pre-fold), fold via Learn, then Classify against the
// snapshot. This is the contract the seam refactor must preserve.
func classifyOnce(t *testing.T, b *logBrain, o core.Observation) core.TypedVerdict {
	t.Helper()
	mean, std, conf := b.Expected(context.Background(), o.Key, o.Timestamp)
	if err := b.Learn(context.Background(), []core.Observation{o}); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	return b.Classify(o, mean, std, conf)
}

func logObs(key string, freq int) core.Observation {
	return core.Observation{
		Key:       key,
		Service:   "api",
		Signal:    "tmpl",
		Frequency: freq,
		Samples:   []core.Signal{{Message: "service=api boom"}},
	}
}

func TestLogBrain_ClassifyLifecycle(t *testing.T) {
	cat := config.AgentCatalogConfig{
		AutoPromoteAfter:      100,
		SpikeMultiplier:       5,
		SpikeMinFrequency:     5,
		SpikeMinBaselineCount: 20,
	}
	b, c := newLogBrainForTest(t, cat)

	// 1. Brand-new, under threshold → Unknown, always confident.
	v := classifyOnce(t, b, logObs("p", 10))
	if v.Class != core.VerdictUnknown {
		t.Fatalf("new pattern verdict = %v, want unknown", v.Class)
	}
	if !v.Confident {
		t.Fatal("log verdicts must always be confident")
	}

	// 2. Accumulate below threshold — still Unknown.
	for i := 0; i < 8; i++ {
		v = classifyOnce(t, b, logObs("p", 10))
	}
	if got := c.Get("p").Count; got != 90 {
		t.Fatalf("count after 9 ticks = %d, want 90", got)
	}
	if v.Class != core.VerdictUnknown {
		t.Fatalf("under-threshold verdict = %v, want unknown", v.Class)
	}

	// 3. Cross the threshold → KnownPattern (suppressed) + marked known.
	v = classifyOnce(t, b, logObs("p", 10))
	if got := c.Get("p").Count; got != 100 {
		t.Fatalf("count = %d, want 100", got)
	}
	if v.Class != core.VerdictKnownPattern {
		t.Fatalf("at-threshold verdict = %v, want known", v.Class)
	}
	if c.Get("p").Verdict != "known" {
		t.Fatalf("catalog verdict = %q, want known (MarkKnown must fire)", c.Get("p").Verdict)
	}

	// 4. Known pattern suddenly floods → Spike supersedes known.
	v = classifyOnce(t, b, logObs("p", 200))
	if v.Class != core.VerdictSpike {
		t.Fatalf("flood verdict = %v, want spike", v.Class)
	}
	if v.Score <= 1 {
		t.Errorf("spike score = %v, want > 1", v.Score)
	}
	if v.Baseline <= 0 {
		t.Errorf("spike baseline = %v, want the pre-fold baseline > 0", v.Baseline)
	}
}
