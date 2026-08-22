package signalsources

import (
	"context"
	"encoding/json"
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
)

// -----------------------------------------------------------------------------
// fakeSigNoz — an httptest stand-in for SigNoz's v5 query API.
//
// It implements exactly the contract the source depends on: POST to
// /api/v5/query_range, epoch-millisecond bounds, `order: [timestamp asc, id
// asc]`, and offset+limit pagination. Anything the source gets wrong about that
// contract shows up here rather than in production.
// -----------------------------------------------------------------------------

type fakeSigNozRow struct {
	id  string
	ts  time.Time
	msg string
	sev string
	// extra is merged into the row's `data` object.
	extra map[string]interface{}
}

type fakeSigNoz struct {
	mu   sync.Mutex
	rows []fakeSigNozRow

	// requests records every request the source issued, in order.
	requests []fakeSigNozRequest
	// omitEnvelopeTimestamp drops the row-level `timestamp` so the source has
	// to recover it from the row payload.
	omitEnvelopeTimestamp bool
	// omitRowID drops `id` from the row payload.
	omitRowID bool
}

type fakeSigNozRequest struct {
	Path    string
	Method  string
	APIKey  string
	RawURL  string
	StartMs int64
	EndMs   int64
	Offset  int
	Limit   int
	Filter  string
	Order   []string
	Signal  string
	ReqType string
}

func newFakeSigNoz() *fakeSigNoz { return &fakeSigNoz{} }

func (f *fakeSigNoz) add(id, msg string, ts time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, fakeSigNozRow{id: id, ts: ts.UTC(), msg: msg})
}

func (f *fakeSigNoz) addFull(r fakeSigNozRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ts = r.ts.UTC()
	f.rows = append(f.rows, r)
}

func (f *fakeSigNoz) lastRequest(t *testing.T) fakeSigNozRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("no request recorded")
	}
	return f.requests[len(f.requests)-1]
}

