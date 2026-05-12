package agent

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// panickySource is a SignalSource whose Pull panics. Used to verify
// the worker's recover() in tick() so a single bad source does not
// kill the agent.
type panickySource struct{ name string }

func (p *panickySource) Name() string { return p.name }
func (p *panickySource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	panic("synthetic panic from " + p.name)
}

// healthySource always returns an empty batch — exercises the
// success-path RecordSuccess so we can prove the second goroutine
// still ran after the first panicked.
type healthySource struct {
	name   string
	called bool
}

func (h *healthySource) Name() string { return h.name }
func (h *healthySource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	h.called = true
	return nil, since, nil
}

// TestWorker_TickRecoversFromSourcePanic verifies H4: a panic inside
// one source's tickSource goroutine is caught, logged, and recorded
// as a failure on the HealthTracker. The other source still runs, and
// the worker is alive for the next tick.
func TestWorker_TickRecoversFromSourcePanic(t *testing.T) {
	bad := &panickySource{name: "bad"}
	good := &healthySource{name: "good"}

	store := newTestStore(t)
	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	miner := NewMiner(0.4, 4, 100)

	w, err := NewWorker(WorkerOptions{
		Cfg: config.AgentConfig{
			Mode:         "training",
			PollInterval: "1s",
			Reliability: config.AgentReliabilityConfig{
				SourceBackoffInitial: "1s",
				SourceBackoffMax:     "5s",
			},
		},
		Sources: []core.SignalSource{bad, good},
		Miner:   miner,
		Catalog: cat,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	// One tick. If the recover() is missing this would crash the
	// test process (goroutines panic out of the test runtime).
	w.tick(context.Background(), "training")

	if !good.called {
		t.Fatal("healthy source must still run when sibling source panics")
	}
	snap := w.Health.Snapshot()
	var badHealth *SourceHealth
	for i := range snap {
		if snap[i].Name == "bad" {
			badHealth = &snap[i]
		}
	}
	if badHealth == nil {
		t.Fatal("bad source missing from health tracker")
	}
	if badHealth.ConsecutiveFailures < 1 {
		t.Fatalf("panic should count as a failure; got consecutive_failures=%d",
			badHealth.ConsecutiveFailures)
	}
	if badHealth.LastError == "" || badHealth.LastError[:6] != "panic:" {
		t.Fatalf("LastError should start with 'panic:'; got %q", badHealth.LastError)
	}

	// A second tick should not crash either — the worker keeps moving.
	w.tick(context.Background(), "training")
}
