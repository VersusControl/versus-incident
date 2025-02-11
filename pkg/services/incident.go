package services

import (
	"fmt"
	"versus-incident/pkg/common"
	"versus-incident/pkg/core"
	m "versus-incident/pkg/models"
)

func CreateIncident(teamID string, content interface{}, params ...*map[string]string) error {
	var cfg *common.Config

	if len(params) > 0 {
		cfg = common.GetConfigWitParamsOverwrite(params[0])
	} else {
		cfg = common.GetConfig()
	}

	incident := m.NewIncident(teamID, content)

	factory := common.NewProviderFactory(cfg)
	providers, err := factory.CreateProviders()
	if err != nil {
		return fmt.Errorf("failed to create providers: %v", err)
	}

	alert := core.NewAlert(providers...)

	return alert.SendAlert(incident)
}
