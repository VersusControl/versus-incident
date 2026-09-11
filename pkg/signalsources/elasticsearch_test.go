package signalsources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	elasticsearchapp "github.com/VersusControl/versus-incident/pkg/elasticsearch"
)

// esResponse builds a JSON body that mirrors what Elasticsearch _search returns.
func esResponse(t *testing.T, hits []esHit) []byte {
	t.Helper()
	resp := esSearchResponse{}
	resp.Hits.Hits = hits
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestElasticsearch_PullBasic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/logs-app-*/_search") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("unexpected content-type %s", ct)
		}
		body, _ := io.ReadAll(r.Body)
		for _, field := range []string{"@timestamp", "event.id", "message", "level", "service"} {
			if !strings.Contains(string(body), `"`+field+`"`) {
				t.Errorf("projected field %q missing from request: %s", field, body)
			}
		}
		if strings.Contains(string(body), "api_key") {
			t.Errorf("unconfigured field requested: %s", body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, []esHit{
			{
				ID: "doc-1",
				Source: map[string]interface{}{
					"@timestamp": "2026-04-20T10:00:01Z",
					"event.id":   "doc-1",
					"message":    "connection refused to db-01 port 5432",
					"level":      "error",
					"service":    "db-pool",
					"api_key":    "must-not-leak",
				},
				Sort: []interface{}{"2026-04-20T10:00:01Z", "doc-1"},
			},
			{
				ID: "doc-2",
				Source: map[string]interface{}{
					"@timestamp": "2026-04-20T10:00:05Z",
					"event.id":   "doc-2",
					"message":    "connection refused to db-02 port 5432",
					"level":      "error",
					"service":    "db-pool",
				},
				Sort: []interface{}{"2026-04-20T10:00:05Z", "doc-2"},
			},
		}))
	}))
	defer ts.Close()

	src, err := NewElasticsearchSource("test", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-app-*",
		TimeField:     "@timestamp",
		MessageField:  "message",
		SeverityField: "level",
		ExtraFields:   []string{"service"},
		PageSize:      10,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if src.Name() != "elasticsearch:test" {
		t.Errorf("unexpected name %q", src.Name())
	}

	since := time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)
	signals, cursor, err := src.Pull(context.Background(), since)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(signals))
	}
	assertNormalizedSignal(t, signals[0], "elasticsearch:test")
	if signals[0].Message != "connection refused to db-01 port 5432" {
		t.Errorf("message[0]: %q", signals[0].Message)
	}
	if signals[0].Severity != "error" {
		t.Errorf("severity[0]: %q", signals[0].Severity)
	}
	if got, _ := signals[0].Fields["service"].(string); got != "db-pool" {
		t.Errorf("fields[service]: %v", signals[0].Fields)
	}
	if _, leaked := signals[0].Raw["api_key"]; leaked {
		t.Fatal("unconfigured field leaked into Raw")
	}
	if len(signals[0].Raw) != 5 || signals[0].Raw["message"] != signals[0].Message || signals[0].Raw["event.id"] != "doc-1" {
		t.Fatalf("projected Raw = %#v", signals[0].Raw)
	}
	// Cursor should be the max timestamp seen.
	want := time.Date(2026, 4, 20, 10, 0, 5, 0, time.UTC)
	if !cursor.Equal(want) {
		t.Errorf("cursor = %v, want %v", cursor, want)
	}
}

func TestElasticsearch_TailAcceptsFiveHundredDocumentPageOverOneMiB(t *testing.T) {
	timestamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	hits := make([]esHit, 500)
	for index := range hits {
		hits[index] = esHit{
			ID: fmt.Sprintf("doc-%03d", index),
			Source: map[string]interface{}{
				"@timestamp": timestamp.Format(time.RFC3339Nano),
				"event.id":   fmt.Sprintf("doc-%03d", index),
				"message":    strings.Repeat("structured-log-value ", 140),
			},
			Sort: []interface{}{float64(timestamp.UnixMilli()), fmt.Sprintf("doc-%03d", index)},
		}
	}
	payload := esResponse(t, hits)
	if len(payload) <= 1<<20 {
		t.Fatalf("fixture size = %d, want over 1 MiB", len(payload))
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writer.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			_, _ = writer.Write(payload)
			return
		}
		_, _ = writer.Write(esResponse(t, nil))
	}))
	defer server.Close()

	source, err := NewElasticsearchSource("large-page", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals, _, err := source.Pull(context.Background(), timestamp.Add(-time.Hour))
	if err != nil || len(signals) != 500 || requests != 2 {
		t.Fatalf("tail signals=%d requests=%d err=%v", len(signals), requests, err)
	}
}

