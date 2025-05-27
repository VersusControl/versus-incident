package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/viper"
)

type Config struct {
	Name       string
	Host       string
	Port       int
	PublicHost string `mapstructure:"public_host"`

	Alert  AlertConfig
	Queue  QueueConfig
	OnCall OnCallConfig
	Proxy  ProxyConfig

	Redis RedisConfig `mapstructure:"redis"`
}

type ProxyConfig struct {
	URL      string `mapstructure:"url"`      // HTTP/HTTPS/SOCKS5 proxy URL
	Username string `mapstructure:"username"` // Optional proxy username
	Password string `mapstructure:"password"` // Optional proxy password
}

type AlertConfig struct {
	DebugBody bool `mapstructure:"debug_body"`
	Slack     SlackConfig
	Telegram  TelegramConfig
	Viber     ViberConfig
	Email     EmailConfig
	MSTeams   MSTeamsConfig
	Lark      LarkConfig
	GoogleChat GoogleChatConfig `yaml:"googlechat" mapstructure:"googlechat"`
}

type SlackConfig struct {
	Enable            bool
	Token             string
	ChannelID         string                 `mapstructure:"channel_id"`
	TemplatePath      string                 `mapstructure:"template_path"`
	MessageProperties SlackMessageProperties `mapstructure:"message_properties"`
}

type SlackMessageProperties struct {
	DisableButton bool   `mapstructure:"disable_button"`
	ButtonText    string `mapstructure:"button_text"`
	ButtonStyle   string `mapstructure:"button_style"`
}

type TelegramConfig struct {
	Enable       bool
	BotToken     string `mapstructure:"bot_token"`
	ChatID       string `mapstructure:"chat_id"`
	TemplatePath string `mapstructure:"template_path"`
	UseProxy     bool   `mapstructure:"use_proxy"`
}

type ViberConfig struct {
	Enable  bool
	APIType string `mapstructure:"api_type"` // "bot" or "channel" - defaults to "channel"
	// Bot API configuration
	BotToken     string `mapstructure:"bot_token"`
	UserID       string `mapstructure:"user_id"`
	TemplatePath string `mapstructure:"template_path"`
	// Channel configuration for Channels Post API
	ChannelID string `mapstructure:"channel_id"`
	UseProxy  bool   `mapstructure:"use_proxy"`
}

type EmailConfig struct {
	Enable       bool
	SMTPHost     string `mapstructure:"smtp_host"`
	SMTPPort     string `mapstructure:"smtp_port"`
	Username     string
	Password     string
	To           string
	Subject      string
	TemplatePath string `mapstructure:"template_path"`
}

type MSTeamsConfig struct {
	Enable         bool
	TemplatePath   string            `mapstructure:"template_path"`
	OtherPowerURLs map[string]string `mapstructure:"other_power_urls"` // Optional alternative Power Automate URLs
	// Power Automate Workflow URL for Teams integration
	PowerAutomateURL string `mapstructure:"power_automate_url"`
}

type LarkConfig struct {
	Enable           bool
	WebhookURL       string            `mapstructure:"webhook_url"`
	TemplatePath     string            `mapstructure:"template_path"`
	OtherWebhookURLs map[string]string `mapstructure:"other_webhook_urls"`
	UseProxy         bool              `mapstructure:"use_proxy"`
}

type GoogleChatMessageProperties struct {
	ButtonText string `yaml:"button_text" mapstructure:"button_text"`
}

type GoogleChatConfig struct {
	Enable            bool                        `yaml:"enable" mapstructure:"enable"`
	WebhookURL        string                      `yaml:"webhook_url" mapstructure:"webhook_url"`
	TemplatePath      string                      `yaml:"template_path" mapstructure:"template_path"`
	MessageProperties GoogleChatMessageProperties `yaml:"message_properties" mapstructure:"message_properties"`
}

