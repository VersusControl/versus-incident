package signalsources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// -----------------------------------------------------------------------------
// fakeES: a stateful, tailing-aware Elasticsearch _search stand-in.
//
// It mirrors the exact behaviour the source's tail depends on:
//
//   - a range lower bound on the time field, honouring BOTH `gt` (strict, the
//     old buggy code) and `gte` (inclusive, the fix) so ONE test exercises the
//     source before and after the change;
//   - ascending sort by (timestamp, _id);
//   - `search_after` pagination.
//
// Docs can be add()ed between Pulls to reproduce the founder's live scenario:
// new documents keep arriving to the SAME running worker/source.
// -----------------------------------------------------------------------------

type fakeESDoc struct {
	id  string
	ts  time.Time
	src map[string]interface{}
}

type fakeES struct {
	mu        sync.Mutex
	docs      []fakeESDoc
	timeField string
}

func newFakeES(timeField string) *fakeES {
	if timeField == "" {
		timeField = "@timestamp"
	}
	return &fakeES{timeField: timeField}
}

func (f *fakeES) add(id, message string, ts time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, fakeESDoc{
		id: id,
		ts: ts,
		src: map[string]interface{}{
			f.timeField: ts.UTC().Format(time.RFC3339Nano),
			"message":   message,
		},
	})
}

// esSortKey is a stable, unique, monotonically-increasing tiebreak used both as
// the emitted `sort` value and for `search_after` comparison. Nanosecond
// precision keeps same-millisecond docs individually addressable.
func esSortKey(ts time.Time, id string) string {
	return fmt.Sprintf("%020d|%s", ts.UnixNano(), id)
}

func (f *fakeES) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q map[string]interface{}
		_ = json.Unmarshal(body, &q)

		size := 500
		if s, ok := q["size"].(float64); ok {
			size = int(s)
		}

		// Extract the range lower bound + its inclusivity from
		// query.bool.must[*].range[timeField].{gt|gte}.
		var lower time.Time
		var inclusive, hasBound bool
		var upper time.Time
		var hasUpper bool
		if query, ok := q["query"].(map[string]interface{}); ok {
			if b, ok := query["bool"].(map[string]interface{}); ok {
				if must, ok := b["must"].([]interface{}); ok {
					for _, m := range must {
						mm, _ := m.(map[string]interface{})
						rng, ok := mm["range"].(map[string]interface{})
						if !ok {
							continue
						}
						tf, ok := rng[f.timeField].(map[string]interface{})
						if !ok {
							continue
						}
						if v, ok := tf["gte"].(string); ok {
							lower, _ = time.Parse(time.RFC3339Nano, v)
							inclusive, hasBound = true, true
						} else if v, ok := tf["gt"].(string); ok {
							lower, _ = time.Parse(time.RFC3339Nano, v)
							inclusive, hasBound = false, true
						}
						// Honour the inclusive upper bound the source now sends
						// (`lte: now`) so tests can prove future-dated docs are
						// excluded from the scan.
						if v, ok := tf["lte"].(string); ok {
							upper, _ = time.Parse(time.RFC3339Nano, v)
							hasUpper = true
						}
					}
				}
			}
		}

		var after string
		hasAfter := false
		if sa, ok := q["search_after"].([]interface{}); ok && len(sa) > 0 {
			if s, ok := sa[0].(string); ok {
				after, hasAfter = s, true
			}
		}

		f.mu.Lock()
		docs := append([]fakeESDoc(nil), f.docs...)
		f.mu.Unlock()

		sort.Slice(docs, func(i, j int) bool {
			return esSortKey(docs[i].ts, docs[i].id) < esSortKey(docs[j].ts, docs[j].id)
		})

		var hits []esHit
		for _, d := range docs {
			if hasBound {
				if inclusive {
					if d.ts.Before(lower) {
						continue
					}
				} else if !d.ts.After(lower) {
					continue
				}
			}
			if hasUpper && d.ts.After(upper) { // inclusive lte: skip strictly-after
				continue
			}
			if hasAfter && esSortKey(d.ts, d.id) <= after {
				continue
			}
			hits = append(hits, esHit{
				ID:     d.id,
				Source: d.src,
				Sort:   []interface{}{esSortKey(d.ts, d.id)},
			})
			if len(hits) >= size {
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(esResponse(t, hits))
	}
}

