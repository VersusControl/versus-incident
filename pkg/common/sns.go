package common

import (
	"fmt"

	"versus-incident/pkg/core"
)

type SNSListener struct{}

func NewSNSListener() (core.QueueListener, error) {
	return nil, fmt.Errorf("SNS not implemented")
}

func (l *SNSListener) StartListening(handler func(content map[string]interface{}) error) error {
	return fmt.Errorf("SNS not implemented")
}
