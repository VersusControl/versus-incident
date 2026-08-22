package agent

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// -----------------------------------------------------------------------------
// Restart durability — the two properties that must hold TOGETHER.
//
// A tailing source re-reads its boundary window on every tick and suppresses
// what it already delivered with a durable id set. That set and the learned
// catalog are two separate durable writes, and the order between them decides
// which way a restart fails:
//
//   - ids durable BEFORE the rows  -> the next process skips rows nothing ever
//     stored. Silent, permanent loss.
//   - ids durable AFTER the rows   -> the next process may re-read rows that
//     were already stored. Bounded duplicates, recoverable.
//
// These tests drive a REAL ElasticsearchSource through the REAL worker loop
// across a simulated process restart and assert both directions at once: every
// row delivered exactly once on a graceful stop, and every uncommitted row
// replayed rather than dropped when the process dies without one.
// -----------------------------------------------------------------------------

// memDedupBackend is a TailDedupBackend that outlives the worker using it, so a
// "restart" is testable: it plays the role Redis plays in production, with the
// same floor and cap semantics.
type memDedupBackend struct {
	mu   sync.Mutex
	sets map[string]map[string]time.Time
}

func newMemDedupBackend() *memDedupBackend {
	return &memDedupBackend{sets: map[string]map[string]time.Time{}}
}

func (b *memDedupBackend) LoadDedup(_ context.Context, source string) (map[string]time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]time.Time, len(b.sets[source]))
	for id, ts := range b.sets[source] {
		out[id] = ts
	}
	return out, nil
}

func (b *memDedupBackend) SaveDedup(_ context.Context, source string, added map[string]time.Time, floor time.Time, max int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
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

func (b *memDedupBackend) ClearDedup(_ context.Context, source string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sets, source)
	return nil
}

func (b *memDedupBackend) size(source string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.sets[source])
}

// deliveryLog counts how many times each row reached the worker, across every
// process in the test. It is the only instrument that can tell a lost row from
// a duplicated one.
type deliveryLog struct {
	mu    sync.Mutex
	count map[string]int
}

func newDeliveryLog() *deliveryLog { return &deliveryLog{count: map[string]int{}} }

func (l *deliveryLog) record(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count[id]++
}

func (l *deliveryLog) total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.count {
		n += c
	}
	return n
}

// reset forgets what earlier processes delivered, so an audit can ask what THIS
// process read rather than what any process ever read.
func (l *deliveryLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count = map[string]int{}
}

// audit reports the rows never delivered and the rows delivered more than once.
func (l *deliveryLog) audit(want []string) (lost, duplicated []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range want {
		switch c := l.count[id]; {
		case c == 0:
			lost = append(lost, id)
		case c > 1:
			duplicated = append(duplicated, fmt.Sprintf("%s x%d", id, c))
		}
	}
	return lost, duplicated
}

// recordingSource is a real ElasticsearchSource that logs what it hands the
// worker. Everything else — Pull's window, the dedup staging, Commit, Rewind —
// is the shipped implementation.
type recordingSource struct {
	*signalsources.ElasticsearchSource
	log *deliveryLog
}

func (r *recordingSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	sigs, cursor, err := r.ElasticsearchSource.Pull(ctx, since)
	for _, s := range sigs {
		fields := strings.Fields(s.Message)
		if len(fields) == 0 {
			continue
		}
		r.log.record(fields[len(fields)-1])
	}
	return sigs, cursor, err
}

// restartHarness holds the state that must survive a restart: the row store the
// source queries, the durable catalog blobs, the durable dedup set and the
// durable poll cursor.
type restartHarness struct {
	t       *testing.T
	index   *esTailIndex
	url     string
	store   storage.Provider
	dedup   *memDedupBackend
	cursors map[string]time.Time
	log     *deliveryLog

	mu     sync.Mutex
	pushed []string
}

func newRestartHarness(t *testing.T) *restartHarness {
	t.Helper()
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	ix := &esTailIndex{}
	srv := httptest.NewServer(ix.handler())
	t.Cleanup(srv.Close)

	return &restartHarness{
		t:       t,
		index:   ix,
		url:     srv.URL,
		store:   storage.NewMemory(),
		dedup:   newMemDedupBackend(),
		cursors: map[string]time.Time{},
		log:     newDeliveryLog(),
	}
}

// push writes one row into the backend the source tails and remembers it as
// ground truth.
func (h *restartHarness) push() string {
	h.mu.Lock()
	id := fmt.Sprintf("row-%04d", len(h.pushed))
	h.pushed = append(h.pushed, id)
	h.mu.Unlock()
	h.index.add(id, "connection refused talking to db-01 "+id, time.Now().UTC())
	return id
}

func (h *restartHarness) groundTruth() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.pushed...)
}

