package common

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// Alert Provider
type ProviderFactory struct {
	cfg *config.Config
}

func NewProviderFactory(cfg *config.Config) *ProviderFactory {
	return &ProviderFactory{cfg: cfg}
}

func (f *ProviderFactory) CreateProviders() ([]core.AlertProvider, error) {
	var providers []core.AlertProvider

	if f.cfg.Alert.Slack.Enable {
		slackProvider, err := f.createSlackProvider()
		if err != nil {
			return nil, fmt.Errorf("failed to create Slack provider: %w", err)
		}
		providers = append(providers, slackProvider)
	}

	if f.cfg.Alert.Telegram.Enable {
		telegramProvider, err := f.createTelegramProvider()
		if err != nil {
			return nil, fmt.Errorf("failed to create Telegram provider: %w", err)
		}
		providers = append(providers, telegramProvider)
	}

	if f.cfg.Alert.Email.Enable {
		emailProvider, err := f.createEmailProvider()
		if err != nil {
			return nil, fmt.Errorf("failed to create Email provider: %w", err)
		}
		providers = append(providers, emailProvider)
	}

	if f.cfg.Alert.MSTeams.Enable {
		msteamsProvider, err := f.createMSTeamsProvider()
		if err != nil {
			return nil, fmt.Errorf("failed to create MS Teams provider: %w", err)
		}
		providers = append(providers, msteamsProvider)
	}

	return providers, nil
}

func (f *ProviderFactory) createSlackProvider() (core.AlertProvider, error) {
	sc := f.cfg.Alert.Slack
	if sc.Token == "" || sc.ChannelID == "" || sc.TemplatePath == "" {
		return nil, fmt.Errorf("missing required Slack configuration")
	}

	return NewSlackProvider(config.SlackConfig{
		Token:        sc.Token,
		ChannelID:    sc.ChannelID,
		TemplatePath: sc.TemplatePath,
	}), nil
}

func (f *ProviderFactory) createTelegramProvider() (core.AlertProvider, error) {
	tc := f.cfg.Alert.Telegram
	if tc.BotToken == "" || tc.ChatID == "" || tc.TemplatePath == "" {
		return nil, fmt.Errorf("missing required Telegram configuration")
	}

	return NewTelegramProvider(config.TelegramConfig{
		BotToken:     tc.BotToken,
		ChatID:       tc.ChatID,
		TemplatePath: tc.TemplatePath,
	}), nil
}

func (f *ProviderFactory) createEmailProvider() (core.AlertProvider, error) {
	ec := f.cfg.Alert.Email
	if ec.SMTPHost == "" || ec.Username == "" || ec.Password == "" || ec.To == "" || ec.TemplatePath == "" {
		return nil, fmt.Errorf("missing required Email configuration")
	}

	return NewEmailProvider(config.EmailConfig{
		SMTPHost:     ec.SMTPHost,
		SMTPPort:     ec.SMTPPort,
		Username:     ec.Username,
		Password:     ec.Password,
		To:           ec.To,
		Subject:      ec.Subject,
		TemplatePath: ec.TemplatePath,
	}), nil
}

func (f *ProviderFactory) createMSTeamsProvider() (core.AlertProvider, error) {
	msc := f.cfg.Alert.MSTeams
	// Check that Power Automate URL and template path are provided
	if msc.PowerAutomateURL == "" || msc.TemplatePath == "" {
		return nil, fmt.Errorf("missing required MS Teams configuration: need power_automate_url and template_path")
	}

	return NewMSTeamsProvider(config.MSTeamsConfig{
		PowerAutomateURL: msc.PowerAutomateURL,
		TemplatePath:     msc.TemplatePath,
	}), nil
}

// Listener
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
