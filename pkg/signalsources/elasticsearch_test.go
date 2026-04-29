package signalsources

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
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

		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, []esHit{
			{
				ID: "doc-1",
				Source: map[string]interface{}{
					"@timestamp": "2026-04-20T10:00:01Z",
					"message":    "connection refused to db-01 port 5432",
					"level":      "error",
					"service":    "db-pool",
				},
				Sort: []interface{}{float64(1745143201000)},
			},
			{
				ID: "doc-2",
				Source: map[string]interface{}{
					"@timestamp": "2026-04-20T10:00:05Z",
					"message":    "connection refused to db-02 port 5432",
					"level":      "error",
					"service":    "db-pool",
				},
				Sort: []interface{}{float64(1745143205000)},
			},
		}))
	}))
	defer ts.Close()

	src, err := NewElasticsearchSource("test", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
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
	if signals[0].Message != "connection refused to db-01 port 5432" {
		t.Errorf("message[0]: %q", signals[0].Message)
	}
	if signals[0].Severity != "error" {
		t.Errorf("severity[0]: %q", signals[0].Severity)
	}
	if got, _ := signals[0].Fields["service"].(string); got != "db-pool" {
		t.Errorf("fields[service]: %v", signals[0].Fields)
	}
	// Cursor should be the max timestamp seen.
	want := time.Date(2026, 4, 20, 10, 0, 5, 0, time.UTC)
	if !cursor.Equal(want) {
		t.Errorf("cursor = %v, want %v", cursor, want)
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
					Source: map[string]interface{}{"@timestamp": "2026-04-20T10:00:01Z", "message": "line 1"},
					Sort:   []interface{}{float64(1)},
				},
				{
					ID:     "p1-2",
					Source: map[string]interface{}{"@timestamp": "2026-04-20T10:00:02Z", "message": "line 2"},
					Sort:   []interface{}{float64(2)},
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
					Source: map[string]interface{}{"@timestamp": "2026-04-20T10:00:03Z", "message": "line 3"},
					Sort:   []interface{}{float64(3)},
				},
			}))
		default:
			t.Error("unexpected third page request")
			w.Write(esResponse(t, nil))
		}
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("pager", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		PageSize:  2, // page size = 2 so first page is "full" and triggers page 2
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
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		Query:     "log.level:(error OR warn)",
	})
	src.Pull(context.Background(), time.Time{})

	if !strings.Contains(receivedBody, `"query_string"`) {
		t.Errorf("query_string not found in body: %s", receivedBody)
	}
	if !strings.Contains(receivedBody, `log.level:(error OR warn)`) {
		t.Errorf("expected query value in body: %s", receivedBody)
	}
}

func TestElasticsearch_BasicAuth(t *testing.T) {
	var authHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, nil))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("auth", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		Username:  "elastic",
		Password:  "changeme",
	})
	src.Pull(context.Background(), time.Time{})

	if !strings.HasPrefix(authHeader, "Basic ") {
		t.Errorf("expected Basic auth, got %q", authHeader)
	}
}

func TestElasticsearch_APIKeyAuth(t *testing.T) {
	var authHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write(esResponse(t, nil))
	}))
	defer ts.Close()

	// API key takes precedence when both are set.
	src, _ := NewElasticsearchSource("apikey", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
		Username:  "elastic",
		Password:  "changeme",
		APIKey:    "my-api-key-encoded",
	})
	src.Pull(context.Background(), time.Time{})

	if authHeader != "ApiKey my-api-key-encoded" {
		t.Errorf("expected ApiKey auth, got %q", authHeader)
	}
}

func TestElasticsearch_ServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"cluster down"}`))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("err", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
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
			{ID: "no-ts", Source: map[string]interface{}{"message": "no timestamp here"}},
			{ID: "has-ts", Source: map[string]interface{}{
				"@timestamp": "2026-04-20T10:00:00Z",
				"message":    "valid",
			}},
		}))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("skip", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
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
				"message":    "epoch hit",
			}},
		}))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("epoch", config.AgentElasticsearchSourceConfig{
		Addresses: []string{ts.URL},
		Index:     "logs-*",
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
				"message":    "nested test",
				"error": map[string]interface{}{
					"stack_trace": "at Foo.bar(Foo.java:42)",
				},
				"service.name": "flat-dotted",
			}},
		}))
	}))
	defer ts.Close()

	src, _ := NewElasticsearchSource("dotted", config.AgentElasticsearchSourceConfig{
		Addresses:   []string{ts.URL},
		Index:       "logs-*",
		ExtraFields: []string{"error.stack_trace", "service.name"},
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
		Addresses: []string{ts.URL},
		Index:     "logs-*",
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
				"message":    "from good node",
			}},
		}))
	}))
	defer good.Close()

	src, _ := NewElasticsearchSource("failover", config.AgentElasticsearchSourceConfig{
		Addresses: []string{bad.URL, good.URL},
		Index:     "logs-*",
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
		Addresses: []string{"http://localhost:9200"},
	})
	if err == nil || !strings.Contains(err.Error(), "index is required") {
		t.Errorf("expected index error, got %v", err)
	}
}

func TestElasticsearch_DefaultFieldValues(t *testing.T) {
	src, err := NewElasticsearchSource("defaults", config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://localhost:9200"},
		Index:     "logs-*",
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
