package agent

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestNewWorker_EwmaAlphaFromConfig asserts the worker reads the EWMA
// smoothing factor from agent.catalog.ewma_alpha and falls back to 0.2
// when unset (0). Previously the value was hardcoded to 0.2.
func TestNewWorker_EwmaAlphaFromConfig(t *testing.T) {
	mk := func(t *testing.T, alpha float64) *Worker {
		t.Helper()
		cat, err := LoadCatalog(storage.NewMemory())
		if err != nil {
			t.Fatalf("LoadCatalog: %v", err)
		}
		w, err := NewWorker(WorkerOptions{
			Cfg:     config.AgentConfig{Catalog: config.AgentCatalogConfig{EwmaAlpha: alpha}},
			Miner:   NewMiner(0.4, 4, 100),
			Catalog: cat,
		})
		if err != nil {
			t.Fatalf("NewWorker: %v", err)
		}
		return w
	}

	if got := mk(t, 0.35).ewmaAlpha; got != 0.35 {
		t.Errorf("configured ewma_alpha = %v, want 0.35", got)
	}
	if got := mk(t, 0).ewmaAlpha; got != 0.2 {
		t.Errorf("unset ewma_alpha default = %v, want 0.2", got)
	}
}
