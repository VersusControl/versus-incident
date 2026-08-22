package signalsources

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// SigNozSource pulls log rows from SigNoz through the v5 query API
// (`requestType: raw`, `signal: logs`).
//
// Cursor contract — the Elasticsearch model, deliberately NOT the Loki one.
// SigNoz has no server-side tail cursor: a builder query paginates with
// offset+limit, and request bounds are MILLISECOND precision. A bare-timestamp
// cursor of the "next tick starts strictly after the newest timestamp seen"
// kind therefore drops every row that shares the boundary millisecond with the
// last row of the previous tick. Instead:
//
//   - each tick queries [cursor - reorderWindow, now] with an INCLUSIVE lower
//     bound, ordered `timestamp asc, id asc` (SigNoz requires the `id`
//     tiebreak for a stable order over timestamp);
//   - `offset` is walked WITHIN a tick only, never carried across ticks — the
//     window moves between ticks, so a carried offset would point at a
//     different row set;
//   - rows already delivered are tracked by `id` and skipped when the
//     overlapping re-scan pulls them back, so each row is learned once.
//
// The dedup set is the shared TailDedup: bounded to the span the next query can
// re-read, and durable through its backend so a restart resumes on both halves
// of the position — the timestamp from the worker's cursor store and the ids
// from the dedup backend — instead of replaying the window.
type SigNozSource struct {
	name    string
	cfg     config.AgentSignozSourceConfig
	querier *SigNozQuerier

	reorderWindow time.Duration

	// replaySpan is added to reorderWindow to size what a tick actually
	// re-reads. The agent wiring sets it to the catalog persist interval so a
	// replacement process re-reads everything a killed one learned but never
	// flushed. See ApplyTailReplaySpan.
	replaySpan time.Duration

	// nowFn is the wall clock used to upper-bound the scan and clamp the
	// cursor. Overridable in tests; nil ⇒ time.Now.
	nowFn func() time.Time

	// mu guards the tick so a concurrent Rewind cannot interleave and leave
	// stale dedup state behind.
	mu sync.Mutex
	// dedup holds the row ids already delivered whose timestamp is still inside
	// the span the next query re-reads.
	dedup *TailDedup
}

// defaultSigNozReorderWindow is used when reorder_window is unset or invalid.
//
// SigNoz documents no ingest-to-queryable delay and no ordering guarantee, so
// this is an INFERENCE from the pipeline shape (OTel collector batching into
// ClickHouse) rather than a measurement: two minutes comfortably covers batch
// flush plus insert lag plus minor clock skew, while keeping the per-tick
// re-scan and the dedup set small. Operators who have measured their own
// pipeline should lower it.
const defaultSigNozReorderWindow = 2 * time.Minute

// maxSigNozPageSize bounds the per-request `limit` so a misconfigured source
// cannot ask SigNoz for an unbounded result set.
const maxSigNozPageSize = 1000

// sigNozMaxPagesPerTick caps the offset walk so a backend that keeps returning
// full pages cannot spin one tick forever.
const sigNozMaxPagesPerTick = 20

// NewSigNozSource validates config and returns a ready source.
func NewSigNozSource(name string, cfg config.AgentSignozSourceConfig) (*SigNozSource, error) {
	if cfg.Address == "" {
		return nil, fmt.Errorf("signoz source %q: address is required", name)
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("signoz source %q: api_key is required", name)
	}
	if cfg.MessageField == "" {
		cfg.MessageField = "body"
	}
	if cfg.SeverityField == "" {
		cfg.SeverityField = "severity_text"
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 500
	}
	if cfg.PageSize > maxSigNozPageSize {
		cfg.PageSize = maxSigNozPageSize
	}

	reorderWindow := defaultSigNozReorderWindow
	if cfg.ReorderWindow != "" {
		if d, err := time.ParseDuration(cfg.ReorderWindow); err == nil && d > 0 {
			reorderWindow = d
		}
	}

	querier, err := NewSigNozQuerier(cfg.Address, cfg.APIKey, cfg.InsecureSkipVerify)
	if err != nil {
		return nil, fmt.Errorf("signoz source %q: %w", name, err)
	}

	src := &SigNozSource{
		name:          name,
		cfg:           cfg,
		querier:       querier,
		reorderWindow: reorderWindow,
	}
	src.dedup = NewTailDedup(src.Name())
	return src, nil
}

func (s *SigNozSource) Name() string { return "signoz:" + s.name }

// SetTailReplaySpan widens what each tick re-reads so it also covers the rows a
// killed process learned but never flushed. It implements TailReplaySpanSetter.
func (s *SigNozSource) SetTailReplaySpan(span time.Duration) (time.Duration, time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if span > 0 {
		s.replaySpan = span
	}
	return s.reorderWindow, s.scanWindow()
}

// scanWindow is how far below the cursor a tick re-reads: the operator's
// lateness budget plus the crash-replay span. Callers hold s.mu.
func (s *SigNozSource) scanWindow() time.Duration { return s.reorderWindow + s.replaySpan }

