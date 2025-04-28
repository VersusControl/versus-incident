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

	Redis RedisConfig `mapstructure:"redis"`
}

type AlertConfig struct {
	DebugBody bool `mapstructure:"debug_body"`
	Slack     SlackConfig
	Telegram  TelegramConfig
	Email     EmailConfig
	MSTeams   MSTeamsConfig
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
	Enable        bool
	TemplatePath  string            `mapstructure:"template_path"`
	OtherPowerURL map[string]string `mapstructure:"other_power_url"` // Optional alternative Power Automate URLs
	// Power Automate Workflow URL for Teams integration
	PowerAutomateURL string `mapstructure:"power_automate_url"`
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
	WaitMinutes        int                      `mapstructure:"wait_minutes"`
	AwsIncidentManager AwsIncidentManagerConfig `mapstructure:"aws_incident_manager"`
}

type AwsIncidentManagerConfig struct {
	ResponsePlanArn string `mapstructure:"response_plan_arn"`
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
		setEnableFromEnv("EMAIL_ENABLE", &cfg.Alert.Email.Enable)
		setEnableFromEnv("MSTEAMS_ENABLE", &cfg.Alert.MSTeams.Enable)
		setEnableFromEnv("SNS_ENABLE", &cfg.Queue.SNS.Enable)

		setEnableFromEnv("ONCALL_ENABLE", &cfg.OnCall.Enable)
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

	if v := (*paramsOverwrite)["email_to"]; v != "" {
		clonedCfg.Alert.Email.To = v
	}

	if v := (*paramsOverwrite)["email_subject"]; v != "" {
		clonedCfg.Alert.Email.Subject = v
	}

	if v := (*paramsOverwrite)["msteams_other_power_url"]; v != "" {
		if clonedCfg.Alert.MSTeams.OtherPowerURL != nil {
			powerUrl := clonedCfg.Alert.MSTeams.OtherPowerURL[v]

			if powerUrl != "" {
				clonedCfg.Alert.MSTeams.PowerAutomateURL = powerUrl
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

	if v := (*paramsOverwrite)["awsim_response_plan_arn"]; v != "" {
		clonedCfg.OnCall.AwsIncidentManager.ResponsePlanArn = v
	}

	return clonedCfg
}
