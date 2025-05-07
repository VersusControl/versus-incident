package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// PagerDuty API v2 payload structures
type PagerDutyEvent struct {
	RoutingKey  string                `json:"routing_key"`
	EventAction string                `json:"event_action"`
	Payload     PagerDutyEventPayload `json:"payload"`
}

type PagerDutyEventPayload struct {
	Summary       string            `json:"summary"`
	Source        string            `json:"source"`
	Severity      string            `json:"severity"`
	CustomDetails map[string]string `json:"custom_details,omitempty"`
}

// PagerDutyProvider implements the OnCallProvider interface for PagerDuty
type PagerDutyProvider struct {
	routingKey string
	httpClient *http.Client
}

// NewPagerDutyProvider creates a new PagerDuty provider
func NewPagerDutyProvider(routingKey string) *PagerDutyProvider {
	return &PagerDutyProvider{
		routingKey: routingKey,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// TriggerOnCall creates an incident in PagerDuty using Events API v2
func (p *PagerDutyProvider) TriggerOnCall(ctx context.Context, incidentID string, cfg *config.OnCallConfig) error {
	// Use the override config if provided, otherwise use the default
	routingKey := p.routingKey
	if cfg != nil && cfg.PagerDuty.RoutingKey != "" {
		routingKey = cfg.PagerDuty.RoutingKey
	}

	event := PagerDutyEvent{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		Payload: PagerDutyEventPayload{
			Summary:  "Incident " + incidentID,
			Source:   "Versus Incident",
			Severity: "critical",
			CustomDetails: map[string]string{
				"incident_id": incidentID,
			},
		},
	}

	// Convert event to JSON
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal PagerDuty event: %v", err)
	}

	// Create and send HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://events.pagerduty.com/v2/enqueue", bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create PagerDuty request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send PagerDuty request: %v", err)
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("PagerDuty API returned non-success status: %d", resp.StatusCode)
	}

	log.Printf("PagerDuty incident escalated: %s", incidentID)
	return nil
}
