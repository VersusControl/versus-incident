package agent

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
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// esTailDoc / esTailIndex / handler form a minimal stateful Elasticsearch
// `_search` stand-in — a range lower bound honouring BOTH `gt` and `gte`,
// ascending sort by (timestamp, _id), and `search_after` pagination — so this
// end-to-end test drives a REAL signalsources.ElasticsearchSource through the
// worker's learn loop against server behaviour, not a stub.
type esTailDoc struct {
	id  string
	ts  time.Time
	msg string
}

type esTailIndex struct {
	mu   sync.Mutex
	docs []esTailDoc
}

func (ix *esTailIndex) add(id, msg string, ts time.Time) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.docs = append(ix.docs, esTailDoc{id: id, ts: ts, msg: msg})
}

func esTailKey(ts time.Time, id string) string {
	return fmt.Sprintf("%020d|%s", ts.UnixNano(), id)
}

func (ix *esTailIndex) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var q map[string]interface{}
		_ = json.Unmarshal(body, &q)

		size := 500
		if s, ok := q["size"].(float64); ok {
			size = int(s)
		}

		var lower time.Time
		var inclusive, hasBound bool
		if query, ok := q["query"].(map[string]interface{}); ok {
			if b, ok := query["bool"].(map[string]interface{}); ok {
				if must, ok := b["must"].([]interface{}); ok {
					for _, m := range must {
						mm, _ := m.(map[string]interface{})
						rng, ok := mm["range"].(map[string]interface{})
						if !ok {
							continue
						}
						tf, ok := rng["@timestamp"].(map[string]interface{})
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

		ix.mu.Lock()
		docs := append([]esTailDoc(nil), ix.docs...)
		ix.mu.Unlock()
		sort.Slice(docs, func(i, j int) bool {
			return esTailKey(docs[i].ts, docs[i].id) < esTailKey(docs[j].ts, docs[j].id)
		})

		type hit struct {
			ID     string                 `json:"_id"`
			Source map[string]interface{} `json:"_source"`
			Sort   []interface{}          `json:"sort"`
		}
		var hits []hit
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
			if hasAfter && esTailKey(d.ts, d.id) <= after {
				continue
			}
			hits = append(hits, hit{
				ID:     d.id,
				Source: map[string]interface{}{"@timestamp": d.ts.UTC().Format(time.RFC3339Nano), "message": d.msg},
				Sort:   []interface{}{esTailKey(d.ts, d.id)},
			})
			if len(hits) >= size {
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"hits": map[string]interface{}{"hits": hits},
		})
	}
}

// TestWorker_ElasticsearchTail_LearnsWithoutClearAndRelearnsAfterClear is the
// founder's exact symptom, end to end through a real ElasticsearchSource and
// the worker's learn loop:
//
//   - pull once (learn the backlog),
//   - new docs keep arriving to the SAME running worker (strictly-after,
//     same-millisecond, and slightly-out-of-order) — with the bug the worker
//     stopped learning here; the fix keeps it learning WITHOUT any clear,
//   - an idle tick learns nothing (no duplicate folding / busy re-pull),
//   - and "Clear all logs" (catalog + miner + poll cursor + the source's own
//     dedup state via Rewind) still recovers a clean relearn.
func TestWorker_ElasticsearchTail_LearnsWithoutClearAndRelearnsAfterClear(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	ix := &esTailIndex{}
	ts := httptest.NewServer(ix.handler())
	defer ts.Close()

	base := time.Now().UTC().Add(-2 * time.Minute)
	ix.add("b1", "connection refused to db-01", base.Add(1*time.Second))
	ix.add("b2", "connection refused to db-02", base.Add(5*time.Second)) // C1 = base+5s

	src, err := signalsources.NewElasticsearchSource("prod", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		PageSize:  50,
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}

	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	miner := NewMiner(0.4, 4, 100)
	m, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	svc, _ := NewServiceMatcher(nil)
	cursors := NewCursorStore(nil)
	w, err := NewWorker(WorkerOptions{
		Cfg:      config.AgentConfig{Mode: "training", Lookback: "5m", Catalog: config.AgentCatalogConfig{AutoPromoteAfter: 100}},
		Sources:  []core.SignalSource{src},
		Cursors:  cursors,
		Matcher:  m,
		Miner:    miner,
		Catalog:  cat,
		Services: svc,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	ctx := context.Background()

	// Tick 1: learn the backlog.
	w.tickSource(ctx, src, "training")
	afterBacklog := cat.Len()
	if afterBacklog == 0 {
		t.Fatal("tick1 learned nothing; test setup is wrong")
	}

	// New docs arrive to the SAME running worker (no clear, no restart).
	c1 := base.Add(5 * time.Second)
	ix.add("a", "timeout talking to cache-01", c1.Add(3*time.Second))   // strictly after
	ix.add("b", "disk full on host web-07", c1)                         // same millisecond
	ix.add("c", "panic in worker goroutine 42", c1.Add(-2*time.Second)) // slightly out of order

	// Tick 2: WITHOUT clear. The founder's bug was that this learned nothing.
	// All THREE new docs (strictly-after, same-millisecond, out-of-order) are
	// distinct templates and must each be learned. Asserting all three — not
	// merely "the catalog grew" — is what makes this bite the strict-`gt` stall:
	// the pre-fix code delivered ONLY the strictly-after doc, which alone grows
	// the catalog, so a `cat.Len() > afterBacklog` check would pass on the bug.
	w.tickSource(ctx, src, "training")
	if got, want := cat.Len(), afterBacklog+3; got != want {
		t.Fatalf("tick2 learned %d patterns, want %d (afterBacklog=%d): same-millisecond / out-of-order docs stranded — the ES tail stall is not fixed", got, want, afterBacklog)
	}
	afterNew := cat.Len()

	// Tick 3: idle. No new docs must mean no new/duplicate learning.
	w.tickSource(ctx, src, "training")
	if cat.Len() != afterNew {
		t.Fatalf("idle tick changed catalog size %d -> %d (duplicate learning / busy re-pull)", afterNew, cat.Len())
	}

	// Operator clears everything, mirroring controller.clearPatterns: reset the
	// catalog + miner, rewind the poll cursor, and rewind the source's own dedup
	// state. The SAME running worker must relearn from scratch.
	if _, err := cat.ResetPatterns(); err != nil {
		t.Fatalf("ResetPatterns: %v", err)
	}
	miner.Reset()
	if err := cursors.Reset(ctx); err != nil {
		t.Fatalf("cursors.Reset: %v", err)
	}
	if err := src.Rewind(ctx); err != nil {
		t.Fatalf("source Rewind: %v", err)
	}

	w.tickSource(ctx, src, "training")
	if cat.Len() == 0 {
		t.Fatal("after clear the same running worker relearned nothing; clear-all no longer recovers the ES source")
	}
}
