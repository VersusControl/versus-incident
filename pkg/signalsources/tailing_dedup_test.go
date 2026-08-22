package signalsources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
)

// -----------------------------------------------------------------------------
// fakeDedupBackend — an in-process stand-in for the Redis backend.
//
// It outlives the source instances that use it, which is what makes a "restart"
// testable: build a source, tick it, throw it away, build a fresh one against
// the same backend. It also enforces the same floor/cap semantics the Redis
// implementation delegates to ZREMRANGEBYSCORE / ZREMRANGEBYRANK, so a source
// that commits a wrong floor fails here rather than only in production.
// -----------------------------------------------------------------------------

type fakeDedupBackend struct {
	mu   sync.Mutex
	sets map[string]map[string]time.Time

	loads, saves, clears int
	loadErr, saveErr     error
}

func newFakeDedupBackend() *fakeDedupBackend {
	return &fakeDedupBackend{sets: map[string]map[string]time.Time{}}
}

func (b *fakeDedupBackend) LoadDedup(_ context.Context, source string) (map[string]time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loads++
	if b.loadErr != nil {
		return nil, b.loadErr
	}
	out := make(map[string]time.Time, len(b.sets[source]))
	for id, ts := range b.sets[source] {
		out[id] = ts
	}
	return out, nil
}

func (b *fakeDedupBackend) SaveDedup(_ context.Context, source string, added map[string]time.Time, floor time.Time, max int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saves++
	if b.saveErr != nil {
		return b.saveErr
	}
	set := b.sets[source]
	if set == nil {
		set = map[string]time.Time{}
		b.sets[source] = set
	}
	for id, ts := range added {
		set[id] = ts
	}
	if !floor.IsZero() {
		for id, ts := range set {
			if ts.Before(floor) {
				delete(set, id)
			}
		}
	}
	for max > 0 && len(set) > max {
		oldest, oldestTS := "", time.Time{}
		for id, ts := range set {
			if oldest == "" || ts.Before(oldestTS) {
				oldest, oldestTS = id, ts
			}
		}
		delete(set, oldest)
	}
	return nil
}

func (b *fakeDedupBackend) ClearDedup(_ context.Context, source string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clears++
	delete(b.sets, source)
	return nil
}

func (b *fakeDedupBackend) size(source string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sets[source])
}

// -----------------------------------------------------------------------------
// TailDedup
// -----------------------------------------------------------------------------

// TestTailDedup_NoBackendIsInMemoryOnly pins the degradation contract: with no
// backend the set still suppresses within the process and touches nothing
// external, so a development setup keeps working without Redis.
func TestTailDedup_NoBackendIsInMemoryOnly(t *testing.T) {
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	d := NewTailDedup("signoz:dev")

	if err := d.Load(context.Background()); err != nil {
		t.Fatalf("load without a backend: %v", err)
	}
	d.Stage([]DedupRow{{ID: "a", TS: base}}, time.Time{})
	if err := d.Commit(context.Background()); err != nil {
		t.Fatalf("commit without a backend: %v", err)
	}
	if !d.Has("a") {
		t.Error("committed id is not suppressed in the same process")
	}
	if d.Has("b") {
		t.Error("an id that was never committed is suppressed")
	}
	if err := d.Clear(context.Background()); err != nil {
		t.Fatalf("clear without a backend: %v", err)
	}
	if d.Has("a") {
		t.Error("Clear left the in-memory set populated")
	}
}

