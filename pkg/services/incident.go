package services

import (
	"fmt"

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

	// Dereference the Pointer and add AckURL if needed
	contentClone := make(map[string]interface{})
	for k, v := range *content {
		contentClone[k] = v
	}

	incident := m.NewIncident(teamID, content)

	factory := common.NewProviderFactory(cfg)
	providers, err := factory.CreateProviders()
	if err != nil {
		return fmt.Errorf("failed to create providers: %v", err)
	}

	alert := core.NewAlert(providers...)

	if cfg.OnCall.Enable {
		ackURL := fmt.Sprintf("http://%s/ack?incident=%s", cfg.PublicHost, incident.ID)
		contentClone["AckURL"] = ackURL

		incident.Content = contentClone
	}

	if err := alert.SendAlert(incident); err != nil {
		return err
	}

	if cfg.OnCall.Enable {
		workflow := core.GetOnCallWorkflow()

		if err := workflow.Start(incident.ID, cfg.OnCall.AwsIncidentManager); err != nil {
			return err
		}
	}

	return nil
}
