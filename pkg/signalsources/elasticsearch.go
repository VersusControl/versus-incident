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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	elasticsearchapp "github.com/VersusControl/versus-incident/pkg/elasticsearch"
)

// ElasticsearchSource pulls log documents from one or more Elasticsearch
// addresses using the `_search` API with a `range` filter on the configured
// time field. It uses time plus a configured doc-values tie breaker and
// `search_after` for stable pagination.
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
// the span the next query re-reads and skips them on the next tick. That dedup
// set is the shared TailDedup: bounded by time and by size, and durable through
// its backend so a restart resumes on both halves of the position rather than
// replaying the window.
type ElasticsearchSource struct {
	name            string
	cfg             config.AgentElasticsearchSourceConfig
	client          *elasticsearchapp.Client
	projectedFields []string

	// reorderWindow is how far below the cursor each tick re-scans (inclusive)
	// to catch out-of-order / late-indexed docs. Documents indexed more than
	// this far behind the newest timestamp already seen are not recovered — the
	// bounded trade-off that keeps the dedup set memory-bounded.
	reorderWindow time.Duration

	// replaySpan is added to reorderWindow to size what a tick actually
	// re-scans. The agent wiring sets it to the catalog persist interval so a
	// replacement process re-reads everything a killed one learned but never
	// flushed. See ApplyTailReplaySpan.
	replaySpan time.Duration

	// nowFn is the wall clock the tail reads to upper-bound the scan (`lte`)
	// and clamp the cursor (ClampCursor). Overridable in tests; nil ⇒ time.Now.
	nowFn func() time.Time

	// mu guards the dedup set. Pull holds it for the whole tick and Rewind
	// takes it to clear the set, so a catalog clear can never interleave with a
	// tick and leave stale dedup state that would suppress a legitimate relearn.
	mu sync.Mutex
	// dedup is the set of document `_id`s already delivered whose timestamp is
	// still inside the span the next query re-reads. They are skipped on the
	// next tick's overlapping re-fetch so each document is learned exactly once.
	dedup *TailDedup
}

// defaultESReorderWindow is used when reorder_window is unset/invalid. One
// minute comfortably covers Elasticsearch's default ~1s refresh lag plus minor
// clock skew and bursty ingestion, while keeping the per-tick re-scan and dedup
// set small.
const (
	defaultESReorderWindow = time.Minute
	maximumESPageSize      = 1000
	maximumESPullItems     = 10000
	maximumESPullBytes     = 8 << 20
	maximumESScanItems     = defaultTailDedupMax
)

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
	if cfg.PageSize > maximumESPageSize {
		cfg.PageSize = maximumESPageSize
	}

	reorderWindow := defaultESReorderWindow
	if cfg.ReorderWindow != "" {
		if d, err := time.ParseDuration(cfg.ReorderWindow); err == nil && d > 0 {
			reorderWindow = d
		}
	}

	service, err := elasticsearchapp.NewService(cfg)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch source %q: %w", name, err)
	}
	src := &ElasticsearchSource{
		name:            name,
		cfg:             service.Config(),
		client:          service.Client(),
		projectedFields: service.ProjectedFields(),
		reorderWindow:   reorderWindow,
	}
	src.dedup = NewTailDedup(src.Name())
	return src, nil
}

func (s *ElasticsearchSource) Name() string { return "elasticsearch:" + s.name }

// SetTailReplaySpan widens what each tick re-scans so it also covers the docs a
// killed process learned but never flushed. It implements TailReplaySpanSetter.
func (s *ElasticsearchSource) SetTailReplaySpan(span time.Duration) (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if span > 0 {
		s.replaySpan = span
	}
	return s.reorderWindow, s.scanWindow()
}

// scanWindow is how far below the cursor a tick re-scans: the operator's
// lateness budget plus the crash-replay span. Callers hold s.mu.
func (s *ElasticsearchSource) scanWindow() time.Duration { return s.reorderWindow + s.replaySpan }