// TestElasticsearch_TailKeepsPullingWithoutClear reproduces the founder's
// feature-breaking bug: with a live ES source, new documents that arrive AT or
// BEFORE the boundary timestamp are stranded forever behind the strict `gt`
// range + max-timestamp cursor, so the agent stops learning until "Clear all
// logs" rewinds the cursor.
//
// It ticks the SAME source instance twice (no clear, no restart) and asserts
// all three new-doc shapes are delivered exactly once and the cursor advances:
//
//	(a) strictly after the cursor,
//	(b) at exactly the cursor's millisecond,
//	(c) slightly before the cursor (minor out-of-order / refresh lag).
func TestElasticsearch_TailKeepsPullingWithoutClear(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	fake.add("b1", "backlog one", base.Add(1*time.Second))
	fake.add("b2", "backlog two", base.Add(5*time.Second)) // C1 = base+5s

	src, err := NewElasticsearchSource("tail", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// Tick 1: learn the backlog. cursor = max ts.
	sigs1, cur1, err := src.Pull(context.Background(), base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if len(sigs1) != 2 {
		t.Fatalf("tick1 expected 2 backlog signals, got %d", len(sigs1))
	}
	c1 := base.Add(5 * time.Second)
	if !cur1.Equal(c1) {
		t.Fatalf("tick1 cursor = %v, want %v", cur1, c1)
	}

	// New docs arrive to the SAME running source (no clear, no restart).
	fake.add("a", "strictly after cursor", c1.Add(3*time.Second))   // (a)
	fake.add("b", "same millisecond as cursor", c1)                 // (b)
	fake.add("c", "slightly before cursor", c1.Add(-2*time.Second)) // (c)

	// Tick 2: WITHOUT clear.
	sigs2, cur2, err := src.Pull(context.Background(), cur1)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	got := map[string]bool{}
	for _, s := range sigs2 {
		got[s.Message] = true
	}
	if !got["strictly after cursor"] {
		t.Errorf("case (a) strictly-after NOT delivered")
	}
	if !got["same millisecond as cursor"] {
		t.Errorf("case (b) same-ms boundary NOT delivered (stranded by strict gt)")
	}
	if !got["slightly before cursor"] {
		t.Errorf("case (c) minor out-of-order NOT delivered (stranded below cursor)")
	}
	if len(sigs2) != 3 {
		msgs := make([]string, len(sigs2))
		for i, s := range sigs2 {
			msgs[i] = s.Message
		}
		t.Errorf("tick2 expected 3 new signals delivered exactly once, got %d: %v", len(sigs2), msgs)
	}
	wantCur := c1.Add(3 * time.Second)
	if !cur2.Equal(wantCur) {
		t.Errorf("tick2 cursor = %v, want %v (cursor must advance)", cur2, wantCur)
	}

	// Tick 3: genuinely idle (no new docs). Must not re-emit → no busy re-pull,
	// no duplicate learning; cursor stays put.
	sigs3, cur3, err := src.Pull(context.Background(), cur2)
	if err != nil {
		t.Fatalf("tick3: %v", err)
	}
	if len(sigs3) != 0 {
		msgs := make([]string, len(sigs3))
		for i, s := range sigs3 {
			msgs[i] = s.Message
		}
		t.Errorf("tick3 idle expected 0 signals, got %d (duplicate learning / busy re-pull): %v", len(sigs3), msgs)
	}
	if !cur3.Equal(cur2) {
		t.Errorf("tick3 idle cursor moved: %v -> %v", cur2, cur3)
	}
}

// TestElasticsearch_BoundaryDocsDedupedButNewDelivered proves the dedup is
// per-document, not "skip the whole overlapping tick": the re-fetched boundary
// docs are suppressed while a genuinely new doc in the same tick still flows.
func TestElasticsearch_BoundaryDocsDedupedButNewDelivered(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	fake.add("d1", "first", base)
	fake.add("d2", "second", base.Add(2*time.Second)) // cursor = base+2s

	src, err := NewElasticsearchSource("dedup", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, cur1, err := src.Pull(context.Background(), base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}

	// A brand-new doc arrives strictly after the cursor. The two boundary docs
	// (d1, d2) are STILL present in the index and fall inside the overlap
	// window, so an inclusive re-fetch will see them — they must be deduped.
	fake.add("d3", "third", cur1.Add(4*time.Second))

	sigs2, _, err := src.Pull(context.Background(), cur1)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(sigs2) != 1 || sigs2[0].Message != "third" {
		msgs := make([]string, len(sigs2))
		for i, s := range sigs2 {
			msgs[i] = s.Message
		}
		t.Fatalf("tick2 expected only the new doc 'third', got %d: %v (boundary docs re-emitted → duplicate learning)", len(sigs2), msgs)
	}
}

// TestElasticsearch_IdleSourceDoesNotRepull proves a source with no new data
// stays put across many ticks: zero signals, cursor frozen — no re-learning.
func TestElasticsearch_IdleSourceDoesNotRepull(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 4, 20, 13, 0, 0, 0, time.UTC)
	fake.add("i1", "idle one", base)
	fake.add("i2", "idle two", base.Add(1*time.Second))

	src, err := NewElasticsearchSource("idle", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, cur, err := src.Pull(context.Background(), base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	for i := 0; i < 3; i++ {
		sigs, next, err := src.Pull(context.Background(), cur)
		if err != nil {
			t.Fatalf("idle tick %d: %v", i, err)
		}
		if len(sigs) != 0 {
			t.Fatalf("idle tick %d re-emitted %d signals (duplicate learning)", i, len(sigs))
		}
		if !next.Equal(cur) {
			t.Fatalf("idle tick %d cursor drifted: %v -> %v", i, cur, next)
		}
		cur = next
	}
}
