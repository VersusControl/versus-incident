package services

import (
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

func CreateIncident(teamID string, content *map[string]interface{}, params ...*map[string]string) error {
	var cfg *config.Config

	if len(params) > 0 {
		cfg = config.GetConfigWitParamsOverwrite(params[0])
	} else {
		cfg = config.GetConfig()
	}

	// Initialization of providers and alert
	factory := common.NewAlertProviderFactory(cfg)
	providers, err := factory.CreateProviders()
	if err != nil {
		return fmt.Errorf("failed to create providers: %v", err)
	}

	alert := core.NewAlert(providers...)

	// Skip AckURL and On-Call if resolved alert
	resolved := isResolved(*content)

	incident := m.NewIncident(teamID, content, resolved)

	// Dereference the Pointer and add AckURL if needed
	contentClone := make(map[string]interface{})
	for k, v := range *content {
		contentClone[k] = v
	}

	if !resolved && cfg.OnCall.Enable {
		ackURL := fmt.Sprintf("%s/api/ack/%s", cfg.PublicHost, incident.ID)
		contentClone["AckURL"] = ackURL

		incident.Content = &contentClone
	}

	if err := alert.SendAlert(incident); err != nil {
		return err
	}

	if !resolved && cfg.OnCall.Enable {
		workflow := core.GetOnCallWorkflow()
		if err := workflow.Start(incident.ID, cfg.OnCall); err != nil {
			return err
		}
	}

	return nil
}

// isResolved checks if the alert is resolved by checking common status fields
func isResolved(content map[string]interface{}) bool {
	// List of common field names that might indicate status
	statusFields := []string{"status", "state", "alertState"}

	for _, field := range statusFields {
		if val, ok := content[field]; ok {
			if strVal, isString := val.(string); isString {
				// Case-insensitive check for "resolved"
				return strings.EqualFold(strVal, "resolved")
			}
		}
	}

	// Not resolved (trigger On-Call)
	return false
}