// TestTailDedup_PrunesBelowFloor proves the set is time-bounded: an id whose
// row is older than the span the next query re-reads is forgotten, in memory
// and in the backend, so a long-lived source cannot accumulate history.
func TestTailDedup_PrunesBelowFloor(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	backend := newFakeDedupBackend()
	d := NewTailDedup("signoz:prune")
	d.SetBackend(backend)

	rows := []DedupRow{
		{ID: "old", TS: base},
		{ID: "new", TS: base.Add(2 * time.Minute)},
	}
	d.Stage(rows, base.Add(-time.Minute))
	if err := d.Commit(ctx); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	if d.Len() != 2 {
		t.Fatalf("retained %d ids, want 2", d.Len())
	}

	// The cursor moved on: the floor now sits above `old`.
	d.Stage(nil, base.Add(time.Minute))
	if err := d.Commit(ctx); err != nil {
		t.Fatalf("commit 2: %v", err)
	}
	if d.Has("old") {
		t.Error("an id below the retention floor is still suppressed in memory")
	}
	if !d.Has("new") {
		t.Error("an id inside the retention floor was pruned")
	}
	if got := backend.size("signoz:prune"); got != 1 {
		t.Errorf("backend retained %d ids, want 1", got)
	}
}

// TestTailDedup_EnforcesSizeCap proves the second half of the bound: a source
// ingesting fast enough to fill the window with more ids than the cap drops the
// OLDEST first, so memory is bounded and the cost is a bounded duplicate rather
// than a dropped row.
func TestTailDedup_EnforcesSizeCap(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	backend := newFakeDedupBackend()
	d := NewTailDedup("signoz:cap")
	d.SetBackend(backend)
	d.max = 3

	rows := make([]DedupRow, 0, 6)
	for i := 0; i < 6; i++ {
		rows = append(rows, DedupRow{ID: fmt.Sprintf("id-%d", i), TS: base.Add(time.Duration(i) * time.Second)})
	}
	d.Stage(rows, base.Add(-time.Minute))
	if err := d.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if d.Len() != 3 {
		t.Fatalf("retained %d ids, want the cap of 3", d.Len())
	}
	for _, id := range []string{"id-3", "id-4", "id-5"} {
		if !d.Has(id) {
			t.Errorf("newest id %q was dropped by the cap", id)
		}
	}
	for _, id := range []string{"id-0", "id-1", "id-2"} {
		if d.Has(id) {
			t.Errorf("oldest id %q survived the cap", id)
		}
	}
	if got := backend.size("signoz:cap"); got != 3 {
		t.Errorf("backend retained %d ids, want 3", got)
	}
}

// TestTailDedup_RetriesAFailedSave proves a backend outage costs nothing
// permanently: the entries the backend refused are still pending on the next
// commit instead of being dropped from the persisted set forever.
func TestTailDedup_RetriesAFailedSave(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	backend := newFakeDedupBackend()
	backend.saveErr = fmt.Errorf("redis down")
	d := NewTailDedup("signoz:retry")
	d.SetBackend(backend)

	d.Stage([]DedupRow{{ID: "a", TS: base}}, time.Time{})
	if err := d.Commit(ctx); err == nil {
		t.Fatal("a failing backend must surface its error")
	}
	backend.saveErr = nil
	if err := d.Commit(ctx); err != nil {
		t.Fatalf("commit after recovery: %v", err)
	}
	if got := backend.size("signoz:retry"); got != 1 {
		t.Errorf("backend holds %d ids after recovery, want the retried 1", got)
	}
}

// TestTailDedup_StageIsNotDurableUntilCommit is the heart of the loss fix: an
// id staged by a Pull suppresses a re-read inside THIS process, but a process
// that dies before the commit leaves nothing behind to suppress it — so the
// next process re-reads the row whose fate it cannot know, instead of skipping
// a row that was never stored.
func TestTailDedup_StageIsNotDurableUntilCommit(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	backend := newFakeDedupBackend()

	died := NewTailDedup("signoz:stage")
	died.SetBackend(backend)
	died.Stage([]DedupRow{{ID: "a", TS: base}}, time.Time{})

	if !died.Has("a") {
		t.Error("a staged id is not suppressed inside the same process")
	}
	if got := backend.size("signoz:stage"); got != 0 {
		t.Fatalf("backend holds %d ids before any commit, want 0 — staging must not persist", got)
	}

	restarted := NewTailDedup("signoz:stage")
	restarted.SetBackend(backend)
	if err := restarted.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	if restarted.Has("a") {
		t.Error("an uncommitted id survived the restart; the row it stands for would be dropped for good")
	}

	// The same set, committed, does survive.
	died.Stage(nil, time.Time{})
	if err := died.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	committed := NewTailDedup("signoz:stage")
	committed.SetBackend(backend)
	if err := committed.Load(ctx); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !committed.Has("a") {
		t.Error("a committed id did not survive the restart; the row would be re-emitted")
	}
}

