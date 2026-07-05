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
	return newLogBrain("es:test", NewMiner(0.4, 4, 100), c, m, svc, 0.2, cat, nil), c
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
	b := newLogBrain("es:test", NewMiner(0.4, 4, 100), c, m, svc, 0.2, config.AgentCatalogConfig{}, nil)

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
// snapshot Expected (pre-fold), fold via Learn, Classify against the snapshot,
// then Promote on the learn path (sequenced AFTER Classify so the detector read
// the pre-fold verdict). This is the contract the seam refactor must preserve.
func classifyOnce(t *testing.T, b *logBrain, o core.Observation) core.TypedVerdict {
	t.Helper()
	mean, std, conf := b.Expected(context.Background(), o.Key, o.Timestamp)
	if err := b.Learn(context.Background(), []core.Observation{o}); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	v := b.Classify(o, mean, std, conf)
	b.Promote(o.Key)
	return v
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

// --- auto_promote_after threshold semantics (QA-028) --------------------------
// Spike is left disabled (SpikeMultiplier == 0) throughout so it can never mask
// the count-based promotion verdict under test.

// (a) The shipped default (100, supplied by the embedded default_config layer
// for an unset key) still promotes exactly at the 100th sighting.
func TestLogBrain_AutoPromoteAfter_DefaultPromotesAt100(t *testing.T) {
	b, c := newLogBrainForTest(t, config.AgentCatalogConfig{AutoPromoteAfter: 100})

	for i := 0; i < 99; i++ {
		classifyOnce(t, b, logObs("p", 1))
	}
	if got := c.Get("p").Count; got != 99 {
		t.Fatalf("count = %d, want 99", got)
	}
	if c.Get("p").Verdict == "known" {
		t.Fatalf("promoted early at count=99 (threshold 100)")
	}

	v := classifyOnce(t, b, logObs("p", 1)) // 100th sighting crosses the threshold
	if got := c.Get("p").Count; got != 100 {
		t.Fatalf("count = %d, want 100", got)
	}
	if v.Class != core.VerdictKnownPattern {
		t.Fatalf("at-threshold verdict = %v, want known", v.Class)
	}
	if c.Get("p").Verdict != "known" {
		t.Fatalf("catalog verdict = %q, want known (MarkKnown must fire at 100)", c.Get("p").Verdict)
	}
}

// (b) A positive custom threshold promotes exactly at that count, not at 100.
func TestLogBrain_AutoPromoteAfter_CustomThresholdPromotes(t *testing.T) {
	b, c := newLogBrainForTest(t, config.AgentCatalogConfig{AutoPromoteAfter: 50})

	for i := 0; i < 49; i++ {
		classifyOnce(t, b, logObs("p", 1))
	}
	if c.Get("p").Verdict == "known" {
		t.Fatalf("promoted before custom threshold 50 (count=%d)", c.Get("p").Count)
	}

	v := classifyOnce(t, b, logObs("p", 1)) // 50th sighting
	if got := c.Get("p").Count; got != 50 {
		t.Fatalf("count = %d, want 50", got)
	}
	if v.Class != core.VerdictKnownPattern {
		t.Fatalf("at-custom-threshold verdict = %v, want known", v.Class)
	}
	if c.Get("p").Verdict != "known" {
		t.Fatalf("catalog verdict = %q, want known at the custom threshold", c.Get("p").Verdict)
	}
}

// (c) QA-028: auto_promote_after: 0 DISABLES count-based promotion — a pattern
// is never marked "known" no matter how many times it is seen. Drives well past
// the old 100 fallback to prove the explicit 0 is honoured, not re-mapped.
func TestLogBrain_AutoPromoteAfter_ZeroDisablesPromotion(t *testing.T) {
	cat := config.AgentCatalogConfig{
		AutoPromoteAfter: 0, // documented: disables promotion
		SpikeMultiplier:  0, // disable spike so it can't mask the verdict
	}
	b, c := newLogBrainForTest(t, cat)

	var v core.TypedVerdict
	for i := 0; i < 12; i++ {
		v = classifyOnce(t, b, logObs("p", 10)) // 120 sightings, well past 100
	}
	if got := c.Get("p").Count; got != 120 {
		t.Fatalf("count = %d, want 120", got)
	}
	if c.Get("p").Verdict == "known" {
		t.Fatalf("auto_promote_after=0 promoted pattern to %q; 0 must disable promotion (QA-028)", c.Get("p").Verdict)
	}
	if v.Class != core.VerdictUnknown {
		t.Fatalf("verdict at count=120 = %v, want unknown (0 disables promotion)", v.Class)
	}
}

// (d) A negative threshold (any value ≤ 0) also disables promotion.
func TestLogBrain_AutoPromoteAfter_NegativeDisablesPromotion(t *testing.T) {
	cat := config.AgentCatalogConfig{
		AutoPromoteAfter: -1,
		SpikeMultiplier:  0,
	}
	b, c := newLogBrainForTest(t, cat)

	var v core.TypedVerdict
	for i := 0; i < 12; i++ {
		v = classifyOnce(t, b, logObs("p", 10)) // 120 sightings
	}
	if c.Get("p").Verdict == "known" {
		t.Fatalf("auto_promote_after=-1 promoted pattern to %q; any value ≤0 must disable promotion", c.Get("p").Verdict)
	}
	if v.Class != core.VerdictUnknown {
		t.Fatalf("verdict = %v, want unknown (negative disables promotion)", v.Class)
	}
}

// (e) An operator-labelled "known" pattern STAYS known even when count-based
// promotion is disabled (threshold 0). This bites the `prevVerdict == "known"`
// clause independently of the count clause: were the guard rewritten as
// `threshold > 0 && (prevVerdict == "known" || postCount >= threshold)`, a
// disabled threshold would silently un-suppress a hand-labelled pattern.
func TestLogBrain_AutoPromoteAfter_AlreadyKnownStaysKnown(t *testing.T) {
	cat := config.AgentCatalogConfig{
		AutoPromoteAfter: 0, // count-based promotion disabled
		SpikeMultiplier:  0, // disable spike so it can't mask the verdict
	}
	b, c := newLogBrainForTest(t, cat)

	// Seed the pattern (stays Unknown — 0 disables count-based promotion), then
	// label it known by hand as an operator would.
	classifyOnce(t, b, logObs("p", 5))
	if !c.MarkKnown("p") {
		t.Fatalf("MarkKnown(p) did not mark the seeded pattern")
	}
	if c.Get("p").Verdict != "known" {
		t.Fatalf("precondition: catalog verdict = %q, want known", c.Get("p").Verdict)
	}

	// Further sightings must keep it known — the prior "known" verdict wins even
	// though the count threshold is disabled.
	v := classifyOnce(t, b, logObs("p", 5))
	if v.Class != core.VerdictKnownPattern {
		t.Fatalf("already-known verdict = %v, want known (prior known must win with threshold 0)", v.Class)
	}
	if c.Get("p").Verdict != "known" {
		t.Fatalf("catalog verdict = %q, want known (an already-known pattern must stay known)", c.Get("p").Verdict)
	}
}
