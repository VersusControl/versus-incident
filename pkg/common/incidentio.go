package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// incidentioAlertEventsBaseURL is the base URL of the incident.io HTTP alert
// events endpoint. The alert source config ID is appended as the final path
// segment.
const incidentioAlertEventsBaseURL = "https://api.incident.io/v2/alert_events/http"

// IncidentioAlertEvent is the JSON payload posted to the incident.io HTTP alert
// events endpoint.
type IncidentioAlertEvent struct {
	Title            string            `json:"title"`
	Description      string            `json:"description,omitempty"`
	DeduplicationKey string            `json:"deduplication_key"`
	Status           string            `json:"status"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// IncidentioProvider implements the OnCallProvider interface for incident.io.
type IncidentioProvider struct {
	apiKey              string
	alertSourceConfigID string
	baseURL             string
	httpClient          *http.Client
}

// NewIncidentioProvider creates a new incident.io provider.
func NewIncidentioProvider(apiKey, alertSourceConfigID string) *IncidentioProvider {
	return &IncidentioProvider{
		apiKey:              apiKey,
		alertSourceConfigID: alertSourceConfigID,
		baseURL:             incidentioAlertEventsBaseURL,
		// Use the shared HTTP client (TLS verification on, no proxy).
		httpClient: utils.CreateHTTPClient(config.ProxyConfig{}, false),
	}
}

// TriggerOnCall creates an alert in incident.io using the HTTP alert events API.
func (p *IncidentioProvider) TriggerOnCall(ctx context.Context, incidentID string, cfg *config.OnCallConfig) error {
	// Use the override config if provided, otherwise use the defaults.
	apiKey := p.apiKey
	alertSourceConfigID := p.alertSourceConfigID

	if cfg != nil {
		if cfg.Incidentio.APIKey != "" {
			apiKey = cfg.Incidentio.APIKey
		}
		if cfg.Incidentio.AlertSourceConfigID != "" {
			alertSourceConfigID = cfg.Incidentio.AlertSourceConfigID
		}
	}

	event := IncidentioAlertEvent{
		Title:            "Incident " + incidentID,
		Description:      "Escalated by Versus Incident",
		DeduplicationKey: incidentID,
		Status:           "firing",
		Metadata: map[string]string{
			"incident_id": incidentID,
		},
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal incident.io alert event: %v", err)
	}

	baseURL := p.baseURL
	if baseURL == "" {
		baseURL = incidentioAlertEventsBaseURL
	}
	url := fmt.Sprintf("%s/%s", baseURL, alertSourceConfigID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create incident.io request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send incident.io request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("incident.io API returned non-success status: %d", resp.StatusCode)
	}

	log.Printf("incident.io alert created for incident: %s", incidentID)
	return nil
}