// SetTailDedupBackend makes this source's boundary dedup set durable. It
// implements TailDedupBinder.
func (s *ElasticsearchSource) SetTailDedupBackend(b TailDedupBackend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dedup == nil {
		s.dedup = NewTailDedup(s.Name())
	}
	s.dedup.SetBackend(b)
}

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
	if !since.IsZero() && s.scanWindow() > 0 {
		lower = since.Add(-s.scanWindow())
	}

	// Hydrate the persisted dedup set before the first query of the process. A
	// failure here costs duplicates, never documents, so it is logged and the
	// tick continues.
	if err := s.dedup.Load(ctx); err != nil {
		log.Printf("agent: %s: loading the persisted dedup set failed: %v (this tick may re-emit up to one reorder window)", s.Name(), err)
	}

	cursor := since // never rewind the cursor below where we already were
	var signals []core.Signal

	// seen is the minimal state the dedup set needs — id + timestamp for every
	// document the query returned.
	var seen []DedupRow
	var searchAfter []interface{}
	var previousTuple *esPaginationTuple
	seenTies := make(map[string]struct{})
	acceptedBytes := 0
	emittedItems := 0
	scannedItems := 0
	scanBudgetExhausted := false
	effectivePageSize := s.cfg.PageSize

	// Cap scanned hits separately from emitted rows. Replayed dedup hits do not
	// consume the delivery budgets, so a later tick can cross the retained
	// same-timestamp prefix and reach unseen rows without an unbounded scan.
	maxPages := maximumESScanItems
pageLoop:
	for page := 0; page < maxPages; page++ {
		var resp *esSearchResponse
		for {
			body, err := s.buildQueryPage(lower, now, searchAfter, effectivePageSize)
			if err != nil {
				return signals, cursor, err
			}
			resp, err = s.doSearch(ctx, body)
			if !errors.Is(err, elasticsearchapp.ErrResponseTooLarge) {
				if err != nil {
					return signals, cursor, err
				}
				break
			}
			if effectivePageSize == 1 {
				return signals, cursor, fmt.Errorf("elasticsearch source %q: one projected document exceeds the 8 MiB response limit; tail cannot advance: %w", s.name, err)
			}
			effectivePageSize = max(1, effectivePageSize/2)
		}
		hits := resp.Hits.Hits
		if len(hits) == 0 {
			break
		}
		validatedTuple, validationErr := s.validatePaginationPage(hits, previousTuple, seenTies)
		if validationErr != nil {
			return nil, since, validationErr
		}
		previousTuple = validatedTuple
		for _, h := range hits {
			if scannedItems >= maximumESScanItems {
				scanBudgetExhausted = true
				break pageLoop
			}
			scannedItems++
			if s.dedup.Has(h.ID) {
				continue
			}
			if emittedItems >= maximumESPullItems {
				break pageLoop
			}
			encoded, encodeErr := json.Marshal(h.Source)
			if encodeErr != nil {
				return signals, ClampCursor(cursor, since, now), elasticsearchapp.ErrResponseTooLarge
			}
			if acceptedBytes+len(encoded) > maximumESPullBytes {
				break pageLoop
			}
			sig, ok := s.signalFromHit(h)
			if !ok {
				continue
			}
			acceptedBytes += len(encoded)
			emittedItems++
			if sig.Timestamp.After(cursor) {
				cursor = sig.Timestamp
			}
			seen = append(seen, DedupRow{ID: h.ID, TS: sig.Timestamp})
			signals = append(signals, sig)
		}
		if emittedItems >= maximumESPullItems {
			break
		}
		if scannedItems >= maximumESScanItems {
			scanBudgetExhausted = true
			break
		}
		if len(hits) < effectivePageSize {
			break
		}
		if page == maxPages-1 {
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

	// Stage the ids IN MEMORY before returning, so the next tick of this process
	// does not re-deliver them, and leave them uncommitted until the worker has
	// flushed the docs they describe. The retention floor is this tick's query
	// lower bound — one reorder window below the cursor the tick started from —
	// so the set covers whichever cursor turns out to be the durable one.
	s.dedup.Stage(seen, lower)
	if scanBudgetExhausted && len(signals) == 0 {
		return nil, since, fmt.Errorf("elasticsearch source %q: scanned %d documents without reaching an unseen row; increase tie_breaker_field selectivity or narrow query/reorder_window", s.name, maximumESScanItems)
	}

	return signals, cursor, nil
}

// Commit makes the `_id`s this source has delivered durable. The worker calls
// it only after flushing the docs they describe, so a process that dies can
// re-deliver docs but can never suppress docs it failed to store. It implements
// core.SourceCommitter.
func (s *ElasticsearchSource) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dedup.Commit(ctx)
}

