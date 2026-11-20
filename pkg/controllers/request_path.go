package controllers

import (
	"github.com/gofiber/fiber/v2"

	"github.com/VersusControl/versus-incident/pkg/middleware"
)

// routingPath returns the request path in the form the router matched it; see
// middleware.RoutePath.
func routingPath(ctx *fiber.Ctx) string {
	return middleware.RoutePath(ctx)
}
