package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func mkSig(msg string, ts time.Time) core.Signal { return core.Signal{Message: msg, Timestamp: ts} }

// buildClearRewindWorker builds a training worker over one source with an
// in-memory cursor store — the single-container OSS shape.
func buildClearRewindWorker(t *testing.T, src core.SignalSource) (*Worker, *Catalog, *Miner, *CursorStore) {
	t.Helper()
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	miner := NewMiner(0.4, 4, 100)
	m, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	svc, _ := NewServiceMatcher([]string{`service=(\w+)`})
	cursors := NewCursorStore(nil)
	w, err := NewWorker(WorkerOptions{
		Cfg:      config.AgentConfig{Mode: "training", Lookback: "5m", Catalog: config.AgentCatalogConfig{AutoPromoteAfter: 100}},
		Sources:  []core.SignalSource{src},
		Cursors:  cursors,
		Matcher:  m,
		Miner:    miner,
		Catalog:  cat,
		Services: svc,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w, cat, miner, cursors
}

// TestWorker_ClearRewindsFileSource_SameWorkerRelearns is the founder-reported
// regression at the worker level, for a source that keeps its OWN read position
// and ignores the worker's poll cursor — the file source (byte offset). It
// proves the "Clear all logs → pulls once then stops until the container is
// recreated" halt and that rewinding the source's own position (the fix) makes
// the SAME running worker re-read the file and relearn in place.
//
// The three phases each bite a distinct half of the bug:
//   - phase 1: the worker learns from the file (baseline).
//   - phase 2: a clear that rewinds ONLY the worker poll cursor + resets
//     catalog/miner (the pre-fix clear) relearns NOTHING — the file source is
//     still pinned at EOF because it ignores the poll cursor. This is the halt.
//   - phase 3: additionally rewinding the source's own position (core
//     .SourceRewinder — what clearPatterns now does) re-reads the whole file and
//     relearns immediately, no restart.
func TestWorker_ClearRewindsFileSource_SameWorkerRelearns(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logPath, []byte(
		"service=api boom id=1\nservice=api boom id=2\nservice=api boom id=3\n"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	fs, err := signalsources.NewFileSource("app", config.AgentFileSourceConfig{Path: logPath, FromBeginning: true})
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	w, cat, miner, cursors := buildClearRewindWorker(t, fs)
	ctx := context.Background()

	// Phase 1 — the worker learns the file's lines.
	w.tickSource(ctx, fs, "training")
	if cat.Len() == 0 {
		t.Fatal("phase1: worker learned nothing from the file")
	}

	// Phase 2 — the PRE-FIX clear: rewind poll cursor + reset catalog/miner, but
	// do NOT rewind the file source's own byte offset. The file source ignores
	// the poll cursor, so it stays pinned at EOF and the next tick (no appends)
	// relearns NOTHING — the founder's halt.
	if err := cursors.Reset(ctx); err != nil {
		t.Fatalf("cursors.Reset: %v", err)
	}
	if _, err := cat.ResetPatterns(); err != nil {
		t.Fatalf("ResetPatterns: %v", err)
	}
	miner.Reset()

	w.tickSource(ctx, fs, "training")
	if cat.Len() != 0 {
		t.Fatalf("phase2: worker relearned %d patterns WITHOUT a source rewind; "+
			"the file-source halt this regression targets is not reproduced", cat.Len())
	}

	// Phase 3 — the FIX: rewind the source's own read position through the
	// generic core.SourceRewinder seam (exactly what clearPatterns now does after
	// the cursor reset). The SAME running worker must re-read the file in place.
	r, ok := interface{}(fs).(core.SourceRewinder)
	if !ok {
		t.Fatal("phase3: file source does not implement core.SourceRewinder")
	}
	if err := r.Rewind(ctx); err != nil {
		t.Fatalf("phase3: Rewind: %v", err)
	}
	w.tickSource(ctx, fs, "training")
	if cat.Len() == 0 {
		t.Fatal("phase3: after the source rewind the SAME worker still relearned nothing; the halt is not fixed")
	}
}

// advanceOnEmptySource emits a batch once, then on every later pull returns NO
// signals but reports a high-water-mark cursor STRICTLY newer than `since` — a
// source that says "I scanned ahead and found nothing new". It models the
// empty-tick-advances-cursor edge the worker must not strand.
type advanceOnEmptySource struct {
	name  string
	batch []core.Signal
	hwm   time.Time
	pulls int
}

func (s *advanceOnEmptySource) Name() string { return s.name }
func (s *advanceOnEmptySource) Pull(_ context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	s.pulls++
	if s.pulls == 1 {
		cur := since
		for _, sig := range s.batch {
			if sig.Timestamp.After(cur) {
				cur = sig.Timestamp
			}
		}
		return s.batch, cur, nil
	}
	return nil, s.hwm, nil
}

// TestWorker_EmptyTickPersistsAdvancedCursor proves an empty tick that reports a
// legitimately-advanced cursor is NOT stranded: the worker persists newCursor
// so the next tick continues from the high-water mark instead of re-scanning
// the same empty range forever. Persisting is data-safe because newCursor is
// the max timestamp the source actually scanned, so nothing after it is skipped.
func TestWorker_EmptyTickPersistsAdvancedCursor(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	base := time.Now().UTC().Add(-2 * time.Minute)
	src := &advanceOnEmptySource{
		name:  "es:prod",
		batch: []core.Signal{mkSig("service=api boom id=1", base.Add(1*time.Second)), mkSig("service=api boom id=2", base.Add(2*time.Second))},
		hwm:   base.Add(1 * time.Minute), // strictly newer than the batch max (base+2s)
	}
	w, _, _, cursors := buildClearRewindWorker(t, src)
	ctx := context.Background()

	// Tick 1 emits the batch; cursor lands on the batch max (base+2s).
	w.tickSource(ctx, src, "training")
	c1, ok := cursors.Get(ctx, src.Name())
	if !ok || !c1.Equal(base.Add(2*time.Second)) {
		t.Fatalf("tick1 cursor = %v ok=%v, want %v", c1, ok, base.Add(2*time.Second))
	}

	// Tick 2 emits nothing but reports the high-water mark. The advanced cursor
	// must be persisted (not dropped by the empty-tick early return).
	w.tickSource(ctx, src, "training")
	c2, ok := cursors.Get(ctx, src.Name())
	if !ok {
		t.Fatal("tick2: cursor missing after an empty tick — advanced cursor was stranded")
	}
	if !c2.Equal(src.hwm) {
		t.Fatalf("tick2 cursor = %v, want high-water mark %v (empty tick did not advance the cursor)", c2, src.hwm)
	}
}

// neverEmitSource always returns no signals and reports newCursor == since — the
// behaviour of every OSS query source on an empty window.
type neverEmitSource struct{ name string }

func (s *neverEmitSource) Name() string { return s.name }
func (s *neverEmitSource) Pull(_ context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	return nil, since, nil
}

// TestWorker_EmptyTickUnchangedCursorIsNoOp locks the no-regression control: an
// empty tick whose newCursor equals `since` (every OSS source today) must NOT
// write a cursor. This guards against the empty-tick persistence spuriously
// pinning a cursor and skipping the lookback window on the next tick.
func TestWorker_EmptyTickUnchangedCursorIsNoOp(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	src := &neverEmitSource{name: "es:prod"}
	w, _, _, cursors := buildClearRewindWorker(t, src)
	ctx := context.Background()

	w.tickSource(ctx, src, "training")
	if _, ok := cursors.Get(ctx, src.Name()); ok {
		t.Error("empty tick with newCursor==since wrote a cursor; the next tick will no longer re-scan the lookback window")
	}
}
