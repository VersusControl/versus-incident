package core

import (
	m "versus-incident/pkg/models"
)

// AlertProvider interface remains the same
type AlertProvider interface {
	SendAlert(incident *m.Incident) error
}

type Alert struct {
	providers []AlertProvider
}

func NewAlert(providers ...AlertProvider) *Alert {
	return &Alert{providers: providers}
}

func (a *Alert) SendAlert(incident *m.Incident) error {
	for _, provider := range a.providers {
		if err := provider.SendAlert(incident); err != nil {
			return err
		}
	}
	return nil
}
