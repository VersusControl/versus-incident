package controllers

import (
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
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

	store := services.Storage()
	if checker, ok := store.(storage.IncidentMutationChecker); ok {
		if err := checker.CheckIncidentMutable(incidentID); errors.Is(err, storage.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "incident not found"})
		} else if errors.Is(err, storage.ErrReadOnlyIncident) {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": err.Error()})
		} else if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}
	}

	if err := core.GetOnCallWorkflow().Ack(incidentID); err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	// Stamp the persisted incident as acknowledged.
	if store != nil {
		if err := store.UpdateIncidentAck(incidentID, time.Now().UTC()); err != nil {
			log.Printf("ack persistence warning: incident_id=%q error=%v", incidentID, err)
		}
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "success"})
}
