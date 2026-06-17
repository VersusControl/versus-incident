package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type failingSource struct {
	name  string
	calls *int32
}

func (f failingSource) Name() string { return f.name }
func (f failingSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	atomic.AddInt32(f.calls, 1)
	return nil, time.Time{}, errors.New("backend down")
}

// TestWorker_TickSource_BreakerSkipsFailingSource asserts a source that
// fails is not pulled again on the next tick while its breaker is open —
// the fix for the retry-every-tick hammering of a downed backend.
func TestWorker_TickSource_BreakerSkipsFailingSource(t *testing.T) {
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	var calls int32
	w, err := NewWorker(WorkerOptions{
		Miner:   NewMiner(0.4, 4, 100),
		Catalog: cat,
		// Long backoff so the second tick is firmly inside the open window.
		Health: NewHealthTracker(time.Hour, time.Hour),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	src := failingSource{name: "es", calls: &calls}

	w.tickSource(context.Background(), src, "training") // fails → breaker opens
	w.tickSource(context.Background(), src, "training") // open → skipped

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("Pull called %d times, want 1 (breaker must skip the 2nd tick)", got)
	}

	// A manual resume re-enables pulling.
	w.Health().Resume("es")
	w.tickSource(context.Background(), src, "training")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("Pull called %d times after resume, want 2", got)
	}
}

func TestCursorStore_Delete(t *testing.T) {
	ctx := context.Background()
	cs := NewCursorStore(nil) // in-memory
	now := time.Now()
	if err := cs.Set(ctx, "es", now); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok := cs.Get(ctx, "es"); !ok {
		t.Fatal("cursor should be present after Set")
	}
	if err := cs.Delete(ctx, "es"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := cs.Get(ctx, "es"); ok {
		t.Error("cursor should be gone after Delete")
	}
}
