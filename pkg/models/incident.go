package models

import "github.com/google/uuid"

type Incident struct {
	ID       string                  `json:"id"`
	TeamID   string                  `json:"team_id"`
	Content  *map[string]interface{} `json:"content"`
	Resolved bool
}

func NewIncident(teamID string, content *map[string]interface{}, resolved bool) *Incident {
	return &Incident{
		ID:       uuid.NewString(), // Generate a new UUID as a string
		TeamID:   teamID,
		Content:  content,
		Resolved: resolved,
	}
}
