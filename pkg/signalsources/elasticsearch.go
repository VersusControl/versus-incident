// Package signalsources contains concrete SignalSource implementations.
//
// Each source implements pkg/core.SignalSource and must:
//
//   - Be cursor-aware: `since` defines the lower bound, the returned cursor
//     is the upper bound that should be passed back next tick.
//   - Be polite: respect AgentSourceConfig.Elasticsearch.PageSize (or its
//     equivalent) and never load arbitrarily many docs into memory.
//   - Be best-effort: a single failed tick must not crash the worker —
//     return the error and let the worker decide whether to retry.
package signalsources

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// ElasticsearchSource pulls log documents from one or more Elasticsearch
// addresses using the `_search` API with a `range` filter on the configured
// time field. It uses sort-by-time + `search_after` for stable pagination.
//
// This intentionally avoids the official ES client to keep the dependency
// surface small. The set of features used (basic auth, API-key auth,
// `_search`, `range`, `query_string`, `sort`, `search_after`) is stable
// across ES 7.x and 8.x.
//
// Tailing is lossless and exactly-once for the near-real-time case. Each tick
// queries an INCLUSIVE lower bound (`gte`) offset a bounded reorderWindow below
// the poll cursor, so documents indexed at — or slightly behind — the boundary
// timestamp (same-millisecond bursts, refresh lag, minor clock skew / late
// ingestion) are still seen instead of being stranded forever behind a strict
// `gt`. To avoid folding those re-scanned documents into the model twice, the
// source tracks the `_id`s it has already emitted whose timestamp falls inside
// the current reorder window and skips them on the next tick. That dedup set is
// pruned to one window each tick, so it stays bounded to (window × ingest rate)
// rather than growing with all-time history.
type ElasticsearchSource struct {
	name   string
	cfg    config.AgentElasticsearchSourceConfig
	client *http.Client

	// reorderWindow is how far below the cursor each tick re-scans (inclusive)
	// to catch out-of-order / late-indexed docs. Documents indexed more than
	// this far behind the newest timestamp already seen are not recovered — the
	// bounded trade-off that keeps the dedup set memory-bounded.
	reorderWindow time.Duration

	// nowFn is the wall clock the tail reads to upper-bound the scan (`lte`)
	// and clamp the cursor (ClampCursor). Overridable in tests; nil ⇒ time.Now.
	nowFn func() time.Time

	// mu guards emitted. Pull holds it for the whole tick and Rewind takes it to
	// clear the set, so a catalog clear can never interleave with a tick and
	// leave stale dedup state that would suppress a legitimate relearn.
	mu sync.Mutex
	// emitted is the set of document `_id`s already delivered whose timestamp is
	// within reorderWindow of the last cursor. They are skipped on the next
	// tick's overlapping re-fetch so each document is learned exactly once.
	emitted map[string]struct{}
}

// defaultESReorderWindow is used when reorder_window is unset/invalid. One
// minute comfortably covers Elasticsearch's default ~1s refresh lag plus minor
// clock skew and bursty ingestion, while keeping the per-tick re-scan and dedup
// set small.
const defaultESReorderWindow = time.Minute

// NewElasticsearchSource validates config and returns a ready source.
func NewElasticsearchSource(name string, cfg config.AgentElasticsearchSourceConfig) (*ElasticsearchSource, error) {
	if len(cfg.Addresses) == 0 {
		return nil, fmt.Errorf("elasticsearch source %q: no addresses configured", name)
	}
	if cfg.Index == "" {
		return nil, fmt.Errorf("elasticsearch source %q: index is required", name)
	}
	if cfg.TimeField == "" {
		cfg.TimeField = "@timestamp"
	}
	if cfg.MessageField == "" {
		cfg.MessageField = "message"
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}

	reorderWindow := defaultESReorderWindow
	if cfg.ReorderWindow != "" {
		if d, err := time.ParseDuration(cfg.ReorderWindow); err == nil && d > 0 {
			reorderWindow = d
		}
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify},
	}
	return &ElasticsearchSource{
		name:          name,
		cfg:           cfg,
		client:        &http.Client{Transport: tr, Timeout: 30 * time.Second},
		reorderWindow: reorderWindow,
		emitted:       make(map[string]struct{}),
	}, nil
}

func (s *ElasticsearchSource) Name() string { return "elasticsearch:" + s.name }

// now returns the source's wall clock. Tests override nowFn to freeze it; the
// nil-guard keeps struct-literal construction working.
func (s *ElasticsearchSource) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn().UTC()
	}
	return time.Now().UTC()
}

