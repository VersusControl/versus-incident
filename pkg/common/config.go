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

		// Manual replacement for environment variables in strings
		cfg.Alert.Slack.Token = expandEnv(cfg.Alert.Slack.Token)
		cfg.Alert.Slack.ChannelID = expandEnv(cfg.Alert.Slack.ChannelID)
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
