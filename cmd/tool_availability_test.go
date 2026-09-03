package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

func TestToolCatalogGETDoesNotConstructDisabledAgentDependencies(t *testing.T) {
	for _, aiEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "AI disabled", true: "AI enabled with invalid construction config"}[aiEnabled], func(t *testing.T) {
			cfg := config.AgentConfig{
				Enable:  false,
				AI:      config.AgentAIConfig{Enable: aiEnabled, Provider: "invalid-provider", Model: "invalid-model"},
				Sources: []config.AgentSourceConfig{{Name: "broken", Type: "invalid-source", Enable: true}},
				Tools: config.ToolsConfig{Kubernetes: config.KubernetesToolConfig{
					Endpoint: "https://cluster.example",
					Auth:     config.KubernetesAuthConfig{Mode: "invalid"},
				}},
			}
			app := fiber.New()
			app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
			app.Use(middleware.OrgInjector())
			availability := agent.NewToolAvailabilityService(cfg, storage.NewMemory())
			service, constructionErr := agent.NewKubernetesService(cfg.Tools.Kubernetes, tenancy.DefaultOrgScope())
			if constructionErr == nil {
				t.Fatal("invalid Kubernetes authentication constructed")
			}
			availability.BindIntegrationConstruction("kubernetes", service != nil)
			registerToolAvailabilityController(app, availability, service)
			response, err := app.Test(httptest.NewRequest("GET", "/api/admin/agent/tools?agent=chat", nil), -1)
			if err != nil || response.StatusCode != fiber.StatusOK {
				t.Fatalf("status = %v, err = %v", response.StatusCode, err)
			}
			defer response.Body.Close()
			var rows []controllers.ToolAvailability
			if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Group == aitools.GroupK8s && (row.State != aitools.StateUnhealthy || row.Health != "configuration") {
					t.Fatalf("Kubernetes tool %q = state %q health %q", row.Name, row.State, row.Health)
				}
			}
			kubernetesResponse, err := app.Test(httptest.NewRequest("GET", "/api/admin/kubernetes/overview", nil), -1)
			if err != nil || kubernetesResponse.StatusCode != fiber.StatusServiceUnavailable {
				t.Fatalf("Kubernetes API status = %v, err = %v", kubernetesResponse.StatusCode, err)
			}
			kubernetesResponse.Body.Close()
		})
	}
}
