package controllers

import (
	"versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

func CreateIncident(c *fiber.Ctx) error {
	body := &map[string]interface{}{}

	if err := c.BodyParser(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid input"})
	}

	err := services.CreateIncident("", body)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "Incident created"})
}

func CreateIncidentWithTeam(c *fiber.Ctx) error {
	id := c.Params("id")
	body := &map[string]interface{}{}

	if err := c.BodyParser(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid input"})
	}

	err := services.CreateIncident(id, body)
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "Incident created"})
}
