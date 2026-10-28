package signalsources

import (
	"context"
	"encoding/json"
	"errors"
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
	elasticsearchapp "github.com/VersusControl/versus-incident/pkg/elasticsearch"
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
	scrolls   map[string]*fakeESScroll
	nextID    int
}

type fakeESScroll struct {
	docs []fakeESDoc
	size int
}

func newFakeES(timeField string) *fakeES {
	if timeField == "" {
		timeField = "@timestamp"
	}
	return &fakeES{timeField: timeField, scrolls: make(map[string]*fakeESScroll)}
}

func (f *fakeES) add(id, message string, ts time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs = append(f.docs, fakeESDoc{
		id: id,
		ts: ts,
		src: map[string]interface{}{
			f.timeField: ts.UTC().Format(time.RFC3339Nano),
			"event.id":  id,
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
		if r.URL.Path == "/_search/scroll" {
			id, _ := q["scroll_id"].(string)
			if r.Method == http.MethodDelete {
				if ids, ok := q["scroll_id"].([]interface{}); ok && len(ids) == 1 {
					id, _ = ids[0].(string)
				}
				f.mu.Lock()
				delete(f.scrolls, id)
				f.mu.Unlock()
				_, _ = w.Write([]byte(`{}`))
				return
			}
			f.mu.Lock()
			state := f.scrolls[id]
			var hits []esHit
			if state != nil {
				count := min(state.size, len(state.docs))
				for _, doc := range state.docs[:count] {
					hits = append(hits, esHit{ID: doc.id, Source: doc.src, Sort: []interface{}{doc.ts.Format(time.RFC3339Nano)}})
				}
				state.docs = state.docs[count:]
			}
			f.mu.Unlock()
			response := esSearchResponse{ScrollID: id}
			response.Hits.Hits = hits
			_ = json.NewEncoder(w).Encode(response)
			return
		}

		size := 500
		if s, ok := q["size"].(float64); ok {
			size = int(s)
		}
		sortEntries, _ := q["sort"].([]interface{})

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
		if sa, ok := q["search_after"].([]interface{}); ok && len(sa) == 2 {
			if timestamp, timestampOK := sa[0].(string); timestampOK {
				if id, idOK := sa[1].(string); idOK {
					parsed, parseErr := time.Parse(time.RFC3339Nano, timestamp)
					if parseErr == nil {
						after, hasAfter = esSortKey(parsed, id), true
					}
				}
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
			sortValues := []interface{}{d.ts.UTC().Format(time.RFC3339Nano), d.id}
			if len(sortEntries) == 1 {
				sortValues = sortValues[:1]
			}
			hits = append(hits, esHit{
				ID:     d.id,
				Source: d.src,
				Sort:   sortValues,
			})
			if len(hits) >= size && len(sortEntries) != 1 {
				break
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if len(sortEntries) == 1 {
			f.mu.Lock()
			f.nextID++
			id := fmt.Sprintf("scroll-%d", f.nextID)
			remaining := hits[min(size, len(hits)):]
			f.scrolls[id] = &fakeESScroll{size: size}
			for _, hit := range remaining {
				f.scrolls[id].docs = append(f.scrolls[id].docs, fakeESDoc{id: hit.ID, src: hit.Source})
				f.scrolls[id].docs[len(f.scrolls[id].docs)-1].ts, _ = extractTime(hit.Source, f.timeField)
			}
			f.mu.Unlock()
			response := esSearchResponse{ScrollID: id}
			response.Hits.Hits = hits[:min(size, len(hits))]
			_ = json.NewEncoder(w).Encode(response)
			return
		}
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
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
		PageSize:      50,
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
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
		PageSize:      50,
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
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
		PageSize:      50,
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

func TestElasticsearch_BuildQueryWithoutTieBreakerSortsByTimeOnly(t *testing.T) {
	src, err := NewElasticsearchSource("time-only-shape", config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*",
	})
	if err != nil {
		t.Fatal(err)
	}
	lower := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	body, err := src.buildQueryPage(lower, lower.Add(time.Minute), nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	var query struct {
		Sort []map[string]interface{} `json:"sort"`
		From *int                     `json:"from"`
	}
	if err := json.Unmarshal(body, &query); err != nil {
		t.Fatal(err)
	}
	if len(query.Sort) != 1 || query.Sort[0]["@timestamp"] == nil || query.From != nil {
		t.Fatalf("sort = %#v from = %v, want only the time field and no offset", query.Sort, query.From)
	}
	if strings.Contains(string(body), "event.id") || strings.Contains(string(body), "search_after") {
		t.Fatalf("time-only query references a tie breaker: %s", body)
	}
}

// A same-timestamp burst larger than a page, split across page boundaries,
// must be delivered completely and exactly once without a tie breaker.
func TestElasticsearch_TimeOnlyPaginationDeliversSameTimestampBurstExactlyOnce(t *testing.T) {
	fake := newFakeES("@timestamp")
	var requests []string
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestsMu.Lock()
		requests = append(requests, string(body))
		requestsMu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		fake.handler(t)(w, r)
	}))
	defer server.Close()

	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Millisecond)
	var want []string
	add := func(prefix string, count int, ts time.Time) {
		for index := 0; index < count; index++ {
			id := fmt.Sprintf("%s-%02d", prefix, index)
			fake.add(id, id, ts)
			want = append(want, id)
		}
	}
	add("first", 3, base.Add(time.Second))
	add("burst", 7, base.Add(2*time.Second))
	add("last", 2, base.Add(3*time.Second))

	src, err := NewElasticsearchSource("time-only", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals, cursor, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	counts := map[string]int{}
	for _, signal := range signals {
		counts[signal.Message]++
	}
	for _, id := range want {
		if counts[id] != 1 {
			t.Errorf("%s delivered %d times, want exactly once", id, counts[id])
		}
	}
	if len(signals) != len(want) || !cursor.Equal(base.Add(3*time.Second)) {
		t.Fatalf("signals=%d cursor=%v, want %d and %v", len(signals), cursor, len(want), base.Add(3*time.Second))
	}
	for _, body := range requests {
		if strings.Contains(body, "search_after") || strings.Contains(body, "event.id") {
			t.Fatalf("time-only request used a tie breaker: %s", body)
		}
	}

	add("late", 4, base.Add(3*time.Second))
	next, _, err := src.Pull(context.Background(), cursor)
	if err != nil || len(next) != 4 {
		t.Fatalf("second pull signals=%d err=%v, want only the 4 new same-timestamp rows", len(next), err)
	}
	for _, signal := range next {
		if !strings.HasPrefix(signal.Message, "late-") {
			t.Fatalf("second pull re-delivered %q", signal.Message)
		}
	}
}

func TestElasticsearch_TimeOnlyPagesPastResultWindow(t *testing.T) {
	const pageSize = 1000
	const total = 10050
	timestamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	stamp := timestamp.Format(time.RFC3339Nano)
	position := 0
	clears := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var query struct {
			From int `json:"from"`
			Size int `json:"size"`
		}
		_ = json.NewDecoder(r.Body).Decode(&query)
		if r.Method == http.MethodDelete {
			clears++
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if query.From != 0 {
			t.Errorf("unexpected from offset %d", query.From)
		}
		if r.URL.Path == "/logs-*/_search" {
			if r.URL.Query().Get("scroll") != "1m" {
				t.Error("initial search lacks scroll keepalive")
			}
			position = 0
		}
		end := min(position+pageSize, total)
		hits := make([]esHit, 0, end-position)
		for index := position; index < end; index++ {
			id := fmt.Sprintf("doc-%05d", index)
			hits = append(hits, esHit{ID: id, Source: map[string]interface{}{"@timestamp": stamp, "message": id}, Sort: []interface{}{float64(timestamp.UnixMilli())}})
		}
		position = end
		response := esSearchResponse{ScrollID: "snapshot"}
		response.Hits.Hits = hits
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	src, err := NewElasticsearchSource("window", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: pageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals, cursor, err := src.Pull(context.Background(), timestamp.Add(-time.Hour))
	if err != nil || len(signals) != maximumESPullItems || !cursor.Equal(timestamp) {
		t.Fatalf("first pull signals=%d cursor=%v err=%v", len(signals), cursor, err)
	}

	signals, next, err := src.Pull(context.Background(), cursor)
	if err != nil || len(signals) != total-maximumESPullItems || !next.Equal(cursor) {
		t.Fatalf("second pull signals=%d cursor=%v err=%v", len(signals), next, err)
	}
	if clears != 2 || position != total {
		t.Fatalf("clears=%d position=%d, want two cleanups and pagination beyond 10000", clears, position)
	}
}

func TestElasticsearch_TimeOnlyScrollCleanupOnCancellationAndFailure(t *testing.T) {
	for _, scenario := range []string{"canceled", "shard failure", "oversized continuation"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clears := 0
			pages := 0
			timestamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodDelete {
					if request.Context().Err() != nil {
						t.Error("cleanup used canceled context")
					}
					clears++
					_, _ = writer.Write([]byte(`{}`))
					return
				}
				pages++
				if pages == 2 {
					switch scenario {
					case "canceled":
						cancel()
					case "shard failure":
						_, _ = writer.Write([]byte(`{"_scroll_id":"snapshot","_shards":{"total":1,"failed":1},"hits":{"hits":[]}}`))
					case "oversized continuation":
						_, _ = writer.Write([]byte(`{"_scroll_id":"snapshot","hits":{"hits":[]},"padding":"` + strings.Repeat("x", 8<<20) + `"}`))
					}
					return
				}
				response := esSearchResponse{ScrollID: "snapshot"}
				response.Hits.Hits = []esHit{{ID: "first", Source: map[string]interface{}{"@timestamp": timestamp.Format(time.RFC3339Nano), "message": "first"}}}
				_ = json.NewEncoder(writer).Encode(response)
			}))
			defer server.Close()
			source, err := NewElasticsearchSource("cleanup", config.AgentElasticsearchSourceConfig{
				Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = source.Pull(ctx, timestamp.Add(-time.Hour))
			if scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want canceled", err)
			}
			if scenario == "shard failure" && !errors.Is(err, elasticsearchapp.ErrShardFailure) {
				t.Fatalf("error = %v, want shard failure", err)
			}
			if scenario == "oversized continuation" && !errors.Is(err, elasticsearchapp.ErrResponseTooLarge) {
				t.Fatalf("error = %v, want bounded response failure", err)
			}
			if clears != 1 || pages > 2 {
				t.Fatalf("pages=%d clears=%d, want cleanup exactly once", pages, clears)
			}
		})
	}
}

func TestElasticsearch_PullFailsOnShardFailuresWithoutAdvancingCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"_shards":{"total":2,"successful":0,"skipped":0,"failed":2,"failures":[{"shard":0,"index":"logs-1","node":"n1","reason":{"type":"query_shard_exception","reason":"No mapping found for [event.id] in order to sort on","index":"logs-1"}}]},"hits":{"total":{"value":0,"relation":"eq"},"hits":[]}}`))
	}))
	defer server.Close()

	for _, tieBreaker := range []string{"", "event.id"} {
		src, err := NewElasticsearchSource("shards", config.AgentElasticsearchSourceConfig{
			Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", TieBreakerField: tieBreaker,
		})
		if err != nil {
			t.Fatal(err)
		}
		since := time.Now().UTC().Add(-time.Minute)
		signals, cursor, err := src.Pull(context.Background(), since)
		if !errors.Is(err, elasticsearchapp.ErrShardFailure) || !strings.Contains(err.Error(), "2 of 2 shards failed") ||
			!strings.Contains(err.Error(), "query_shard_exception: No mapping found for [event.id]") {
			t.Fatalf("tie breaker %q: error = %v, want shard failure", tieBreaker, err)
		}
		if signals != nil || !cursor.Equal(since) {
			t.Fatalf("tie breaker %q: signals=%d cursor=%v, want none at %v", tieBreaker, len(signals), cursor, since)
		}
	}
}
