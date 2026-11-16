package controllers

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/utils"

	"github.com/gofiber/fiber/v2"
)

func CreateIncident(c *fiber.Ctx) error {
	cfg := config.GetConfig()
	raw := c.Body()

	if cfg.Alert.DebugBody {
		// Log the raw request body for debugging purposes, quoted: the body is
		// caller-controlled, so an embedded newline would forge a log line.
		fmt.Printf("Raw Request Body: %q\n", raw)
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

		var overwrite *map[string]string
		if len(c.Queries()) > 0 {
			overwriteVaule := c.Queries()
			delete(overwriteVaule, "incident_source") // reserved: ingress-only, not client-settable
			overwrite = &overwriteVaule
		}
		results := make([]fiber.Map, 0, len(records))
		for _, record := range records {
			// capture pointer per iteration
			rec := record
			outcome := ingest(&rec, overwrite)
			if outcome.Err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": utils.SafeErrorText(outcome.Err)})
			}
			results = append(results, intakeOutcomeBody(outcome))
		}

		return c.Status(fiber.StatusCreated).JSON(fiber.Map{
			"status":    "Incidents created",
			"count":     len(records),
			"incidents": results,
		})
	}

	body := &map[string]interface{}{}
	if err := c.BodyParser(body); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "Invalid input"})
	}

	var overwrite *map[string]string
	if len(c.Queries()) > 0 {
		overwriteVaule := c.Queries()
		delete(overwriteVaule, "incident_source") // reserved: ingress-only, not client-settable
		overwrite = &overwriteVaule
	}

	// Once the incident is stored the answer is 201 even when a channel or
	// on-call failed: the failure is recorded on the incident and returned
	// here, and a sender that retries on 5xx would otherwise store one new
	// incident per retry. Only an incident that was not stored answers 500.
	outcome := ingest(body, overwrite)
	if outcome.Err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": utils.SafeErrorText(outcome.Err)})
	}

	response := intakeOutcomeBody(outcome)
	response["status"] = "Incident created"
	return c.Status(fiber.StatusCreated).JSON(response)
}

func ingest(content *map[string]interface{}, overwrite *map[string]string) services.IntakeOutcome {
	if overwrite != nil {
		return services.IngestIncident("", content, overwrite)
	}
	return services.IngestIncident("", content)
}

// intakeOutcomeBody reports the stored incident and its delivery result.
func intakeOutcomeBody(outcome services.IntakeOutcome) fiber.Map {
	body := fiber.Map{}
	if outcome.IncidentID != "" && outcome.Stored {
		body["incident_id"] = outcome.IncidentID
	}
	if outcome.NotifyStatus != "" {
		body["notify_status"] = outcome.NotifyStatus
	}
	if outcome.NotifyError != "" {
		body["notify_error"] = utils.RedactSecrets(outcome.NotifyError)
	}
	if outcome.OnCallError != "" {
		body["oncall_error"] = utils.RedactSecrets(outcome.OnCallError)
	}
	return body
}