// ledger sums the per-pattern counts in the durable catalog. It is what an
// operator sees, and what a lost row silently subtracts from.
func (h *restartHarness) ledger() int {
	cat, err := LoadCatalog(h.store)
	if err != nil {
		h.t.Fatalf("reload catalog: %v", err)
	}
	n := 0
	for _, p := range cat.All() {
		n += p.Count
	}
	return n
}

// process builds one agent process over the shared durable state: a fresh
// source, a fresh catalog loaded from storage, a fresh worker.
func (h *restartHarness) process(persistEvery time.Duration) (*Worker, *recordingSource, *CursorStore) {
	h.t.Helper()
	return h.processWithWindow(persistEvery, "2m")
}

// processWithWindow is process with the source's configured reorder window
// spelled out, so a test can set it BELOW the persist interval.
func (h *restartHarness) processWithWindow(persistEvery time.Duration, reorderWindow string) (*Worker, *recordingSource, *CursorStore) {
	h.t.Helper()

	es, err := signalsources.NewElasticsearchSource("tail", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{h.url},
		Index:         "logs-*",
		PageSize:      500,
		ReorderWindow: reorderWindow,
	})
	if err != nil {
		h.t.Fatalf("new source: %v", err)
	}
	src := &recordingSource{ElasticsearchSource: es, log: h.log}
	if n := signalsources.AttachTailDedupBackend([]core.SignalSource{src}, h.dedup); n != 1 {
		h.t.Fatalf("bound %d sources to the dedup backend, want 1", n)
	}

	cat, err := LoadCatalog(h.store)
	if err != nil {
		h.t.Fatalf("LoadCatalog: %v", err)
	}
	miner := NewMiner(0.4, 4, 100)
	for _, p := range cat.All() {
		miner.AddCluster(p.ID, p.Template, p.Count)
	}
	matcher, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	svc, _ := NewServiceMatcher(nil)

	cursors := NewCursorStore(nil)
	for name, ts := range h.cursors {
		_ = cursors.Set(context.Background(), name, ts)
	}

	w, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode:         "training",
			Lookback:     "5m",
			PollInterval: "20ms",
			Catalog: config.AgentCatalogConfig{
				AutoPromoteAfter: 100000,
				PersistInterval:  persistEvery.String(),
			},
		},
		Sources:  []core.SignalSource{src},
		Cursors:  cursors,
		Matcher:  matcher,
		Miner:    miner,
		Catalog:  cat,
		Services: svc,
	})
	if err != nil {
		h.t.Fatalf("NewWorker: %v", err)
	}
	return w, src, cursors
}

// saveCursors copies the process's poll cursors into the harness, standing in
// for the Redis cursor store that survives a restart in production.
func (h *restartHarness) saveCursors(src core.SignalSource, cursors *CursorStore) {
	if ts, ok := cursors.Get(context.Background(), src.Name()); ok {
		h.cursors[src.Name()] = ts
	}
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWorker_RestartUnderLoadLosesNothingAndRepeatsNothing is the acceptance
// case, both properties in one test: a process is stopped while rows are still
// arriving, and the process that replaces it must neither drop a row the first
// one read nor re-deliver a row the first one already stored.
//
// The catalog persist interval is set far longer than the run, so NOTHING is
// flushed periodically — every row learned before the stop is riding on the
// shutdown flush alone, which is exactly the state a restart under load leaves
// behind and the state a lost row hides in.
func TestWorker_RestartUnderLoadLosesNothingAndRepeatsNothing(t *testing.T) {
	h := newRestartHarness(t)

	// Sustained ingest: rows keep landing straight through the restart.
	stopProducer := make(chan struct{})
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for {
			select {
			case <-stopProducer:
				return
			default:
			}
			h.push()
			time.Sleep(3 * time.Millisecond)
		}
	}()

	// ---- process one -------------------------------------------------------
	ctx1, cancel1 := context.WithCancel(context.Background())
	w1, src1, cursors1 := h.process(time.Hour) // no periodic flush will ever fire
	done1 := StartWorker(ctx1, w1)

	waitFor(t, "the first process to ingest under load", func() bool { return h.log.total() >= 30 })
	deliveredBeforeStop := h.log.total()

	// The ordering invariant, checked while the process is still alive: many
	// rows have been read and none has been flushed yet, so NOTHING may be
	// recorded as delivered. An id that is durable here stands for a row that is
	// not, and a stop at this instant would drop that row for good.
	if got := h.ledger(); got != 0 {
		t.Fatalf("durable ledger holds %d rows before any flush, want 0 — the test is not exercising the gap", got)
	}
	if got := h.dedup.size(src1.Name()); got != 0 {
		t.Fatalf("%d row ids are durable while 0 rows are — a stop here loses them permanently", got)
	}

	cancel1()
	if !WaitForShutdownFlush(done1, 10*time.Second) {
		t.Fatal("the first process never finished its shutdown flush")
	}
	h.saveCursors(src1, cursors1)

	// Everything the stopped process read is in storage. Without the shutdown
	// flush this is where the rows learned since the last periodic flush — all
	// of them here — would silently vanish.
	if got := h.ledger(); got != deliveredBeforeStop {
		t.Fatalf("after the stop the durable ledger holds %d rows, want the %d the process had read", got, deliveredBeforeStop)
	}

	// Rows keep arriving while nothing is running.
	time.Sleep(30 * time.Millisecond)

	// ---- process two -------------------------------------------------------
	ctx2, cancel2 := context.WithCancel(context.Background())
	w2, src2, cursors2 := h.process(time.Hour)
	done2 := StartWorker(ctx2, w2)

	close(stopProducer)
	<-producerDone
	want := h.groundTruth()

	waitFor(t, "the second process to catch up with the backlog", func() bool {
		lost, _ := h.log.audit(want)
		return len(lost) == 0
	})

	cancel2()
	if !WaitForShutdownFlush(done2, 10*time.Second) {
		t.Fatal("the second process never finished its shutdown flush")
	}
	h.saveCursors(src2, cursors2)

	// ---- both properties, together ----------------------------------------
	lost, duplicated := h.log.audit(want)
	if len(lost) > 0 {
		t.Errorf("%d of %d rows were never delivered after the restart: %v", len(lost), len(want), lost)
	}
	if len(duplicated) > 0 {
		t.Errorf("%d rows were delivered more than once across the restart: %v", len(duplicated), duplicated)
	}
	if got := h.ledger(); got != len(want) {
		t.Errorf("durable ledger holds %d rows, want the %d pushed (delta %d)", got, len(want), got-len(want))
	}
}

