package common

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// ServiceNowRecord is the JSON payload posted to the ServiceNow Table API.
// incidentID is mapped onto both the human-readable short_description and the
// correlation_id used by ServiceNow to de-duplicate inbound events.
type ServiceNowRecord struct {
	ShortDescription string `json:"short_description"`
	CorrelationID    string `json:"correlation_id"`
}

// ServiceNowProvider implements the OnCallProvider interface for ServiceNow.
type ServiceNowProvider struct {
	instanceURL string
	username    string
	password    string
	table       string
	httpClient  *http.Client
}

// NewServiceNowProvider creates a new ServiceNow provider. An empty table
// defaults to "incident".
func NewServiceNowProvider(instanceURL, username, password, table string) *ServiceNowProvider {
	if table == "" {
		table = "incident"
	}

	return &ServiceNowProvider{
		instanceURL: instanceURL,
		username:    username,
		password:    password,
		table:       table,
		// Use the shared HTTP client (TLS verification on, no proxy).
		httpClient: utils.CreateHTTPClient(config.ProxyConfig{}, false),
	}
}

// TriggerOnCall creates a record in ServiceNow using the Table API.
func (p *ServiceNowProvider) TriggerOnCall(ctx context.Context, incidentID string, cfg *config.OnCallConfig) error {
	// Use the override config if provided, otherwise use the defaults.
	instanceURL := p.instanceURL
	username := p.username
	password := p.password
	table := p.table

	if cfg != nil {
		if cfg.ServiceNow.InstanceURL != "" {
			instanceURL = cfg.ServiceNow.InstanceURL
		}
		if cfg.ServiceNow.Username != "" {
			username = cfg.ServiceNow.Username
		}
		if cfg.ServiceNow.Password != "" {
			password = cfg.ServiceNow.Password
		}
		if cfg.ServiceNow.Table != "" {
			table = cfg.ServiceNow.Table
		}
	}

	if table == "" {
		table = "incident"
	}

	record := ServiceNowRecord{
		ShortDescription: "Incident " + incidentID,
		CorrelationID:    incidentID,
	}

	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal ServiceNow record: %v", err)
	}

	url := fmt.Sprintf("%s/api/now/table/%s", strings.TrimRight(instanceURL, "/"), table)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create ServiceNow request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(username, password)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send ServiceNow request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ServiceNow API returned non-success status: %d", resp.StatusCode)
	}

	log.Printf("ServiceNow record created for incident: %s", incidentID)
	return nil
}
