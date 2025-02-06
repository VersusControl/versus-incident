package common

import (
	"fmt"
)

type SQSListener struct {
	QueueURL string
}

func NewSQSListener(cfg SQSConfig) *SQSListener {
	return &SQSListener{
		QueueURL: cfg.QueueURL,
	}
}

func (s *SQSListener) StartListening(handler func(content map[string]interface{}) error) error {
	return fmt.Errorf("SQS not implemented")
}
