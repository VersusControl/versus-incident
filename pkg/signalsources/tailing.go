package signalsources

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/redis/go-redis/v9"
)

// -----------------------------------------------------------------------------
// Tailing cursor convention (shared OSS seam).
//
// Every cursor-driven SignalSource derives its next poll cursor from the MAX
// document timestamp it observed this tick, and every subsequent tick queries
// `>= cursor` (offset by an optional reorder window). That timestamp is
// UNTRUSTED DATA: it is whatever the producer stamped on the record. A single
// document dated in the future — a bad client clock, a mis-configured shipper,
// injected/garbage data — would otherwise push the cursor far ahead of the
// wall clock, after which every `>= cursor` query matches nothing until real
// time catches up. The source "learns the first batch then stops pulling",
// and only wiping the cursor (Clear-all) makes it briefly resume before
// stranding again. Observed live on a real cluster: docs dated 2048 pinned the
// cursor there while present-day logs were silently skipped.
//
// The invariant that keeps a tail honest, shared by every source that does NOT
// exhibit the bug (Loki, Graylog, Splunk, the enterprise standing sources):
// the scan is upper-bounded at `now` and the cursor never advances past `now`.
// ClampCursor is the single, testable realization of the second half so the
// affected sources (Elasticsearch, CloudWatch Logs) enforce it identically,
// and so any future source — OSS or enterprise — can adopt one convention.
// -----------------------------------------------------------------------------

// ClampCursor bounds a tailing source's next cursor to the closed interval
// [since, now]:
//
//   - it never rewinds below `since` (the lower bound the worker asked for, so
//     an empty or all-older tick reports "still here" rather than moving back);
//   - it never advances beyond `now` (the wall clock at pull time), so an
//     untrusted future-dated document cannot strand the cursor ahead of real
//     time and blank every following query.
//
// `now` should be the same clock reading the source used to upper-bound its
// scan window, so the returned cursor and the query stay consistent within the
// tick. When `since` is itself already in the future (a cursor persisted before
// this convention existed), the result collapses to `now`, letting the next
// tick resume real tailing instead of querying an empty future window forever.
func ClampCursor(candidate, since, now time.Time) time.Time {
	if candidate.Before(since) {
		candidate = since
	}
	if candidate.After(now) {
		candidate = now
	}
	return candidate
}

// -----------------------------------------------------------------------------
// Boundary dedup set (shared OSS seam, second half of the tailing convention).
//
// The cursor above is only half of a tail's position. Because the query lower
// bound is INCLUSIVE and offset one reorder window below the cursor, every tick
// re-reads rows it already delivered; what stops them being learned twice is a
// set of the row ids inside that window. The cursor is durable (pkg/agent
// CursorStore) but the id set was not, so a process restart re-queried the same
// window with an empty set and replayed all of it. The newest row sits exactly
// on the cursor, so at least one duplicate per restart was structural.
//
// TailDedup is that set, made durable. It is deliberately one type shared by
// every tailing source rather than a copy per source: the sources differ only
// in what they call the id (SigNoz `id`, Elasticsearch `_id`, CloudWatch
// `eventId`), and a per-source copy is how the pruning rule and the Rewind
// contract drift apart.
//
// It stores IDS AND TIMESTAMPS ONLY — never a message body, never a field map.
// Nothing that leaves this process through the backend can carry log payload.
// -----------------------------------------------------------------------------

// DedupRow is one row a tailing source's query returned this tick: the id it
// dedupes on plus the timestamp that decides how long the id is retained.
type DedupRow struct {
	ID string
	TS time.Time
}

// TailDedupBackend persists one source's boundary dedup set. It degrades the
// same way the worker's CursorStore does: no backend means in-memory only, so
// a development setup keeps working without Redis and behaves exactly as it
// did before the set was made durable.
//
// `source` is the SignalSource name, so keys never bleed across sources. An
// implementation serving more than one organisation adds the org to its own key
// prefix; the OSS implementation below is single-tenant and does not.
type TailDedupBackend interface {
	// LoadDedup returns the persisted id → row-timestamp entries for a source.
	LoadDedup(ctx context.Context, source string) (map[string]time.Time, error)
	// SaveDedup adds entries, drops everything stamped before floor, and keeps
	// at most max ids (oldest dropped first).
	SaveDedup(ctx context.Context, source string, added map[string]time.Time, floor time.Time, max int) error
	// ClearDedup removes a source's whole persisted set.
	ClearDedup(ctx context.Context, source string) error
}

