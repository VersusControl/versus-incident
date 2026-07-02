package signalsources

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// TestElasticsearch_FutureTimestampDoesNotStrandCursor is the live-bug
// regression. On the real cluster the source found log:error docs dated in the
// future (up to 2048). Because the cursor was the MAX document timestamp with
// no upper bound, tick 1 pinned the cursor at 2048 and every later tick queried
// `>= 2048`, matching nothing real — "learns the first batch then stops pulling
// until Clear-all". This test freezes the clock, adds present-day docs plus one
// future-dated doc, and proves:
//
//   - the future doc is NOT emitted (excluded by the `lte: now` scan bound);
//   - the cursor lands on the newest PRESENT doc, never the future one;
//   - a later present-day doc is picked up and advances the cursor (tailing
//     keeps working without any clear/restart);
//   - a genuinely idle tick emits nothing and holds the cursor.
func TestElasticsearch_FutureTimestampDoesNotStrandCursor(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	frozenNow := base.Add(10 * time.Minute) // 10:10 — the source's wall clock

	// Present-day docs inside the first tick's [since-window, now] range.
	fake.add("p1", "present one", base.Add(6*time.Minute)) // 10:06
	fake.add("p2", "present two", base.Add(8*time.Minute)) // 10:08 — newest present
	// A future-dated garbage doc (bad producer clock / injected). Must never
	// become the cursor and must not be tailed.
	future := time.Date(2048, 1, 6, 1, 2, 6, 0, time.UTC)
	fake.add("future", "future dated garbage", future)

	src, err := NewElasticsearchSource("tail", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	src.nowFn = func() time.Time { return frozenNow }

	// Tick 1: since = now - lookback (5m) = 10:05.
	sigs1, cur1, err := src.Pull(context.Background(), frozenNow.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	for _, s := range sigs1 {
		if s.Message == "future dated garbage" {
			t.Fatalf("future-dated doc was emitted — lte:now scan bound not applied")
		}
	}
	if len(sigs1) != 2 {
		t.Fatalf("tick1 expected 2 present signals, got %d", len(sigs1))
	}
	wantCur1 := base.Add(8 * time.Minute) // 10:08, the newest PRESENT doc
	if !cur1.Equal(wantCur1) {
		t.Fatalf("tick1 cursor = %v, want %v (must be newest present doc, NOT the future doc)", cur1, wantCur1)
	}
	if cur1.After(frozenNow) {
		t.Fatalf("tick1 cursor %v is ahead of now %v — the stall would recur", cur1, frozenNow)
	}

	// A new present-day doc arrives to the SAME running source (no clear).
	fake.add("p3", "present three", base.Add(9*time.Minute)) // 10:09

	sigs2, cur2, err := src.Pull(context.Background(), cur1)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	got := map[string]bool{}
	for _, s := range sigs2 {
		got[s.Message] = true
	}
	if !got["present three"] {
		t.Errorf("tick2 did NOT deliver the new present-day doc — tailing stalled")
	}
	if len(sigs2) != 1 {
		msgs := make([]string, len(sigs2))
		for i, s := range sigs2 {
			msgs[i] = s.Message
		}
		t.Errorf("tick2 expected exactly the 1 new doc, got %d: %v", len(sigs2), msgs)
	}
	wantCur2 := base.Add(9 * time.Minute)
	if !cur2.Equal(wantCur2) {
		t.Errorf("tick2 cursor = %v, want %v (must advance)", cur2, wantCur2)
	}

	// Tick 3: idle. No new docs → nothing emitted, cursor holds.
	sigs3, cur3, err := src.Pull(context.Background(), cur2)
	if err != nil {
		t.Fatalf("tick3: %v", err)
	}
	if len(sigs3) != 0 {
		t.Errorf("tick3 idle expected 0 signals, got %d", len(sigs3))
	}
	if !cur3.Equal(cur2) {
		t.Errorf("tick3 idle cursor moved: %v -> %v", cur2, cur3)
	}
}

// TestElasticsearch_HealsPoisonedFutureSince proves a cursor already poisoned
// into the future (e.g. persisted by a pre-fix build) self-heals: Pull clamps
// the incoming `since` back to the wall clock, so it scans the real tail this
// tick instead of an empty future window, and never returns a future cursor.
func TestElasticsearch_HealsPoisonedFutureSince(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	frozenNow := base.Add(10 * time.Minute)
	fake.add("r1", "recent one", base.Add(9*time.Minute+30*time.Second)) // 10:09:30, inside [now-window, now]

	src, err := NewElasticsearchSource("heal", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	src.nowFn = func() time.Time { return frozenNow }

	poisoned := time.Date(2048, 1, 6, 0, 0, 0, 0, time.UTC)
	sigs, cur, err := src.Pull(context.Background(), poisoned)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if cur.After(frozenNow) {
		t.Fatalf("cursor %v still ahead of now %v — poisoned since not healed", cur, frozenNow)
	}
	if len(sigs) != 1 || sigs[0].Message != "recent one" {
		t.Fatalf("expected the recent doc to be recovered after healing, got %d signals", len(sigs))
	}
}

// TestElasticsearch_BuildQueryHasUpperBound pins the scan upper bound at the
// query level: the range filter must carry `lte` so future-dated docs are
// excluded server-side, not just clamped after the fact.
func TestElasticsearch_BuildQueryHasUpperBound(t *testing.T) {
	src, err := NewElasticsearchSource("q", config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://es:9200"},
		Index:     "logs-*",
		TimeField: "@timestamp",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	lower := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	upper := lower.Add(time.Minute)
	body, err := src.buildQuery(lower, upper, nil)
	if err != nil {
		t.Fatalf("buildQuery: %v", err)
	}
	var q map[string]interface{}
	if err := json.Unmarshal(body, &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	must := q["query"].(map[string]interface{})["bool"].(map[string]interface{})["must"].([]interface{})
	var rangeTF map[string]interface{}
	for _, m := range must {
		if rng, ok := m.(map[string]interface{})["range"].(map[string]interface{}); ok {
			rangeTF = rng["@timestamp"].(map[string]interface{})
		}
	}
	if rangeTF == nil {
		t.Fatalf("no range filter in query: %s", body)
	}
	lte, ok := rangeTF["lte"].(string)
	if !ok || !strings.HasPrefix(lte, "2026-07-02T10:01:00") {
		t.Errorf("range filter missing/incorrect lte upper bound: %v (body=%s)", rangeTF["lte"], body)
	}
	if _, ok := rangeTF["gte"].(string); !ok {
		t.Errorf("range filter missing gte lower bound: %s", body)
	}
}