func TestElasticsearch_PullRetriesOversizedPageAtSmallerSize(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	var requestedSizes []int
	var searchAfters [][]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var query struct {
			Size        int           `json:"size"`
			SearchAfter []interface{} `json:"search_after"`
		}
		if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
			t.Fatalf("decode query: %v", err)
		}
		requestedSizes = append(requestedSizes, query.Size)
		searchAfters = append(searchAfters, query.SearchAfter)

		count := query.Size
		if len(query.SearchAfter) > 0 {
			count = 1
		}
		hits := make([]esHit, count)
		for index := range hits {
			document := index
			if len(query.SearchAfter) > 0 {
				document = 250
			}
			timestamp := base.Add(time.Duration(document) * time.Millisecond)
			hits[index] = esHit{
				ID: fmt.Sprintf("doc-%03d", document),
				Source: map[string]interface{}{
					"@timestamp": timestamp.Format(time.RFC3339Nano),
					"event.id":   fmt.Sprintf("doc-%03d", document),
					"message":    strings.Repeat("x", 20<<10),
				},
				Sort: []interface{}{float64(timestamp.UnixMilli()), fmt.Sprintf("doc-%03d", document)},
			}
		}
		_, _ = writer.Write(esResponse(t, hits))
	}))
	defer server.Close()

	source, err := NewElasticsearchSource("adaptive-page", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals, cursor, err := source.Pull(context.Background(), base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 251 || !cursor.Equal(base.Add(250*time.Millisecond)) {
		t.Fatalf("signals=%d cursor=%v", len(signals), cursor)
	}
	if got, want := fmt.Sprint(requestedSizes), "[500 250 250]"; got != want {
		t.Fatalf("requested sizes = %s, want %s", got, want)
	}
	if len(searchAfters[0]) != 0 || len(searchAfters[1]) != 0 || len(searchAfters[2]) == 0 {
		t.Fatalf("search_after sequence = %#v", searchAfters)
	}
}

func TestElasticsearch_PullFailsWhenSingleDocumentExceedsResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(esResponse(t, []esHit{{
			ID: "oversized",
			Source: map[string]interface{}{
				"@timestamp": time.Now().UTC().Format(time.RFC3339Nano),
				"message":    strings.Repeat("x", (8<<20)+1024),
			},
		}}))
	}))
	defer server.Close()

	source, err := NewElasticsearchSource("single-oversized", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = source.Pull(context.Background(), time.Now().Add(-time.Hour))
	if !errors.Is(err, elasticsearchapp.ErrResponseTooLarge) || !strings.Contains(err.Error(), "one projected document exceeds the 8 MiB response limit") || !strings.Contains(err.Error(), "tail cannot advance") {
		t.Fatalf("error = %v", err)
	}
}