// SetTailDedupBackend makes this source's boundary dedup set durable. It
// implements TailDedupBinder.
func (s *SigNozSource) SetTailDedupBackend(b TailDedupBackend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dedup == nil {
		s.dedup = NewTailDedup(s.Name())
	}
	s.dedup.SetBackend(b)
}

func (s *SigNozSource) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn().UTC()
	}
	return time.Now().UTC()
}

// Pull walks one tick's worth of rows over [since - reorderWindow, now].
func (s *SigNozSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	// Heal a persisted future cursor: scanning from ahead of the wall clock
	// would invert the window and the source would never recover on its own.
	if since.After(now) {
		since = now
	}

	lower := since
	if !since.IsZero() && s.scanWindow() > 0 {
		lower = since.Add(-s.scanWindow())
	}

	// Hydrate the persisted dedup set before the first query of the process. A
	// failure here costs duplicates, never rows, so it is logged and the tick
	// continues.
	if err := s.dedup.Load(ctx); err != nil {
		log.Printf("agent: %s: loading the persisted dedup set failed: %v (this tick may re-emit up to one reorder window)", s.Name(), err)
	}

	cursor := since
	var signals []core.Signal

	// seen is the minimal state the dedup set needs: the id and the timestamp
	// of every row this tick's query returned.
	var seen []DedupRow

	for page := 0; page < sigNozMaxPagesPerTick; page++ {
		results, err := s.querier.QueryRangeRaw(ctx, SigNozQueryRangeRequest{
			Start:       lower,
			End:         now,
			RequestType: SigNozRequestTypeRaw,
			Queries: []SigNozBuilderQuery{{
				Name:   "A",
				Signal: SigNozSignalLogs,
				Filter: s.cfg.Query,
				Order: []SigNozOrderBy{
					{Key: "timestamp", Direction: "asc"},
					{Key: "id", Direction: "asc"},
				},
				Offset: page * s.cfg.PageSize,
				Limit:  s.cfg.PageSize,
			}},
		})
		if err != nil {
			return signals, cursor, err
		}

		rows := 0
		for _, res := range results {
			for _, row := range res.Rows {
				if row == nil {
					continue
				}
				rows++
				sig, id, ok := s.signalFromRow(row)
				if !ok {
					continue
				}
				if sig.Timestamp.After(cursor) {
					cursor = sig.Timestamp
				}
				seen = append(seen, DedupRow{ID: id, TS: sig.Timestamp})
				if s.dedup.Has(id) {
					continue
				}
				signals = append(signals, sig)
			}
		}
		if rows < s.cfg.PageSize {
			break
		}
	}

	cursor = ClampCursor(cursor, since, now)

	// Stage the ids IN MEMORY before returning, so the next tick of this process
	// does not re-deliver them, and leave them uncommitted until the worker has
	// flushed the rows they describe. The retention floor is this tick's query
	// lower bound — one reorder window below the cursor the tick started from —
	// so the set covers whichever cursor turns out to be the durable one.
	s.dedup.Stage(seen, lower)

	return signals, cursor, nil
}

// Commit makes the ids this source has delivered durable. The worker calls it
// only after flushing the rows they describe, so a process that dies can
// re-deliver rows but can never suppress rows it failed to store. It implements
// core.SourceCommitter.
func (s *SigNozSource) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dedup.Commit(ctx)
}

// Rewind clears the boundary dedup set — in memory and in its backend — so a
// catalog clear makes this source re-emit, and therefore relearn, its whole
// window from scratch, exactly like a fresh process start. It implements
// core.SourceRewinder.
func (s *SigNozSource) Rewind(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dedup.Clear(ctx)
}

// signalFromRow maps a raw row to a Signal plus the key used for dedup. It
// returns false when no usable timestamp can be read — the cursor could not
// advance past such a row, so skipping it is safer than guessing.
func (s *SigNozSource) signalFromRow(row *SigNozRawRow) (core.Signal, string, bool) {
	data := row.Data
	if data == nil {
		data = map[string]interface{}{}
	}

	ts := row.Timestamp.UTC()
	if row.Timestamp.IsZero() {
		var ok bool
		if ts, ok = sigNozRowTime(data); !ok {
			return core.Signal{}, "", false
		}
	}

	msg := sigNozString(data, s.cfg.MessageField)

	// `id` is the tiebreak SigNoz orders on and the natural dedup key. When a
	// row arrives without one, fall back to a composite so dedup still
	// suppresses the overlapping re-scan instead of silently re-emitting the
	// whole reorder window every tick.
	id := sigNozString(data, "id")
	if id == "" {
		id = ts.Format(time.RFC3339Nano) + "|" + truncate(msg, 256)
	}

	fields := make(map[string]interface{}, len(s.cfg.ExtraFields))
	for _, f := range s.cfg.ExtraFields {
		if v, ok := sigNozLookupField(data, f); ok {
			fields[sigNozFieldKey(f)] = v
		}
	}

	return core.Signal{
		Source:    s.Name(),
		Timestamp: ts,
		Severity:  sigNozString(data, s.cfg.SeverityField),
		Message:   msg,
		Fields:    fields,
		Raw:       data,
	}, id, true
}

