package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
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
// ascending sort by (timestamp, event.id), and `search_after` pagination — so this
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

type recordedESPull struct {
	signals int
	cursor  time.Time
	err     error
}

type recordingESSource struct {
	source   *signalsources.ElasticsearchSource
	mu       sync.Mutex
	pulls    []recordedESPull
	commits  int
	pullDone chan struct{}
}

func newRecordingESSource(source *signalsources.ElasticsearchSource) *recordingESSource {
	return &recordingESSource{source: source, pullDone: make(chan struct{}, 4)}
}

func (source *recordingESSource) Name() string { return source.source.Name() }

func (source *recordingESSource) SetTailReplaySpan(span time.Duration) (time.Duration, time.Duration) {
	return source.source.SetTailReplaySpan(span)
}

func (source *recordingESSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	signals, cursor, err := source.source.Pull(ctx, since)
	source.mu.Lock()
	source.pulls = append(source.pulls, recordedESPull{signals: len(signals), cursor: cursor, err: err})
	source.mu.Unlock()
	select {
	case source.pullDone <- struct{}{}:
	default:
	}
	return signals, cursor, err
}

func (source *recordingESSource) Commit(ctx context.Context) error {
	if err := source.source.Commit(ctx); err != nil {
		return err
	}
	source.mu.Lock()
	source.commits++
	source.mu.Unlock()
	return nil
}

func (source *recordingESSource) snapshot() ([]recordedESPull, int) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return append([]recordedESPull(nil), source.pulls...), source.commits
}