// TestTailDedup_CommitCarriesTheHighestStagedFloor proves several ticks can
// stage between two commits without the retention span regressing: the backend
// is pruned to the newest floor, so the persisted set stays bounded even when
// commits are rarer than ticks.
func TestTailDedup_CommitCarriesTheHighestStagedFloor(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	backend := newFakeDedupBackend()
	d := NewTailDedup("signoz:floor")
	d.SetBackend(backend)

	d.Stage([]DedupRow{{ID: "old", TS: base}}, base.Add(-time.Minute))
	d.Stage([]DedupRow{{ID: "new", TS: base.Add(2 * time.Minute)}}, base.Add(time.Minute))
	if err := d.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if d.Has("old") {
		t.Error("an id below the newest floor is still suppressed in memory")
	}
	if got := backend.size("signoz:floor"); got != 1 {
		t.Errorf("backend retained %d ids, want only the one inside the floor", got)
	}
}

// TestScopedTailDedupBackend_IsolatesScopes proves the multi-tenant seam: two
// scopes polling a SOURCE OF THE SAME NAME share a backing store without
// sharing a set — neither reads, overwrites, evicts nor clears the other's ids.
func TestScopedTailDedupBackend_IsolatesScopes(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	shared := newFakeDedupBackend()

	acme := NewTailDedup("signoz:app")
	acme.SetBackend(NewScopedTailDedupBackend("acme", shared))
	globex := NewTailDedup("signoz:app")
	globex.SetBackend(NewScopedTailDedupBackend("globex", shared))

	acme.Stage([]DedupRow{{ID: "row-1", TS: base}}, time.Time{})
	if err := acme.Commit(ctx); err != nil {
		t.Fatalf("acme commit: %v", err)
	}

	if err := globex.Load(ctx); err != nil {
		t.Fatalf("globex load: %v", err)
	}
	if globex.Has("row-1") {
		t.Fatal("one organisation's delivered ids suppressed another's rows")
	}

	globex.Stage([]DedupRow{{ID: "row-1", TS: base}}, time.Time{})
	if err := globex.Commit(ctx); err != nil {
		t.Fatalf("globex commit: %v", err)
	}
	if err := globex.Clear(ctx); err != nil {
		t.Fatalf("globex clear: %v", err)
	}

	survivor := NewTailDedup("signoz:app")
	survivor.SetBackend(NewScopedTailDedupBackend("acme", shared))
	if err := survivor.Load(ctx); err != nil {
		t.Fatalf("acme reload: %v", err)
	}
	if !survivor.Has("row-1") {
		t.Error("one organisation's clear-all wiped another organisation's dedup set")
	}
}

// TestNewScopedTailDedupBackend_Degrades pins the two ways the wrapper must
// disappear: no backing store means no backend at all (in-memory only), and an
// empty scope means the single-tenant path is untouched.
func TestNewScopedTailDedupBackend_Degrades(t *testing.T) {
	if NewScopedTailDedupBackend("acme", nil) != nil {
		t.Error("scoping a missing backend must yield no backend, not a wrapper around nil")
	}
	inner := newFakeDedupBackend()
	if got := NewScopedTailDedupBackend("", inner); got != TailDedupBackend(inner) {
		t.Error("an empty scope must return the backend unchanged")
	}
}

