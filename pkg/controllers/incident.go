package controllers

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

func CreateIncident(c *fiber.Ctx) error {
	cfg := config.GetConfig()
	raw := c.Body()

	if cfg.Alert.DebugBody {
		// Log the raw request body for debugging purposes
		fmt.Println("Raw Request Body:", string(raw))
	}

	// Detect if payload is an array; otherwise treat as single JSON object
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Empty body"})
	}

	// Handle JSON array
	if trimmed[0] == '[' {
		var records []map[string]interface{}
		if err := json.Unmarshal(raw, &records); err != nil {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid JSON array input"})
		}
		if len(records) == 0 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "No incidents found in array"})
		}

		var err error
		if len(c.Queries()) > 0 {
			overwriteVaule := c.Queries()
			for _, record := range records {
				// capture pointer per iteration
				rec := record
				if err = services.CreateIncident("", &rec, &overwriteVaule); err != nil {
					break
				}
			}
		} else {
			for _, record := range records {
				rec := record
				if err = services.CreateIncident("", &rec); err != nil {
					break
				}
			}
		}

		if err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
		}

		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"status": "Incidents created",
			"count":  len(records),
		})
	}

	body := &map[string]interface{}{}
	if err := c.BodyParser(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid input"})
	}

	var err error
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
