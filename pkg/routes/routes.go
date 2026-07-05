package routes

import (
	"github.com/VersusControl/versus-incident/pkg/controllers"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/teams"

	"github.com/gofiber/fiber/v2"
)

func SetupRoutes(app *fiber.App, teamsStore *teams.Store) {
	// Health check endpoint
	app.Get("/healthz", controllers.HealthCheck)

	// API routes
	api := app.Group("/api")

	// Enterprise auth slot (X2-T3). No-op pass-through in community mode;
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
}
