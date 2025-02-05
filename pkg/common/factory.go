package common

import (
	"fmt"
	"versus-incident/pkg/core"
)

type ProviderFactory struct {
	cfg *Config
}

func NewProviderFactory(cfg *Config) *ProviderFactory {
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

	return providers, nil
}

func (f *ProviderFactory) createSlackProvider() (core.AlertProvider, error) {
	sc := f.cfg.Alert.Slack
	if sc.Token == "" || sc.ChannelID == "" || sc.TemplatePath == "" {
		return nil, fmt.Errorf("missing required Slack configuration")
	}

	return NewSlackProvider(SlackConfig{
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

	return NewTelegramProvider(TelegramConfig{
		BotToken:     tc.BotToken,
		ChatID:       tc.ChatID,
		TemplatePath: tc.TemplatePath,
	}), nil
}
