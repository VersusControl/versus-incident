package models

import "github.com/google/uuid"

type Incident struct {
	ID       string                  `json:"id"`
	TeamID   string                  `json:"team_id"`
	Content  *map[string]interface{} `json:"content"`
	Resolved bool
	// DeliveryID names this notification across every attempt to send it,
	// when the sender knows one. Webhook channels send it as the
	// Idempotency-Key header so a receiver can drop a repeat.
	DeliveryID string `json:"-"`
}

func NewIncident(teamID string, content *map[string]interface{}, resolved bool) *Incident {
	return &Incident{
		ID:       uuid.NewString(), // Generate a new UUID as a string
		TeamID:   teamID,
		Content:  content,
		Resolved: resolved,
	}
}
