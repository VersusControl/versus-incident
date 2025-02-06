package common

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/spf13/viper"
)

type Config struct {
	Name  string
	Host  string
	Port  int
	Alert AlertConfig
	Queue QueueConfig
}

type AlertConfig struct {
	Slack    SlackConfig
	Telegram TelegramConfig
}

type SlackConfig struct {
	Enable       bool
	Token        string
	ChannelID    string `mapstructure:"channel_id"`
	TemplatePath string `mapstructure:"template_path"`
}

type TelegramConfig struct {
	Enable       bool
	BotToken     string `mapstructure:"bot_token"`
	ChatID       string `mapstructure:"chat_id"`
	TemplatePath string `mapstructure:"template_path"`
}

type QueueConfig struct {
	Enable bool         `mapstructure:"enable"`
	SNS    SNSConfig    `mapstructure:"sns"`
	SQS    SQSConfig    `mapstructure:"sqs"`
	PubSub PubSubConfig `mapstructure:"pubsub"`
	AzBus  AzBusConfig  `mapstructure:"azbus"`
}

type SNSConfig struct {
	Enable bool `mapstructure:"enable"`
}

type SQSConfig struct {
	Enable   bool   `mapstructure:"enable"`
	QueueURL string `mapstructure:"queue_url"`
}

type PubSubConfig struct {
	Enable bool `mapstructure:"enable"`
}

type AzBusConfig struct {
	Enable bool `mapstructure:"enable"`
}

var (
	cfg     *Config
	cfgOnce sync.Once
)

func LoadConfig(path string) error {
	var err error

	cfgOnce.Do(func() {
		v := viper.New()
		v.SetConfigFile(path)
		v.SetConfigType("yaml")
		v.AutomaticEnv()
		v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))

		// Replace ${VAR} with environment variables
		v.SetTypeByDefaultValue(true)

		if err = v.ReadInConfig(); err != nil {
			err = fmt.Errorf("failed to read config: %w", err)
			return
		}

		if err = v.Unmarshal(&cfg); err != nil {
			err = fmt.Errorf("failed to unmarshal config: %w", err)
			return
		}

		if slackEnable := os.Getenv("SLACK_ENABLE"); slackEnable != "" {
			cfg.Alert.Slack.Enable = strings.ToLower(slackEnable) == "true"
		}
		if telegramEnable := os.Getenv("TELEGRAM_ENABLE"); telegramEnable != "" {
			cfg.Alert.Telegram.Enable = strings.ToLower(telegramEnable) == "true"
		}

		// Manual replacement for environment variables in strings
		cfg.Alert.Slack.Token = expandEnv(cfg.Alert.Slack.Token)
		cfg.Alert.Slack.ChannelID = expandEnv(cfg.Alert.Slack.ChannelID)
		cfg.Alert.Telegram.BotToken = expandEnv(cfg.Alert.Telegram.BotToken)
		cfg.Alert.Telegram.ChatID = expandEnv(cfg.Alert.Telegram.ChatID)
	})

	return err
}

func expandEnv(s string) string {
	return os.ExpandEnv(s)
}

func GetConfig() *Config {
	if cfg == nil {
		panic("config not initialized - call Load first")
	}
	return cfg
}