// decodeRequest parses the v5 body into the flattened shape the assertions use.
func (f *fakeSigNoz) decodeRequest(t *testing.T, r *http.Request) fakeSigNozRequest {
	t.Helper()
	var body struct {
		Start          int64  `json:"start"`
		End            int64  `json:"end"`
		RequestType    string `json:"requestType"`
		CompositeQuery struct {
			Queries []struct {
				Type string `json:"type"`
				Spec struct {
					Name   string `json:"name"`
					Signal string `json:"signal"`
					Filter *struct {
						Expression string `json:"expression"`
					} `json:"filter"`
					Order []struct {
						Key struct {
							Name string `json:"name"`
						} `json:"key"`
						Direction string `json:"direction"`
					} `json:"order"`
					Offset int `json:"offset"`
					Limit  int `json:"limit"`
				} `json:"spec"`
			} `json:"queries"`
		} `json:"compositeQuery"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if len(body.CompositeQuery.Queries) != 1 {
		t.Fatalf("expected exactly 1 builder query, got %d", len(body.CompositeQuery.Queries))
	}
	q := body.CompositeQuery.Queries[0]
	if q.Type != "builder_query" {
		t.Errorf("query type = %q, want builder_query", q.Type)
	}
	req := fakeSigNozRequest{
		Path:    r.URL.Path,
		Method:  r.Method,
		APIKey:  r.Header.Get("SIGNOZ-API-KEY"),
		RawURL:  r.URL.String(),
		StartMs: body.Start,
		EndMs:   body.End,
		Offset:  q.Spec.Offset,
		Limit:   q.Spec.Limit,
		Signal:  q.Spec.Signal,
		ReqType: body.RequestType,
	}
	if q.Spec.Filter != nil {
		req.Filter = q.Spec.Filter.Expression
	}
	for _, o := range q.Spec.Order {
		req.Order = append(req.Order, o.Key.Name+" "+o.Direction)
	}
	return req
}

func (f *fakeSigNoz) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != SigNozQueryRangePath {
			t.Errorf("source requested %q, but only %q is allowed", r.URL.Path, SigNozQueryRangePath)
			http.Error(w, "forbidden path", http.StatusNotFound)
			return
		}
		req := f.decodeRequest(t, r)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		rows := append([]fakeSigNozRow(nil), f.rows...)
		omitTS := f.omitEnvelopeTimestamp
		omitID := f.omitRowID
		f.mu.Unlock()

		start := time.UnixMilli(req.StartMs).UTC()
		end := time.UnixMilli(req.EndMs).UTC()

		var matched []fakeSigNozRow
		for _, row := range rows {
			if row.ts.Before(start) || row.ts.After(end) {
				continue
			}
			matched = append(matched, row)
		}
		// SigNoz's documented order: timestamp asc, id asc.
		sort.SliceStable(matched, func(i, j int) bool {
			if matched[i].ts.Equal(matched[j].ts) {
				return matched[i].id < matched[j].id
			}
			return matched[i].ts.Before(matched[j].ts)
		})

		if req.Offset >= len(matched) {
			matched = nil
		} else {
			matched = matched[req.Offset:]
		}
		if req.Limit > 0 && len(matched) > req.Limit {
			matched = matched[:req.Limit]
		}

		out := make([]map[string]interface{}, 0, len(matched))
		for _, row := range matched {
			data := map[string]interface{}{"body": row.msg}
			if !omitID {
				data["id"] = row.id
			}
			if row.sev != "" {
				data["severity_text"] = row.sev
			}
			for k, v := range row.extra {
				data[k] = v
			}
			entry := map[string]interface{}{"data": data}
			if omitTS {
				data["timestamp"] = row.ts.UnixNano()
			} else {
				entry["timestamp"] = row.ts.Format(time.RFC3339Nano)
			}
			out = append(out, entry)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{
				"type": "raw",
				"data": map[string]interface{}{
					"results": []map[string]interface{}{
						{"queryName": "A", "rows": out},
					},
				},
			},
		})
	}
}

func newTestSigNozSource(t *testing.T, addr string, mutate func(*config.AgentSignozSourceConfig)) *SigNozSource {
	t.Helper()
	cfg := config.AgentSignozSourceConfig{
		Address:  addr,
		APIKey:   "test-signoz-key",
		PageSize: 50,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	src, err := NewSigNozSource("test", cfg)
	if err != nil {
		t.Fatalf("NewSigNozSource: %v", err)
	}
	return src
}

func messagesOf(sigs []core.Signal) []string {
	out := make([]string, len(sigs))
	for i, s := range sigs {
		out[i] = s.Message
	}
	return out
}

// -----------------------------------------------------------------------------
// Constructor / config
// -----------------------------------------------------------------------------

func TestSigNozSource_ConstructorValidation(t *testing.T) {
	cases := []struct {
		name    string
		cfg     config.AgentSignozSourceConfig
		wantErr string
	}{
		{
			name:    "missing address",
			cfg:     config.AgentSignozSourceConfig{APIKey: "k"},
			wantErr: `signoz source "s": address is required`,
		},
		{
			name:    "missing api key",
			cfg:     config.AgentSignozSourceConfig{Address: "http://signoz:8080"},
			wantErr: `signoz source "s": api_key is required`,
		},
		{
			name:    "non-http scheme",
			cfg:     config.AgentSignozSourceConfig{Address: "file:///etc/passwd", APIKey: "k"},
			wantErr: "must use http or https",
		},
		{
			name:    "no host",
			cfg:     config.AgentSignozSourceConfig{Address: "http://", APIKey: "k"},
			wantErr: "missing a host",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSigNozSource("s", tc.cfg)
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestSigNozSource_Defaults(t *testing.T) {
	src := newTestSigNozSource(t, "http://signoz:8080/", func(c *config.AgentSignozSourceConfig) {
		c.PageSize = 0
	})
	if src.cfg.MessageField != "body" {
		t.Errorf("message_field default = %q, want body", src.cfg.MessageField)
	}
	if src.cfg.SeverityField != "severity_text" {
		t.Errorf("severity_field default = %q, want severity_text", src.cfg.SeverityField)
	}
	if src.cfg.PageSize != 500 {
		t.Errorf("page_size default = %d, want 500", src.cfg.PageSize)
	}
	if src.reorderWindow != defaultSigNozReorderWindow {
		t.Errorf("reorder window default = %v, want %v", src.reorderWindow, defaultSigNozReorderWindow)
	}
	if src.Name() != "signoz:test" {
		t.Errorf("Name() = %q, want signoz:test", src.Name())
	}

	clamped := newTestSigNozSource(t, "http://signoz:8080", func(c *config.AgentSignozSourceConfig) {
		c.PageSize = 100000
	})
	if clamped.cfg.PageSize != maxSigNozPageSize {
		t.Errorf("page_size = %d, want it clamped to %d", clamped.cfg.PageSize, maxSigNozPageSize)
	}

	cases := map[string]time.Duration{
		"":               defaultSigNozReorderWindow,
		"not-a-duration": defaultSigNozReorderWindow,
		"-5s":            defaultSigNozReorderWindow,
		"30s":            30 * time.Second,
	}
	for in, want := range cases {
		s := newTestSigNozSource(t, "http://signoz:8080", func(c *config.AgentSignozSourceConfig) {
			c.ReorderWindow = in
		})
		if s.reorderWindow != want {
			t.Errorf("reorder_window %q = %v, want %v", in, s.reorderWindow, want)
		}
	}
}

// -----------------------------------------------------------------------------
// Request shape: path allowlist, auth header, epoch-ms bounds, ordering
// -----------------------------------------------------------------------------

// TestSigNozSource_RequestShape pins everything the v5 contract requires, in one
// place: the single allowed path, POST, the SIGNOZ-API-KEY header, epoch
// MILLISECOND bounds (not seconds, not nanoseconds), signal/requestType, the
// filter expression, and the mandatory `timestamp asc, id asc` tiebreak order.
func TestSigNozSource_RequestShape(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.add("a1", "hello", base)

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.Query = "severity_text = 'ERROR'"
		c.ReorderWindow = "1m"
	})
	now := base.Add(time.Minute)
	src.nowFn = func() time.Time { return now }

	if _, _, err := src.Pull(context.Background(), base.Add(-time.Second)); err != nil {
		t.Fatalf("pull: %v", err)
	}

	req := fake.lastRequest(t)
	if req.Path != SigNozQueryRangePath {
		t.Errorf("path = %q, want %q", req.Path, SigNozQueryRangePath)
	}
	if req.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.Method)
	}
	if req.APIKey != "test-signoz-key" {
		t.Errorf("SIGNOZ-API-KEY = %q, want the configured key", req.APIKey)
	}
	if req.Signal != SigNozSignalLogs {
		t.Errorf("signal = %q, want logs", req.Signal)
	}
	if req.ReqType != SigNozRequestTypeRaw {
		t.Errorf("requestType = %q, want raw", req.ReqType)
	}
	if req.Filter != "severity_text = 'ERROR'" {
		t.Errorf("filter expression = %q, want the configured query", req.Filter)
	}

	wantStart := base.Add(-time.Second).Add(-time.Minute).UnixMilli()
	if req.StartMs != wantStart {
		t.Errorf("start = %d ms, want %d ms (cursor - reorder_window, epoch MILLIS)", req.StartMs, wantStart)
	}
	if req.EndMs != now.UnixMilli() {
		t.Errorf("end = %d ms, want %d ms (now, epoch MILLIS)", req.EndMs, now.UnixMilli())
	}

	wantOrder := []string{"timestamp asc", "id asc"}
	if len(req.Order) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", req.Order, wantOrder)
	}
	for i := range wantOrder {
		if req.Order[i] != wantOrder[i] {
			t.Errorf("order[%d] = %q, want %q", i, req.Order[i], wantOrder[i])
		}
	}
}

// TestSigNozQuerier_PathIsPinned proves the client cannot be steered off
// /api/v5/query_range: the endpoint is derived from the configured base address
// alone, and a base address carrying its own path or query is still resolved to
// the one allowed path.
func TestSigNozQuerier_PathIsPinned(t *testing.T) {
	for _, addr := range []string{
		"http://signoz:8080",
		"http://signoz:8080/",
		"https://eu.signoz.cloud",
	} {
		q, err := NewSigNozQuerier(addr, "k", false)
		if err != nil {
			t.Fatalf("NewSigNozQuerier(%q): %v", addr, err)
		}
		if !strings.HasSuffix(q.Endpoint(), SigNozQueryRangePath) {
			t.Errorf("endpoint %q does not end in %q", q.Endpoint(), SigNozQueryRangePath)
		}
		if strings.Count(q.Endpoint(), SigNozQueryRangePath) != 1 {
			t.Errorf("endpoint %q should contain the pinned path exactly once", q.Endpoint())
		}
	}
}

// TestSigNozSource_NoSecretInURLOrErrors asserts what the header-based auth
// buys us: the API key is never in a URL, so it can never reach an error
// string, a log line, or a proxy access log. Asserted, not assumed.
func TestSigNozSource_NoSecretInURLOrErrors(t *testing.T) {
	const secret = "super-secret-signoz-key"

	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.APIKey = secret
	})
	if _, _, err := src.Pull(context.Background(), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("pull: %v", err)
	}
	req := fake.lastRequest(t)
	if strings.Contains(req.RawURL, secret) {
		t.Fatalf("request URL %q carries the API key — it must be a header only", req.RawURL)
	}
	if req.APIKey != secret {
		t.Fatalf("SIGNOZ-API-KEY header = %q, want the key", req.APIKey)
	}

	// A rejecting backend must not echo the key into the error either.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	defer bad.Close()

	src2 := newTestSigNozSource(t, bad.URL, func(c *config.AgentSignozSourceConfig) {
		c.APIKey = secret
	})
	_, _, err := src2.Pull(context.Background(), time.Now().Add(-time.Minute))
	if err == nil {
		t.Fatal("expected an error from a 401 backend")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error string leaks the API key: %q", err.Error())
	}
}

// -----------------------------------------------------------------------------
// Cursor behaviour
// -----------------------------------------------------------------------------

// TestSigNozSource_ForwardOrderAndCursorAdvance walks the ordinary tail: rows
// come back oldest-first, the cursor lands on the newest timestamp seen, and an
// idle tick leaves the cursor where it was.
func TestSigNozSource_ForwardOrderAndCursorAdvance(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.add("r1", "one", base)
	fake.add("r2", "two", base.Add(10*time.Second))
	fake.add("r3", "three", base.Add(20*time.Second))

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.ReorderWindow = "30s"
	})
	now := base.Add(time.Minute)
	src.nowFn = func() time.Time { return now }

	sigs, cursor, err := src.Pull(context.Background(), base.Add(-time.Second))
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if got := messagesOf(sigs); len(got) != 3 || got[0] != "one" || got[2] != "three" {
		t.Fatalf("tick1 messages = %v, want [one two three] in forward order", got)
	}
	if want := base.Add(20 * time.Second); !cursor.Equal(want) {
		t.Fatalf("tick1 cursor = %v, want %v (max timestamp seen)", cursor, want)
	}

	sigs2, cursor2, err := src.Pull(context.Background(), cursor)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(sigs2) != 0 {
		t.Errorf("idle tick re-emitted %d signals: %v", len(sigs2), messagesOf(sigs2))
	}
	if !cursor2.Equal(cursor) {
		t.Errorf("idle tick moved the cursor from %v to %v", cursor, cursor2)
	}
}

// TestSigNozSource_MillisecondTieDoesNotDropRows is the reason this source uses
// the Elasticsearch model rather than the Loki one.
//
// SigNoz request bounds are MILLISECOND precision and there is no server cursor.
// A Loki-style tail (start = cursor + 1 unit, cursor = max timestamp) drops
// every row that shares the boundary millisecond with the last row of the
// previous tick — the second and third rows below would be lost forever.
// Written to fail against that design: it asserts all three same-millisecond
// rows are delivered exactly once across the two ticks.
func TestSigNozSource_MillisecondTieDoesNotDropRows(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	tie := base.Add(10 * time.Second)

	// Three rows in the SAME millisecond, only distinguishable by `id`. The
	// first tick sees one of them; the rest arrive before tick two.
	fake.add("tie-a", "tie a", tie)

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.ReorderWindow = "30s"
	})
	now := base.Add(time.Minute)
	src.nowFn = func() time.Time { return now }

	sigs1, cursor, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if got := messagesOf(sigs1); len(got) != 1 || got[0] != "tie a" {
		t.Fatalf("tick1 messages = %v, want [tie a]", got)
	}
	if !cursor.Equal(tie) {
		t.Fatalf("tick1 cursor = %v, want %v", cursor, tie)
	}

	fake.add("tie-b", "tie b", tie)
	fake.add("tie-c", "tie c", tie)

	sigs2, _, err := src.Pull(context.Background(), cursor)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	got := map[string]int{}
	for _, m := range messagesOf(sigs2) {
		got[m]++
	}
	if got["tie b"] != 1 || got["tie c"] != 1 {
		t.Errorf("same-millisecond rows lost: tick2 delivered %v, want tie b and tie c exactly once", messagesOf(sigs2))
	}
	if got["tie a"] != 0 {
		t.Errorf("tick2 re-emitted %q — the id dedup set did not suppress the re-scan", "tie a")
	}
}

// TestSigNozSource_OffsetWalkWithinTick proves pagination: with page_size 2 and
// five matching rows the source walks offset 0, 2, 4 within ONE tick, and does
// not carry an offset into the next tick (the window moves, so a carried offset
// would point at a different row set).
func TestSigNozSource_OffsetWalkWithinTick(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	for i, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		fake.add(id, id, base.Add(time.Duration(i)*time.Second))
	}

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.PageSize = 2
		c.ReorderWindow = "1m"
	})
	now := base.Add(time.Minute)
	src.nowFn = func() time.Time { return now }

	sigs, cursor, err := src.Pull(context.Background(), base.Add(-time.Second))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got := messagesOf(sigs); len(got) != 5 {
		t.Fatalf("got %d signals (%v), want all 5 — the offset walk stopped early", len(got), got)
	}
	if want := base.Add(4 * time.Second); !cursor.Equal(want) {
		t.Errorf("cursor = %v, want %v", cursor, want)
	}

	fake.mu.Lock()
	offsets := make([]int, len(fake.requests))
	for i, r := range fake.requests {
		offsets[i] = r.Offset
	}
	fake.mu.Unlock()
	want := []int{0, 2, 4}
	if len(offsets) != len(want) {
		t.Fatalf("offsets = %v, want %v", offsets, want)
	}
	for i := range want {
		if offsets[i] != want[i] {
			t.Errorf("offsets = %v, want %v", offsets, want)
			break
		}
	}

	// The next tick must restart at offset 0.
	beforeTick2 := len(offsets)
	if _, _, err := src.Pull(context.Background(), cursor); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	fake.mu.Lock()
	firstOfTick2 := fake.requests[beforeTick2].Offset
	fake.mu.Unlock()
	if firstOfTick2 != 0 {
		t.Errorf("tick2 first offset = %d, want 0 — offset must not be carried across ticks", firstOfTick2)
	}
}

// TestSigNozSource_ReorderWindowBound locks the bounded trade-off: a row that
// becomes queryable late but lands INSIDE the reorder window is recovered; one
// beyond the window is not, which is what keeps the dedup set bounded.
func TestSigNozSource_ReorderWindowBound(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.add("b1", "backlog one", base)
	fake.add("b2", "backlog two", base.Add(20*time.Second))

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.ReorderWindow = "10s"
	})
	now := base.Add(2 * time.Minute)
	src.nowFn = func() time.Time { return now }

	_, cursor, err := src.Pull(context.Background(), base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}

	fake.add("late-in", "inside window", cursor.Add(-5*time.Second))
	fake.add("late-out", "beyond window", cursor.Add(-30*time.Second))

	sigs, _, err := src.Pull(context.Background(), cursor)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	got := map[string]bool{}
	for _, m := range messagesOf(sigs) {
		got[m] = true
	}
	if !got["inside window"] {
		t.Error("a late row inside the reorder window was not recovered")
	}
	if got["beyond window"] {
		t.Error("a row beyond the reorder window was recovered — the bound is not honoured")
	}
	if len(sigs) != 1 {
		t.Errorf("tick2 delivered %v, want exactly the one in-window row", messagesOf(sigs))
	}
}

// TestSigNozSource_CursorNeverPassesWallClock proves a future-dated row — an
// untrusted producer timestamp — cannot strand the tail past `now`.
func TestSigNozSource_CursorNeverPassesWallClock(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	now := base.Add(time.Minute)
	fake.add("sane", "sane row", base.Add(10*time.Second))
	fake.add("future", "future row", base.AddDate(22, 0, 0))

	src := newTestSigNozSource(t, ts.URL, nil)
	src.nowFn = func() time.Time { return now }

	_, cursor, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if cursor.After(now) {
		t.Fatalf("cursor = %v advanced past the wall clock %v", cursor, now)
	}

	// A cursor persisted from the future must heal rather than wedge.
	_, healed, err := src.Pull(context.Background(), now.AddDate(10, 0, 0))
	if err != nil {
		t.Fatalf("healing pull: %v", err)
	}
	if healed.After(now) {
		t.Fatalf("healed cursor = %v, want <= %v", healed, now)
	}
}

// TestSigNozSource_CursorPersistenceAcrossRestart pins the NO-GAP half of the
// restart contract on the in-memory path (no dedup backend attached): a fresh
// source resuming on the persisted timestamp must still deliver a row that
// shares the cursor's boundary millisecond, because the query lower bound is
// inclusive. It must also not reach further back than one reorder window.
//
// The zero-duplicate half is covered by the persisted-set tests in
// tailing_dedup_test.go.
func TestSigNozSource_CursorPersistenceAcrossRestart(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	boundary := base.Add(30 * time.Second)
	// `before` sits further back than one reorder window below the boundary, so
	// the post-restart re-scan must not reach it.
	fake.add("k1", "before", base.Add(10*time.Second))
	fake.add("k2", "boundary a", boundary)

	cfg := func(c *config.AgentSignozSourceConfig) { c.ReorderWindow = "5s" }
	now := base.Add(2 * time.Minute)

	before := newTestSigNozSource(t, ts.URL, cfg)
	before.nowFn = func() time.Time { return now }
	_, persisted, err := before.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pre-restart tick: %v", err)
	}
	if !persisted.Equal(boundary) {
		t.Fatalf("persisted cursor = %v, want %v", persisted, boundary)
	}

	// A second row lands in the SAME millisecond as the persisted cursor while
	// the process is down — the exact case a timestamp-only cursor would drop.
	fake.add("k3", "boundary b", boundary)

	// Restart: brand-new source, only the timestamp survived.
	after := newTestSigNozSource(t, ts.URL, cfg)
	after.nowFn = func() time.Time { return now }
	sigs, _, err := after.Pull(context.Background(), persisted)
	if err != nil {
		t.Fatalf("post-restart tick: %v", err)
	}
	got := map[string]int{}
	for _, m := range messagesOf(sigs) {
		got[m]++
	}
	if got["boundary b"] != 1 {
		t.Errorf("row sharing the cursor millisecond was dropped across restart: got %v", messagesOf(sigs))
	}
	// The re-scan stops one reorder window below the cursor.
	for _, m := range messagesOf(sigs) {
		if m == "before" {
			t.Errorf("re-emitted %q, which is older than one reorder window below the cursor", m)
		}
	}
}

// TestSigNozSource_RewindClearsDedup asserts a catalog clear makes the source
// relearn its window instead of being suppressed by pre-clear ids.
func TestSigNozSource_RewindClearsDedup(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.add("r1", "one", base.Add(5*time.Second))

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.ReorderWindow = "1m"
	})
	src.nowFn = func() time.Time { return base.Add(time.Minute) }

	if _, _, err := src.Pull(context.Background(), base); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if err := src.Rewind(context.Background()); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	sigs, _, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(sigs) != 1 {
		t.Errorf("after Rewind got %v, want the window re-emitted", messagesOf(sigs))
	}
}

// -----------------------------------------------------------------------------
// Field mapping
// -----------------------------------------------------------------------------

func TestSigNozSource_FieldMapping(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.addFull(fakeSigNozRow{
		id:  "f1",
		ts:  base.Add(time.Second),
		msg: "payment declined",
		sev: "ERROR",
		extra: map[string]interface{}{
			"service.name": "billing",
			"k8s":          map[string]interface{}{"pod": "billing-7f9"},
		},
	})

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.ExtraFields = []string{"service.name", "k8s.pod", "absent.field"}
	})
	src.nowFn = func() time.Time { return base.Add(time.Minute) }

	sigs, _, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("got %d signals, want 1", len(sigs))
	}
	sig := sigs[0]
	if sig.Source != "signoz:test" {
		t.Errorf("Source = %q, want signoz:test", sig.Source)
	}
	if sig.Message != "payment declined" {
		t.Errorf("Message = %q, want the body attribute", sig.Message)
	}
	if sig.Severity != "ERROR" {
		t.Errorf("Severity = %q, want ERROR", sig.Severity)
	}
	if sig.Fields["service.name"] != "billing" {
		t.Errorf("Fields[service.name] = %v, want billing (flat dotted lookup)", sig.Fields["service.name"])
	}
	if sig.Fields["k8s.pod"] != "billing-7f9" {
		t.Errorf("Fields[k8s.pod] = %v, want billing-7f9 (nested path walk)", sig.Fields["k8s.pod"])
	}
	if _, present := sig.Fields["absent.field"]; present {
		t.Error("an absent extra field should not be materialised in Fields")
	}
	if sig.Raw["body"] != "payment declined" {
		t.Error("Raw should carry the original row payload")
	}
}

// TestSigNozSource_TimestampFallbacks covers the row shapes the envelope does
// not fully specify: SigNoz omits the row-level `timestamp` when zero, so the
// source must recover it from the payload, where the unit is nanoseconds.
func TestSigNozSource_TimestampFallbacks(t *testing.T) {
	fake := newFakeSigNoz()
	fake.omitEnvelopeTimestamp = true
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.add("t1", "nano timestamp", base.Add(3*time.Second))

	src := newTestSigNozSource(t, ts.URL, nil)
	src.nowFn = func() time.Time { return base.Add(time.Minute) }

	sigs, cursor, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("got %d signals, want 1", len(sigs))
	}
	if want := base.Add(3 * time.Second); !sigs[0].Timestamp.Equal(want) {
		t.Errorf("timestamp = %v, want %v (nanosecond epoch from the row payload)", sigs[0].Timestamp, want)
	}
	if !cursor.Equal(base.Add(3 * time.Second)) {
		t.Errorf("cursor = %v, want it to follow the recovered timestamp", cursor)
	}
}

// TestSigNozSource_DedupWithoutRowID covers a backend that does not return `id`.
// Without a fallback key the dedup set would be empty every tick and the whole
// reorder window would be re-emitted on every poll.
func TestSigNozSource_DedupWithoutRowID(t *testing.T) {
	fake := newFakeSigNoz()
	fake.omitRowID = true
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.add("x1", "no id here", base.Add(5*time.Second))

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.ReorderWindow = "1m"
	})
	src.nowFn = func() time.Time { return base.Add(time.Minute) }

	if _, _, err := src.Pull(context.Background(), base); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	sigs, _, err := src.Pull(context.Background(), base.Add(5*time.Second))
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(sigs) != 0 {
		t.Errorf("tick2 re-emitted %v — dedup must still work without a row id", messagesOf(sigs))
	}
}

// TestSigNozSource_NestedAttributeContainers is the end-to-end regression for
// the shape a real SigNoz returns: OTLP attributes are NOT flattened, they sit
// in per-type maps whose keys carry the dots. A plain dotted-path walk finds
// neither a flat `service.name` nor a `service` map, so the field an operator
// most wants used to resolve to nothing.
func TestSigNozSource_NestedAttributeContainers(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.addFull(fakeSigNozRow{
		id:  "n1",
		ts:  base.Add(time.Second),
		msg: "upstream returned 500",
		sev: "ERROR",
		extra: map[string]interface{}{
			"resources_string": map[string]interface{}{
				"service.name":           "checkout",
				"deployment.environment": "prod",
				"k8s.namespace.name":     "shop",
			},
			"attributes_string": map[string]interface{}{
				"http.status_code": "500",
				"http.method":      "POST",
			},
			"scope_string": map[string]interface{}{
				"otel.library.name": "net/http",
			},
		},
	})

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.ExtraFields = []string{
			"service.name",                        // bare, resolved via the containers
			"resources_string.k8s.namespace.name", // container-qualified
			"http.status_code",                    // bare, only in attributes_string
			"otel.library.name",                   // bare, only in scope_string
			"resources_string",                    // the whole container map
			"nope.not.here",                       // absent everywhere
		}
	})
	src.nowFn = func() time.Time { return base.Add(time.Minute) }

	sigs, _, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("got %d signals, want 1", len(sigs))
	}
	f := sigs[0].Fields

	if f["service.name"] != "checkout" {
		t.Errorf("Fields[service.name] = %v, want checkout — the bare dotted name must resolve through resources_string", f["service.name"])
	}
	// The container prefix is a storage detail, so it is stripped from the key.
	if f["k8s.namespace.name"] != "shop" {
		t.Errorf("Fields[k8s.namespace.name] = %v, want shop", f["k8s.namespace.name"])
	}
	if _, present := f["resources_string.k8s.namespace.name"]; present {
		t.Error("the container prefix must not survive into the emitted Fields key")
	}
	if f["http.status_code"] != "500" {
		t.Errorf("Fields[http.status_code] = %v, want 500", f["http.status_code"])
	}
	if f["otel.library.name"] != "net/http" {
		t.Errorf("Fields[otel.library.name] = %v, want net/http", f["otel.library.name"])
	}
	if m, ok := f["resources_string"].(map[string]interface{}); !ok || m["service.name"] != "checkout" {
		t.Errorf("Fields[resources_string] = %v, want the whole container map", f["resources_string"])
	}
	if _, present := f["nope.not.here"]; present {
		t.Error("an absent attribute must not be materialised in Fields")
	}
}

// TestSigNozSource_SeverityFromNestedContainer pins that severity_field goes
// through the same resolver — an operator who stamps severity as an OTLP log
// attribute rather than using the severity_text column must not silently get "".
func TestSigNozSource_SeverityFromNestedContainer(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.addFull(fakeSigNozRow{
		id:  "s1",
		ts:  base.Add(time.Second),
		msg: "pool exhausted",
		extra: map[string]interface{}{
			"attributes_string": map[string]interface{}{
				"log.level": "CRITICAL",
				"log.body":  "pool exhausted (attribute copy)",
			},
		},
	})

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.SeverityField = "log.level"
		c.MessageField = "attributes_string.log.body"
	})
	src.nowFn = func() time.Time { return base.Add(time.Minute) }

	sigs, _, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("got %d signals, want 1", len(sigs))
	}
	if sigs[0].Severity != "CRITICAL" {
		t.Errorf("Severity = %q, want CRITICAL from attributes_string", sigs[0].Severity)
	}
	if sigs[0].Message != "pool exhausted (attribute copy)" {
		t.Errorf("Message = %q, want the container-qualified attribute", sigs[0].Message)
	}
}

// TestSigNozLookupField_Precedence pins the resolution order. It must be
// deterministic: the containers are searched as an ordered slice, never by
// ranging a map, so a name present in two containers always resolves the same
// way.
func TestSigNozLookupField_Precedence(t *testing.T) {
	data := map[string]interface{}{
		"body":          "flat column wins",
		"severity_text": "ERROR",
		"resources_string": map[string]interface{}{
			"service.name": "from-resources",
			"host.name":    "node-a",
		},
		"attributes_string": map[string]interface{}{
			"service.name": "from-attributes",
			"http.route":   "/checkout",
		},
		"scope_string": map[string]interface{}{
			"service.name": "from-scope",
		},
		"k8s": map[string]interface{}{"pod": "checkout-7f9"},
	}

	cases := []struct {
		name  string
		path  string
		want  interface{}
		found bool
	}{
		{"top-level column", "body", "flat column wins", true},
		{"bare name prefers resource attributes", "service.name", "from-resources", true},
		{"explicit container beats the bare precedence", "attributes_string.service.name", "from-attributes", true},
		{"explicit scope container", "scope_string.service.name", "from-scope", true},
		{"bare name only in log attributes", "http.route", "/checkout", true},
		{"bare name only in resource attributes", "host.name", "node-a", true},
		{"whole container map is addressable", "resources_string", data["resources_string"], true},
		{"genuinely nested json still walks", "k8s.pod", "checkout-7f9", true},
		{"absent", "service.version", nil, false},
		{"absent inside a real container", "resources_string.service.version", nil, false},
		{"empty path", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Repeat: a map-iteration-order bug would only show intermittently.
			for i := 0; i < 50; i++ {
				got, ok := sigNozLookupField(data, tc.path)
				if ok != tc.found {
					t.Fatalf("found = %v, want %v", ok, tc.found)
				}
				if !tc.found {
					continue
				}
				if m, isMap := tc.want.(map[string]interface{}); isMap {
					gm, isGotMap := got.(map[string]interface{})
					if !isGotMap || len(gm) != len(m) {
						t.Fatalf("got %#v, want %#v", got, tc.want)
					}
					continue
				}
				if got != tc.want {
					t.Fatalf("iteration %d: got %#v, want %#v", i, got, tc.want)
				}
			}
		})
	}
}

// TestSigNozFieldKey pins how a configured name maps to the emitted
// Signal.Fields key: a recognised container prefix is stripped, anything else
// is passed through verbatim.
func TestSigNozFieldKey(t *testing.T) {
	cases := map[string]string{
		"service.name":                       "service.name",
		"resources_string.service.name":      "service.name",
		"attributes_string.http.status_code": "http.status_code",
		"scope_string.otel.library.name":     "otel.library.name",
		"resources_string":                   "resources_string",
		"body":                               "body",
		"k8s.pod":                            "k8s.pod",
		"resources_string.":                  "resources_string.",
	}
	for in, want := range cases {
		if got := sigNozFieldKey(in); got != want {
			t.Errorf("sigNozFieldKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSigNozSource_UnixEpochByMagnitude(t *testing.T) {
	want := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	cases := map[string]int64{
		"seconds":     want.Unix(),
		"millis":      want.UnixMilli(),
		"nanoseconds": want.UnixNano(),
	}
	for name, in := range cases {
		if got := unixEpochByMagnitude(in); !got.Equal(want) {
			t.Errorf("%s: unixEpochByMagnitude(%d) = %v, want %v", name, in, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// Failure handling
// -----------------------------------------------------------------------------

// TestSigNozSource_MalformedPayloadsFailSoft feeds the source garbage and
// asserts each case returns (no panic, no wedged worker) and never advances the
// cursor past what was actually understood.
func TestSigNozSource_MalformedPayloadsFailSoft(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "not json", body: `<html>502 bad gateway</html>`, wantErr: true},
		{name: "truncated json", body: `{"status":"success","data":{"data":{"results":[`, wantErr: true},
		{name: "null body", body: `null`},
		{name: "empty object", body: `{}`},
		{name: "results wrong shape", body: `{"data":{"data":{"results":[]}}}`},
		{name: "row is null", body: `{"data":{"data":{"results":[{"rows":[null]}]}}}`},
		{name: "row has no timestamp", body: `{"data":{"data":{"results":[{"rows":[{"data":{"body":"x"}}]}]}}}`},
		{name: "data is not an object", body: `{"data":{"data":{"results":[{"rows":[{"timestamp":"2026-05-01T08:00:00Z","data":"nope"}]}]}}}`, wantErr: true},
		{name: "timestamp is nonsense", body: `{"data":{"data":{"results":[{"rows":[{"data":{"timestamp":"yesterday","body":"x"}}]}]}}}`},
	}

	since := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			src := newTestSigNozSource(t, ts.URL, nil)
			src.nowFn = func() time.Time { return since.Add(time.Minute) }

			sigs, cursor, err := src.Pull(context.Background(), since)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error for a malformed payload")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(sigs) != 0 {
				t.Errorf("got %d signals from a malformed payload, want none", len(sigs))
			}
			if cursor.Before(since) {
				t.Errorf("cursor rewound to %v, below since %v", cursor, since)
			}
		})
	}
}

// TestSigNozQuerier_RetriesTransientAndNotBadRequest pins the retry policy:
// 429 and 5xx are retried with backoff up to the bounded attempt count, a 400
// is permanent and issued exactly once, and a recovery on the second attempt
// succeeds.
func TestSigNozQuerier_RetriesTransientAndNotBadRequest(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		recoverAfter int
		wantCalls    int
		wantErr      bool
	}{
		{name: "429 exhausts attempts", status: http.StatusTooManyRequests, recoverAfter: -1, wantCalls: sigNozMaxAttempts, wantErr: true},
		{name: "503 exhausts attempts", status: http.StatusServiceUnavailable, recoverAfter: -1, wantCalls: sigNozMaxAttempts, wantErr: true},
		{name: "400 is never retried", status: http.StatusBadRequest, recoverAfter: -1, wantCalls: 1, wantErr: true},
		{name: "401 is never retried", status: http.StatusUnauthorized, recoverAfter: -1, wantCalls: 1, wantErr: true},
		{name: "503 then success", status: http.StatusServiceUnavailable, recoverAfter: 1, wantCalls: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				n := calls
				calls++
				mu.Unlock()
				if tc.recoverAfter >= 0 && n >= tc.recoverAfter {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":{"data":{"results":[]}}}`))
					return
				}
				http.Error(w, "backend says no", tc.status)
			}))
			defer ts.Close()

			q, err := NewSigNozQuerier(ts.URL, "k", false)
			if err != nil {
				t.Fatalf("new querier: %v", err)
			}
			var slept []time.Duration
			q.sleep = func(d time.Duration) { slept = append(slept, d) }

			_, err = q.QueryRangeRaw(context.Background(), SigNozQueryRangeRequest{
				Start:   time.Now().Add(-time.Minute),
				End:     time.Now(),
				Queries: []SigNozBuilderQuery{{Name: "A", Signal: SigNozSignalLogs, Limit: 10}},
			})
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			mu.Lock()
			got := calls
			mu.Unlock()
			if got != tc.wantCalls {
				t.Errorf("backend was called %d times, want %d", got, tc.wantCalls)
			}
			if len(slept) != tc.wantCalls-1 {
				t.Errorf("slept %d times (%v), want %d backoffs", len(slept), slept, tc.wantCalls-1)
			}
			for i := 1; i < len(slept); i++ {
				if slept[i] <= slept[i-1] {
					t.Errorf("backoff did not grow: %v", slept)
					break
				}
			}
		})
	}
}