type QueueConfig struct {
	Enable    bool         `mapstructure:"enable"`
	DebugBody bool         `mapstructure:"debug_body"`
	SNS       SNSConfig    `mapstructure:"sns"`
	SQS       SQSConfig    `mapstructure:"sqs"`
	PubSub    PubSubConfig `mapstructure:"pubsub"`
	AzBus     AzBusConfig  `mapstructure:"azbus"`
}

type SNSConfig struct {
	Enable       bool   `mapstructure:"enable"`
	TopicARN     string `mapstructure:"topic_arn"`
	Endpoint     string `mapstructure:"https_endpoint_subscription"`
	EndpointPath string `mapstructure:"https_endpoint_subscription_path"`
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

type OnCallConfig struct {
	Enable             bool
	InitializedOnly    bool                     `mapstructure:"initialized_only"` // Initialize infrastructure but don't enable by default
	WaitMinutes        int                      `mapstructure:"wait_minutes"`
	Provider           string                   `mapstructure:"provider"` // "aws_incident_manager" or "pagerduty"
	AwsIncidentManager AwsIncidentManagerConfig `mapstructure:"aws_incident_manager"`
	PagerDuty          PagerDutyConfig          `mapstructure:"pagerduty"`
}

type AwsIncidentManagerConfig struct {
	ResponsePlanArn       string            `mapstructure:"response_plan_arn"`
	OtherResponsePlanArns map[string]string `mapstructure:"other_response_plan_arns"`
}

type PagerDutyConfig struct {
	RoutingKey       string            `mapstructure:"routing_key"`
	OtherRoutingKeys map[string]string `mapstructure:"other_routing_keys"`
}

type RedisConfig struct {
	Host               string `mapstructure:"host"`
	Port               int    `mapstructure:"port"`
	Password           string `mapstructure:"password"`
	DB                 int    `mapstructure:"db"`
	InsecureSkipVerify bool   `mapstructure:"insecure_skip_verify"`
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

		// Replace ${VAR} with environment variables
		v.SetTypeByDefaultValue(true)

		if err = v.ReadInConfig(); err != nil {
			err = fmt.Errorf("failed to read config: %w", err)
			return
		}

		for _, k := range v.AllKeys() {
			if value, ok := v.Get(k).(string); ok {
				v.Set(k, os.ExpandEnv(value))
			}
		}

		v.AutomaticEnv()
		v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
		v.AllowEmptyEnv(true)
		v.SetTypeByDefaultValue(true)

		if err = v.Unmarshal(&cfg); err != nil {
			err = fmt.Errorf("failed to unmarshal config: %w", err)
			return
		}

		setEnableFromEnv := func(envVar string, config *bool) {
			if value := os.Getenv(envVar); value != "" {
				*config = strings.ToLower(value) == "true"
			}
		}

		setEnableFromEnv("DEBUG_BODY", &cfg.Alert.DebugBody)
		setEnableFromEnv("DEBUG_BODY", &cfg.Queue.DebugBody)

		setEnableFromEnv("SLACK_ENABLE", &cfg.Alert.Slack.Enable)
		setEnableFromEnv("TELEGRAM_ENABLE", &cfg.Alert.Telegram.Enable)
		setEnableFromEnv("TELEGRAM_USE_PROXY", &cfg.Alert.Telegram.UseProxy)
		setEnableFromEnv("VIBER_ENABLE", &cfg.Alert.Viber.Enable)
		setEnableFromEnv("VIBER_USE_PROXY", &cfg.Alert.Viber.UseProxy)
		setEnableFromEnv("EMAIL_ENABLE", &cfg.Alert.Email.Enable)
		setEnableFromEnv("MSTEAMS_ENABLE", &cfg.Alert.MSTeams.Enable)
		setEnableFromEnv("LARK_ENABLE", &cfg.Alert.Lark.Enable)
		setEnableFromEnv("LARK_USE_PROXY", &cfg.Alert.Lark.UseProxy)
		setEnableFromEnv("GOOGLECHAT_ENABLE", &cfg.Alert.GoogleChat.Enable)
		setEnableFromEnv("SNS_ENABLE", &cfg.Queue.SNS.Enable)

		setEnableFromEnv("ONCALL_ENABLE", &cfg.OnCall.Enable)

		// Set provider from environment variable if provided
		if provider := os.Getenv("ONCALL_PROVIDER"); provider != "" {
			cfg.OnCall.Provider = provider
		}
	})

	return err
}

