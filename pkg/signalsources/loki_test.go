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

	"github.com/VersusControl/versus-incident/pkg/config"
)

// lokiBody marshals a fake `query_range` response into JSON.
func lokiBody(t *testing.T, streams []lokiStreamResult) []byte {
	t.Helper()
	resp := lokiQueryRangeResponse{Status: "success"}
	resp.Data.ResultType = "streams"
	resp.Data.Result = streams
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestLoki_PullBasic(t *testing.T) {
	t1 := time.Date(2026, 4, 20, 10, 0, 1, 0, time.UTC).UnixNano()
	t2 := time.Date(2026, 4, 20, 10, 0, 5, 0, time.UTC).UnixNano()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/loki/api/v1/query_range" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("query") != `{app="api"} |= "error"` {
			t.Errorf("unexpected query %q", q.Get("query"))
		}
		if q.Get("direction") != "forward" {
			t.Errorf("expected direction=forward, got %q", q.Get("direction"))
		}
		if q.Get("limit") != "10" {
			t.Errorf("expected limit=10, got %q", q.Get("limit"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(lokiBody(t, []lokiStreamResult{
			{
				Stream: map[string]string{
					"app":       "api",
					"namespace": "prod",
					"level":     "error",
				},
				Values: [][]string{
					{itoa(t1), "connection refused to db-01"},
					{itoa(t2), "connection refused to db-02"},
				},
			},
		}))
	}))
	defer ts.Close()

	src, err := NewLokiSource("test", config.AgentLokiSourceConfig{
		Address:       ts.URL,
		Query:         `{app="api"} |= "error"`,
		SeverityField: "level",
		ExtraLabels:   []string{"app", "namespace"},
		PageSize:      10,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if src.Name() != "loki:test" {
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
	if signals[0].Severity != "error" {
		t.Errorf("expected severity from stream label, got %q", signals[0].Severity)
	}
	if signals[0].Fields["app"] != "api" || signals[0].Fields["namespace"] != "prod" {
		t.Errorf("expected extra labels in fields, got %#v", signals[0].Fields)
	}
	if cursor.UnixNano() != t2 {
		t.Errorf("cursor = %d, want %d", cursor.UnixNano(), t2)
	}
}

func TestLoki_AdvancesStartByOneNs(t *testing.T) {
	since := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	wantStart := since.UnixNano() + 1

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("start")
		if got != itoa(wantStart) {
			t.Errorf("expected start=%d (since+1ns), got %s", wantStart, got)
		}
		w.Write(lokiBody(t, nil))
	}))
	defer ts.Close()

	src, err := NewLokiSource("t", config.AgentLokiSourceConfig{
		Address: ts.URL,
		Query:   `{app="x"}`,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, _, err := src.Pull(context.Background(), since); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestLoki_BearerAuthOverridesBasic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-token" {
			t.Errorf("expected Bearer auth, got %q", auth)
		}
		if r.Header.Get("X-Scope-OrgID") != "tenant-1" {
			t.Errorf("expected tenant header")
		}
		w.Write(lokiBody(t, nil))
	}))
	defer ts.Close()

	src, _ := NewLokiSource("t", config.AgentLokiSourceConfig{
		Address:     ts.URL,
		Query:       `{app="x"}`,
		TenantID:    "tenant-1",
		BearerToken: "my-token",
		Username:    "ignored",
		Password:    "ignored",
	})
	if _, _, err := src.Pull(context.Background(), time.Time{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestLoki_BasicAuth(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("unexpected auth %q", got)
		}
		w.Write(lokiBody(t, nil))
	}))
	defer ts.Close()

	src, _ := NewLokiSource("t", config.AgentLokiSourceConfig{
		Address:  ts.URL,
		Query:    `{app="x"}`,
		Username: "user",
		Password: "pass",
	})
	if _, _, err := src.Pull(context.Background(), time.Time{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestLoki_HTTPErrorReturnsCursor(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	src, _ := NewLokiSource("t", config.AgentLokiSourceConfig{
		Address: ts.URL,
		Query:   `{app="x"}`,
	})
	since := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	signals, cursor, err := src.Pull(context.Background(), since)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "loki") {
		t.Errorf("expected loki in error, got %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("expected no signals, got %d", len(signals))
	}
	if !cursor.Equal(since) {
		t.Errorf("cursor advanced on error: %v", cursor)
	}
}

func TestLoki_ValidationErrors(t *testing.T) {
	if _, err := NewLokiSource("t", config.AgentLokiSourceConfig{Query: "x"}); err == nil {
		t.Errorf("expected error for missing address")
	}
	if _, err := NewLokiSource("t", config.AgentLokiSourceConfig{Address: "http://x"}); err == nil {
		t.Errorf("expected error for missing query")
	}
}

// itoa is shorthand for strconv.FormatInt(n, 10) used in test fixtures.
func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
