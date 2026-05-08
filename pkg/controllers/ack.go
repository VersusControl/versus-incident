package controllers

import (
	"log"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/gofiber/fiber/v2"
)

func HandleAck(c *fiber.Ctx) error {
	incidentID := c.Params("incidentID")

	if err := core.GetOnCallWorkflow().Ack(incidentID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	// Stamp the persisted incident as acknowledged. Non-fatal: ack still
	// succeeds even when storage isn't configured.
	if store := services.Storage(); store != nil {
		if err := store.UpdateIncidentAck(incidentID, time.Now().UTC()); err != nil {
			log.Printf("ack: persist warning: %v", err)
		}
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "success"})
}