// TailDedupBinder is implemented by sources that keep a TailDedup, so the
// process wiring can hand every one of them the same backend without knowing
// which concrete types are in the slice.
type TailDedupBinder interface {
	SetTailDedupBackend(TailDedupBackend)
}

// defaultTailDedupMax caps the retained ids per source. The set is already
// time-bounded to the effective re-read span — the configured reorder window
// plus one catalog persist interval — plus one poll interval, so this only
// binds when a source ingests fast enough for that span to hold more than this
// many rows (50k ids ≈ a few MB, and ≈ 275 rows/s over the shipped 3-minute
// span: a 2m window, a 30s persist interval and a 30s poll). Past the cap the
// OLDEST ids are dropped, which trades a bounded duplicate for bounded memory —
// never a dropped row.
const defaultTailDedupMax = 50000

// tailDedupTTL expires a persisted set that nothing refreshes, so removing a
// source from the config does not leave its key behind forever. Every save
// renews it, and it is orders of magnitude longer than any reorder window, so
// it cannot expire under a running source.
const tailDedupTTL = 24 * time.Hour

// TailDedup is the per-source boundary dedup set: the ids a tailing source has
// already delivered whose timestamps are still inside the span its next query
// will re-read.
//
// Crash consistency. The set is staged in memory at the END of Pull and only
// becomes DURABLE when the worker calls Commit — which it does after the flush
// that puts the rows those ids describe into storage (core.SourceCommitter).
// That ordering is the whole point, and it has two halves:
//
//   - Rows before ids. The catalog reaches storage on its own flush interval,
//     so committing the ids inside Pull would record rows as delivered up to a
//     whole interval before they were durable; a process that died in that gap
//     would never re-read them and never have stored them. Ids trailing the
//     rows makes the worst case a REPLAY, never a hole.
//   - Ids before the cursor. Staging happens before the worker persists the
//     advanced cursor, and the retention floor is one reorder window below the
//     cursor the tick STARTED from, not the one it ends at — so the set still
//     covers the query the next process issues whichever of the two cursors
//     turns out to be the durable one.
//
// The cost is a bounded duplicate when a process dies between the catalog flush
// and the commit that follows it. That is the acceptable failure direction: a
// replayed row is re-learned, a dropped row is gone.
type TailDedup struct {
	source string
	max    int

	mu      sync.Mutex
	backend TailDedupBackend
	loaded  bool
	// ids maps a retained row id to the timestamp that row carried.
	ids map[string]time.Time
	// unsaved holds entries the backend has not accepted yet — everything
	// staged since the last successful commit, plus anything a failed save
	// left behind, so an outage costs duplicates rather than a corrupt set.
	unsaved map[string]time.Time
	// floor is the highest retention floor staged so far. Commit hands it to
	// the backend so the persisted set is pruned to the same span as the
	// in-memory one. It never moves backwards.
	floor time.Time
}

// NewTailDedup returns an in-memory dedup set for one source. Call
// SetTailDedupBackend to make it durable.
func NewTailDedup(source string) *TailDedup {
	return &TailDedup{
		source:  source,
		max:     defaultTailDedupMax,
		ids:     make(map[string]time.Time),
		unsaved: make(map[string]time.Time),
	}
}

// SetBackend attaches (or detaches, with nil) the persistence backend. The next
// Load re-hydrates from it.
func (d *TailDedup) SetBackend(b TailDedupBackend) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.backend = b
	d.loaded = false
}