// TestSigNozQuerier_NoQueriesRejected keeps a caller from sending an empty
// composite query, which SigNoz rejects with a 400 that costs a round trip.
func TestSigNozQuerier_NoQueriesRejected(t *testing.T) {
	q, err := NewSigNozQuerier("http://signoz:8080", "k", false)
	if err != nil {
		t.Fatalf("new querier: %v", err)
	}
	if _, err := q.QueryRange(context.Background(), SigNozQueryRangeRequest{}); err == nil {
		t.Fatal("expected an error for a query_range with no queries")
	}
}

// TestSigNozQuerier_BodyIsEpochMillis pins the unit at the wire level: SigNoz
// reads `start`/`end` as epoch MILLISECONDS, and sending seconds or nanoseconds
// silently returns the wrong window rather than an error.
func TestSigNozQuerier_BodyIsEpochMillis(t *testing.T) {
	start := time.Date(2026, 5, 1, 8, 0, 0, 500*int(time.Millisecond), time.UTC)
	end := start.Add(90 * time.Second)

	body, err := buildSigNozQueryBody(SigNozQueryRangeRequest{
		Start:       start,
		End:         end,
		RequestType: SigNozRequestTypeRaw,
		Queries:     []SigNozBuilderQuery{{Name: "A", Signal: SigNozSignalLogs, Limit: 10}},
	})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	var got struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Start != start.UnixMilli() {
		t.Errorf("start = %d, want %d (epoch millis)", got.Start, start.UnixMilli())
	}
	if got.End != end.UnixMilli() {
		t.Errorf("end = %d, want %d (epoch millis)", got.End, end.UnixMilli())
	}
}