// TestWorker_AbruptStopReplaysRatherThanDropsUncommittedRows covers the half a
// graceful shutdown cannot: a SIGKILL or an OOM kill, where no final flush and
// no commit ever run. The rows the dead process read are not in storage, so the
// next process MUST read them again. Bounded duplicates are the acceptable
// failure direction here; a hole is not.
func TestWorker_AbruptStopReplaysRatherThanDropsUncommittedRows(t *testing.T) {
	h := newRestartHarness(t)

	for i := 0; i < 5; i++ {
		h.push()
	}
	want := h.groundTruth()

	// A process that ticks and then dies: no Run loop, so no flush and no
	// commit — the state a killed container leaves behind.
	killed, src1, cursors1 := h.process(time.Hour)
	killed.tickSource(context.Background(), killed.sources[0], "training")
	if h.log.total() != len(want) {
		t.Fatalf("the doomed process read %d rows, want %d", h.log.total(), len(want))
	}
	if got := h.ledger(); got != 0 {
		t.Fatalf("durable ledger holds %d rows after a kill with no flush, want 0", got)
	}
	h.saveCursors(src1, cursors1)

	// The replacement process re-reads what was never stored.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w2, _, _ := h.process(time.Hour)
	done := StartWorker(ctx, w2)
	waitFor(t, "the replacement process to re-read the uncommitted rows", func() bool {
		return h.ledger() >= len(want) || h.log.total() >= 2*len(want)
	})
	cancel()
	if !WaitForShutdownFlush(done, 10*time.Second) {
		t.Fatal("the replacement process never finished its shutdown flush")
	}

	if got := h.ledger(); got < len(want) {
		t.Errorf("durable ledger holds %d rows, want at least the %d pushed — an abrupt stop must never lose a row", got, len(want))
	}
	lost, _ := h.log.audit(want)
	if len(lost) > 0 {
		t.Errorf("%d rows were never re-read after the abrupt stop: %v", len(lost), lost)
	}
}

