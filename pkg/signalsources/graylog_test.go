package signalsources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

func graylogBody(t *testing.T, messages []graylogSearchHit) []byte {
	t.Helper()
	b, err := json.Marshal(graylogSearchResponse{Messages: messages, TotalResults: len(messages)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestGraylog_PullBasic(t *testing.T) {
	t1 := "2026-04-20T10:00:01.000Z"
	t2 := "2026-04-20T10:00:05.000Z"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/search/universal/absolute" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-Requested-By") == "" {
			t.Errorf("missing X-Requested-By header")
		}
		q := r.URL.Query()
		if q.Get("query") != "level:ERROR" {
			t.Errorf("unexpected query %q", q.Get("query"))
		}
		if q.Get("limit") != "10" {
			t.Errorf("expected limit=10, got %q", q.Get("limit"))
		}
		if q.Get("sort") != "timestamp:asc" {
			t.Errorf("expected sort=timestamp:asc, got %q", q.Get("sort"))
		}
		if q.Get("filter") != "streams:abc123" {
			t.Errorf("expected stream filter, got %q", q.Get("filter"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(graylogBody(t, []graylogSearchHit{
			{
				Index: "graylog_0",
				Message: map[string]interface{}{
					"_id":       "msg-1",
					"timestamp": t1,
					"message":   "connection refused to db-01",
					"source":    "host-1",
					"level":     float64(3), // Graylog uses numeric syslog levels
				},
			},
			{
				Index: "graylog_0",
				Message: map[string]interface{}{
					"_id":       "msg-2",
					"timestamp": t2,
					"message":   "connection refused to db-02",
					"source":    "host-2",
					"level":     float64(3),
				},
			},
		}))
	}))
	defer ts.Close()

	src, err := NewGraylogSource("test", config.AgentGraylogSourceConfig{
		Address:     ts.URL,
		Query:       "level:ERROR",
		StreamID:    "abc123",
		PageSize:    10,
		ExtraFields: []string{"source"},
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if src.Name() != "graylog:test" {
		t.Errorf("unexpected name %q", src.Name())
	}

	signals, cursor, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(signals))
	}
	if signals[0].Message != "connection refused to db-01" {
		t.Errorf("unexpected message %q", signals[0].Message)
	}
	if signals[0].Severity != "3" {
		t.Errorf("expected severity 3 (stringified numeric level), got %q", signals[0].Severity)
	}
	if signals[0].Fields["source"] != "host-1" {
		t.Errorf("expected source in fields, got %#v", signals[0].Fields)
	}
	wantCursor, _ := time.Parse(time.RFC3339, t2)
	if !cursor.Equal(wantCursor) {
		t.Errorf("cursor = %v, want %v", cursor, wantCursor)
	}
}

func TestGraylog_FiltersMessagesAtOrBeforeSince(t *testing.T) {
	older := "2026-04-20T10:00:00.000Z"
	newer := "2026-04-20T10:00:10.000Z"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(graylogBody(t, []graylogSearchHit{
			{Message: map[string]interface{}{"timestamp": older, "message": "old"}},
			{Message: map[string]interface{}{"timestamp": newer, "message": "new"}},
		}))
	}))
	defer ts.Close()

	src, _ := NewGraylogSource("t", config.AgentGraylogSourceConfig{
		Address: ts.URL,
		Query:   "*",
	})
	since, _ := time.Parse(time.RFC3339, older)
	signals, _, err := src.Pull(context.Background(), since)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 1 || signals[0].Message != "new" {
		t.Errorf("expected only the newer message, got %#v", signals)
	}
}

func TestGraylog_APITokenAuth(t *testing.T) {
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("my-token:token"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("unexpected auth %q", got)
		}
		w.Write(graylogBody(t, nil))
	}))
	defer ts.Close()

	src, _ := NewGraylogSource("t", config.AgentGraylogSourceConfig{
		Address:  ts.URL,
		Query:    "*",
		APIToken: "my-token",
		Username: "ignored",
		Password: "ignored",
	})
	if _, _, err := src.Pull(context.Background(), time.Time{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestGraylog_BasicAuth(t *testing.T) {
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("unexpected auth %q", got)
		}
		w.Write(graylogBody(t, nil))
	}))
	defer ts.Close()

	src, _ := NewGraylogSource("t", config.AgentGraylogSourceConfig{
		Address:  ts.URL,
		Query:    "*",
		Username: "user",
		Password: "pass",
	})
	if _, _, err := src.Pull(context.Background(), time.Time{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestGraylog_HTTPErrorReturnsCursor(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	src, _ := NewGraylogSource("t", config.AgentGraylogSourceConfig{
		Address: ts.URL,
		Query:   "*",
	})
	since := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	signals, cursor, err := src.Pull(context.Background(), since)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "graylog") {
		t.Errorf("expected graylog in error, got %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("expected no signals, got %d", len(signals))
	}
	if !cursor.Equal(since) {
		t.Errorf("cursor advanced on error: %v", cursor)
	}
}

func TestGraylog_ValidationErrors(t *testing.T) {
	if _, err := NewGraylogSource("t", config.AgentGraylogSourceConfig{Query: "*"}); err == nil {
		t.Errorf("expected error for missing address")
	}
	// Empty query is allowed (defaults to "*").
	if _, err := NewGraylogSource("t", config.AgentGraylogSourceConfig{Address: "http://x"}); err != nil {
		t.Errorf("expected empty query to default, got %v", err)
	}
}
