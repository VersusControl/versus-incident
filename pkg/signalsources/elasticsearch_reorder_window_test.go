package signalsources

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// TestElasticsearch_ReorderWindowDefaultAndConfigurable pins the reorder-window
// knob: unset (or invalid) falls back to the documented 1m default, and a valid
// Go duration is honoured. The window is the bounded trade-off that keeps the
// per-tick dedup set small, so its size must be operator-controllable and its
// default must not silently drift.
func TestElasticsearch_ReorderWindowDefaultAndConfigurable(t *testing.T) {
	base := config.AgentElasticsearchSourceConfig{
		Addresses: []string{"http://es:9200"},
		Index:     "logs-*",
	}

	def, err := NewElasticsearchSource("default", base)
	if err != nil {
		t.Fatalf("new default: %v", err)
	}
	if def.reorderWindow != time.Minute {
		t.Errorf("unset reorder window = %v, want 1m default", def.reorderWindow)
	}

	cfgd := base
	cfgd.ReorderWindow = "10s"
	configured, err := NewElasticsearchSource("configured", cfgd)
	if err != nil {
		t.Fatalf("new configured: %v", err)
	}
	if configured.reorderWindow != 10*time.Second {
		t.Errorf("configured reorder window = %v, want 10s", configured.reorderWindow)
	}

	bad := base
	bad.ReorderWindow = "not-a-duration"
	invalid, err := NewElasticsearchSource("invalid", bad)
	if err != nil {
		t.Fatalf("new invalid: %v", err)
	}
	if invalid.reorderWindow != time.Minute {
		t.Errorf("invalid reorder window = %v, want 1m default fallback", invalid.reorderWindow)
	}
}

// TestElasticsearch_ReorderWindowBoundIsHonored proves the documented bound: a
// late-indexed document that lands INSIDE the reorder window below the cursor is
// recovered on the next tick, while one that lands FURTHER back than the window
// is (by design) not recovered — the inclusive `gte lower = cursor - window`
// query never reaches it. This is the bounded cost that keeps the dedup set
// memory-bounded; the test locks the trade-off in so the window can't silently
// widen to "all history" or narrow to "strict boundary only".
func TestElasticsearch_ReorderWindowBoundIsHonored(t *testing.T) {
	fake := newFakeES("@timestamp")
	ts := httptest.NewServer(fake.handler(t))
	defer ts.Close()

	base := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	fake.add("b1", "backlog one", base)
	fake.add("b2", "backlog two", base.Add(20*time.Second)) // cursor1 = base+20s

	src, err := NewElasticsearchSource("bound", config.AgentElasticsearchSourceConfig{
		Addresses:     []string{ts.URL},
		Index:         "logs-*",
		PageSize:      50,
		ReorderWindow: "10s",
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	_, cur1, err := src.Pull(context.Background(), base.Add(-time.Minute))
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if want := base.Add(20 * time.Second); !cur1.Equal(want) {
		t.Fatalf("tick1 cursor = %v, want %v", cur1, want)
	}

	// Two docs arrive late, both behind the cursor:
	//   within: cursor-5s  (inside the 10s window) → must be recovered
	//   outside: cursor-30s (beyond the 10s window) → must NOT be recovered
	fake.add("within", "inside reorder window", cur1.Add(-5*time.Second))
	fake.add("outside", "beyond reorder window", cur1.Add(-30*time.Second))

	sigs, _, err := src.Pull(context.Background(), cur1)
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	got := map[string]bool{}
	for _, s := range sigs {
		got[s.Message] = true
	}
	if !got["inside reorder window"] {
		t.Errorf("doc INSIDE the reorder window was not recovered (window too narrow / gte bound wrong)")
	}
	if got["beyond reorder window"] {
		t.Errorf("doc BEYOND the reorder window was recovered (bound not honored — set would be unbounded)")
	}
	if len(sigs) != 1 {
		msgs := make([]string, len(sigs))
		for i, s := range sigs {
			msgs[i] = s.Message
		}
		t.Errorf("tick2 expected exactly 1 recovered doc (within window, boundary docs deduped), got %d: %v", len(sigs), msgs)
	}
}
