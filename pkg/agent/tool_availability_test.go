package agent

import (
	"testing"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestToolAvailabilityServiceWorksWithoutWorkerOrAIConstruction(t *testing.T) {
	service := NewToolAvailabilityService(config.AgentConfig{Tools: config.ToolsConfig{
		QueryMetrics: config.QueryMetricsToolConfig{Prometheus: config.QueryMetricsPrometheusConfig{Address: "http://prometheus"}},
		QueryTraces:  config.QueryTracesToolConfig{Tempo: config.QueryTracesTempoConfig{Address: "http://tempo"}},
	}}, storage.NewMemory())
	if service.Manager == nil {
		t.Fatal("manager is nil")
	}
	snapshot := service.Snapshot(tenancy.DefaultOrgScope())
	for _, kind := range []string{"metrics", "traces"} {
		status := snapshot.DataSources[kind]
		if !status.Configured || !status.Constructed || !status.Healthy {
			t.Fatalf("configured %s status = %+v", kind, status)
		}
	}
}

func TestToolAvailabilityServiceBindsLiveSnapshot(t *testing.T) {
	service := NewToolAvailabilityService(config.AgentConfig{}, storage.NewMemory())
	service.BindLiveSnapshot(func(tenancy.OrgScope) aitools.Snapshot {
		return aitools.Snapshot{DataSources: map[string]aitools.DependencyStatus{"logs": {Configured: true, Healthy: false, Health: "connection"}}}
	})
	if got := service.Snapshot(tenancy.DefaultOrgScope()).DataSources["logs"]; got.Healthy || got.Health != "connection" {
		t.Fatalf("live status = %+v", got)
	}
}

func TestToolAvailabilityServiceBindsIntegrationConstruction(t *testing.T) {
	service := NewToolAvailabilityService(config.AgentConfig{Tools: config.ToolsConfig{
		Kubernetes: config.KubernetesToolConfig{Auth: config.KubernetesAuthConfig{Mode: "invalid"}},
	}}, storage.NewMemory())
	service.BindIntegrationConstruction("kubernetes", false)
	got := service.Snapshot(tenancy.DefaultOrgScope()).Integrations["kubernetes"]
	if !got.Configured || got.Constructed || got.Healthy || got.Health != "configuration" {
		t.Fatalf("Kubernetes status = %+v", got)
	}
}
