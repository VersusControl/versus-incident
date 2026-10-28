package controllers

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/services"

	"github.com/gofiber/fiber/v2"
)

type SNSMessage struct {
	Type             string `json:"Type"`
	MessageId        string `json:"MessageId"`
	Token            string `json:"Token,omitempty"` // Omit empty for Notification type
	TopicArn         string `json:"TopicArn"`
	Subject          string `json:"Subject,omitempty"` // Notification only, optional
	Message          string `json:"Message"`
	SubscribeURL     string `json:"SubscribeURL,omitempty"` // Omit empty for Notification type
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
}

func SNS(c *fiber.Ctx) error {
	cfg := config.GetConfig()

	var msg SNSMessage

	rawBody := c.Body()

	if err := decodeSNSMessage(rawBody, &msg); err != nil {
		// Quoted: the body is third-party content, so an embedded newline would
		// forge a log line. Only the reason is recorded — never a URL, a
		// signature, or a payload read out of the body.
		log.Printf("SNS message rejected: reason=%q", err.Error())
		return c.Status(fiber.StatusBadRequest).SendString("SNS message rejected")
	}

	// Anyone with an AWS account can have Amazon sign a message, so a valid
	// signature proves "some SNS topic", not "our SNS topic". Without the pin an
	// attacker's own topic is accepted here, and its SubscriptionConfirmation
	// would subscribe this endpoint to it for good. TopicArn is covered by the
	// signature and read from the same parse the canonical string uses, so
	// checking it first is sound — and it keeps an unverified body from costing
	// an outbound certificate fetch.
	if want := cfg.Queue.SNS.TopicARN; want == "" || msg.TopicArn != want {
		log.Printf("SNS message rejected: type=%q reason=%q", msg.Type, "topic arn is not the configured topic")
		return c.Status(fiber.StatusBadRequest).SendString("SNS message rejected")
	}

	// The endpoint is unauthenticated, so nothing in the body is trusted until
	// the RSA signature over its canonical form verifies against an
	// Amazon-served certificate. Fail closed: an unverified body is neither
	// confirmed nor turned into an incident.
	if err := verifySNSMessage(rawBody, &msg); err != nil {
		log.Printf("SNS message rejected: type=%q reason=%q", msg.Type, err.Error())
		return c.Status(fiber.StatusBadRequest).SendString("SNS message rejected")
	}

	switch msg.Type {
	case "SubscriptionConfirmation":
		{
			if err := confirmSNSSubscription(c.UserContext(), msg.SubscribeURL); err != nil {
				log.Printf("SNS subscription confirmation refused: reason=%q", err.Error())
				return c.Status(fiber.StatusBadRequest).SendString("SNS subscription confirmation refused")
			}

			log.Println("SNS subscription confirmed")
		}

	case "Notification":
		{
			if cfg.Queue.DebugBody {
				// Log the raw queue message for debugging purposes, quoted: the
				// message is third-party content, so an embedded newline would
				// forge a log line.
				fmt.Printf("Queue Message: %q\n", msg.Message)
			}

			content := &map[string]interface{}{}

			if err := json.Unmarshal([]byte(msg.Message), content); err != nil {
				return c.Status(400).SendString("Invalid message content")
			}

			// If query parameters exist, get the value to overwrite the default configuration
			var err error

			if len(c.Queries()) > 0 {
				overwriteVaule := c.Queries()
				overwriteVaule["incident_source"] = "sns"
				err = services.CreateIncident("", content, &overwriteVaule)
			} else {
				overwriteVaule := map[string]string{"incident_source": "sns"}
				err = services.CreateIncident("", content, &overwriteVaule)
			}

			if err != nil {
				return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": err.Error()})
			}

			return c.Status(fiber.StatusCreated).JSON(fiber.Map{"status": "Incident created"})
		}
	}

	return c.SendStatus(200)
}
