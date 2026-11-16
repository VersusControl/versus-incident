package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// cappedTimelineSource is a cursor-respecting source over a fixed timeline
// that, like Loki, returns at most limit lines per read (oldest first) and
// only lines up to horizon — the test advances horizon to model time passing.
type cappedTimelineSource struct {
	name    string
	lines   []core.Signal // sorted by Timestamp
	limit   int
	horizon time.Time
}

func (s *cappedTimelineSource) Name() string { return s.name }
func (s *cappedTimelineSource) Pull(_ context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	var out []core.Signal
	cursor := since
	for _, l := range s.lines {
		if !l.Timestamp.After(since) || l.Timestamp.After(s.horizon) {
			continue
		}
		if len(out) == s.limit {
			break
		}
		out = append(out, l)
		cursor = l.Timestamp
	}
	return out, cursor, nil
}

// steadyTimeline writes, every 2 s from start to end, one line for each of two
// error patterns (0.5/s each) and three unmatched info lines.
func steadyTimeline(start, end time.Time) []core.Signal {
	var lines []core.Signal
	n := 0
	for ts := start; !ts.After(end); ts = ts.Add(2 * time.Second) {
		n++
		lines = append(lines,
			core.Signal{Timestamp: ts, Message: fmt.Sprintf("service=checkout error payment declined id=%d", n)},
			core.Signal{Timestamp: ts, Message: fmt.Sprintf("service=cart error stock lookup failed id=%d", n)},
		)
		for i := 0; i < 3; i++ {
			lines = append(lines, core.Signal{Timestamp: ts, Message: fmt.Sprintf("service=web info request served id=%d-%d", n, i)})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Timestamp.Before(lines[j].Timestamp) })
	return lines
}

// A fresh agent against a source that already holds minutes of history reads
// it in two capped catch-up ticks: the first stops at 500 lines (about 200 s
// of history), the second reads the rest. Each tick is folded over the source
// time it read, so the learned rate is the real 0.5/s from the first folded
// tick on, and stays there through the steady ticks that follow.
func TestWorkerCappedBackfillLearnsRealRateFromFirstTick(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	now := time.Now().UTC()
	start := now.Add(-20 * time.Minute)
	const history = 328 * time.Second // what the source holds at the first tick
	const steadyTicks = 6
	src := &cappedTimelineSource{
		name:    "loki",
		lines:   steadyTimeline(start, start.Add(history+steadyTicks*30*time.Second+30*time.Second)),
		limit:   500,
		horizon: start.Add(history),
	}

	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	m, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: "error"})
	svc, _ := NewServiceMatcher([]string{`service=(\w+)`})
	w, err := NewWorker(WorkerOptions{
		// The lookback reaches back to the start of the source's history.
		Cfg:      config.AgentConfig{Mode: "training", PollInterval: "30s", Lookback: "21m", Catalog: config.AgentCatalogConfig{AutoPromoteAfter: 5}},
		Sources:  []core.SignalSource{src},
		Cursors:  NewCursorStore(nil),
		Matcher:  m,
		Miner:    NewMiner(0.4, 4, 100),
		Catalog:  cat,
		Services: svc,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	ctx := context.Background()

	assertRates := func(when string) {
		t.Helper()
		pats := cat.All()
		if len(pats) != 2 {
			t.Fatalf("%s: learned %d patterns, want 2", when, len(pats))
		}
		for _, p := range pats {
			if math.Abs(p.BaselineFrequency-0.5) > 0.05 {
				t.Fatalf("%s: pattern %q baseline = %.3f/s, want ≈ 0.5/s", when, p.Template, p.BaselineFrequency)
			}
		}
	}

	w.tickSource(ctx, src, "training") // capped: 500 lines, about 200 s
	for _, p := range cat.All() {
		if p.Count != 100 {
			t.Fatalf("first tick folded %d lines of %q, want 100 (the 500-line cap)", p.Count, p.Template)
		}
	}
	assertRates("after the capped first tick")

	src.horizon = src.horizon.Add(30 * time.Second)
	w.tickSource(ctx, src, "training") // catch-up: the remaining ~158 s
	assertRates("after the catch-up tick")

	for i := 0; i < steadyTicks; i++ {
		src.horizon = src.horizon.Add(30 * time.Second)
		w.tickSource(ctx, src, "training")
	}
	assertRates("after the steady ticks")
	for _, p := range cat.All() {
		if math.Abs(p.BaselineAvg-0.5) > 0.05 {
			t.Fatalf("pattern %q average baseline = %.3f/s, want ≈ 0.5/s", p.Template, p.BaselineAvg)
		}
	}
}

// After warm-up a tick is scored over the same seconds the baseline folded it
// with: a steady catch-up tick that read two polls of history is not a spike,
// while a burst inside one poll still is.
func TestSpikeScoresTickOverItsReadWindow(t *testing.T) {
	params := resolveSpikeParams(config.AgentCatalogConfig{}, 30)
	c, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	c.SetBaselineFold(params.fold())
	for i := 0; i < 30; i++ {
		c.UpsertOver("k", "tmpl", "src", 14+i%3, 30*time.Second, 0.2, "default", "")
	}
	p := c.Get("k")
	mean := p.BaselineFrequency
	std := math.Sqrt(p.BaselineVariance)

	catchUp := evalSpikeOver(mean, std, p.Count, 30, 60*time.Second, params) // 0.5/s over 60 s
	if catchUp.Spike || math.Abs(catchUp.Rate-0.5) > 1e-9 {
		t.Fatalf("catch-up tick: spike=%v rate=%.3f, want no spike at 0.5/s", catchUp.Spike, catchUp.Rate)
	}
	perPoll := evalSpike(mean, std, p.Count, 30, params) // the same 30 lines read as one poll
	if !perPoll.Spike {
		t.Fatalf("30 lines in one poll (1/s against ≈0.5/s) scored z=%.2f, want a spike", perPoll.Z)
	}
	burst := evalSpikeOver(mean, std, p.Count, 150, 10*time.Second, params) // burst inside one poll
	if !burst.Spike || math.Abs(burst.Rate-5) > 1e-9 {
		t.Fatalf("burst: spike=%v rate=%.3f, want a spike scored per poll at 5/s", burst.Spike, burst.Rate)
	}
}

// The read span is the span of the lines' own timestamps, clipped to the
// cursor range, and unknown (zero) when it cannot be measured.
func TestReadSpan(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(s int) core.Signal { return core.Signal{Timestamp: base.Add(time.Duration(s) * time.Second)} }
	cases := []struct {
		name          string
		signals       []core.Signal
		since, cursor int
		want          time.Duration
	}{
		{"span of the lines", []core.Signal{at(10), at(200), at(50)}, 0, 200, 190 * time.Second},
		{"quiet gap before the first line is not counted", []core.Signal{at(570), at(600)}, 0, 600, 30 * time.Second},
		{"straggler older than the cursor is clipped", []core.Signal{at(-500), at(30)}, 0, 30, 30 * time.Second},
		{"lines without timestamps are ignored", []core.Signal{{}, at(5), at(45)}, 0, 45, 40 * time.Second},
		{"single timestamp is unknown", []core.Signal{at(5)}, 0, 5, 0},
		{"cursor that did not move is unknown", []core.Signal{at(5), at(9)}, 10, 10, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readSpan(tc.signals, base.Add(time.Duration(tc.since)*time.Second), base.Add(time.Duration(tc.cursor)*time.Second))
			if got != tc.want {
				t.Fatalf("readSpan = %v, want %v", got, tc.want)
			}
		})
	}
}