// TestSigNozQuerier_DecodesBothEnvelopeNestings covers the wrapped
// (`data.data.results`) and unwrapped (`data.results`) response shapes.
func TestSigNozQuerier_DecodesBothEnvelopeNestings(t *testing.T) {
	cases := map[string]string{
		"wrapped":   `{"status":"success","data":{"type":"raw","data":{"results":[{"queryName":"A","rows":[{"timestamp":"2026-05-01T08:00:00Z","data":{"id":"1"}}]}]}}}`,
		"unwrapped": `{"type":"raw","data":{"results":[{"queryName":"A","rows":[{"timestamp":"2026-05-01T08:00:00Z","data":{"id":"1"}}]}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			results, err := decodeSigNozRawResults([]byte(body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(results) != 1 || len(results[0].Rows) != 1 {
				t.Fatalf("decoded %+v, want one result with one row", results)
			}
			if results[0].QueryName != "A" {
				t.Errorf("queryName = %q, want A", results[0].QueryName)
			}
		})
	}
}

// TestSigNozQuerier_ContextCancellationStopsRetry asserts a cancelled tick does
// not keep hammering the backend through its backoff schedule.
func TestSigNozQuerier_ContextCancellationStopsRetry(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	q, err := NewSigNozQuerier(ts.URL, "k", false)
	if err != nil {
		t.Fatalf("new querier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.sleep = func(time.Duration) { cancel() }

	_, err = q.QueryRangeRaw(ctx, SigNozQueryRangeRequest{
		Start:   time.Now().Add(-time.Minute),
		End:     time.Now(),
		Queries: []SigNozBuilderQuery{{Name: "A", Signal: SigNozSignalLogs, Limit: 10}},
	})
	if err == nil {
		t.Fatal("expected an error after cancellation")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("backend called %d times after cancellation, want 1", calls)
	}
}

// -----------------------------------------------------------------------------
// Metrics aggregation seam
// -----------------------------------------------------------------------------

// TestSigNozSource_LogsRequestBodyIsByteIdentical is a golden test over the
// bytes the LOGS source actually puts on the wire. The aggregation, step and
// select-field seams are additive, and the failure mode of getting that wrong
// is silent: a body that gained a key SigNoz rejects (or reinterprets) stops
// log ingestion without anyone noticing — an unset seam must leave no trace in
// these bytes. Freeze the clock, freeze the cursor, compare the raw bytes.
func TestSigNozSource_LogsRequestBodyIsByteIdentical(t *testing.T) {
	var got []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got == nil {
			got, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"results":[]}}}`))
	}))
	defer ts.Close()

	src := newTestSigNozSource(t, ts.URL, func(c *config.AgentSignozSourceConfig) {
		c.Query = "severity_text = 'ERROR'"
		c.PageSize = 50
	})
	now := time.Date(2026, 5, 1, 8, 1, 30, 0, time.UTC)
	src.nowFn = func() time.Time { return now }

	if _, _, err := src.Pull(context.Background(), time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// `start` is the cursor minus the default 2m reorder window, in epoch millis.
	const want = `{"compositeQuery":{"queries":[{"spec":{"filter":{"expression":"severity_text = 'ERROR'"},"limit":50,"name":"A","offset":0,"order":[{"direction":"asc","key":{"name":"timestamp"}},{"direction":"asc","key":{"name":"id"}}],"signal":"logs"},"type":"builder_query"}]},"end":1777622490000,"requestType":"raw","start":1777622280000}`
	if string(got) != want {
		t.Errorf("logs request body changed.\n got: %s\nwant: %s", got, want)
	}
}