func TestElasticsearch_PullStopsAtAggregateItemBudget(t *testing.T) {
	const pageSize = 999
	base := time.Now().UTC().Add(-4 * time.Hour).Truncate(time.Millisecond)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		start := (requests - 1) * pageSize
		if requests == 12 {
			start = maximumESPullItems
		}
		end := start + pageSize
		if end > maximumESPullItems+1 {
			end = maximumESPullItems + 1
		}
		hits := make([]esHit, 0, end-start)
		for index := start; index < end; index++ {
			timestamp := base.Add(time.Duration(index) * time.Second)
			hits = append(hits, esHit{
				ID: fmt.Sprintf("doc-%05d", index),
				Source: map[string]interface{}{
					"@timestamp": timestamp.Format(time.RFC3339Nano),
					"event.id":   fmt.Sprintf("doc-%05d", index),
					"message":    fmt.Sprintf("bounded-%05d", index),
				},
				Sort: []interface{}{float64(timestamp.UnixMilli()), fmt.Sprintf("doc-%05d", index)},
			})
		}
		_, _ = writer.Write(esResponse(t, hits))
	}))
	defer server.Close()

	source, err := NewElasticsearchSource("item-budget", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: pageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals, cursor, err := source.Pull(context.Background(), base.Add(-time.Hour))
	if err != nil || len(signals) != maximumESPullItems || requests != 11 {
		t.Fatalf("first pull signals=%d requests=%d err=%v", len(signals), requests, err)
	}
	if got, want := signals[len(signals)-1].Message, "bounded-09999"; got != want {
		t.Fatalf("last first-pull signal = %q, want %q", got, want)
	}
	if want := base.Add(9999 * time.Second); !cursor.Equal(want) {
		t.Fatalf("first cursor = %v, want %v", cursor, want)
	}

	signals, nextCursor, err := source.Pull(context.Background(), cursor)
	if err != nil || len(signals) != 1 || requests != 12 {
		t.Fatalf("second pull signals=%d requests=%d err=%v", len(signals), requests, err)
	}
	if got, want := signals[0].Message, "bounded-10000"; got != want {
		t.Fatalf("second-pull signal = %q, want %q", got, want)
	}
	if want := base.Add(10000 * time.Second); !nextCursor.Equal(want) {
		t.Fatalf("second cursor = %v, want %v", nextCursor, want)
	}
}

func TestElasticsearch_PullStopsAtAggregateDecodedByteBudget(t *testing.T) {
	base := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	hits := []esHit{
		{
			ID: "accepted",
			Source: map[string]interface{}{
				"@timestamp": base.Format(time.RFC3339Nano),
				"event.id":   "accepted",
				"message":    "accepted-" + strings.Repeat("x", maximumESPullBytes/2),
			},
			Sort: []interface{}{float64(base.UnixMilli()), "accepted"},
		},
		{
			ID: "deferred",
			Source: map[string]interface{}{
				"@timestamp": base.Add(time.Second).Format(time.RFC3339Nano),
				"event.id":   "deferred",
				"message":    "deferred-" + strings.Repeat("y", maximumESPullBytes/2),
			},
			Sort: []interface{}{float64(base.Add(time.Second).UnixMilli()), "deferred"},
		},
	}
	firstEncoded, _ := json.Marshal(hits[0])
	secondEncoded, _ := json.Marshal(hits[1])
	if len(firstEncoded) > maximumESPullBytes || len(firstEncoded)+len(secondEncoded) <= maximumESPullBytes {
		t.Fatalf("fixture bytes first=%d aggregate=%d budget=%d", len(firstEncoded), len(firstEncoded)+len(secondEncoded), maximumESPullBytes)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		var responseHits []esHit
		switch requests {
		case 1:
			responseHits = hits[:1]
		case 2, 3:
			responseHits = hits[1:]
		}
		_, _ = writer.Write(esResponse(t, responseHits))
	}))
	defer server.Close()

	source, err := NewElasticsearchSource("byte-budget", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals, cursor, err := source.Pull(context.Background(), base.Add(-time.Hour))
	if err != nil || len(signals) != 1 || requests != 2 {
		t.Fatalf("first pull signals=%d requests=%d err=%v", len(signals), requests, err)
	}
	if !strings.HasPrefix(signals[0].Message, "accepted-") || !cursor.Equal(base) {
		t.Fatalf("first pull included the wrong document or cursor: message prefix=%q cursor=%v", signals[0].Message[:9], cursor)
	}

	signals, nextCursor, err := source.Pull(context.Background(), cursor)
	if err != nil || len(signals) != 1 || requests != 4 {
		t.Fatalf("second pull signals=%d requests=%d err=%v", len(signals), requests, err)
	}
	if !strings.HasPrefix(signals[0].Message, "deferred-") || !nextCursor.Equal(base.Add(time.Second)) {
		t.Fatalf("second pull included the wrong document or cursor: message prefix=%q cursor=%v", signals[0].Message[:9], nextCursor)
	}
}

