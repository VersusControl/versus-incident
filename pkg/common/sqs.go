package common

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
)

type SQSListener struct {
	QueueURL string
}

func NewSQSListener(cfg config.SQSConfig) *SQSListener {
	return &SQSListener{
		QueueURL: cfg.QueueURL,
	}
}

func (s *SQSListener) StartListening(handler func(content *map[string]interface{}) error) error {
	return fmt.Errorf("SQS not implemented")
}