// TestSigNozQuerier_AggregationFieldsOmittedWhenUnset backs the golden above at
// the builder level: SigNoz validates the envelope, so an empty `aggregations`
// array or a zero `stepInterval` is NOT the same as the key being absent.
func TestSigNozQuerier_AggregationFieldsOmittedWhenUnset(t *testing.T) {
	body, err := buildSigNozQueryBody(SigNozQueryRangeRequest{
		Start:       time.Now().Add(-time.Minute),
		End:         time.Now(),
		RequestType: SigNozRequestTypeRaw,
		Queries: []SigNozBuilderQuery{{
			Name:         "A",
			Signal:       SigNozSignalLogs,
			Limit:        10,
			Aggregations: []SigNozAggregation{},
			StepInterval: 0,
			SelectFields: []SigNozSelectField{},
		}},
	})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	spec := sigNozSpecOf(t, body)
	for _, k := range []string{"aggregations", "stepInterval", "selectFields"} {
		if _, present := spec[k]; present {
			t.Errorf("spec carries %q when unset: %v", k, spec)
		}
	}
	// A sub-second step is not representable on the wire (whole seconds), so it
	// must be omitted rather than sent as 0 — which SigNoz reads as "no step".
	body, err = buildSigNozQueryBody(SigNozQueryRangeRequest{
		Start:       time.Now().Add(-time.Minute),
		End:         time.Now(),
		RequestType: SigNozRequestTypeRaw,
		Queries:     []SigNozBuilderQuery{{Name: "A", Signal: SigNozSignalLogs, StepInterval: 500 * time.Millisecond}},
	})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	if _, present := sigNozSpecOf(t, body)["stepInterval"]; present {
		t.Error("a sub-second step interval must be omitted, not rounded to 0")
	}
}

