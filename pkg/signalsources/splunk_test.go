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
	assertNormalizedSignal(t, signals[0], "splunk:test")
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

// fakeSplunkExport mimics the export endpoint of a stock Splunk install:
// unless the request names an output_time_format, `_time` comes back in the
// server's display format ("2026-11-15 08:40:23.294 GMT"), not RFC 3339. That
// is the shape a real Splunk 9/10 container returned for `search index=main`.
// The rows stream newest first, as export does for an event search.
func fakeSplunkExport(t *testing.T, times []time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, err := parseForm(string(body))
		if err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		format := func(ts time.Time) string {
			switch form["output_time_format"] {
			case "":
				return ts.UTC().Format("2006-01-02 15:04:05.000") + " GMT"
			case splunkOutputTimeFormat:
				return fmt.Sprintf("%d.%06d", ts.Unix(), ts.Nanosecond()/1000)
			default:
				t.Errorf("unexpected output_time_format %q", form["output_time_format"])
				return ""
			}
		}
		var rows []map[string]interface{}
		for i := len(times) - 1; i >= 0; i-- {
			rows = append(rows, map[string]interface{}{
				"_time":       format(times[i]),
				"_raw":        fmt.Sprintf("service=api message=\"probe %d\"", i),
				"_indextime":  fmt.Sprintf("%d", times[i].Unix()),
				"_bkt":        "main~0~84E5C37B",
				"_cd":         fmt.Sprintf("0:%d", i),
				"_sourcetype": "noisy",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(splunkExportBody(t, rows))
	}))
}

func TestSplunk_PullLearnsEventsFromStockSplunkExport(t *testing.T) {
	since := time.Date(2026, 11, 15, 8, 40, 0, 0, time.UTC)
	times := []time.Time{
		since.Add(1*time.Second + 294836*time.Microsecond),
		since.Add(2*time.Second + 500*time.Millisecond),
		since.Add(3*time.Second + 123456*time.Microsecond),
	}
	ts := fakeSplunkExport(t, times)
	defer ts.Close()

	src, err := NewSplunkSource("t", config.AgentSplunkSourceConfig{
		Address: ts.URL,
		Search:  "search index=main",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	signals, cursor, err := src.Pull(context.Background(), since)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(signals) != len(times) {
		t.Fatalf("got %d signals, want %d: a stock Splunk export must not be dropped", len(signals), len(times))
	}
	// Rows arrive newest first. Microsecond-exact timestamps only survive
	// when Pull asks for the epoch rendering; Splunk's default display format
	// carries milliseconds at most.
	for i, sig := range signals {
		if want := times[len(times)-1-i]; !sig.Timestamp.Equal(want) {
			t.Errorf("signal %q timestamp = %v, want %v", sig.Message, sig.Timestamp, want)
		}
	}
	if want := times[len(times)-1]; !cursor.Equal(want) {
		t.Errorf("cursor = %v, want %v", cursor, want)
	}
}

func TestSplunk_ParseTimeAcceptsSplunkRenderings(t *testing.T) {
	want := time.Date(2026, 11, 15, 8, 40, 23, 294000000, time.UTC)
	cases := []string{
		"2026-11-15T08:40:23.294+00:00",
		"2026-11-15T10:40:23.294+02:00",
		"2026-11-15 08:40:23.294 GMT",
		"2026-11-15 08:40:23.294 UTC",
		"2026-11-15 08:40:23.294 +0000",
		"2026-11-15 03:40:23.294 -05:00",
		"1794732023.294000",
		"1794732023.294",
	}
	for _, in := range cases {
		if got := parseSplunkTime(in); !got.Equal(want) {
			t.Errorf("parseSplunkTime(%q) = %v, want %v", in, got, want)
		}
	}
	// A bare zone abbreviation other than GMT/UTC is ambiguous (Go would read
	// it as offset zero); refuse it rather than misplace the event in time.
	for _, in := range []string{"2026-11-15 08:40:23.294 PST", "", "not a time"} {
		if got := parseSplunkTime(in); !got.IsZero() {
			t.Errorf("parseSplunkTime(%q) = %v, want zero", in, got)
		}
	}
}

func TestSplunk_MalformedExportRecordDoesNotAdvanceCursor(t *testing.T) {
	// Export streams newest first: keeping the rows before a bad record and
	// advancing the cursor to their max would skip the older rows after it
	// for good. A malformed stream fails the pull instead, so the next tick
	// re-reads the whole window.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"preview":false,"offset":0,"result":{"_time":"2026-11-15T08:40:05.000+00:00","_raw":"newest"}}` + "\n" +
			`{"preview":false,"offset":1,"result":` + "\n"))
	}))
	defer ts.Close()

	src, _ := NewSplunkSource("t", config.AgentSplunkSourceConfig{Address: ts.URL, Search: "search index=main"})
	since := time.Date(2026, 11, 15, 8, 40, 0, 0, time.UTC)
	signals, cursor, err := src.Pull(context.Background(), since)
	if err == nil {
		t.Fatalf("expected an error for a truncated export stream, got %d signals", len(signals))
	}
	if len(signals) != 0 || !cursor.Equal(since) {
		t.Errorf("malformed stream returned %d signals and cursor %v; want none and %v", len(signals), cursor, since)
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
