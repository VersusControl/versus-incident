package routes

import (
	"github.com/VersusControl/versus-incident/pkg/controllers"

	"github.com/gofiber/fiber/v2"
)

func SetupRoutes(app *fiber.App) {
	// Health check endpoint
	app.Get("/healthz", controllers.HealthCheck)

	// API routes
	api := app.Group("/api")

	incidents := api.Group("/incidents")
	incidents.Post("/", controllers.CreateIncident)

	api.Get("/ack/:incidentID", controllers.HandleAck)

	// Admin read endpoints (gated by X-Gateway-Secret). Mounted here so
	// the controller can attach its own middleware via the group.
	controllers.NewIncidentAdminController().Register(api)
}