// sigNozAttributeContainers are the row maps SigNoz nests OTLP attributes in,
// listed in the order a bare attribute name is resolved against them. The
// dots live inside these maps' KEYS — `resources_string: {"service.name":
// "checkout"}` — so a dotted attribute is one flat key there, never a nested
// path.
//
// Resource attributes come first because `service.name`, `deployment.
// environment` and the `k8s.*` set describe the emitter's stable identity: when
// the same name also appears as a per-record attribute, the resource value is
// the one that names the workload. This is an ordered slice, never a map, so
// the winner of a duplicate name is fixed rather than whatever iteration order
// the runtime picked.
var sigNozAttributeContainers = []string{
	"resources_string",
	"attributes_string",
	"attributes_number",
	"attributes_bool",
	"scope_string",
}

// sigNozLookupField resolves one configured field name against a raw SigNoz row.
//
// SigNoz does not flatten dotted attribute names the way Elasticsearch does, so
// the generic walk in lookupField cannot reach `service.name`: there is no
// top-level `service` map to descend into, only a `resources_string` map that
// holds `service.name` as a literal key. Resolution order, first hit wins:
//
//  1. the whole name as a top-level column — `body`, `id`, `severity_text`,
//     and also `resources_string` itself when an operator wants the whole map;
//  2. a container-qualified name — `resources_string.service.name` — where the
//     first segment names the container and the remainder is the attribute key;
//  3. a bare attribute name searched across sigNozAttributeContainers in their
//     declared order, because that is the spelling operators actually write;
//  4. the generic nested walk, for payloads that really do nest.
func sigNozLookupField(data map[string]interface{}, path string) (interface{}, bool) {
	if path == "" {
		return nil, false
	}
	if v, ok := data[path]; ok {
		return v, true
	}
	if container, attr, qualified := strings.Cut(path, "."); qualified && attr != "" &&
		slices.Contains(sigNozAttributeContainers, container) {
		if v, ok := sigNozContainerLookup(data[container], attr); ok {
			return v, true
		}
	}
	for _, container := range sigNozAttributeContainers {
		if v, ok := sigNozContainerLookup(data[container], path); ok {
			return v, true
		}
	}
	return lookupField(data, path)
}

// sigNozContainerLookup reads one attribute out of an attribute container: the
// dotted name as a flat key first, then a nested walk for payloads that nest.
func sigNozContainerLookup(container interface{}, attr string) (interface{}, bool) {
	m, ok := container.(map[string]interface{})
	if !ok {
		return nil, false
	}
	return lookupField(m, attr)
}

// sigNozFieldKey is the Signal.Fields key a configured field name is emitted
// under. A recognised container prefix is stripped: the container names
// SigNoz's storage column, not the attribute, so `resources_string.service.
// name` and the bare `service.name` land on the same key and downstream service
// attribution does not depend on which spelling the operator chose. Configuring
// both spellings of one attribute is a key collision resolved in extra_fields
// order, last write winning.
func sigNozFieldKey(path string) string {
	if container, attr, qualified := strings.Cut(path, "."); qualified && attr != "" &&
		slices.Contains(sigNozAttributeContainers, container) {
		return attr
	}
	return path
}

// sigNozString resolves a field with sigNozLookupField and renders it as text.
func sigNozString(data map[string]interface{}, path string) string {
	if path == "" {
		return ""
	}
	v, ok := sigNozLookupField(data, path)
	if !ok {
		return ""
	}
	return fieldString(v)
}

// sigNozRowTime reads a timestamp out of the row payload for the case where the
// envelope omitted it. SigNoz stamps log timestamps in NANOSECONDS; JSON numbers
// decode as float64, so the magnitude decides the unit.
func sigNozRowTime(data map[string]interface{}) (time.Time, bool) {
	v, ok := sigNozLookupField(data, "timestamp")
	if !ok {
		return time.Time{}, false
	}
	switch t := v.(type) {
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts.UTC(), true
			}
		}
	case float64:
		return unixEpochByMagnitude(int64(t)), true
	}
	return time.Time{}, false
}

// unixEpochByMagnitude interprets an integer epoch whose unit is not stated.
// Boundaries are chosen so any plausible present-day instant lands in the right
// bucket: below 1e12 is seconds, below 1e15 is milliseconds, otherwise
// nanoseconds.
func unixEpochByMagnitude(n int64) time.Time {
	switch {
	case n < 1e12:
		return time.Unix(n, 0).UTC()
	case n < 1e15:
		return time.UnixMilli(n).UTC()
	default:
		return time.Unix(0, n).UTC()
	}
}
