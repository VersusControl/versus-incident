package routes

import (
	"versus-incident/pkg/controllers"

	"github.com/gofiber/fiber/v2"
)

func SetupRoutes(app *fiber.App) {
	api := app.Group("/api") // /api

	incidents := api.Group("/incidents")
	incidents.Post("/", controllers.CreateIncident)
	incidents.Post("/teams/:id", controllers.CreateIncidentWithTeam)
}
