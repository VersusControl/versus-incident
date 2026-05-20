package signalsources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// splunkExportBody marshals one result per newline-delimited JSON
// record — the wire format Splunk's `export` endpoint produces.
func splunkExportBody(t *testing.T, results []map[string]interface{}) []byte {
	t.Helper()
	var sb strings.Builder
	for i, r := range results {
		b, err := json.Marshal(splunkExportLine{Offset: i, Result: r})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return []byte(sb.String())
}

func TestSplunk_PullBasic(t *testing.T) {
	t1 := "2026-04-20T10:00:01.000+00:00"
	t2 := "2026-04-20T10:00:05.000+00:00"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/services/search/v2/jobs/export" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("unexpected content-type %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		form, err := parseForm(string(body))
		if err != nil {
			t.Fatalf("parse form: %v", err)
		}
		// `search` should have been prefixed automatically.
		if form["search"] != "search index=main error" {
			t.Errorf("unexpected search %q", form["search"])
		}
		if form["output_mode"] != "json" {
			t.Errorf("expected output_mode=json")
		}
		if form["count"] != "10" {
			t.Errorf("expected count=10, got %q", form["count"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(splunkExportBody(t, []map[string]interface{}{
			{
				"_time": t1,
				"_raw":  "ERROR connection refused to db-01",
				"host":  "host-1",
				"level": "error",
			},
			{
				"_time": t2,
				"_raw":  "ERROR timeout on db-02",
				"host":  "host-2",
				"level": "error",
			},
		}))
	}))
	defer ts.Close()

	src, err := NewSplunkSource("test", config.AgentSplunkSourceConfig{
		Address:       ts.URL,
		Search:        "index=main error",
		SeverityField: "level",
		ExtraFields:   []string{"host"},
		PageSize:      10,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if src.Name() != "splunk:test" {
		t.Errorf("unexpected name %q", src.Name())
	}

	signals, cursor, err := src.Pull(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 2 {
		t.Fatalf("expected 2 signals, got %d", len(signals))
	}
	if signals[0].Message != "ERROR connection refused to db-01" {
		t.Errorf("unexpected message %q", signals[0].Message)
	}
	if signals[0].Severity != "error" {
		t.Errorf("expected severity 'error', got %q", signals[0].Severity)
	}
	if signals[0].Fields["host"] != "host-1" {
		t.Errorf("expected host in fields, got %#v", signals[0].Fields)
	}
	wantCursor, _ := time.Parse(time.RFC3339Nano, t2)
	if !cursor.Equal(wantCursor) {
		t.Errorf("cursor = %v, want %v", cursor, wantCursor)
	}
}

func TestSplunk_FiltersResultsAtOrBeforeSince(t *testing.T) {
	older := "2026-04-20T10:00:00.000+00:00"
	newer := "2026-04-20T10:00:10.000+00:00"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(splunkExportBody(t, []map[string]interface{}{
			{"_time": older, "_raw": "old"},
			{"_time": newer, "_raw": "new"},
		}))
	}))
	defer ts.Close()

	src, _ := NewSplunkSource("t", config.AgentSplunkSourceConfig{
		Address: ts.URL,
		Search:  "search index=main",
	})
	since, _ := time.Parse(time.RFC3339, older)
	signals, _, err := src.Pull(context.Background(), since)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != 1 || signals[0].Message != "new" {
		t.Errorf("expected only newer message, got %#v", signals)
	}
}

func TestSplunk_BearerAuthOverridesBasic(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-123" {
			t.Errorf("expected bearer, got %q", got)
		}
		w.Write(splunkExportBody(t, nil))
	}))
	defer ts.Close()

	src, _ := NewSplunkSource("t", config.AgentSplunkSourceConfig{
		Address:  ts.URL,
		Search:   "search index=main",
		Token:    "tok-123",
		Username: "ignored",
		Password: "ignored",
	})
	if _, _, err := src.Pull(context.Background(), time.Time{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestSplunk_BasicAuth(t *testing.T) {
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("unexpected auth %q", got)
		}
		w.Write(splunkExportBody(t, nil))
	}))
	defer ts.Close()

	src, _ := NewSplunkSource("t", config.AgentSplunkSourceConfig{
		Address:  ts.URL,
		Search:   "search index=main",
		Username: "user",
		Password: "pass",
	})
	if _, _, err := src.Pull(context.Background(), time.Time{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
}

func TestSplunk_HTTPErrorReturnsCursor(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	src, _ := NewSplunkSource("t", config.AgentSplunkSourceConfig{
		Address: ts.URL,
		Search:  "search index=main",
	})
	since := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	signals, cursor, err := src.Pull(context.Background(), since)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "splunk") {
		t.Errorf("expected splunk in error, got %v", err)
	}
	if len(signals) != 0 {
		t.Errorf("expected no signals, got %d", len(signals))
	}
	if !cursor.Equal(since) {
		t.Errorf("cursor advanced on error: %v", cursor)
	}
}

func TestSplunk_ValidationErrors(t *testing.T) {
	if _, err := NewSplunkSource("t", config.AgentSplunkSourceConfig{Search: "x"}); err == nil {
		t.Errorf("expected error for missing address")
	}
	if _, err := NewSplunkSource("t", config.AgentSplunkSourceConfig{Address: "http://x"}); err == nil {
		t.Errorf("expected error for missing search")
	}
}

func TestSplunk_NormalizeSearch(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"index=main", "search index=main"},
		{"search index=main", "search index=main"},
		{"| stats count", "| stats count"},
		{"  search index=main  ", "search index=main"},
	}
	for _, c := range cases {
		if got := normalizeSplunkSearch(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// parseForm decodes a URL-encoded request body into a flat map.
func parseForm(body string) (map[string]string, error) {
	out := make(map[string]string)
	for _, pair := range strings.Split(body, "&") {
		if pair == "" {
			continue
		}
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad pair %q", pair)
		}
		k, err := url.QueryUnescape(kv[0])
		if err != nil {
			return nil, err
		}
		v, err := url.QueryUnescape(kv[1])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}
