package signalsources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func tempoBody(t *testing.T, traces []tempoTrace) []byte {
	t.Helper()
	b, err := json.Marshal(tempoSearchResponse{Traces: traces})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func nanoStr(ts time.Time) string {
	return strconv.FormatInt(ts.UTC().UnixNano(), 10)
}

func TestTempoQuerier_SearchParsesAndSorts(t *testing.T) {
	t1 := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	t2 := t1.Add(time.Minute)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("querier must be GET-only, got %s", r.Method)
		}
		if r.URL.Query().Get("q") != "{ status = error }" {
			t.Errorf("unexpected q %q", r.URL.Query().Get("q"))
		}
		w.Write(tempoBody(t, []tempoTrace{
			{
				TraceID:           "abc123",
				RootServiceName:   "api",
				RootTraceName:     "GET /orders",
				StartTimeUnixNano: nanoStr(t2),
				DurationMs:        842,
				SpanSet: &tempoSpanSet{Spans: []tempoSpan{
					{Attributes: []tempoAttr{{Key: "status", Value: tempoAttrValue{StringValue: "error"}}}},
				}},
			},
			{
				TraceID:           "def456",
				RootServiceName:   "web",
				RootTraceName:     "GET /home",
				StartTimeUnixNano: nanoStr(t1),
				DurationMs:        12,
			},
		}))
	}))
	defer ts.Close()

	q, err := NewTempoQuerier(ts.URL, PrometheusAuth{}, false)
	if err != nil {
		t.Fatalf("new querier: %v", err)
	}
	summaries, err := q.Search(context.Background(), "{ status = error }", t1, t2, 20)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}
	// Newest first.
	if summaries[0].TraceID != "abc123" {
		t.Errorf("expected newest trace first, got %v", summaries[0].TraceID)
	}
	if !summaries[0].Error {
		t.Errorf("expected error flag on the error trace")
	}
	if summaries[1].Error {
		t.Errorf("expected no error flag on the ok trace")
	}
}

func TestTempoQuerier_SearchLimitCap(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "2" {
			t.Errorf("expected limit=2, got %q", r.URL.Query().Get("limit"))
		}
		w.Write(tempoBody(t, []tempoTrace{
			{TraceID: "a", StartTimeUnixNano: nanoStr(now), DurationMs: 1},
			{TraceID: "b", StartTimeUnixNano: nanoStr(now.Add(time.Second)), DurationMs: 2},
		}))
	}))
	defer ts.Close()

	q, _ := NewTempoQuerier(ts.URL, PrometheusAuth{}, false)
	summaries, err := q.Search(context.Background(), "", now.Add(-time.Hour), now, 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2, got %d", len(summaries))
	}
}

func TestTempoQuerier_Auth(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			t.Errorf("basic auth = %q", r.Header.Get("Authorization"))
		}
		w.Write(tempoBody(t, nil))
	}))
	defer ts.Close()
	q, _ := NewTempoQuerier(ts.URL, PrometheusAuth{Username: "u", Password: "p"}, false)
	if _, err := q.Search(context.Background(), "", time.Now().Add(-time.Hour), time.Now(), 10); err != nil {
		t.Fatalf("search: %v", err)
	}
}

func TestTempoQuerier_HTTPErrorSurfaces(t *testing.T) {
	tsErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer tsErr.Close()
	q, _ := NewTempoQuerier(tsErr.URL, PrometheusAuth{}, false)
	_, err := q.Search(context.Background(), "", time.Now().Add(-time.Hour), time.Now(), 10)
	if err == nil || !strings.Contains(err.Error(), "tempo") {
		t.Fatalf("expected tempo error, got %v", err)
	}
}

func TestTempoQuerier_AddressRequired(t *testing.T) {
	if _, err := NewTempoQuerier("", PrometheusAuth{}, false); err == nil {
		t.Error("expected error for empty address")
	}
}

// -----------------------------------------------------------------------------
// Discovery reads (GET-only).
// -----------------------------------------------------------------------------

func TestTempoQuerier_TagsV2ScopesDedupedAndSorted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery must be GET-only, got %s", r.Method)
		}
		if r.URL.Path != "/api/v2/search/tags" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		// service.name appears in two scopes — must be de-duplicated.
		w.Write([]byte(`{"scopes":[` +
			`{"name":"resource","tags":["service.name","cluster"]},` +
			`{"name":"span","tags":["http.method","service.name"]}]}`))
	}))
	defer ts.Close()

	q, _ := NewTempoQuerier(ts.URL, PrometheusAuth{}, false)
	tags, err := q.Tags(context.Background())
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	want := []string{"cluster", "http.method", "service.name"}
	if len(tags) != len(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Fatalf("tags = %v, want %v (sorted)", tags, want)
		}
	}
}

func TestTempoQuerier_TagsFallsBackToV1(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/search/tags":
			// Empty scopes → the client must fall back to v1.
			w.Write([]byte(`{"scopes":[]}`))
		case "/api/search/tags":
			w.Write([]byte(`{"tagNames":["service.name","http.status_code",""]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer ts.Close()

	q, _ := NewTempoQuerier(ts.URL, PrometheusAuth{}, false)
	tags, err := q.Tags(context.Background())
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	// Empty entry dropped, sorted.
	want := []string{"http.status_code", "service.name"}
	if len(tags) != len(want) || tags[0] != want[0] || tags[1] != want[1] {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
}

func TestTempoQuerier_TagValuesParses(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery must be GET-only, got %s", r.Method)
		}
		if r.URL.Path != "/api/search/tag/service.name/values" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Write([]byte(`{"tagValues":["web","api","api"]}`))
	}))
	defer ts.Close()

	q, _ := NewTempoQuerier(ts.URL, PrometheusAuth{}, false)
	vals, err := q.TagValues(context.Background(), "service.name")
	if err != nil {
		t.Fatalf("tag values: %v", err)
	}
	// De-duplicated and sorted.
	want := []string{"api", "web"}
	if len(vals) != len(want) || vals[0] != want[0] || vals[1] != want[1] {
		t.Fatalf("values = %v, want %v", vals, want)
	}
}

func TestTempoQuerier_TagValuesRequiresTag(t *testing.T) {
	q, _ := NewTempoQuerier("http://example", PrometheusAuth{}, false)
	if _, err := q.TagValues(context.Background(), ""); err == nil {
		t.Error("expected error for empty tag")
	}
}