func GetConfig() *Config {
	if cfg == nil {
		panic("config not initialized - call Load first")
	}
	return cfg
}

func GetConfigWitParamsOverwrite(paramsOverwrite *map[string]string) *Config {
	// Clone the global cfg
	clonedCfg := cloneConfig(cfg)

	if v := (*paramsOverwrite)["slack_channel_id"]; v != "" {
		clonedCfg.Alert.Slack.ChannelID = v
	}

	if v := (*paramsOverwrite)["telegram_chat_id"]; v != "" {
		clonedCfg.Alert.Telegram.ChatID = v
	}

	if v := (*paramsOverwrite)["viber_user_id"]; v != "" {
		clonedCfg.Alert.Viber.UserID = v
	}

	if v := (*paramsOverwrite)["viber_channel_id"]; v != "" {
		clonedCfg.Alert.Viber.ChannelID = v
	}

	if v := (*paramsOverwrite)["email_to"]; v != "" {
		clonedCfg.Alert.Email.To = v
	}

	if v := (*paramsOverwrite)["email_subject"]; v != "" {
		clonedCfg.Alert.Email.Subject = v
	}

	if v := (*paramsOverwrite)["msteams_other_power_url"]; v != "" {
		if clonedCfg.Alert.MSTeams.OtherPowerURLs != nil {
			powerUrl := clonedCfg.Alert.MSTeams.OtherPowerURLs[v]

			if powerUrl != "" {
				clonedCfg.Alert.MSTeams.PowerAutomateURL = powerUrl
			}
		}
	}

	if v := (*paramsOverwrite)["lark_other_webhook_url"]; v != "" {
		if clonedCfg.Alert.Lark.OtherWebhookURLs != nil {
			webhookURL := clonedCfg.Alert.Lark.OtherWebhookURLs[v]

			if webhookURL != "" {
				clonedCfg.Alert.Lark.WebhookURL = webhookURL
			}
		}
	}

	if v := (*paramsOverwrite)["oncall_enable"]; v != "" {
		if parsedBool, err := strconv.ParseBool(v); err == nil {
			clonedCfg.OnCall.Enable = parsedBool
		}
	}

	if v := (*paramsOverwrite)["oncall_wait_minutes"]; v != "" {
		if waitMinutesFloat, err := strconv.ParseFloat(v, 64); err == nil {
			clonedCfg.OnCall.WaitMinutes = int(waitMinutesFloat) // Truncates to 3 if v is "3.14"
		}
	}

	if v := (*paramsOverwrite)["awsim_other_response_plan"]; v != "" {
		if clonedCfg.OnCall.AwsIncidentManager.OtherResponsePlanArns != nil {
			responsePlanArn := clonedCfg.OnCall.AwsIncidentManager.OtherResponsePlanArns[v]

			if responsePlanArn != "" {
				clonedCfg.OnCall.AwsIncidentManager.ResponsePlanArn = responsePlanArn
			}
		}
	}

	if v := (*paramsOverwrite)["pagerduty_other_routing_key"]; v != "" {
		if clonedCfg.OnCall.PagerDuty.OtherRoutingKeys != nil {
			routingKey := clonedCfg.OnCall.PagerDuty.OtherRoutingKeys[v]

			if routingKey != "" {
				clonedCfg.OnCall.PagerDuty.RoutingKey = routingKey
			}
		}
	}

	return clonedCfg
}
