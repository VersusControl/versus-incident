package controllers

import (
	"errors"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

// CountSettingsController exposes the incident-count window as a runtime
// setting. One window bounds every count surface (header badge, Now tiles,
// Incidents tabs), so counts describe recent load instead of an all-time total
// that only ever grows. Non-secret, persisted via the storage-backed settings
// store, guarded by the same X-Gateway-Secret as the rest of the admin surface.
type CountSettingsController struct{}

// NewCountSettingsController returns a controller. No state of its own; the
// settings store is reached via the services storage seam.
func NewCountSettingsController() *CountSettingsController {
	return &CountSettingsController{}
}

// Register attaches the endpoints under /api/admin/agent.
//
//	GET /api/admin/agent/count-settings   current incident-count window
//	PUT /api/admin/agent/count-settings   update the incident-count window
func (cc *CountSettingsController) Register(router fiber.Router) {
	g := router.Group("/admin/agent", adminGatewayGuard)
	g.Get("/count-settings", cc.getSettings)
	g.Put("/count-settings", cc.putSettings)
}

// getSettings returns the current incident-count window (or the built-in
// default when none is stored).
func (cc *CountSettingsController) getSettings(c *fiber.Ctx) error {
	return c.JSON(agent.LoadCountSettings(services.Storage()))
}

// putSettings persists the incident-count window. An unknown window is
// rejected with a 400 rather than silently folded to the default, so an
// operator never believes they set something the system ignored.
func (cc *CountSettingsController) putSettings(c *fiber.Ctx) error {
	var s agent.CountSettings
	if err := c.BodyParser(&s); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid settings body"})
	}
	// Copy off the pooled request buffer before it outlives the request.
	w := strings.Clone(strings.TrimSpace(s.Window))
	if !agent.KnownCountWindow(w) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid count window (want 24h|7d|30d|90d|all)"})
	}
	s.Window = w
	if err := agent.SaveCountSettings(services.Storage(), s); err != nil {
		if errors.Is(err, agent.ErrCountNoStorage) {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	// Return the effective (sanitized) settings after the write.
	return c.JSON(agent.LoadCountSettings(services.Storage()))
}
