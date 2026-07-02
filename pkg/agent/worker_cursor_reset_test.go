package agent

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// cursorRespectingSource models a REAL backend (Elasticsearch/Loki/file): per
// the core.SignalSource contract it returns only signals STRICTLY NEWER than
// `since`, and reports newCursor = the max timestamp it emitted. This is the
// behaviour a naive in-test batch source omits (returning its whole batch every
// Pull), which is precisely why the halt escaped earlier reproductions.
type cursorRespectingSource struct {
	name string
	logs []core.Signal
}

func (s *cursorRespectingSource) Name() string { return s.name }
func (s *cursorRespectingSource) Pull(_ context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	var out []core.Signal
	newCursor := since
	for _, l := range s.logs {
		if l.Timestamp.After(since) {
			out = append(out, l)
			if l.Timestamp.After(newCursor) {
				newCursor = l.Timestamp
			}
		}
	}
	return out, newCursor, nil
}

// TestWorker_ClearThenRelearn_RewindsCursor is the founder-reported regression:
// after clearing all learned patterns, the SAME running worker must relearn
// from the available log window WITHOUT recreating the container.
//
// The bug: clearing wiped the catalog + miner but left the poll cursor pinned
// past the already-consumed window, so a cursor-respecting source returned
// nothing and learning appeared to halt permanently (only a container restart —
// which resets in-memory cursors — recovered it).
//
// This test asserts both halves:
//   - WITHOUT the cursor rewind, the second tick relearns NOTHING (the halt).
//   - WITH the cursor rewind (what clearPatterns now does), the same worker
//     relearns immediately — byte-identical to a fresh process start.
func TestWorker_ClearThenRelearn_RewindsCursor(t *testing.T) {
	SetCatalogStore(nil)
	t.Cleanup(func() { SetCatalogStore(nil) })

	base := time.Now().UTC().Add(-2 * time.Minute)
	logs := []core.Signal{
		{Message: "service=api error code=1", Timestamp: base.Add(1 * time.Second)},
		{Message: "service=api error code=2", Timestamp: base.Add(2 * time.Second)},
		{Message: "service=web crash id=3", Timestamp: base.Add(3 * time.Second)},
	}
	src := &cursorRespectingSource{name: "es", logs: logs}

	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	miner := NewMiner(0.4, 4, 100)
	m, _ := NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
	svc, _ := NewServiceMatcher([]string{`service=(\w+)`})
	cursors := NewCursorStore(nil) // in-memory, like a single OSS container
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
	ctx := context.Background()

	// Tick 1: learns the patterns; cursor advances past the newest log.
	w.tickSource(ctx, src, "training")
	if cat.Len() == 0 {
		t.Fatal("tick1 learned nothing; test setup is wrong")
	}

	// --- Operator clears all patterns, mirroring controller.clearPatterns
	//     BUT without the cursor rewind (the old, broken behaviour). ---
	if _, err := cat.ResetPatterns(); err != nil {
		t.Fatalf("ResetPatterns: %v", err)
	}
	miner.Reset()

	w.tickSource(ctx, src, "training")
	if cat.Len() != 0 {
		t.Fatalf("without cursor rewind the worker unexpectedly relearned %d patterns; the cursor is not the guard this regression targets", cat.Len())
	}

	// --- Now do what the FIX adds: rewind the cursors. The same running
	//     worker must re-read the lookback window and relearn immediately. ---
	if err := cursors.Reset(ctx); err != nil {
		t.Fatalf("cursors.Reset: %v", err)
	}
	w.tickSource(ctx, src, "training")
	if cat.Len() == 0 {
		t.Fatal("after cursor rewind the same running worker still relearned nothing; the halt is not fixed")
	}
}
