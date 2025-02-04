package services

import (
	"fmt"
	"versus-incident/pkg/common"
	"versus-incident/pkg/core"
	m "versus-incident/pkg/models"
)

func CreateIncident(teamID string, content interface{}) error {
	incident := m.NewIncident(teamID, content)
	cfg := common.GetConfig()

	factory := common.NewProviderFactory(cfg)
	providers, err := factory.CreateProviders()
	if err != nil {
		return fmt.Errorf("failed to create providers: %v", err)
	}

	alert := core.NewAlert(providers...)

	return alert.SendAlert(incident)
}