// TestRedisTailDedupBackend_KeysAreSourceScoped pins the Redis key shape the
// scoping wrapper composes with: one key per source, under a single prefix.
func TestRedisTailDedupBackend_KeysAreSourceScoped(t *testing.T) {
	b := &redisTailDedupBackend{prefix: "versus:agent:taildedup:"}
	if got, want := b.key("signoz:app"), "versus:agent:taildedup:signoz:app"; got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if b.key("acme/signoz:app") == b.key("globex/signoz:app") {
		t.Error("scoped source names collapsed to one key")
	}
}

// -----------------------------------------------------------------------------
// SigNoz — the source QA-069 was measured on
// -----------------------------------------------------------------------------

// TestSigNozSource_PersistedDedupSurvivesRestart is the QA-069 regression: a
// process restart must re-emit ZERO rows and still leave no gap.
//
// The pre-restart tick leaves a full reorder window of rows sitting at and
// below the cursor — the exact state that used to replay in full. A brand-new
// source, holding only the persisted timestamp and the persisted id set,
// re-reads that window and delivers nothing from it, while a row that landed in
// the cursor's own millisecond while the process was down still arrives.
func TestSigNozSource_PersistedDedupSurvivesRestart(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	boundary := base.Add(90 * time.Second)
	// A whole window of traffic in flight at the moment of the restart.
	fake.add("w1", "window one", base.Add(31*time.Second))
	fake.add("w2", "window two", base.Add(60*time.Second))
	fake.add("w3", "window three", boundary)

	backend := newFakeDedupBackend()
	cfg := func(c *config.AgentSignozSourceConfig) { c.ReorderWindow = "2m" }
	now := base.Add(2 * time.Minute)

	before := newTestSigNozSource(t, ts.URL, cfg)
	before.SetTailDedupBackend(backend)
	before.nowFn = func() time.Time { return now }
	sigs, persisted, err := before.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pre-restart tick: %v", err)
	}
	if len(sigs) != 3 {
		t.Fatalf("pre-restart tick delivered %v, want all three rows", messagesOf(sigs))
	}
	if !persisted.Equal(boundary) {
		t.Fatalf("persisted cursor = %v, want %v", persisted, boundary)
	}
	// The worker flushed those rows, so their ids may become durable.
	if err := before.Commit(context.Background()); err != nil {
		t.Fatalf("pre-restart commit: %v", err)
	}

	// A row lands in the SAME millisecond as the persisted cursor while the
	// process is down — the half of the contract that must not regress.
	fake.add("w4", "boundary twin", boundary)

	// Restart: a brand-new source, the persisted timestamp, the persisted ids.
	after := newTestSigNozSource(t, ts.URL, cfg)
	after.SetTailDedupBackend(backend)
	after.nowFn = func() time.Time { return now }
	sigs, _, err = after.Pull(context.Background(), persisted)
	if err != nil {
		t.Fatalf("post-restart tick: %v", err)
	}

	got := messagesOf(sigs)
	if len(got) != 1 || got[0] != "boundary twin" {
		t.Fatalf("restart delivered %v, want exactly the one new row (zero duplicates, no gap)", got)
	}
}

// TestSigNozSource_UncommittedRowsReplayAfterAnAbruptRestart is the other half
// of the contract, and the one that keeps a crash from eating data: rows a
// process delivered but never committed — because it died before the flush that
// would have stored them — are re-read by the next process rather than
// suppressed. The duplicate is the point; the alternative is a hole.
func TestSigNozSource_UncommittedRowsReplayAfterAnAbruptRestart(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 8, 21, 8, 30, 0, 0, time.UTC)
	fake.add("u1", "unflushed one", base.Add(30*time.Second))
	fake.add("u2", "unflushed two", base.Add(45*time.Second))

	backend := newFakeDedupBackend()
	cfg := func(c *config.AgentSignozSourceConfig) { c.ReorderWindow = "2m" }
	now := base.Add(time.Minute)

	before := newTestSigNozSource(t, ts.URL, cfg)
	before.SetTailDedupBackend(backend)
	before.nowFn = func() time.Time { return now }
	sigs, persisted, err := before.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pre-crash tick: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("pre-crash tick delivered %v, want both rows", messagesOf(sigs))
	}
	// No Commit: the process died before the catalog reached storage.

	after := newTestSigNozSource(t, ts.URL, cfg)
	after.SetTailDedupBackend(backend)
	after.nowFn = func() time.Time { return now }
	sigs, _, err = after.Pull(context.Background(), persisted)
	if err != nil {
		t.Fatalf("post-crash tick: %v", err)
	}
	if got := messagesOf(sigs); len(got) != 2 {
		t.Fatalf("post-crash tick delivered %v, want both uncommitted rows replayed", got)
	}
}

