package common

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type AzBusListener struct{}

func NewAzBusListener() (core.QueueListener, error) {
	return nil, fmt.Errorf("AZURE Service Bus not implemented")
}

func (l *AzBusListener) StartListening(handler func(content map[string]interface{}) error) error {
	return fmt.Errorf("AZURE Service Bus not implemented")
}
