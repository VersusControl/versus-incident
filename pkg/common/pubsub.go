package common

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/core"
)

type PubSubListener struct{}

func NewPubSubListener() (core.QueueListener, error) {
	return nil, fmt.Errorf("GCP Pub/Sub not implemented")
}

func (l *PubSubListener) StartListening(handler func(content map[string]interface{}) error) error {
	return fmt.Errorf("GCP Pub/Sub not implemented")
}
