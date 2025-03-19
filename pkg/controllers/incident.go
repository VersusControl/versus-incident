package controllers

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

func CreateIncident(c *fiber.Ctx) error {
	cfg := config.GetConfig()

	if cfg.Alert.DebugBody {
		rawBody := c.Body()

		// Log the raw request body for debugging purposes
		fmt.Println("Raw Request Body:", string(rawBody))
	}

	body := &map[string]interface{}{}

	if err := c.BodyParser(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid input"})
	}

	var err error

	// If query parameters exist, get the value to overwrite the default configuration
	if len(c.Queries()) > 0 {
		overwriteVaule := c.Queries()
		err = services.CreateIncident("", body, &overwriteVaule)
	} else {
		err = services.CreateIncident("", body)
	}

	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
	}

	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "Incident created"})
}
