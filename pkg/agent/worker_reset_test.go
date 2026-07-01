package agent

import (
	"context"
	"testing"
)

// TestWorker_ResetPatterns_SameWorkerKeepsLearning is the ITEM A regression
// guard. It reproduces the founder's "Clear all stops learning" report at the
// worker level: after the scoped patterns reset (Catalog.ResetPatterns + the
// controller's shared-miner reset), the SAME running worker — never
// reconstructed — must learn a brand-new log line into a brand-new pattern on
// the very next tick, and a line the miner had already clustered before the
// reset must be re-discovered as NEW (proof the drain miner was truly wiped,
// not left detached with stale templates).
func TestWorker_ResetPatterns_SameWorkerKeepsLearning(t *testing.T) {
	SetCatalogStore(nil)

	src := &batchSource{name: "es", signals: repeatSignals("service=api oops id=", 5)}
	w := newSeamWorker(t, "training", src, AIBundle{}, nil)

	// Learn an initial pattern + service.
	w.tickSource(context.Background(), src, "training")
	if w.catalog.Len() != 1 {
		t.Fatalf("pre-reset: catalog has %d patterns, want 1", w.catalog.Len())
	}
	if _, ok := w.catalog.AllServices()["api"]; !ok {
		t.Fatalf("pre-reset: service 'api' not registered")
	}

	// Exactly what DELETE /api/agent/patterns → clearPatterns does: wipe the
	// pattern half and reset the shared drain miner. The worker is NOT rebuilt.
	if _, err := w.catalog.ResetPatterns(); err != nil {
		t.Fatalf("ResetPatterns: %v", err)
	}
	w.miner.Reset()

	if w.catalog.Len() != 0 {
		t.Fatalf("post-reset: catalog has %d patterns, want 0", w.catalog.Len())
	}
	// Services are the OTHER half — the patterns reset must leave them intact.
	if _, ok := w.catalog.AllServices()["api"]; !ok {
		t.Fatalf("post-reset: service 'api' was wiped by a PATTERNS reset")
	}

	// A NEW distinct line must mint a NEW pattern in the same process.
	src.signals = repeatSignals("service=web crashed hard id=", 5)
	w.tickSource(context.Background(), src, "training")
	if w.catalog.Len() != 1 {
		t.Fatalf("post-reset learn: catalog has %d patterns, want 1 (the worker stopped learning)", w.catalog.Len())
	}

	// A line the miner had ALREADY clustered before the reset must be
	// re-discovered as new — this is what fails if the miner reset is skipped
	// (a stale cluster would return isNew=false, the "no new patterns" symptom).
	if _, _, isNew := w.miner.Cluster("service=api oops id=999"); !isNew {
		t.Fatalf("post-reset: a previously-seen line was NOT re-discovered as new — miner left detached with stale templates")
	}
}

// TestWorker_ResetServices_SameWorkerKeepsDiscovering proves the service half of
// the scoped reset: after Catalog.ResetServices the SAME worker re-discovers a
// service on the next tick, while the learned patterns AND the drain miner are
// left untouched (ResetServices must not reset the miner).
func TestWorker_ResetServices_SameWorkerKeepsDiscovering(t *testing.T) {
	SetCatalogStore(nil)

	src := &batchSource{name: "es", signals: repeatSignals("service=api oops id=", 5)}
	w := newSeamWorker(t, "training", src, AIBundle{}, nil)

	w.tickSource(context.Background(), src, "training")
	if _, ok := w.catalog.AllServices()["api"]; !ok {
		t.Fatalf("pre-reset: service 'api' not registered")
	}
	patternsBefore := w.catalog.Len()
	minerBefore := len(w.miner.Snapshot())
	if patternsBefore == 0 || minerBefore == 0 {
		t.Fatalf("pre-reset: patterns=%d miner=%d, want both > 0", patternsBefore, minerBefore)
	}

	// Exactly what DELETE /api/agent/services → clearServices does: wipe the
	// service half only. Patterns + miner are LEFT INTACT.
	if _, err := w.catalog.ResetServices(); err != nil {
		t.Fatalf("ResetServices: %v", err)
	}
	if n := len(w.catalog.AllServices()); n != 0 {
		t.Fatalf("post-reset: %d services remain, want 0", n)
	}
	if w.catalog.Len() != patternsBefore {
		t.Fatalf("post-reset: patterns = %d, want %d (a SERVICES reset must keep patterns)", w.catalog.Len(), patternsBefore)
	}
	if n := len(w.miner.Snapshot()); n != minerBefore {
		t.Fatalf("post-reset: miner has %d clusters, want %d (a SERVICES reset must not reset the miner)", n, minerBefore)
	}

	// The SAME worker re-discovers a service on the next tick.
	src.signals = repeatSignals("service=payments boom id=", 5)
	w.tickSource(context.Background(), src, "training")
	if _, ok := w.catalog.AllServices()["payments"]; !ok {
		t.Fatalf("post-reset: worker did not re-discover a service after a services reset")
	}
}