// Pull issues a `_search` query with an INCLUSIVE `range[time_field] >= lower`
// (where lower = since - reorderWindow) and walks pages with `search_after`
// until the page is short or we've collected enough docs. Documents already
// delivered on a previous tick — tracked by `_id` within the reorder window —
// are skipped so each is learned exactly once. The returned cursor is the
// maximum timestamp seen (never below `since`), so it advances tick-over-tick
// as new data arrives and stays put when the source is idle.
//
// The scan is also upper-bounded at `now` (`range[time_field] <= now`) and the
// returned cursor is clamped to `now` (ClampCursor). Without this a single
// future-dated document — an untrusted producer timestamp — would advance the
// cursor past the wall clock, after which every following `>= cursor` query
// matches nothing real until that future time arrives (the "learns the first
// batch then stops until Clear-all" stall, reproduced live with docs dated
// 2048). Bounding at `now` keeps the tail on real data; future-dated docs are
// intentionally not tailed. Minor clock skew (a producer a few seconds ahead)
// is not lost: once the wall clock passes such a document it falls inside the
// next tick's inclusive `[cursor - reorderWindow, now]` re-scan.
func (s *ElasticsearchSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	// Hold the lock for the whole tick so a concurrent Rewind (catalog clear)
	// cannot interleave: it either fully precedes this Pull (we re-emit from an
	// empty dedup set) or fully follows it (it clears the set we rebuild here).
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	// Heal a persisted future cursor: never scan from ahead of the wall clock,
	// or the [since - window, now] window would be an empty/inverted range and
	// the source would never recover on its own.
	if since.After(now) {
		since = now
	}

	// Inclusive lower bound offset by the reorder window. A zero `since` (only
	// tests / a first tick with no lookback) is left untouched so the query
	// still matches from the beginning of time rather than a negative year.
	lower := since
	if !since.IsZero() && s.reorderWindow > 0 {
		lower = since.Add(-s.reorderWindow)
	}

	cursor := since // never rewind the cursor below where we already were
	var signals []core.Signal

	// seenHit is the minimal state needed to rebuild the dedup set after the
	// tick — id + timestamp for every document the query returned.
	type seenHit struct {
		id string
		ts time.Time
	}
	var seen []seenHit
	var searchAfter []interface{}

	// Cap total iterations so a misconfigured query can't loop forever.
	const maxPages = 20
	for page := 0; page < maxPages; page++ {
		body, err := s.buildQuery(lower, now, searchAfter)
		if err != nil {
			return signals, cursor, err
		}
		resp, err := s.doSearch(ctx, body)
		if err != nil {
			return signals, cursor, err
		}
		hits := resp.Hits.Hits
		if len(hits) == 0 {
			break
		}
		for _, h := range hits {
			sig, ok := s.signalFromHit(h)
			if !ok {
				continue
			}
			if sig.Timestamp.After(cursor) {
				cursor = sig.Timestamp
			}
			seen = append(seen, seenHit{id: h.ID, ts: sig.Timestamp})
			if _, dup := s.emitted[h.ID]; dup {
				// Already delivered on a previous tick — the inclusive
				// re-scan pulled it back; skip so it isn't learned twice.
				continue
			}
			signals = append(signals, sig)
		}
		if len(hits) < s.cfg.PageSize {
			break
		}
		searchAfter = hits[len(hits)-1].Sort
	}

	// Clamp so the cursor never advances past the wall clock. The `lte: now`
	// scan bound already excludes future-dated docs, so in practice cursor is
	// already <= now; this is the explicit, source-agnostic invariant (shared
	// with CloudWatch Logs) that guarantees a future timestamp can never strand
	// the tail even if a doc slips through at the boundary.
	cursor = ClampCursor(cursor, since, now)

	// Rebuild the dedup set for the next tick: keep only the `_id`s whose
	// timestamp is within reorderWindow of the NEW cursor — exactly the docs the
	// next inclusive re-scan can pull back. Rebuilding from the hits actually
	// returned (emitted + re-scanned) self-prunes anything the advancing window
	// has moved past, so the set stays bounded to one window's worth of ids.
	windowStart := cursor.Add(-s.reorderWindow)
	next := make(map[string]struct{}, len(seen))
	for _, h := range seen {
		if h.id == "" {
			continue
		}
		if !h.ts.Before(windowStart) { // ts >= windowStart
			next[h.id] = struct{}{}
		}
	}
	s.emitted = next

	return signals, cursor, nil
}

// Rewind clears the boundary dedup set so a catalog clear (which rewinds the
// worker poll cursor to the lookback window) makes this source re-emit — and
// therefore relearn — its whole window from scratch, exactly like a fresh
// process start. Without it the pre-clear `_id`s would suppress the very docs
// the operator asked to relearn.
//
// It implements core.SourceRewinder. The poll cursor is the source's primary
// position, but the dedup set is a second, source-owned piece of state the
// cursor reset cannot reach; Rewind reconciles it. Safe to call concurrently
// with Pull (both take mu) and leaves the source in the state a freshly
// constructed instance would have.
func (s *ElasticsearchSource) Rewind(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitted = make(map[string]struct{})
	return nil
}