func TestElasticsearch_PullRejectsRepeatedPageInsteadOfLooping(t *testing.T) {
	timestamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	payload := esResponse(t, []esHit{{
		ID: "page",
		Source: map[string]interface{}{
			"@timestamp": timestamp.Format(time.RFC3339Nano),
			"event.id":   "page",
			"message":    "bounded",
		},
		Sort: []interface{}{float64(timestamp.UnixMilli()), "page"},
	}})
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	source, err := NewElasticsearchSource("page-budget", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	signals, _, err := source.Pull(context.Background(), timestamp.Add(-time.Hour))
	if err == nil || signals != nil || requests != 2 || !strings.Contains(err.Error(), `tie_breaker_field "event.id"`) {
		t.Fatalf("signals=%d requests=%d err=%v", len(signals), requests, err)
	}
}

func TestElasticsearch_PullReportsExhaustedDedupScan(t *testing.T) {
	timestamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	rows := make([]DedupRow, maximumESScanItems)
	for index := range rows {
		id := fmt.Sprintf("doc-%05d", index)
		rows[index] = DedupRow{ID: id, TS: timestamp}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var query struct {
			SearchAfter []interface{} `json:"search_after"`
		}
		if err := json.NewDecoder(request.Body).Decode(&query); err != nil {
			t.Fatal(err)
		}
		start := 0
		if len(query.SearchAfter) == 2 {
			lastTie, ok := query.SearchAfter[1].(string)
			if !ok {
				t.Fatalf("search_after tie = %#v", query.SearchAfter[1])
			}
			if _, err := fmt.Sscanf(lastTie, "doc-%05d", &start); err != nil {
				t.Fatalf("parse search_after tie: %v", err)
			}
			start++
		}
		end := min(start+1000, maximumESScanItems)
		hits := make([]esHit, 0, end-start)
		for index := start; index < end; index++ {
			id := fmt.Sprintf("doc-%05d", index)
			hits = append(hits, esHit{
				ID: id,
				Source: map[string]interface{}{
					"@timestamp": timestamp.Format(time.RFC3339Nano), "event.id": id, "message": "seen",
				},
				Sort: []interface{}{timestamp.Format(time.RFC3339Nano), id},
			})
		}
		_, _ = writer.Write(esResponse(t, hits))
	}))
	defer server.Close()

	source, err := NewElasticsearchSource("scan-bound", config.AgentElasticsearchSourceConfig{
		Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	source.dedup.Stage(rows, timestamp.Add(-time.Minute))
	signals, cursor, err := source.Pull(context.Background(), timestamp)
	if err == nil || !strings.Contains(err.Error(), "scanned 50000 documents") || !strings.Contains(err.Error(), "tie_breaker_field") {
		t.Fatalf("error = %v, want actionable scan-budget health condition", err)
	}
	if len(signals) != 0 || !cursor.Equal(timestamp) {
		t.Fatalf("signals=%d cursor=%v, want empty partial at original cursor %v", len(signals), cursor, timestamp)
	}
}

func TestElasticsearch_BuildQueryUsesStableTieBreakerAndSearchAfter(t *testing.T) {
	src, err := NewElasticsearchSource("shape", config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*",
		TimeField: " @timestamp ", TieBreakerField: " ingest.sequence ", MessageField: " message ",
	})
	if err != nil {
		t.Fatal(err)
	}
	lower := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	upper := lower.Add(time.Minute)
	body, err := src.buildQuery(lower, upper, []interface{}{"2026-09-07T10:00:05Z", float64(42)})
	if err != nil {
		t.Fatal(err)
	}
	var query struct {
		Source      []string                 `json:"_source"`
		Sort        []map[string]interface{} `json:"sort"`
		SearchAfter []interface{}            `json:"search_after"`
	}
	if err := json.Unmarshal(body, &query); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(query.Source), "[@timestamp ingest.sequence message]"; got != want {
		t.Fatalf("_source = %s, want %s", got, want)
	}
	if len(query.Sort) != 2 || query.Sort[0]["@timestamp"] == nil || query.Sort[1]["ingest.sequence"] == nil {
		t.Fatalf("sort = %#v, want time then configured tie breaker", query.Sort)
	}
	if got, want := fmt.Sprint(query.SearchAfter), "[2026-09-07T10:00:05Z 42]"; got != want {
		t.Fatalf("search_after = %s, want %s", got, want)
	}
}