// TestSigNozSource_RewindClearsPersistedDedup is the highest-risk regression in
// the persisted set: clear-all relearns by rewinding the poll cursor, and a
// dedup set that outlived the clear would silently suppress the very rows the
// operator asked to relearn — in this process AND in the next one.
func TestSigNozSource_RewindClearsPersistedDedup(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	fake.add("r1", "one", base.Add(5*time.Second))
	fake.add("r2", "two", base.Add(6*time.Second))

	backend := newFakeDedupBackend()
	cfg := func(c *config.AgentSignozSourceConfig) { c.ReorderWindow = "1m" }

	src := newTestSigNozSource(t, ts.URL, cfg)
	src.SetTailDedupBackend(backend)
	src.nowFn = func() time.Time { return base.Add(time.Minute) }

	if _, _, err := src.Pull(context.Background(), base); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if err := src.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if backend.size("signoz:test") != 2 {
		t.Fatalf("backend holds %d ids before the rewind, want 2", backend.size("signoz:test"))
	}

	if err := src.Rewind(context.Background()); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got := backend.size("signoz:test"); got != 0 {
		t.Fatalf("backend still holds %d ids after Rewind — clear-all would stop relearning", got)
	}

	// The same running source relearns…
	sigs, _, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(sigs) != 2 {
		t.Errorf("after Rewind the running source delivered %v, want the window re-emitted", messagesOf(sigs))
	}

	// …and so does the next process, which is what the persisted half adds.
	restarted := newTestSigNozSource(t, ts.URL, cfg)
	restarted.SetTailDedupBackend(newFakeDedupBackend())
	restarted.nowFn = func() time.Time { return base.Add(time.Minute) }
	if _, _, err := restarted.Pull(context.Background(), base); err != nil {
		t.Fatalf("post-rewind restart tick: %v", err)
	}
}

// TestSigNozSource_RestartWithoutBackendIsUnchanged pins the no-backend path:
// with nothing to persist through, a restart behaves exactly as it did before
// the set was made durable — the window replays, and still nothing is lost.
func TestSigNozSource_RestartWithoutBackendIsUnchanged(t *testing.T) {
	fake := newFakeSigNoz()
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 8, 21, 8, 0, 0, 0, time.UTC)
	fake.add("n1", "in window", base.Add(30*time.Second))

	cfg := func(c *config.AgentSignozSourceConfig) { c.ReorderWindow = "2m" }
	now := base.Add(time.Minute)

	before := newTestSigNozSource(t, ts.URL, cfg)
	before.nowFn = func() time.Time { return now }
	_, persisted, err := before.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pre-restart tick: %v", err)
	}

	after := newTestSigNozSource(t, ts.URL, cfg)
	after.nowFn = func() time.Time { return now }
	sigs, _, err := after.Pull(context.Background(), persisted)
	if err != nil {
		t.Fatalf("post-restart tick: %v", err)
	}
	if len(sigs) != 1 {
		t.Fatalf("without a backend the restart delivered %v, want the window replayed once", messagesOf(sigs))
	}
}

// -----------------------------------------------------------------------------
// Elasticsearch — the same seam, the same guarantee
// -----------------------------------------------------------------------------

