package controllers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

func SNS(c *fiber.Ctx) error {
	var msg struct {
		Type         string `json:"Type"`
		Message      string `json:"Message"`
		SubscribeURL string `json:"SubscribeURL"`
	}

	if err := c.BodyParser(&msg); err != nil {
		return c.Status(400).SendString("Invalid SNS message")
	}

	switch msg.Type {
	case "SubscriptionConfirmation":
		{
			resp, err := http.Get(msg.SubscribeURL)

			if err != nil {
				return fmt.Errorf("subscription confirmation failed: %w", err)
			}
			defer resp.Body.Close()

			log.Println("SNS subscription confirmed")
		}

	case "Notification":
		{
			content := &map[string]interface{}{}

			if err := json.Unmarshal([]byte(msg.Message), content); err != nil {
				return c.Status(400).SendString("Invalid message content")
			}

			return services.CreateIncident("", content) // teamID as empty string
		}
	}

	return c.SendStatus(200)
}