// TestWorker_AbruptStopLosesNothingWithAReorderWindowBelowThePersistInterval is
// the case a tail can be configured into: `reorder_window` is a lateness
// budget, so shortening it is a reasonable thing to do — and it used to also
// shorten the span a replacement process replays, leaving every unflushed row
// older than that budget below the next query's lower bound. Read, never
// stored, never read again.
//
// The window here is two orders of magnitude below the persist interval and the
// doomed process runs far longer than the window, so before the effective span
// was coupled to the persist interval nearly every row was lost.
func TestWorker_AbruptStopLosesNothingWithAReorderWindowBelowThePersistInterval(t *testing.T) {
	h := newRestartHarness(t)

	const (
		reorderWindow = 30 * time.Millisecond
		// Longer than the whole doomed run, so no periodic flush ever fires and
		// every row read below is riding on a flush that never happens.
		persistEvery = 3 * time.Second
	)

	// A process that reads under sustained ingest for many multiples of the
	// reorder window and then dies: no Run loop, so no flush and no commit.
	doomed, src1, cursors1 := h.processWithWindow(persistEvery, reorderWindow.String())
	for i := 0; i < 10; i++ {
		for j := 0; j < 3; j++ {
			h.push()
		}
		doomed.tickSource(context.Background(), doomed.sources[0], "training")
		time.Sleep(50 * time.Millisecond)
	}
	want := h.groundTruth()
	if h.log.total() != len(want) {
		t.Fatalf("the doomed process read %d rows, want the %d pushed", h.log.total(), len(want))
	}
	if got := h.ledger(); got != 0 {
		t.Fatalf("durable ledger holds %d rows after a kill with no flush, want 0", got)
	}
	if got := h.dedup.size(src1.Name()); got != 0 {
		t.Fatalf("%d row ids are durable while 0 rows are — a kill here loses them permanently", got)
	}
	h.saveCursors(src1, cursors1)

	// From here the audit asks what the REPLACEMENT read. Everything the doomed
	// process read is gone with it.
	h.log.reset()

	// The replacement process must re-read every one of them, not just the
	// handful inside the configured window.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w2, _, _ := h.processWithWindow(persistEvery, reorderWindow.String())
	done := StartWorker(ctx, w2)
	waitForQuiescence(t, h.log, 300*time.Millisecond)
	cancel()
	if !WaitForShutdownFlush(done, 10*time.Second) {
		t.Fatal("the replacement process never finished its shutdown flush")
	}

	lost, _ := h.log.audit(want)
	if len(lost) > 0 {
		t.Errorf("%d of %d rows were never re-read after the abrupt stop with a %s reorder window against a %s persist interval: %v",
			len(lost), len(want), reorderWindow, persistEvery, lost)
	}
	if got := h.ledger(); got < len(want) {
		t.Errorf("durable ledger holds %d rows, want at least the %d pushed", got, len(want))
	}
}

// waitForQuiescence returns once the delivery log has been unchanged for the
// given settle period, so a test can audit a running worker that has caught up
// rather than waiting for a condition that a regression makes unreachable.
func waitForQuiescence(t *testing.T, l *deliveryLog, settle time.Duration) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	last, stableSince := l.total(), time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		if n := l.total(); n != last {
			last, stableSince = n, time.Now()
			continue
		}
		if time.Since(stableSince) >= settle {
			return
		}
	}
	t.Fatal("the delivery log never settled")
}

// committingSource records when the worker asked it to commit and whether the
// context it was handed was still usable.
type committingSource struct {
	mu       sync.Mutex
	commits  int
	deadCtxs int
}

func (s *committingSource) Name() string { return "fake:committer" }

func (s *committingSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return nil, time.Time{}, nil
}

func (s *committingSource) Commit(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	if ctx.Err() != nil {
		s.deadCtxs++
	}
	return nil
}

func (s *committingSource) counts() (commits, dead int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commits, s.deadCtxs
}

// plainSource keeps no delivery state, so it must never be asked to commit.
type plainSource struct{}

func (plainSource) Name() string { return "fake:plain" }

func (plainSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return nil, time.Time{}, nil
}

// TestWorker_CommitsAfterEveryFlushWithALiveContext pins the two things the
// commit seam gets wrong most easily: skipping sources that do not implement it
// (they must behave exactly as before), and reusing the worker's own context on
// the shutdown path — where it has already been canceled, so every backend call
// would fail and the restart would replay a window for nothing.
func TestWorker_CommitsAfterEveryFlushWithALiveContext(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	committer := &committingSource{}
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	matcher, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	svc, _ := NewServiceMatcher(nil)
	w, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode:         "training",
			Lookback:     "5m",
			PollInterval: "20ms",
			Catalog:      config.AgentCatalogConfig{PersistInterval: "10ms"},
		},
		Sources:  []core.SignalSource{committer, plainSource{}},
		Cursors:  NewCursorStore(nil),
		Matcher:  matcher,
		Miner:    NewMiner(0.4, 4, 100),
		Catalog:  cat,
		Services: svc,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := StartWorker(ctx, w)
	waitFor(t, "the periodic flush to commit", func() bool {
		n, _ := committer.counts()
		return n >= 1
	})
	cancel()
	if !WaitForShutdownFlush(done, 10*time.Second) {
		t.Fatal("the worker never finished its shutdown flush")
	}

	commits, dead := committer.counts()
	if commits < 2 {
		t.Errorf("the committing source was asked to commit %d times, want at least one periodic and one on shutdown", commits)
	}
	if dead > 0 {
		t.Errorf("%d commits ran on an already-canceled context; a real backend call would fail every time", dead)
	}
}
