package main

import (
	"net/http/httptest"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

func TestToolCatalogGETDoesNotConstructDisabledAgentDependencies(t *testing.T) {
	for _, aiEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "AI disabled", true: "AI enabled with invalid construction config"}[aiEnabled], func(t *testing.T) {
			cfg := config.AgentConfig{
				Enable:  false,
				AI:      config.AgentAIConfig{Enable: aiEnabled, Provider: "invalid-provider", Model: "invalid-model"},
				Sources: []config.AgentSourceConfig{{Name: "broken", Type: "invalid-source", Enable: true}},
			}
			app := fiber.New()
			app.Use(func(ctx *fiber.Ctx) error { middleware.MarkAuthorized(ctx); return ctx.Next() })
			app.Use(middleware.OrgInjector())
			registerToolAvailabilityController(app, agent.NewToolAvailabilityService(cfg, storage.NewMemory()))
			response, err := app.Test(httptest.NewRequest("GET", "/api/admin/agent/tools?agent=chat", nil), -1)
			if err != nil || response.StatusCode != fiber.StatusOK {
				t.Fatalf("status = %v, err = %v", response.StatusCode, err)
			}
			response.Body.Close()
		})
	}
}
