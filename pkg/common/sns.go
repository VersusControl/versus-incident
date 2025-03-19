package common

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	c "github.com/VersusControl/versus-incident/pkg/config"
)

type SNSListener struct {
	topicARN               string
	endpointURL            string
	autoCreateSubscription bool
}

func NewSNSListener(cfg c.SNSConfig, endpointURL string, autoCreateSubscription bool) *SNSListener {
	return &SNSListener{
		topicARN:               cfg.TopicARN,
		endpointURL:            endpointURL,
		autoCreateSubscription: autoCreateSubscription,
	}
}

func (l *SNSListener) StartListening(handler func(content *map[string]interface{}) error) error {
	if l.autoCreateSubscription {
		ctx := context.Background()
		awsCfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return fmt.Errorf("failed to load AWS config: %w", err)
		}

		client := sns.NewFromConfig(awsCfg)
		_, err = client.Subscribe(ctx, &sns.SubscribeInput{
			Protocol: aws.String("https"),
			TopicArn: aws.String(l.topicARN),
			Endpoint: aws.String(l.endpointURL),
		})

		if err != nil {
			return fmt.Errorf("SNS subscription failed: %w", err)
		}
	}

	return nil
}