// Load hydrates the set from the backend once. It is called at the top of every
// Pull; after the first success it is a no-op, and a failure is retried on the
// next tick rather than being cached as "empty".
func (d *TailDedup) Load(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loaded {
		return nil
	}
	if d.backend == nil {
		d.loaded = true
		return nil
	}
	stored, err := d.backend.LoadDedup(ctx, d.source)
	if err != nil {
		return err
	}
	for id, ts := range stored {
		if _, ok := d.ids[id]; !ok {
			d.ids[id] = ts
		}
	}
	d.loaded = true
	return nil
}

// Has reports whether an id was already delivered and is still retained.
func (d *TailDedup) Has(id string) bool {
	if d == nil || id == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.ids[id]
	return ok
}

// Stage folds this tick's rows into the in-memory set, prunes everything
// stamped before floor and enforces the size cap. It touches NO backend: an id
// only becomes durable in Commit, once the rows it describes are durable too.
// Staging alone is enough to suppress the next tick's re-read inside this
// process, which is where all but the restart duplicates come from.
//
// floor must be the query's inclusive lower bound (cursor-at-tick-start minus
// the reorder window), which is exactly the span the next query can re-read.
func (d *TailDedup) Stage(rows []DedupRow, floor time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, r := range rows {
		if r.ID == "" {
			continue
		}
		if _, known := d.ids[r.ID]; !known {
			d.unsaved[r.ID] = r.TS
		}
		d.ids[r.ID] = r.TS
	}
	if !floor.IsZero() {
		for id, ts := range d.ids {
			if ts.Before(floor) {
				delete(d.ids, id)
				delete(d.unsaved, id)
			}
		}
		if floor.After(d.floor) {
			d.floor = floor
		}
	}
	d.trimLocked()
}

// Commit pushes everything staged since the last commit to the backend, along
// with the retention floor and the size cap so the persisted set is bounded the
// same way the in-memory one is. The worker calls it after the flush that made
// the staged rows durable (core.SourceCommitter), so the persisted ids can only
// ever trail the rows, never lead them.
//
// A failed save keeps the pending entries so the next commit retries them; the
// cost of an outage is a possible replay, never a lost row.
func (d *TailDedup) Commit(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.backend == nil {
		d.unsaved = make(map[string]time.Time)
		return nil
	}
	if err := d.backend.SaveDedup(ctx, d.source, d.unsaved, d.floor, d.max); err != nil {
		return err
	}
	d.unsaved = make(map[string]time.Time)
	return nil
}

// Clear empties the set in memory AND in the backend. It is what a source's
// Rewind calls: a catalog clear-all rewinds the poll cursor so the operator's
// history is relearned, and a dedup set that survived that would suppress the
// very rows the relearn is supposed to re-read.
func (d *TailDedup) Clear(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ids = make(map[string]time.Time)
	d.unsaved = make(map[string]time.Time)
	d.floor = time.Time{}
	// Stay "loaded" so a backend that failed to clear cannot be re-hydrated
	// into the set the operator just asked to forget.
	d.loaded = true
	if d.backend == nil {
		return nil
	}
	return d.backend.ClearDedup(ctx, d.source)
}

// Len returns the number of retained ids.
func (d *TailDedup) Len() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.ids)
}

// trimLocked enforces the size cap by dropping the oldest ids first.
func (d *TailDedup) trimLocked() {
	if d.max <= 0 || len(d.ids) <= d.max {
		return
	}
	type entry struct {
		id string
		ts time.Time
	}
	all := make([]entry, 0, len(d.ids))
	for id, ts := range d.ids {
		all = append(all, entry{id: id, ts: ts})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ts.Equal(all[j].ts) {
			return all[i].id < all[j].id
		}
		return all[i].ts.Before(all[j].ts)
	})
	for _, e := range all[:len(all)-d.max] {
		delete(d.ids, e.id)
		delete(d.unsaved, e.id)
	}
}

// AttachTailDedupBackend hands one backend to every source in the slice that
// keeps a boundary dedup set, and reports how many took it. Sources that do not
// tail — and every source when the backend is nil — are left untouched.
func AttachTailDedupBackend(sources []core.SignalSource, backend TailDedupBackend) int {
	if backend == nil {
		return 0
	}
	n := 0
	for _, src := range sources {
		if binder, ok := src.(TailDedupBinder); ok {
			binder.SetTailDedupBackend(backend)
			n++
		}
	}
	return n
}

