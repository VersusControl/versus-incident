package controllers

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"versus-incident/pkg/common"
	"versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

type SNSMessage struct {
	Type             string `json:"Type"`
	MessageId        string `json:"MessageId"`
	Token            string `json:"Token,omitempty"` // Omit empty for Notification type
	TopicArn         string `json:"TopicArn"`
	Message          string `json:"Message"`
	SubscribeURL     string `json:"SubscribeURL,omitempty"` // Omit empty for Notification type
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
}

func SNS(c *fiber.Ctx) error {
	cfg := common.GetConfig()

	var msg SNSMessage

	rawBody := c.Body()

	if err := json.Unmarshal(rawBody, &msg); err != nil {
		return c.Status(400).SendString("Invalid SNS message: " + err.Error())
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
			if cfg.Queue.DebugBody {
				// Log the raw queue message for debugging purposes
				fmt.Println("Queue Message:", msg.Message)
			}

			content := &map[string]interface{}{}

			if err := json.Unmarshal([]byte(msg.Message), content); err != nil {
				return c.Status(400).SendString("Invalid message content")
			}

			// If query parameters exist, get the value to overwrite the default configuration
			var err error

			if len(c.Queries()) > 0 {
				overwriteVaule := c.Queries()
				err = services.CreateIncident("", content, &overwriteVaule)
			} else {
				err = services.CreateIncident("", content)
			}

			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
			}

			return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "Incident created"})
		}
	}

	return c.SendStatus(200)
}
