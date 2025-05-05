package common

import (
	"fmt"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// Alert Provider
type AlertProviderFactory struct {
	cfg *config.Config
}

func NewAlertProviderFactory(cfg *config.Config) *AlertProviderFactory {
	return &AlertProviderFactory{cfg: cfg}
}

func (f *AlertProviderFactory) CreateProviders() ([]core.AlertProvider, error) {
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

	if f.cfg.Alert.Lark.Enable {
		larkProvider, err := f.createLarkProvider()
		if err != nil {
			return nil, fmt.Errorf("failed to create Lark provider: %w", err)
		}
		providers = append(providers, larkProvider)
	}

	return providers, nil
}

func (f *AlertProviderFactory) createSlackProvider() (core.AlertProvider, error) {
	sc := f.cfg.Alert.Slack
	if sc.Token == "" || sc.ChannelID == "" || sc.TemplatePath == "" {
		return nil, fmt.Errorf("missing required Slack configuration")
	}

	return NewSlackProvider(config.SlackConfig{
		Token:             sc.Token,
		ChannelID:         sc.ChannelID,
		TemplatePath:      sc.TemplatePath,
		MessageProperties: sc.MessageProperties,
	}), nil
}

func (f *AlertProviderFactory) createTelegramProvider() (core.AlertProvider, error) {
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

func (f *AlertProviderFactory) createEmailProvider() (core.AlertProvider, error) {
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

func (f *AlertProviderFactory) createMSTeamsProvider() (core.AlertProvider, error) {
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

func (f *AlertProviderFactory) createLarkProvider() (core.AlertProvider, error) {
	lc := f.cfg.Alert.Lark
	// Check that webhook URL and template path are provided
	if lc.WebhookURL == "" || lc.TemplatePath == "" {
		return nil, fmt.Errorf("missing required Lark configuration: need webhook_url and template_path")
	}

	return NewLarkProvider(config.LarkConfig{
		WebhookURL:   lc.WebhookURL,
		TemplatePath: lc.TemplatePath,
	}), nil
}