func TestElasticsearchSource_PersistedDedupSurvivesRestart(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	boundary := base.Add(20 * time.Second)
	fake.add("e1", "window one", base.Add(5*time.Second))
	fake.add("e2", "window two", boundary)

	backend := newFakeDedupBackend()
	now := base.Add(time.Minute)
	newSrc := func() *ElasticsearchSource {
		src, err := NewElasticsearchSource("tail", config.AgentElasticsearchSourceConfig{
			Addresses:     []string{ts.URL},
			Index:         "logs-*",
			PageSize:      50,
			ReorderWindow: "2m",
		})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		src.nowFn = func() time.Time { return now }
		src.SetTailDedupBackend(backend)
		return src
	}

	before := newSrc()
	sigs, persisted, err := before.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pre-restart tick: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("pre-restart tick delivered %d docs, want 2", len(sigs))
	}
	if err := before.Commit(context.Background()); err != nil {
		t.Fatalf("pre-restart commit: %v", err)
	}

	// A doc indexed in the cursor's own millisecond while the process is down.
	fake.add("e3", "boundary twin", boundary)

	after := newSrc()
	sigs, _, err = after.Pull(context.Background(), persisted)
	if err != nil {
		t.Fatalf("post-restart tick: %v", err)
	}
	if len(sigs) != 1 || sigs[0].Message != "boundary twin" {
		msgs := make([]string, len(sigs))
		for i, s := range sigs {
			msgs[i] = s.Message
		}
		t.Fatalf("restart delivered %v, want exactly the one new doc (zero duplicates, no gap)", msgs)
	}
}

func TestElasticsearchSource_RewindClearsPersistedDedup(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 8, 21, 10, 30, 0, 0, time.UTC)
	fake.add("c1", "one", base.Add(time.Second))

	backend := newFakeDedupBackend()
	src, err := NewElasticsearchSource("clear", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		Index:         "logs-*",
		PageSize:      50,
		ReorderWindow: "1m",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	src.nowFn = func() time.Time { return base.Add(time.Minute) }
	src.SetTailDedupBackend(backend)

	if _, _, err := src.Pull(context.Background(), base); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if err := src.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if backend.size("elasticsearch:clear") != 1 {
		t.Fatalf("backend holds %d ids before the rewind, want 1", backend.size("elasticsearch:clear"))
	}
	if err := src.Rewind(context.Background()); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got := backend.size("elasticsearch:clear"); got != 0 {
		t.Fatalf("backend still holds %d ids after Rewind — clear-all would stop relearning", got)
	}
	sigs, _, err := src.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if len(sigs) != 1 {
		t.Errorf("after Rewind got %d docs, want the window re-emitted", len(sigs))
	}
}

// -----------------------------------------------------------------------------
// CloudWatch Logs — the third consumer
// -----------------------------------------------------------------------------