// -----------------------------------------------------------------------------
// Crash-replay span (shared OSS seam, third part of the tailing convention).
//
// A tail's inclusive re-read span below the cursor has two jobs. The first is
// the one it is configured for: `reorder_window` is a LATENESS budget, how far
// behind the cursor a row may become queryable and still be picked up. The
// second is invisible from the config file: because delivered ids are only made
// durable after the flush that stored the rows they describe, the same span is
// what a REPLACEMENT PROCESS re-reads to recover everything a killed one
// learned but never flushed.
//
// Those unflushed rows span at most one catalog persist interval, and while
// they accumulate the cursor keeps advancing with them. So a span sized only by
// the lateness budget leaves the oldest of them below the next process's query
// bound: they were read, they were never stored, and they are never read again.
// Silent, permanent loss — with no bug anywhere, just two settings in two files
// that nothing relates.
//
// The rule that relates them: the effective span is the operator's lateness
// budget PLUS one persist interval, not the larger of the two. Taking the max
// would silently spend the lateness budget on crash replay — a source
// configured to tolerate a minute of late rows would tolerate none of them
// after a kill. Adding keeps both promises whole and makes the guarantee
// statable: any row that was within its configured lateness budget of the
// cursor when it was learned survives an abrupt kill.
//
// The cost is a proportionally wider per-tick re-scan and a dedup set holding
// one persist interval more ids. The set stays bounded by defaultTailDedupMax,
// and past that cap the oldest ids drop — a bounded duplicate, never a lost row.
// -----------------------------------------------------------------------------

// TailReplaySpanSetter is implemented by tailing sources whose inclusive
// re-read span below the cursor doubles as the span a restarted process
// replays. It lets the agent wiring — the only layer that knows the catalog
// persist interval — widen every tail through one call, including tails
// registered from outside this package.
type TailReplaySpanSetter interface {
	// SetTailReplaySpan adds span to the configured reorder window and returns
	// the configured window and the resulting effective one. It is idempotent:
	// calling it twice with the same span leaves the same effective window. A
	// non-positive span changes nothing and only reports.
	SetTailReplaySpan(span time.Duration) (configured, effective time.Duration)
}

// TailWindow reports one source's configured reorder window against the
// effective span its queries actually re-read.
type TailWindow struct {
	Source     string
	Configured time.Duration
	Effective  time.Duration
}

func (w TailWindow) String() string {
	return fmt.Sprintf("%s %s->%s", w.Source, w.Configured, w.Effective)
}

// ApplyTailReplaySpan widens every tailing source in the slice so its re-read
// span covers one catalog persist interval on top of the configured reorder
// window, and reports what each source ended up with so the caller can say it
// once at boot. A non-positive span leaves every source untouched.
func ApplyTailReplaySpan(sources []core.SignalSource, span time.Duration) []TailWindow {
	if span <= 0 {
		return nil
	}
	return tailWindows(sources, span)
}

// FormatTailWindows renders the widenings as one comma-separated line.
func FormatTailWindows(windows []TailWindow) string {
	parts := make([]string, 0, len(windows))
	for _, w := range windows {
		parts = append(parts, w.String())
	}
	return strings.Join(parts, ", ")
}

// TailWindows reports what each tailing source in the slice currently
// re-reads, changing nothing. It is how a process that wires its own worker
// checks that its tails were widened rather than assuming it.
func TailWindows(sources []core.SignalSource) []TailWindow {
	return tailWindows(sources, 0)
}

func tailWindows(sources []core.SignalSource, span time.Duration) []TailWindow {
	var out []TailWindow
	for _, src := range sources {
		setter, ok := src.(TailReplaySpanSetter)
		if !ok {
			continue
		}
		configured, effective := setter.SetTailReplaySpan(span)
		out = append(out, TailWindow{Source: src.Name(), Configured: configured, Effective: effective})
	}
	return out
}

