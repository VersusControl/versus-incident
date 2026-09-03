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

// BindIntegrationConstruction records whether a configured integration was
// constructed successfully before a worker-side live snapshot is available.
func (service *ToolAvailabilityService) BindIntegrationConstruction(name string, constructed bool) {
	snapshot := service.Snapshot(tenancy.DefaultOrgScope())
	status, ok := snapshot.Integrations[name]
	if !ok {
		return
	}
	integrations := make(map[string]aitools.DependencyStatus, len(snapshot.Integrations))
	for key, value := range snapshot.Integrations {
		integrations[key] = value
	}
	status.Constructed = constructed
	status.Healthy = status.Configured && constructed
	if status.Configured && !constructed {
		status.Health = "configuration"
	}
	integrations[name] = status
	snapshot.Integrations = integrations
	service.BindLiveSnapshot(func(tenancy.OrgScope) aitools.Snapshot { return snapshot })
}