// newFakeCWL serves FilterLogEvents from a fixed event list, honouring the
// inclusive `startTime` / `endTime` bounds the source sends.
func newFakeCWL(t *testing.T, events func() []map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StartTime int64 `json:"startTime"`
			EndTime   int64 `json:"endTime"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		matched := []map[string]interface{}{}
		for _, e := range events() {
			ms, _ := e["timestamp"].(int64)
			if ms < body.StartTime || ms > body.EndTime {
				continue
			}
			matched = append(matched, e)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"events":             matched,
			"searchedLogStreams": []interface{}{},
		})
	}))
}

// TestCloudWatchLogs_PersistedDedupSurvivesRestart proves the third tail adopts
// the same seam. A configured reorder window turns the strict `cursor + 1ms`
// scan into the inclusive re-read that recovers an event sharing the cursor's
// millisecond; the persisted id set is what keeps that re-read from replaying
// after a restart.
func TestCloudWatchLogs_PersistedDedupSurvivesRestart(t *testing.T) {
	base := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)
	boundary := base.Add(10 * time.Second)

	var mu sync.Mutex
	events := []map[string]interface{}{
		{"timestamp": base.Add(2 * time.Second).UnixMilli(), "message": "window one", "logStreamName": "s", "eventId": "a"},
		{"timestamp": boundary.UnixMilli(), "message": "window two", "logStreamName": "s", "eventId": "b"},
	}
	srv := newFakeCWL(t, func() []map[string]interface{} {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]interface{}(nil), events...)
	})
	defer srv.Close()

	client := cloudwatchlogs.New(cloudwatchlogs.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
		HTTPClient:   srv.Client(),
	})
	backend := newFakeDedupBackend()
	now := base.Add(time.Minute)
	newSrc := func() *CloudWatchLogsSource {
		src := &CloudWatchLogsSource{
			name:          "tail",
			cfg:           config.AgentCloudWatchLogsSourceConfig{Region: "us-east-1", LogGroupName: "g", PageSize: 50},
			client:        client,
			reorderWindow: 2 * time.Minute,
			nowFn:         func() time.Time { return now },
		}
		src.dedup = NewTailDedup(src.Name())
		src.SetTailDedupBackend(backend)
		return src
	}

	before := newSrc()
	sigs, persisted, err := before.Pull(context.Background(), base)
	if err != nil {
		t.Fatalf("pre-restart tick: %v", err)
	}
	if len(sigs) != 2 {
		t.Fatalf("pre-restart tick delivered %d events, want 2", len(sigs))
	}
	if !persisted.Equal(boundary) {
		t.Fatalf("cursor = %v, want %v", persisted, boundary)
	}
	if err := before.Commit(context.Background()); err != nil {
		t.Fatalf("pre-restart commit: %v", err)
	}

	// An event in the cursor's own millisecond, which the strict scan drops.
	mu.Lock()
	events = append(events, map[string]interface{}{
		"timestamp": boundary.UnixMilli(), "message": "boundary twin", "logStreamName": "s", "eventId": "c",
	})
	mu.Unlock()

	after := newSrc()
	sigs, _, err = after.Pull(context.Background(), persisted)
	if err != nil {
		t.Fatalf("post-restart tick: %v", err)
	}
	if len(sigs) != 1 || sigs[0].Message != "boundary twin" {
		msgs := make([]string, len(sigs))
		for i, s := range sigs {
			msgs[i] = s.Message
		}
		t.Fatalf("restart delivered %v, want exactly the one new event (zero duplicates, no gap)", msgs)
	}

	if err := after.Rewind(context.Background()); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got := backend.size("cloudwatchlogs:tail"); got != 0 {
		t.Fatalf("backend still holds %d ids after Rewind", got)
	}
}

// TestAttachTailDedupBackend_OnlyBindsTailingSources proves the process wiring
// reaches every tail and skips everything else, and that a nil backend (no
// Redis) binds nothing at all.
func TestAttachTailDedupBackend_OnlyBindsTailingSources(t *testing.T) {
	es, err := NewElasticsearchSource("es", config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://es:9200"}, Index: "logs-*",
	})
	if err != nil {
		t.Fatalf("new es: %v", err)
	}
	sz, err := NewSigNozSource("sz", config.AgentSignozSourceConfig{
		Address: "http://signoz:8080", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("new signoz: %v", err)
	}
	fs, err := NewFileSource("f", config.AgentFileSourceConfig{Path: t.TempDir() + "/app.log"})
	if err != nil {
		t.Fatalf("new file: %v", err)
	}

	sources := []core.SignalSource{es, sz, fs}
	if got := AttachTailDedupBackend(sources, newFakeDedupBackend()); got != 2 {
		t.Errorf("bound %d sources, want the 2 tailing ones", got)
	}
	if got := AttachTailDedupBackend(sources, nil); got != 0 {
		t.Errorf("bound %d sources with no backend, want 0", got)
	}
	if NewRedisTailDedupBackend(nil) != nil {
		t.Error("NewRedisTailDedupBackend(nil) must yield no backend, not a nil-client wrapper")
	}
}