// -----------------------------------------------------------------------------
// internals
// -----------------------------------------------------------------------------

type esSearchResponse struct {
	Hits struct {
		Hits []esHit `json:"hits"`
	} `json:"hits"`
}

type esHit struct {
	ID     string                 `json:"_id"`
	Source map[string]interface{} `json:"_source"`
	Sort   []interface{}          `json:"sort,omitempty"`
}

func (s *ElasticsearchSource) buildQuery(lower, upper time.Time, searchAfter []interface{}) ([]byte, error) {
	rangeFilter := map[string]interface{}{
		s.cfg.TimeField: map[string]interface{}{
			"gte":    lower.UTC().Format(time.RFC3339Nano),
			"lte":    upper.UTC().Format(time.RFC3339Nano),
			"format": "strict_date_optional_time_nanos",
		},
	}

	must := []interface{}{
		map[string]interface{}{"range": rangeFilter},
	}
	if strings.TrimSpace(s.cfg.Query) != "" {
		must = append(must, map[string]interface{}{
			"query_string": map[string]interface{}{"query": s.cfg.Query},
		})
	}

	body := map[string]interface{}{
		"size": s.cfg.PageSize,
		"sort": []interface{}{
			map[string]interface{}{s.cfg.TimeField: map[string]interface{}{"order": "asc"}},
		},
		"query": map[string]interface{}{
			"bool": map[string]interface{}{"must": must},
		},
	}
	if len(searchAfter) > 0 {
		body["search_after"] = searchAfter
	}
	return json.Marshal(body)
}

func (s *ElasticsearchSource) doSearch(ctx context.Context, body []byte) (*esSearchResponse, error) {
	var lastErr error
	for _, addr := range s.cfg.Addresses {
		u := strings.TrimRight(addr, "/") + "/" + s.cfg.Index + "/_search"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		s.applyAuth(req)

		resp, err := s.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("elasticsearch %s: %d %s", u, resp.StatusCode, truncate(string(data), 256))
			continue
		}
		var out esSearchResponse
		if err := json.Unmarshal(data, &out); err != nil {
			lastErr = fmt.Errorf("decode elasticsearch response: %w", err)
			continue
		}
		return &out, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no elasticsearch addresses configured")
	}
	return nil, lastErr
}

func (s *ElasticsearchSource) applyAuth(req *http.Request) {
	if s.cfg.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+s.cfg.APIKey)
		return
	}
	if s.cfg.Username != "" {
		token := base64.StdEncoding.EncodeToString(
			[]byte(s.cfg.Username + ":" + s.cfg.Password),
		)
		req.Header.Set("Authorization", "Basic "+token)
	}
}

// signalFromHit maps an _source document to a core.Signal. It returns false
// when the document is missing the configured time field (we cannot cursor
// past it, so it's safer to skip).
func (s *ElasticsearchSource) signalFromHit(h esHit) (core.Signal, bool) {
	ts, ok := extractTime(h.Source, s.cfg.TimeField)
	if !ok {
		return core.Signal{}, false
	}

	msg := stringField(h.Source, s.cfg.MessageField)
	sev := stringField(h.Source, s.cfg.SeverityField)

	fields := make(map[string]interface{})
	for _, f := range s.cfg.ExtraFields {
		if v, ok := lookupField(h.Source, f); ok {
			fields[f] = v
		}
	}
	return core.Signal{
		Source:    s.Name(),
		Timestamp: ts,
		Severity:  sev,
		Message:   msg,
		Fields:    fields,
		Raw:       h.Source,
	}, true
}

// -----------------------------------------------------------------------------
// small field helpers (dotted-path lookup, time parsing, truncation)
// -----------------------------------------------------------------------------

func stringField(src map[string]interface{}, path string) string {
	if path == "" {
		return ""
	}
	v, ok := lookupField(src, path)
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		// Best-effort stringification for nested objects.
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// lookupField walks a dotted path ("error.stack_trace") through nested maps.
// Elasticsearch flattens dotted field names automatically, so we try the
// flat name first and fall back to a recursive walk.
func lookupField(src map[string]interface{}, path string) (interface{}, bool) {
	if v, ok := src[path]; ok {
		return v, true
	}
	parts := strings.Split(path, ".")
	var cur interface{} = src
	for _, p := range parts {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func extractTime(src map[string]interface{}, field string) (time.Time, bool) {
	v, ok := lookupField(src, field)
	if !ok {
		return time.Time{}, false
	}
	switch t := v.(type) {
	case string:
		// Try a couple of common ES formats.
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts.UTC(), true
			}
		}
	case float64:
		// epoch millis
		return time.UnixMilli(int64(t)).UTC(), true
	}
	return time.Time{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
