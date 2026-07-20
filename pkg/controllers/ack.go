package controllers

import (
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/gofiber/fiber/v2"
)

func HandleAck(c *fiber.Ctx) error {
	incidentID := c.Params("incidentID")

	// The ack link must carry a signed.
	exp, _ := strconv.ParseInt(c.Query("exp"), 10, 64)
	if err := services.VerifyAckToken(services.AckSigningKey(), incidentID, exp, c.Query("sig"), time.Now()); err != nil {
		if errors.Is(err, services.ErrAckTokenExpired) {
			return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "ack link expired"})
		}
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid ack link"})
	}

	// On-call is disabled by default. Skip the singleton entirely when it
	// isn't initialized so an unauthenticated request can't panic (and take
	// down) the process.
	if !core.IsOnCallWorkflowInitialized() {
		return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "on-call is not enabled"})
	}

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