func TestElasticsearch_Pagination(t *testing.T) {
	page := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case 0:
			page++
			w.Write(esResponse(t, []esHit{
				{
					ID:     "p1-1",
					Source: map[string]interface{}{"@timestamp": "2026-04-20T10:00:01Z", "event.id": "p1-1", "message": "line 1"},
					Sort:   []interface{}{"2026-04-20T10:00:01Z", "p1-1"},
				},
				{
					ID:     "p1-2",
					Source: map[string]interface{}{"@timestamp": "2026-04-20T10:00:02Z", "event.id": "p1-2", "message": "line 2"},
					Sort:   []interface{}{"2026-04-20T10:00:02Z", "p1-2"},
				},
			}))
		case 1:
			// Verify search_after was sent.
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"search_after"`) {
				t.Error("expected search_after in second page request")
			}
			page++
			// Return fewer than PageSize → stops pagination.
			w.Write(esResponse(t, []esHit{
				{
					ID:     "p2-1",
					Source: map[string]interface{}{"@timestamp": "2026-04-20T10:00:03Z", "event.id": "p2-1", "message": "line 3"},
					Sort:   []interface{}{"2026-04-20T10:00:03Z", "p2-1"},
				},
			}))
		default:
			t.Error("unexpected third page request")
			w.Write(esResponse(t, nil))
		}
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("pager", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
		PageSize:      2, // page size = 2 so first page is "full" and triggers page 2
	})

	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 3 {
		t.Fatalf("expected 3 signals across 2 pages, got %d", len(signals))
	}
	if page != 2 {
		t.Errorf("expected 2 pages fetched, got %d", page)
	}
}

func TestElasticsearch_PullRejectsInvalidPaginationOrderingWithoutAdvancingCursor(t *testing.T) {
	timestamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	validHit := func(tie string) esHit {
		return esHit{
			ID: tie,
			Source: map[string]interface{}{
				"@timestamp": timestamp.Format(time.RFC3339Nano),
				"event.id":   tie,
				"message":    "message",
			},
			Sort: []interface{}{timestamp.Format(time.RFC3339Nano), tie},
		}
	}

	tests := []struct {
		name     string
		pageSize int
		pages    [][]esHit
	}{
		{name: "missing tie sort value", pages: [][]esHit{{func() esHit { hit := validHit("missing-sort"); hit.Sort = hit.Sort[:1]; return hit }()}}},
		{name: "null timestamp sort value", pages: [][]esHit{{func() esHit { hit := validHit("null-timestamp"); hit.Sort[0] = nil; return hit }()}}},
		{name: "non-scalar timestamp sort value", pages: [][]esHit{{func() esHit {
			hit := validHit("object-timestamp")
			hit.Sort[0] = map[string]interface{}{"value": "hidden"}
			return hit
		}()}}},
		{name: "null tie sort value", pages: [][]esHit{{func() esHit { hit := validHit("null-sort"); hit.Sort[1] = nil; return hit }()}}},
		{name: "non-string tie sort value", pages: [][]esHit{{func() esHit { hit := validHit("numeric-sort"); hit.Sort[1] = float64(1); return hit }()}}},
		{name: "empty tie sort value", pages: [][]esHit{{func() esHit { hit := validHit("empty-sort"); hit.Sort[1] = " "; return hit }()}}},
		{name: "missing projected tie value", pages: [][]esHit{{func() esHit { hit := validHit("missing-source"); delete(hit.Source, "event.id"); return hit }()}}},
		{name: "null projected tie value", pages: [][]esHit{{func() esHit { hit := validHit("null-source"); hit.Source["event.id"] = nil; return hit }()}}},
		{name: "mismatched projected tie value", pages: [][]esHit{{func() esHit { hit := validHit("sort-value"); hit.Source["event.id"] = "different-value"; return hit }()}}},
		{name: "duplicate tuple within page", pages: [][]esHit{{validHit("duplicate"), validHit("duplicate")}}},
		{name: "non-increasing tie within page", pages: [][]esHit{{validHit("tie-b"), validHit("tie-a")}}},
		{name: "non-increasing timestamp within page", pages: [][]esHit{{validHit("earlier-first"), func() esHit {
			hit := validHit("later-second")
			hit.Sort[0] = timestamp.Add(-time.Second).Format(time.RFC3339Nano)
			return hit
		}()}}},
		{name: "duplicate tuple across page boundary", pageSize: 1, pages: [][]esHit{{validHit("cross-page")}, {validHit("cross-page")}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, requestBody *http.Request) {
				if request >= len(test.pages) {
					_, _ = writer.Write(esResponse(t, nil))
					return
				}
				_, _ = writer.Write(esResponse(t, test.pages[request]))
				request++
			}))
			defer server.Close()

			pageSize := test.pageSize
			if pageSize == 0 {
				pageSize = 10
			}
			source, err := NewElasticsearchSource("invalid-order", config.AgentElasticsearchSourceConfig{
				Addresses: []string{server.URL}, AllowLoopback: true, Index: "logs-*", PageSize: pageSize,
			})
			if err != nil {
				t.Fatal(err)
			}
			since := timestamp.Add(-time.Hour)
			signals, cursor, err := source.Pull(context.Background(), since)
			if err == nil || signals != nil || !cursor.Equal(since) {
				t.Fatalf("signals=%d cursor=%v err=%v, want no signals and unchanged cursor %v", len(signals), cursor, err, since)
			}
			if !strings.Contains(err.Error(), `tie_breaker_field "event.id"`) || !strings.Contains(err.Error(), "unique keyword field with doc-values") || strings.Contains(err.Error(), "different-value") {
				t.Fatalf("error is not actionable and secret-free: %v", err)
			}
		})
	}
}

func TestElasticsearch_QueryStringSent(t *testing.T) {
	var receivedBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, nil))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("qs", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
		Query:         "log.level:(error OR warn)",
	})
	src.Pull(context.Background(), time.Time{})

	if !strings.Contains(receivedBody, `"query_string"`) {
		t.Errorf("query_string not found in body: %s", receivedBody)
	}
	if !strings.Contains(receivedBody, `log.level:(error OR warn)`) {
		t.Errorf("expected query value in body: %s", receivedBody)
	}
}

func TestElasticsearch_RejectsCredentialsOverHTTP(t *testing.T) {
	for _, credentials := range []config.AgentElasticsearchSourceConfig{
		{Username: "elastic", Password: "changeme"},
		{APIKey: "my-api-key-encoded"},
	} {
		credentials.Addresses = []string{"http://localhost:9200"}
		credentials.AllowLoopback = true
		credentials.Index = "logs-*"
		if _, err := NewElasticsearchSource("auth", credentials); !errors.Is(err, elasticsearchapp.ErrInvalidConfig) || !strings.Contains(err.Error(), "credentials require verified HTTPS") || strings.Contains(err.Error(), "changeme") || strings.Contains(err.Error(), "my-api-key-encoded") {
			t.Fatalf("plaintext credential error = %v", err)
		}
	}
}

func TestElasticsearch_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"cluster down"}`))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("err", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
	})
	_, _, err := src.Pull(context.Background(), time.Time{})
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should contain status code: %v", err)
	}
}

