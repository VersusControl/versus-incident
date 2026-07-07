package controllers

import (
	"errors"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/middleware"
	"github.com/VersusControl/versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

// SpikeAdminController exposes the log volume-spike detector's GLOBAL default
// baseline mode as a runtime setting: read it and update it. The mode is a
// non-secret operational choice, persisted via the storage-backed spike
// settings store (not YAML). Same X-Gateway-Secret guard as the rest of the
// admin surface.
type SpikeAdminController struct{}

// NewSpikeAdminController returns a controller. No state of its own; the
// settings store is reached via the services storage seam.
func NewSpikeAdminController() *SpikeAdminController {
	return &SpikeAdminController{}
}

// Register attaches the endpoints under /api/admin/agent.
//
//	GET /api/admin/agent/spike-settings   current global spike baseline mode
//	PUT /api/admin/agent/spike-settings   update the global spike baseline mode
func (sc *SpikeAdminController) Register(router fiber.Router) {
	g := router.Group("/admin/agent", sc.authMiddleware)
	g.Get("/spike-settings", sc.getSettings)
	g.Put("/spike-settings", sc.putSettings)
}

// authMiddleware reuses the agent gateway secret (constant-time compare),
// mirroring the rest of the admin surface.
func (sc *SpikeAdminController) authMiddleware(c *fiber.Ctx) error {
	if middleware.RequestAuthorized(c) {
		return c.Next()
	}
	cfg := config.GetConfig()
	expected := cfg.GatewaySecret
	got := c.Get("X-Gateway-Secret")
	if expected == "" || !secureEqual(got, expected) {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return c.Next()
}

// getSettings returns the current global spike baseline mode (or the built-in
// default when none is stored).
func (sc *SpikeAdminController) getSettings(c *fiber.Ctx) error {
	return c.JSON(agent.LoadSpikeSettings(services.Storage()))
}

// putSettings persists the updated global spike baseline mode. An unknown mode
// is rejected with a 400 (the store would otherwise fold it to the default);
// only the three recognized modes are accepted.
func (sc *SpikeAdminController) putSettings(c *fiber.Ctx) error {
	var s agent.SpikeSettings
	if err := c.BodyParser(&s); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid settings body"})
	}
	// Copy the string off the pooled request buffer before it outlives the
	// request.
	mode := strings.Clone(strings.TrimSpace(s.BaselineMode))
	if !agent.KnownBaselineMode(mode) {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid baseline mode (want default|average|time_of_day)"})
	}
	s.BaselineMode = mode
	if err := agent.SaveSpikeSettings(services.Storage(), s); err != nil {
		if errors.Is(err, agent.ErrSpikeNoStorage) {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "storage not configured"})
		}
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}
	// Return the effective (sanitized) settings after the write.
	return c.JSON(agent.LoadSpikeSettings(services.Storage()))
}
