package routes

import (
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/servicetopology"
	"github.com/VersusControl/versus-incident/pkg/teams"

	"github.com/gofiber/fiber/v2"
)

func SetupRoutes(app *fiber.App, teamsStore *teams.Store, healthManagers ...*servicehealth.Manager) {
	var healthManager *servicehealth.Manager
	if len(healthManagers) > 0 {
		healthManager = healthManagers[0]
	}
	SetupRoutesWithTopology(app, teamsStore, healthManager, nil)
}

// SetupRoutesWithTopology mounts the standard routes with an independent topology service.
func SetupRoutesWithTopology(app *fiber.App, teamsStore *teams.Store, healthManager *servicehealth.Manager, topologyManager *servicetopology.Manager) {
	// Health check endpoint
	app.Get("/healthz", controllers.HealthCheck)

	// API routes
	api := app.Group("/api")
	controllers.RegisterGatewaySessionRoutes(api)

	// Enterprise auth slot. No-op pass-through in community mode;
	// an external module registers SSO/JWT enforcement via
	// middleware.SetAuthMiddleware before the server starts.
	api.Use(middleware.AuthMiddleware())

	incidents := api.Group("/incidents")
	incidents.Post("/", controllers.CreateIncident)

	api.Get("/ack/:incidentID", controllers.HandleAck)

	// Admin read endpoints (gated by X-Gateway-Secret). Mounted here so
	// the controller can attach its own middleware via the group.
	controllers.NewIncidentAdminController().Register(api)
	controllers.NewConfigAdminController().Register(api)
	controllers.NewTeamsAdminController(teamsStore).Register(api)
	controllers.NewReportsAdminController().Register(api)
	controllers.NewSpikeAdminController().Register(api)
	controllers.NewCountSettingsController().Register(api)
	controllers.NewChatAdminController(nil).Register(api)
	// The static graph is boot-pinned global configuration in the current
	// single-organization OSS architecture; request authorization remains scoped.
	controllers.NewServiceHealthControllerWithTopology(healthManager, topologyManager).Register(api)
}