func TestElasticsearch_SkipsHitsWithoutTimestamp(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, []esHit{
			{ID: "no-ts", Source: map[string]interface{}{"event.id": "no-ts", "message": "no timestamp here"}, Sort: []interface{}{"2026-04-20T09:59:59Z", "no-ts"}},
			{ID: "has-ts", Source: map[string]interface{}{
				"@timestamp": "2026-04-20T10:00:00Z",
				"event.id":   "has-ts",
				"message":    "valid",
			}, Sort: []interface{}{"2026-04-20T10:00:00Z", "has-ts"}},
		}))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("skip", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
	})
	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("expected 1 signal (skipping no-ts), got %d", len(signals))
	}
	if signals[0].Message != "valid" {
		t.Errorf("wrong signal kept: %q", signals[0].Message)
	}
}

func TestElasticsearch_EpochMillisTimestamp(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, []esHit{
			{ID: "epoch", Source: map[string]interface{}{
				"@timestamp": float64(1745143200000), // 2025-04-20T10:00:00Z in millis
				"event.id":   "epoch",
				"message":    "epoch hit",
			}, Sort: []interface{}{float64(1745143200000), "epoch"}},
		}))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("epoch", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
	})
	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("expected 1, got %d", len(signals))
	}
	if signals[0].Message != "epoch hit" {
		t.Errorf("message: %q", signals[0].Message)
	}
	// Just verify it parsed to something reasonable (non-zero).
	if signals[0].Timestamp.IsZero() {
		t.Error("timestamp should not be zero for epoch millis")
	}
}

