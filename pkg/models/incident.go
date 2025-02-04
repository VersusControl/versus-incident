package models

import "github.com/google/uuid"

type Incident struct {
	ID           string      `json:"id"`
	TeamID       string      `json:"team_id"`
	Content      interface{} `json:"content"`
	Acknowledged bool        `json:"acknowledged"`
}

func NewIncident(teamID string, content interface{}) *Incident {
	return &Incident{
		ID:           uuid.NewString(), // Generate a new UUID as a string
		TeamID:       teamID,
		Content:      content,
		Acknowledged: false, // Default to not acknowledged
	}
}