// TestSigNozQuerier_MetricsAggregationBody pins the metrics shape SigNoz's own
// query-builder types require: the metric NAME lives only inside the
// aggregation, alongside temporality and the two reducers, plus a step interval
// in whole SECONDS.
func TestSigNozQuerier_MetricsAggregationBody(t *testing.T) {
	body, err := buildSigNozQueryBody(SigNozQueryRangeRequest{
		Start:       time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		RequestType: SigNozRequestTypeTimeSeries,
		Queries: []SigNozBuilderQuery{{
			Name:   "A",
			Signal: SigNozSignalMetrics,
			Filter: "service.name = 'checkout'",
			Aggregations: []SigNozAggregation{{
				MetricName:       "http_server_duration",
				Temporality:      "delta",
				TimeAggregation:  "rate",
				SpaceAggregation: "p95",
			}},
			StepInterval: 60 * time.Second,
		}},
	})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}

	spec := sigNozSpecOf(t, body)
	if got, ok := spec["stepInterval"].(float64); !ok || int64(got) != 60 {
		t.Errorf("stepInterval = %v, want 60 (whole seconds)", spec["stepInterval"])
	}
	aggs, ok := spec["aggregations"].([]interface{})
	if !ok || len(aggs) != 1 {
		t.Fatalf("aggregations = %v, want one entry", spec["aggregations"])
	}
	agg, _ := aggs[0].(map[string]interface{})
	for k, want := range map[string]string{
		"metricName":       "http_server_duration",
		"temporality":      "delta",
		"timeAggregation":  "rate",
		"spaceAggregation": "p95",
	} {
		if agg[k] != want {
			t.Errorf("aggregation[%q] = %v, want %q", k, agg[k], want)
		}
	}
}