// A burst is scored over its own lines' span, not the whole read: a known
// pattern with 150 lines in 5 s still spikes when the same read also carries
// an unrelated line from 10 minutes earlier (a cursor left at the start of a
// quiet gap). Measured over the whole batch it would score 150/604 s ≈ 0.25/s
// and pass as normal.
func TestBurstScoredOverItsOwnSpanNotTheWholeRead(t *testing.T) {
	b, _ := newLogBrainForTest(t, config.AgentCatalogConfig{AutoPromoteAfter: 5})
	ctx := context.Background()
	burstLine := func(i int) string { return fmt.Sprintf("service=checkout error payment declined id=%d", i) }

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cursor := base
	n := 0
	// Warm up at 0.5/s: 15 lines per 30 s tick.
	for tick := 0; tick < 20; tick++ {
		since := cursor
		var batch []core.Signal
		for i := 0; i < 15; i++ {
			n++
			cursor = since.Add(time.Duration(2*(i+1)) * time.Second)
			batch = append(batch, core.Signal{Timestamp: cursor, Message: burstLine(n)})
		}
		obs, err := b.Group(withReadRange(ctx, since, cursor), batch)
		if err != nil || len(obs) != 1 {
			t.Fatalf("warm-up Group: %d observations, err %v", len(obs), err)
		}
		if err := b.Learn(ctx, obs); err != nil {
			t.Fatalf("Learn: %v", err)
		}
	}

	since := cursor
	batch := []core.Signal{{Timestamp: since.Add(time.Second), Message: "service=cart error stock lookup failed id=1"}}
	burstStart := since.Add(10 * time.Minute)
	for i := 0; i < 150; i++ {
		n++
		batch = append(batch, core.Signal{Timestamp: burstStart.Add(time.Duration(i) * 5 * time.Second / 150), Message: burstLine(n)})
	}
	cursor = batch[len(batch)-1].Timestamp
	obs, err := b.Group(withReadRange(ctx, since, cursor), batch)
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	var burst *core.Observation
	for i := range obs {
		if obs[i].Frequency == 150 {
			burst = &obs[i]
		}
	}
	if burst == nil {
		t.Fatalf("no 150-line observation in %+v", obs)
	}
	if burst.Window > 30*time.Second {
		t.Fatalf("burst window = %v, want its own ~5 s span, not the whole read", burst.Window)
	}
	mean, std, confident := b.Expected(ctx, burst.Key, burst.Timestamp)
	if err := b.Learn(ctx, []core.Observation{*burst}); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	v := b.Classify(*burst, mean, std, confident)
	if v.Class != core.VerdictSpike {
		t.Fatalf("burst verdict = %v (score %.2f, baseline %.3f), want a spike", v.Class, v.Score, mean)
	}
}