func waitForESCursor(t *testing.T, cursors *CursorStore, source string, want time.Time) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			cursor, ok := cursors.Get(context.Background(), source)
			t.Fatalf("cursor = %v ok=%v, want %v", cursor, ok, want)
		case <-ticker.C:
			if cursor, ok := cursors.Get(context.Background(), source); ok && cursor.Equal(want) {
				return
			}
		}
	}
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
		if sa, ok := q["search_after"].([]interface{}); ok && len(sa) == 2 {
			if timestamp, timestampOK := sa[0].(string); timestampOK {
				if id, idOK := sa[1].(string); idOK {
					parsed, parseErr := time.Parse(time.RFC3339Nano, timestamp)
					if parseErr == nil {
						after, hasAfter = esTailKey(parsed, id), true
					}
				}
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
				Source: map[string]interface{}{"@timestamp": d.ts.UTC().Format(time.RFC3339Nano), "event.id": d.id, "message": d.msg},
				Sort:   []interface{}{d.ts.UTC().Format(time.RFC3339Nano), d.id},
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
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
		PageSize:      50,
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

func TestWorker_ElasticsearchTail_CappedBacklogAdvancesAcrossTicks(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	index := &esTailIndex{}
	server := httptest.NewServer(index.handler())
	defer server.Close()

	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Millisecond)
	for item := 0; item <= 10000; item++ {
		index.add(fmt.Sprintf("doc-%05d", item), "connection refused", base.Add(time.Duration(item)*time.Second))
	}

	esSource, err := signalsources.NewElasticsearchSource("capped-backlog", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 999,
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	source := newRecordingESSource(esSource)
	store := storage.NewMemory()
	catalog, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	matcher, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	cursors := NewCursorStore(nil)
	worker, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode: "training", Lookback: "5h", PollInterval: "1h",
			Catalog: config.AgentCatalogConfig{AutoPromoteAfter: 20000, PersistInterval: "1ms"},
		},
		Sources: []core.SignalSource{source}, Cursors: cursors, Matcher: matcher,
		Miner: NewMiner(0.4, 4, 100), Catalog: catalog,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := StartWorker(ctx, worker)
	select {
	case <-source.pullDone:
	case <-time.After(10 * time.Second):
		t.Fatal("first capped pull did not finish")
	}
	firstCursor := base.Add(9999 * time.Second)
	waitForESCursor(t, cursors, source.Name(), firstCursor)
	cancel()
	if !WaitForShutdownFlush(done, 10*time.Second) {
		t.Fatal("worker did not finish its shutdown flush")
	}
	pulls, commits := source.snapshot()
	if len(pulls) != 1 || pulls[0].signals != 10000 || pulls[0].err != nil || !pulls[0].cursor.Equal(firstCursor) {
		t.Fatalf("first pull = %#v, want exactly 10000 signals through cursor %v", pulls, firstCursor)
	}
	if commits == 0 {
		t.Fatal("shutdown flush did not commit the Elasticsearch delivery set")
	}
	if persisted, readErr := store.ReadBlob("patterns"); readErr != nil || len(persisted) == 0 {
		t.Fatalf("persisted catalog bytes=%d err=%v", len(persisted), readErr)
	}
	if catalog.Len() == 0 {
		t.Fatal("capped tick did not flush signals into the catalog")
	}

	worker.tickSource(context.Background(), source, "training")
	secondCursor, ok := cursors.Get(context.Background(), source.Name())
	if !ok || !secondCursor.After(firstCursor) {
		t.Fatalf("cursor after second tick = %v ok=%v, want after first cursor %v", secondCursor, ok, firstCursor)
	}
	want := base.Add(10000 * time.Second)
	if !secondCursor.Equal(want) {
		t.Fatalf("cursor after second tick = %v, want final backlog timestamp %v", secondCursor, want)
	}
	pulls, _ = source.snapshot()
	if len(pulls) != 2 || pulls[1].signals != 1 || pulls[1].err != nil || !pulls[1].cursor.Equal(want) {
		t.Fatalf("second pull = %#v, want one deferred signal through cursor %v", pulls, want)
	}
}

func TestWorker_ElasticsearchTail_SameTimestampBacklogCompletesAcrossTicks(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	index := &esTailIndex{}
	server := httptest.NewServer(index.handler())
	defer server.Close()

	timestamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	for item := 0; item <= maximumTailTestItems; item++ {
		index.add(fmt.Sprintf("doc-%05d", item), "connection refused", timestamp)
	}

	esSource, err := signalsources.NewElasticsearchSource("same-timestamp", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 1000,
		TieBreakerField: "event.id",
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	source := newRecordingESSource(esSource)
	catalog, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	matcher, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	cursors := NewCursorStore(nil)
	worker, err := NewWorker(WorkerOptions{
		Cfg:     config.AgentConfig{Mode: "training", Lookback: "5m", Catalog: config.AgentCatalogConfig{AutoPromoteAfter: 20000}},
		Sources: []core.SignalSource{source}, Cursors: cursors, Matcher: matcher,
		Miner: NewMiner(0.4, 4, 100), Catalog: catalog,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	for tick := 0; tick < 3; tick++ {
		worker.tickSource(context.Background(), source, "training")
	}
	pulls, _ := source.snapshot()
	if len(pulls) != 3 || pulls[0].signals != maximumTailTestItems || pulls[1].signals != 1 || pulls[2].signals != 0 {
		t.Fatalf("pulls = %#v, want 10000 then 1 then 0 same-timestamp signals", pulls)
	}
	for index, pull := range pulls {
		if pull.err != nil || !pull.cursor.Equal(timestamp) {
			t.Fatalf("pull[%d] = %#v, want successful cursor %v", index, pull, timestamp)
		}
	}
}

const maximumTailTestItems = 10000

func TestWorker_ElasticsearchTail_DecodedByteBudgetPersistsAndAdvances(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	hits := []map[string]interface{}{
		{"_id": "large", "_source": map[string]interface{}{"@timestamp": base.Format(time.RFC3339Nano), "event.id": "large", "message": "accepted-large", "payload": strings.Repeat("x", 5<<20)}, "sort": []interface{}{base.UnixMilli(), "large"}},
		{"_id": "small", "_source": map[string]interface{}{"@timestamp": base.Add(time.Second).Format(time.RFC3339Nano), "event.id": "small", "message": "accepted-small"}, "sort": []interface{}{base.Add(time.Second).UnixMilli(), "small"}},
		{"_id": "deferred", "_source": map[string]interface{}{"@timestamp": base.Add(2 * time.Second).Format(time.RFC3339Nano), "event.id": "deferred", "message": "deferred", "payload": strings.Repeat("y", 4<<20)}, "sort": []interface{}{base.Add(2 * time.Second).UnixMilli(), "deferred"}},
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		var responseHits []map[string]interface{}
		switch requests {
		case 1:
			responseHits = hits[:1]
		case 2:
			responseHits = hits[1:2]
		case 3, 4:
			responseHits = hits[2:]
		}
		_ = json.NewEncoder(writer).Encode(map[string]interface{}{"hits": map[string]interface{}{"hits": responseHits}})
	}))
	defer server.Close()

	esSource, err := signalsources.NewElasticsearchSource("byte-budget", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 1, ExtraFields: []string{"payload"},
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	source := newRecordingESSource(esSource)
	store := storage.NewMemory()
	catalog, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	matcher, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	cursors := NewCursorStore(nil)
	worker, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode: "training", Lookback: "5m", PollInterval: "1h",
			Catalog: config.AgentCatalogConfig{AutoPromoteAfter: 100, PersistInterval: "1ms"},
		},
		Sources: []core.SignalSource{source}, Cursors: cursors, Matcher: matcher,
		Miner: NewMiner(0.4, 4, 100), Catalog: catalog,
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := StartWorker(ctx, worker)
	select {
	case <-source.pullDone:
	case <-time.After(10 * time.Second):
		t.Fatal("first byte-budget pull did not finish")
	}
	firstCursor := base.Add(time.Second)
	waitForESCursor(t, cursors, source.Name(), firstCursor)
	cancel()
	if !WaitForShutdownFlush(done, 10*time.Second) {
		t.Fatal("worker did not finish its shutdown flush")
	}
	pulls, commits := source.snapshot()
	if len(pulls) != 1 || pulls[0].signals != 2 || pulls[0].err != nil || !pulls[0].cursor.Equal(firstCursor) {
		t.Fatalf("first pull = %#v, want two accepted signals through cursor %v", pulls, firstCursor)
	}
	if commits == 0 {
		t.Fatal("shutdown flush did not commit the partial byte-budget batch")
	}
	if persisted, readErr := store.ReadBlob("patterns"); readErr != nil || len(persisted) == 0 {
		t.Fatalf("persisted catalog bytes=%d err=%v", len(persisted), readErr)
	}
	if got := catalog.Len(); got != 2 {
		t.Fatalf("catalog patterns after partial batch = %d, want 2; deferred document was included", got)
	}

	worker.tickSource(context.Background(), source, "training")
	secondCursor, ok := cursors.Get(context.Background(), source.Name())
	wantCursor := base.Add(2 * time.Second)
	if !ok || !secondCursor.Equal(wantCursor) {
		t.Fatalf("cursor after second tick = %v ok=%v, want %v", secondCursor, ok, wantCursor)
	}
	pulls, _ = source.snapshot()
	if len(pulls) != 2 || pulls[1].signals != 1 || pulls[1].err != nil || !pulls[1].cursor.Equal(wantCursor) {
		t.Fatalf("second pull = %#v, want one deferred signal through cursor %v", pulls, wantCursor)
	}
	if got := catalog.Len(); got != 3 {
		t.Fatalf("catalog patterns after second tick = %d, want 3", got)
	}
}
