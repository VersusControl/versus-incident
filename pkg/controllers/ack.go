package controllers

import (
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/gofiber/fiber/v2"
)

func HandleAck(c *fiber.Ctx) error {
	incidentID := c.Params("incidentID")

	if err := core.GetOnCallWorkflow().Ack(incidentID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "success"})
}