// Rewind clears the boundary dedup set — in memory and in its backend — so a
// catalog clear (which rewinds the worker poll cursor to the lookback window)
// makes this source re-emit, and therefore relearn, its whole window from
// scratch, exactly like a fresh process start. Without it the pre-clear `_id`s
// would suppress the very docs the operator asked to relearn.
//
// It implements core.SourceRewinder. The poll cursor is the source's primary
// position, but the dedup set is a second, source-owned piece of state the
// cursor reset cannot reach; Rewind reconciles it. Safe to call concurrently
// with Pull (both take mu) and leaves the source in the state a freshly
// constructed instance would have.
func (s *ElasticsearchSource) Rewind(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dedup.Clear(ctx)
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

type esPaginationTuple struct {
	timestamp time.Time
	tie       string
}

func (s *ElasticsearchSource) validatePaginationPage(hits []esHit, previous *esPaginationTuple, seenTies map[string]struct{}) (*esPaginationTuple, error) {
	for _, hit := range hits {
		if len(hit.Sort) != 2 {
			return nil, s.paginationTupleError()
		}
		sortTimestamp, sortTimestampOK := parseElasticsearchTime(hit.Sort[0])
		sortTie, sortTieOK := hit.Sort[1].(string)
		sourceTieValue, sourceTieFound := lookupField(hit.Source, s.cfg.TieBreakerField)
		sourceTie, sourceTieOK := sourceTieValue.(string)
		if !sortTimestampOK || !sortTieOK || strings.TrimSpace(sortTie) == "" ||
			!sourceTieFound || !sourceTieOK || strings.TrimSpace(sourceTie) == "" || sourceTie != sortTie {
			return nil, s.paginationTupleError()
		}
		if _, duplicate := seenTies[sortTie]; duplicate {
			return nil, s.paginationTupleError()
		}
		current := &esPaginationTuple{timestamp: sortTimestamp, tie: sortTie}
		if previous != nil && (!current.timestamp.After(previous.timestamp) &&
			(!current.timestamp.Equal(previous.timestamp) || current.tie <= previous.tie)) {
			return nil, s.paginationTupleError()
		}
		seenTies[sortTie] = struct{}{}
		previous = current
	}
	return previous, nil
}

func (s *ElasticsearchSource) paginationTupleError() error {
	return fmt.Errorf("elasticsearch source %q: invalid pagination ordering for tie_breaker_field %q; configure a non-empty unique keyword field with doc-values whose value is present in _source and returned as sort[1]", s.name, s.cfg.TieBreakerField)
}

func (s *ElasticsearchSource) buildQuery(lower, upper time.Time, searchAfter []interface{}) ([]byte, error) {
	return s.buildQueryPage(lower, upper, searchAfter, s.cfg.PageSize)
}

func (s *ElasticsearchSource) buildQueryPage(lower, upper time.Time, searchAfter []interface{}, pageSize int) ([]byte, error) {
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
		"size":    pageSize,
		"_source": s.projectedFields,
		"sort": []interface{}{
			map[string]interface{}{s.cfg.TimeField: map[string]interface{}{"order": "asc"}},
			map[string]interface{}{s.cfg.TieBreakerField: map[string]interface{}{"order": "asc"}},
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
	var out esSearchResponse
	if err := s.client.SearchIngestJSON(ctx, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
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
	projected := make(map[string]interface{})
	for _, field := range s.projectedFields {
		if value, found := lookupField(h.Source, field); found {
			projected[field] = value
		}
	}
	return core.Signal{
		Source:    s.Name(),
		Timestamp: ts,
		Severity:  sev,
		Message:   msg,
		Fields:    fields,
		Raw:       projected,
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
	return fieldString(v)
}

// fieldString renders one looked-up field value as text.
func fieldString(v interface{}) string {
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
	return parseElasticsearchTime(v)
}

func parseElasticsearchTime(value interface{}) (time.Time, bool) {
	switch typed := value.(type) {
	case string:
		// Try a couple of common ES formats.
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
			if ts, err := time.Parse(layout, typed); err == nil {
				return ts.UTC(), true
			}
		}
	case float64:
		// epoch millis
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) || typed < math.MinInt64 || typed >= math.MaxInt64 {
			return time.Time{}, false
		}
		return time.UnixMilli(int64(typed)).UTC(), true
	}
	return time.Time{}, false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