// scopedTailDedupBackend namespaces every source name it forwards, so one
// backing store can hold independent sets for several scopes.
type scopedTailDedupBackend struct {
	scope string
	inner TailDedupBackend
}

// NewScopedTailDedupBackend wraps a backend so every source it stores is
// namespaced by `scope`. A single-tenant deployment needs nothing here and
// passes an empty scope, which returns the backend unchanged; a deployment
// serving one organisation per process passes its organisation id, so two
// organisations polling a SAME-NAMED source can never read, overwrite or evict
// each other's set — including when one clears its own.
//
// The scope is an opaque string: this seam knows nothing about organisations,
// licensing or tenancy. It is the generic half of multi-tenant key isolation,
// and works over any TailDedupBackend, not just the Redis one.
func NewScopedTailDedupBackend(scope string, inner TailDedupBackend) TailDedupBackend {
	if inner == nil {
		return nil
	}
	if scope == "" {
		return inner
	}
	return &scopedTailDedupBackend{scope: scope, inner: inner}
}

func (b *scopedTailDedupBackend) scoped(source string) string { return b.scope + "/" + source }

func (b *scopedTailDedupBackend) LoadDedup(ctx context.Context, source string) (map[string]time.Time, error) {
	return b.inner.LoadDedup(ctx, b.scoped(source))
}

func (b *scopedTailDedupBackend) SaveDedup(ctx context.Context, source string, added map[string]time.Time, floor time.Time, max int) error {
	return b.inner.SaveDedup(ctx, b.scoped(source), added, floor, max)
}

func (b *scopedTailDedupBackend) ClearDedup(ctx context.Context, source string) error {
	return b.inner.ClearDedup(ctx, b.scoped(source))
}

// redisTailDedupBackend stores each source's set as a sorted set scored by the
// row timestamp in epoch milliseconds. The score is what makes the bound cheap:
// pruning below the floor and capping the size are both single range-removals
// server-side, and only ids new to this tick are written.
type redisTailDedupBackend struct {
	rdb    redis.UniversalClient
	prefix string
}

// NewRedisTailDedupBackend returns a Redis-backed dedup backend, or nil when
// there is no Redis — in which case every source stays in-memory only.
func NewRedisTailDedupBackend(rdb redis.UniversalClient) TailDedupBackend {
	if rdb == nil {
		return nil
	}
	return &redisTailDedupBackend{rdb: rdb, prefix: "versus:agent:taildedup:"}
}

func (b *redisTailDedupBackend) key(source string) string { return b.prefix + source }

func (b *redisTailDedupBackend) LoadDedup(ctx context.Context, source string) (map[string]time.Time, error) {
	members, err := b.rdb.ZRangeWithScores(ctx, b.key(source), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]time.Time, len(members))
	for _, m := range members {
		id, ok := m.Member.(string)
		if !ok || id == "" {
			continue
		}
		out[id] = time.UnixMilli(int64(m.Score)).UTC()
	}
	return out, nil
}

func (b *redisTailDedupBackend) SaveDedup(ctx context.Context, source string, added map[string]time.Time, floor time.Time, max int) error {
	key := b.key(source)
	pipe := b.rdb.Pipeline()
	if len(added) > 0 {
		members := make([]redis.Z, 0, len(added))
		for id, ts := range added {
			members = append(members, redis.Z{Score: float64(ts.UTC().UnixMilli()), Member: id})
		}
		pipe.ZAdd(ctx, key, members...)
	}
	if !floor.IsZero() {
		pipe.ZRemRangeByScore(ctx, key, "-inf", "("+strconv.FormatInt(floor.UTC().UnixMilli(), 10))
	}
	if max > 0 {
		// Keep the `max` highest-scored (newest) ids, drop the rest.
		pipe.ZRemRangeByRank(ctx, key, 0, int64(-max-1))
	}
	pipe.Expire(ctx, key, tailDedupTTL)
	_, err := pipe.Exec(ctx)
	return err
}

func (b *redisTailDedupBackend) ClearDedup(ctx context.Context, source string) error {
	return b.rdb.Del(ctx, b.key(source)).Err()
}