func TestElasticsearch_DottedFieldLookup(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, []esHit{
			{ID: "nested", Source: map[string]interface{}{
				"@timestamp": "2026-04-20T10:00:00Z",
				"event.id":   "nested",
				"message":    "nested test",
				"error": map[string]interface{}{
					"stack_trace": "at Foo.bar(Foo.java:42)",
				},
				"service.name": "flat-dotted",
			}, Sort: []interface{}{"2026-04-20T10:00:00Z", "nested"}},
		}))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("dotted", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
		ExtraFields:   []string{"error.stack_trace", "service.name"},
	})
	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("expected 1, got %d", len(signals))
	}
	// Nested walk: error.stack_trace
	if got, _ := signals[0].Fields["error.stack_trace"].(string); got != "at Foo.bar(Foo.java:42)" {
		t.Errorf("nested field: %v", signals[0].Fields)
	}
	// Flat dotted key: service.name
	if got, _ := signals[0].Fields["service.name"].(string); got != "flat-dotted" {
		t.Errorf("flat dotted field: %v", signals[0].Fields)
	}
}

func TestElasticsearch_EmptyResultReturnsOriginalCursor(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, nil))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("empty", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		AllowLoopback: true,
		Index:         "logs-*",
	})
	since := time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)
	signals, cursor, err := src.Pull(context.Background(), since)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("expected 0 signals, got %d", len(signals))
	}
	if !cursor.Equal(since) {
		t.Errorf("cursor should equal since when no results: got %v, want %v", cursor, since)
	}
}

func TestElasticsearch_FailoverToSecondAddress(t *testing.T) {
	// First address: always fails.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"node down"}`))
	}))
	defer bad.Close()

	// Second address: succeeds.
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, []esHit{
			{ID: "ok", Source: map[string]interface{}{
				"@timestamp": "2026-04-20T10:00:00Z",
				"event.id":   "ok",
				"message":    "from good node",
			}, Sort: []interface{}{"2026-04-20T10:00:00Z", "ok"}},
		}))
	}))
	defer good.Close()

	src, _ := NewElasticsearchSource("failover", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{bad.URL, good.URL},
		AllowLoopback: true,
		Index:         "logs-*",
	})
	signals, _, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("expected failover to second address, got error: %v", err)
	}
	if len(signals) != 1 || signals[0].Message != "from good node" {
		t.Errorf("unexpected signals: %+v", signals)
	}
}

func TestElasticsearch_ValidationErrors(t *testing.T) {
	_, err := NewElasticsearchSource("noaddr", config.AgentElasticsearchSourceConfig{
		Index: "logs-*",
	})
	if err == nil || !strings.Contains(err.Error(), "no addresses") {
		t.Errorf("expected no-addresses error, got %v", err)
	}

	_, err = NewElasticsearchSource("noindex", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{"http://localhost:9200"},
		AllowLoopback: true,
	})
	if err == nil || !strings.Contains(err.Error(), "index is required") {
		t.Errorf("expected index error, got %v", err)
	}
}

func TestElasticsearch_DefaultFieldValues(t *testing.T) {
	src, err := NewElasticsearchSource("defaults", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{"http://localhost:9200"},
		AllowLoopback: true,
		Index:         "logs-*",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if src.cfg.TimeField != "@timestamp" {
		t.Errorf("default time field: %q", src.cfg.TimeField)
	}
	if src.cfg.MessageField != "message" {
		t.Errorf("default message field: %q", src.cfg.MessageField)
	}
	if src.cfg.PageSize != 500 {
		t.Errorf("default page size: %d", src.cfg.PageSize)
	}
}

func TestElasticsearch_PageSizeIsClamped(t *testing.T) {
	src, err := NewElasticsearchSource("clamped", config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://localhost:9200"}, AllowLoopback: true, Index: "logs-*", PageSize: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.cfg.PageSize != maximumESPageSize {
		t.Fatalf("page size = %d, want %d", src.cfg.PageSize, maximumESPageSize)
	}
}
