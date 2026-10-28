package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/agentapi"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"

	"github.com/gofiber/fiber/v2"
)

func TestActionHistoryGETWithoutAgent(t *testing.T) {
	if err := registerActionHistoryService(storage.NewMemory()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controllers.SetAgentApprovalServiceFactory(nil) })
	for _, permission := range []bool{false, true} {
		app := fiber.New(fiber.Config{Immutable: true})
		app.Use(middleware.OrgInjector())
		app.Use(func(ctx *fiber.Ctx) error {
			middleware.MarkAuthorized(ctx)
			middleware.SetRequestPermission(ctx, string(core.PermissionAgentApprove), permission)
			return ctx.Next()
		})
		controllers.NewChatAdminController(nil).Register(app.Group("/api"))
		response, err := app.Test(httptest.NewRequest("GET", "/api/v1/agent/approvals?state=all", nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := fiber.StatusForbidden
		if permission {
			want = fiber.StatusOK
		}
		if response.StatusCode != want {
			t.Fatalf("permission=%t status=%d want=%d", permission, response.StatusCode, want)
		}
		bootstrap, err := app.Test(httptest.NewRequest("GET", "/api/v1/agent/bootstrap", nil), -1)
		if err != nil {
			t.Fatal(err)
		}
		var payload agentapi.Bootstrap
		decodeErr := json.NewDecoder(bootstrap.Body).Decode(&payload)
		bootstrap.Body.Close()
		if bootstrap.StatusCode != fiber.StatusOK || decodeErr != nil || payload.Features.Approvals || payload.Features.Ledger {
			t.Fatalf("history-only bootstrap status=%d features=%+v err=%v", bootstrap.StatusCode, payload.Features, decodeErr)
		}
	}
}

func TestToolCatalogGETDoesNotConstructDisabledAgentDependencies(t *testing.T) {
	for _, aiEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "AI disabled", true: "AI enabled with invalid construction config"}[aiEnabled], func(t *testing.T) {
			cfg := config.AgentConfig{
				Enable:  false,
				AI:      config.AgentAIConfig{Enable: aiEnabled, Provider: "invalid-provider", Model: "invalid-model"},
				Sources: []config.AgentSourceConfig{{Name: "broken", Type: "invalid-source", Enable: true}},
			}
			connectors := config.ConnectorsConfig{Kubernetes: config.KubernetesConnectorConfig{KubernetesToolConfig: config.KubernetesToolConfig{
				Endpoint: "https://cluster.example",
				Auth:     config.KubernetesAuthConfig{Mode: "invalid"},
			}}}
			app := fiber.New()
			app.Use(func(ctx *fiber.Ctx) error {
				middleware.MarkAuthorized(ctx)
				middleware.SetRequestPermission(ctx, string(core.PermissionInfrastructureView), true)
				return ctx.Next()
			})
			app.Use(middleware.OrgInjector())
			availability := agent.NewToolAvailabilityService(cfg, storage.NewMemory(), connectors)
			service, constructionErrors := agent.NewKubernetesRegistry(connectors.Kubernetes, tenancy.DefaultOrgScope())
			if len(constructionErrors) == 0 {
				t.Fatal("invalid Kubernetes authentication constructed")
			}
			availability.BindIntegrationConstruction("kubernetes", len(constructionErrors) == 0)
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
			readerFound, actionFound := false, false
			for _, row := range rows {
				if row.Group != aitools.GroupK8s {
					continue
				}
				if row.Name == "propose_action" {
					actionFound = true
					if row.State != aitools.StateNeedsCapability || row.Health != "" {
						t.Fatalf("Kubernetes action tool %q = state %q health %q", row.Name, row.State, row.Health)
					}
					continue
				}
				readerFound = true
				if row.State != aitools.StateUnhealthy || row.Health != "configuration" {
					t.Fatalf("Kubernetes tool %q = state %q health %q", row.Name, row.State, row.Health)
				}
			}
			if !readerFound || !actionFound {
				t.Fatalf("Kubernetes catalog presence: reader = %t, action = %t", readerFound, actionFound)
			}
			kubernetesResponse, err := app.Test(httptest.NewRequest("GET", "/api/admin/kubernetes/overview", nil), -1)
			if err != nil || kubernetesResponse.StatusCode != fiber.StatusServiceUnavailable {
				t.Fatalf("Kubernetes API status = %v, err = %v", kubernetesResponse.StatusCode, err)
			}
			kubernetesResponse.Body.Close()
		})
	}
}