// TestSigNozQuerier_MetricsAggregationOptionalKeysOmitted keeps the blank
// reducers off the wire: SigNoz defaults them, but rejects an empty string.
func TestSigNozQuerier_MetricsAggregationOptionalKeysOmitted(t *testing.T) {
	body, err := buildSigNozQueryBody(SigNozQueryRangeRequest{
		Start:       time.Now().Add(-time.Hour),
		End:         time.Now(),
		RequestType: SigNozRequestTypeTimeSeries,
		Queries: []SigNozBuilderQuery{{
			Name:         "A",
			Signal:       SigNozSignalMetrics,
			Aggregations: []SigNozAggregation{{MetricName: "up"}},
		}},
	})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	aggs := sigNozSpecOf(t, body)["aggregations"].([]interface{})
	agg := aggs[0].(map[string]interface{})
	if len(agg) != 1 || agg["metricName"] != "up" {
		t.Errorf("aggregation = %v, want only metricName", agg)
	}
}

// TestSigNozQuerier_SelectFieldsBody pins the shape of an extended column set:
// SigNoz returns a span's `has_error` flag ONLY when it is selected, so a
// consumer that reads the flag off a default raw row sees every span succeed.
func TestSigNozQuerier_SelectFieldsBody(t *testing.T) {
	body, err := buildSigNozQueryBody(SigNozQueryRangeRequest{
		Start:       time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC),
		RequestType: SigNozRequestTypeRaw,
		Queries: []SigNozBuilderQuery{{
			Name:   "A",
			Signal: SigNozSignalTraces,
			Limit:  10,
			SelectFields: []SigNozSelectField{
				{Name: "has_error", FieldContext: "span", FieldDataType: "bool"},
				{Name: "service.name"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}

	fields, ok := sigNozSpecOf(t, body)["selectFields"].([]interface{})
	if !ok || len(fields) != 2 {
		t.Fatalf("selectFields = %v, want two entries", sigNozSpecOf(t, body)["selectFields"])
	}
	first, _ := fields[0].(map[string]interface{})
	for k, want := range map[string]string{
		"name":          "has_error",
		"fieldContext":  "span",
		"fieldDataType": "bool",
	} {
		if first[k] != want {
			t.Errorf("selectFields[0][%q] = %v, want %q", k, first[k], want)
		}
	}
	// The optional keys stay off the wire rather than going out blank.
	second, _ := fields[1].(map[string]interface{})
	if len(second) != 1 || second["name"] != "service.name" {
		t.Errorf("selectFields[1] = %v, want only name", second)
	}
}

// TestSigNozQuerier_MetricsValidation catches the locally-visible defects
// before a round trip, so the operator sees our message instead of an opaque
// 400 from SigNoz.
func TestSigNozQuerier_MetricsValidation(t *testing.T) {
	cases := []struct {
		name    string
		query   SigNozBuilderQuery
		wantSub string
	}{
		{
			name:    "metrics without aggregations has no subject",
			query:   SigNozBuilderQuery{Name: "A", Signal: SigNozSignalMetrics},
			wantSub: "aggregation",
		},
		{
			name: "aggregation without a metric name",
			query: SigNozBuilderQuery{
				Name:         "A",
				Signal:       SigNozSignalMetrics,
				Aggregations: []SigNozAggregation{{MetricName: "  ", TimeAggregation: "avg"}},
			},
			wantSub: "metricName",
		},
		{
			name: "select field without a name",
			query: SigNozBuilderQuery{
				Name:         "A",
				Signal:       SigNozSignalTraces,
				SelectFields: []SigNozSelectField{{Name: "  ", FieldContext: "span"}},
			},
			wantSub: "selectField",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildSigNozQueryBody(SigNozQueryRangeRequest{
				Start:       time.Now().Add(-time.Hour),
				End:         time.Now(),
				RequestType: SigNozRequestTypeTimeSeries,
				Queries:     []SigNozBuilderQuery{tc.query},
			})
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q should mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestSigNozQuerier_ValidationRunsBeforeAnyRequest proves an invalid metrics
// query costs zero round trips.
func TestSigNozQuerier_ValidationRunsBeforeAnyRequest(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":{"data":{"results":[]}}}`))
	}))
	defer ts.Close()

	q, err := NewSigNozQuerier(ts.URL, "k", false)
	if err != nil {
		t.Fatalf("new querier: %v", err)
	}
	if _, err := q.QueryRange(context.Background(), SigNozQueryRangeRequest{
		Start:       time.Now().Add(-time.Hour),
		End:         time.Now(),
		RequestType: SigNozRequestTypeTimeSeries,
		Queries:     []SigNozBuilderQuery{{Name: "A", Signal: SigNozSignalMetrics}},
	}); err == nil {
		t.Fatal("expected a validation error")
	}
	if calls != 0 {
		t.Errorf("backend called %d times for an invalid query, want 0", calls)
	}
}

// sigNozSpecOf pulls the single builder query's spec out of a rendered body.
func sigNozSpecOf(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var parsed struct {
		CompositeQuery struct {
			Queries []struct {
				Spec map[string]interface{} `json:"spec"`
			} `json:"queries"`
		} `json:"compositeQuery"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(parsed.CompositeQuery.Queries) != 1 {
		t.Fatalf("want exactly one builder query, got %d", len(parsed.CompositeQuery.Queries))
	}
	return parsed.CompositeQuery.Queries[0].Spec
}
