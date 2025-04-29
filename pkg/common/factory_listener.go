package common

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// Listener Factory
type ListenerFactory struct {
	cfg *config.Config
}

func NewListenerFactory(cfg *config.Config) *ListenerFactory {
	return &ListenerFactory{cfg: cfg}
}

func (f *ListenerFactory) CreateListeners() ([]core.QueueListener, error) {
	var listeners []core.QueueListener

	if f.cfg.Queue.SNS.Enable {
		snsListener, err := f.createSNSListener()
		if err != nil {
			return nil, fmt.Errorf("failed to create SNS listener: %w", err)
		}
		listeners = append(listeners, snsListener)
	}

	if f.cfg.Queue.SQS.Enable {
		sqsListener, err := f.createSQSListener()
		if err != nil {
			return nil, fmt.Errorf("failed to create SQS listener: %w", err)
		}
		listeners = append(listeners, sqsListener)
	}

	if f.cfg.Queue.PubSub.Enable {
		return nil, fmt.Errorf("GCP Pub/Sub listener not implemented")
	}

	if f.cfg.Queue.AzBus.Enable {
		return nil, fmt.Errorf("AZURE Service Bus listener not implemented")
	}

	return listeners, nil
}

func (f *ListenerFactory) createSNSListener() (core.QueueListener, error) {
	sc := f.cfg.Queue.SNS
	if sc.EndpointPath == "" {
		return nil, fmt.Errorf("missing SNS endpoint path configuration")
	}

	// If the user configures an HTTPS endpoint, then an SNS subscription will be automatically created
	autoCreateSubscription := (f.cfg.Queue.SNS.Endpoint != "")

	if autoCreateSubscription && sc.TopicARN == "" {
		return nil, fmt.Errorf("missing SNS topic ARN configuration")
	}

	endpointURL := f.cfg.Queue.SNS.Endpoint + f.cfg.Queue.SNS.EndpointPath
	fmt.Printf("SNS endpoint subscription %s\n", endpointURL)

	return NewSNSListener(config.SNSConfig{
		TopicARN: sc.TopicARN,
	}, endpointURL, autoCreateSubscription), nil
}

func (f *ListenerFactory) createSQSListener() (core.QueueListener, error) {
	sc := f.cfg.Queue.SQS
	if sc.QueueURL == "" {
		return nil, fmt.Errorf("missing required SQS configuration")
	}

	return NewSQSListener(config.SQSConfig{
		Enable:   sc.Enable,
		QueueURL: sc.QueueURL,
	}), nil
}
