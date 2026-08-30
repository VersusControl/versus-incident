package agent

import (
	"sync"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// ToolAvailabilityService serves configured availability without constructing
// workers, sources, model clients, git readers, or runbook embeddings.
type ToolAvailabilityService struct {
	Manager *aitools.Manager

	mu       sync.RWMutex
	snapshot func(tenancy.OrgScope) aitools.Snapshot
}

// NewToolAvailabilityService builds the lightweight catalog dependencies.
func NewToolAvailabilityService(cfg config.AgentConfig, store storage.Provider) *ToolAvailabilityService {
	configured := configuredToolAvailabilitySnapshot(cfg, store)
	return &ToolAvailabilityService{
		Manager:  aitools.NewManager(store),
		snapshot: func(tenancy.OrgScope) aitools.Snapshot { return configured },
	}
}

// Snapshot returns the current configured or live availability generation.
func (service *ToolAvailabilityService) Snapshot(scope tenancy.OrgScope) aitools.Snapshot {
	service.mu.RLock()
	snapshot := service.snapshot
	service.mu.RUnlock()
	return snapshot(scope)
}

// BindLiveSnapshot enriches the service after worker-side construction succeeds.
func (service *ToolAvailabilityService) BindLiveSnapshot(snapshot func(tenancy.OrgScope) aitools.Snapshot) {
	if snapshot == nil {
		return
	}
	service.mu.Lock()
	service.snapshot = snapshot
	service.mu.Unlock()
}
