package controllers

import (
	"github.com/VersusControl/versus-incident/pkg/config"

	"github.com/gofiber/fiber/v2"
)

// ConfigAdminController exposes a read-only, secret-redacted view of the
// running config so the admin dashboard can render it without ever
// exposing tokens, passwords, or webhook URLs. Same gateway-secret guard
// as the rest of the admin surface.
type ConfigAdminController struct{}

func NewConfigAdminController() *ConfigAdminController {
	return &ConfigAdminController{}
}

// Register attaches:
//
//	GET /api/admin/config/incidents   alert channels + queue + on-call
//	GET /api/admin/config/agent       agent runtime config
func (c *ConfigAdminController) Register(router fiber.Router) {
	g := router.Group("/admin/config", c.authMiddleware)
	g.Get("/incidents", c.incidents)
	g.Get("/agent", c.agent)
}

func (c *ConfigAdminController) authMiddleware(ctx *fiber.Ctx) error {
	cfg := config.GetConfig()
	expected := cfg.GatewaySecret
	got := ctx.Get("X-Gateway-Secret")
	if expected == "" || got == "" || got != expected {
		return ctx.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "unauthorized"})
	}
	return ctx.Next()
}

// secretSet returns "set" or "" depending on whether the secret value is
// configured. We never echo the actual value back to the UI.
func secretSet(s string) string {
	if s == "" {
		return ""
	}
	return "set"
}

// keysOf returns the sorted-ish set of keys from a map[string]string,
// dropping the values entirely (they are typically secret URLs / ARNs).
func keysOf(m map[string]string) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (c *ConfigAdminController) incidents(ctx *fiber.Ctx) error {
	cfg := config.GetConfig()

	alert := cfg.Alert
	channels := []fiber.Map{
		{
			"id":     "slack",
			"name":   "Slack",
			"enable": alert.Slack.Enable,
			"fields": []fiber.Map{
				{"label": "Token", "value": secretSet(alert.Slack.Token), "secret": true},
				{"label": "Channel ID", "value": secretSet(alert.Slack.ChannelID), "secret": true},
				{"label": "Template", "value": alert.Slack.TemplatePath},
				{"label": "Button Text", "value": alert.Slack.MessageProperties.ButtonText},
				{"label": "Button Style", "value": alert.Slack.MessageProperties.ButtonStyle},
				{"label": "Button Disabled", "value": boolStr(alert.Slack.MessageProperties.DisableButton)},
			},
		},
		{
			"id":     "telegram",
			"name":   "Telegram",
			"enable": alert.Telegram.Enable,
			"fields": []fiber.Map{
				{"label": "Bot Token", "value": secretSet(alert.Telegram.BotToken), "secret": true},
				{"label": "Chat ID", "value": secretSet(alert.Telegram.ChatID), "secret": true},
				{"label": "Template", "value": alert.Telegram.TemplatePath},
				{"label": "Use Proxy", "value": boolStr(alert.Telegram.UseProxy)},
			},
		},
		{
			"id":     "viber",
			"name":   "Viber",
			"enable": alert.Viber.Enable,
			"fields": []fiber.Map{
				{"label": "API Type", "value": alert.Viber.APIType},
				{"label": "Bot Token", "value": secretSet(alert.Viber.BotToken), "secret": true},
				{"label": "Channel ID", "value": secretSet(alert.Viber.ChannelID), "secret": true},
				{"label": "User ID", "value": secretSet(alert.Viber.UserID), "secret": true},
				{"label": "Template", "value": alert.Viber.TemplatePath},
				{"label": "Use Proxy", "value": boolStr(alert.Viber.UseProxy)},
			},
		},
		{
			"id":     "email",
			"name":   "Email",
			"enable": alert.Email.Enable,
			"fields": []fiber.Map{
				{"label": "SMTP Host", "value": alert.Email.SMTPHost},
				{"label": "SMTP Port", "value": alert.Email.SMTPPort},
				{"label": "Username", "value": secretSet(alert.Email.Username), "secret": true},
				{"label": "Password", "value": secretSet(alert.Email.Password), "secret": true},
				{"label": "To", "value": alert.Email.To},
				{"label": "Subject", "value": alert.Email.Subject},
				{"label": "Template", "value": alert.Email.TemplatePath},
			},
		},
		{
			"id":     "msteams",
			"name":   "Microsoft Teams",
			"enable": alert.MSTeams.Enable,
			"fields": []fiber.Map{
				{"label": "Power Automate URL", "value": secretSet(alert.MSTeams.PowerAutomateURL), "secret": true},
				{"label": "Template", "value": alert.MSTeams.TemplatePath},
				{"label": "Other URL Keys", "value": keysOf(alert.MSTeams.OtherPowerURLs)},
			},
		},
		{
			"id":     "lark",
			"name":   "Lark",
			"enable": alert.Lark.Enable,
			"fields": []fiber.Map{
				{"label": "Webhook URL", "value": secretSet(alert.Lark.WebhookURL), "secret": true},
				{"label": "Template", "value": alert.Lark.TemplatePath},
				{"label": "Other Webhook Keys", "value": keysOf(alert.Lark.OtherWebhookURLs)},
				{"label": "Use Proxy", "value": boolStr(alert.Lark.UseProxy)},
			},
		},
	}

	q := cfg.Queue
	queue := fiber.Map{
		"enable":     q.Enable,
		"debug_body": q.DebugBody,
		"providers": []fiber.Map{
			{
				"id":     "sns",
				"name":   "AWS SNS",
				"enable": q.SNS.Enable,
				"fields": []fiber.Map{
					{"label": "Topic ARN", "value": secretSet(q.SNS.TopicARN), "secret": true},
					{"label": "HTTPS Endpoint", "value": secretSet(q.SNS.Endpoint), "secret": true},
					{"label": "Endpoint Path", "value": q.SNS.EndpointPath},
				},
			},
			{
				"id":     "sqs",
				"name":   "AWS SQS",
				"enable": q.SQS.Enable,
				"fields": []fiber.Map{
					{"label": "Queue URL", "value": secretSet(q.SQS.QueueURL), "secret": true},
				},
			},
			{
				"id":     "pubsub",
				"name":   "GCP Pub/Sub",
				"enable": q.PubSub.Enable,
				"fields": []fiber.Map{
					{"label": "Status", "value": "stub — not implemented"},
				},
			},
			{
				"id":     "azbus",
				"name":   "Azure Service Bus",
				"enable": q.AzBus.Enable,
				"fields": []fiber.Map{
					{"label": "Status", "value": "stub — not implemented"},
				},
			},
		},
	}

	oc := cfg.OnCall
	oncall := fiber.Map{
		"enable":           oc.Enable,
		"initialized_only": oc.InitializedOnly,
		"wait_minutes":     oc.WaitMinutes,
		"provider":         oc.Provider,
		"aws_incident_manager": fiber.Map{
			"response_plan_arn":        secretSet(oc.AwsIncidentManager.ResponsePlanArn),
			"other_response_plan_keys": keysOf(oc.AwsIncidentManager.OtherResponsePlanArns),
		},
		"pagerduty": fiber.Map{
			"routing_key":        secretSet(oc.PagerDuty.RoutingKey),
			"other_routing_keys": keysOf(oc.PagerDuty.OtherRoutingKeys),
		},
	}

	return ctx.JSON(fiber.Map{
		"name":        cfg.Name,
		"host":        cfg.Host,
		"port":        cfg.Port,
		"public_host": cfg.PublicHost,
		"alert": fiber.Map{
			"debug_body": alert.DebugBody,
			"channels":   channels,
		},
		"queue":  queue,
		"oncall": oncall,
		"storage": fiber.Map{
			"type": cfg.Storage.Type,
			"file": fiber.Map{
				"data_dir":      cfg.Storage.File.DataDir,
				"max_incidents": cfg.Storage.File.MaxIncidents,
			},
		},
	})
}

func (c *ConfigAdminController) agent(ctx *fiber.Ctx) error {
	cfg := config.GetConfig()
	a := cfg.Agent

	// Sources: list names + types + enable only. Connection URLs and
	// credentials are never echoed back.
	sources := make([]fiber.Map, 0, len(a.Sources))
	for _, s := range a.Sources {
		entry := fiber.Map{
			"name":   s.Name,
			"type":   s.Type,
			"enable": s.Enable,
		}
		switch s.Type {
		case "elasticsearch":
			entry["details"] = fiber.Map{
				"index":                s.Elasticsearch.Index,
				"time_field":           s.Elasticsearch.TimeField,
				"message_field":        s.Elasticsearch.MessageField,
				"page_size":            s.Elasticsearch.PageSize,
				"address_count":        len(s.Elasticsearch.Addresses),
				"insecure_skip_verify": s.Elasticsearch.InsecureSkipVerify,
				"auth": secretEither(s.Elasticsearch.APIKey,
					s.Elasticsearch.Username),
			}
		case "file":
			entry["details"] = fiber.Map{
				"path":               s.File.Path,
				"format":             s.File.Format,
				"from_beginning":     s.File.FromBeginning,
				"max_lines_per_pull": s.File.MaxLinesPerPull,
			}
		}
		sources = append(sources, entry)
	}

	// Regex rules: names + count of patterns. The pattern strings are
	// not secret per se but we keep this view tidy.
	rules := make([]fiber.Map, 0, len(a.Regex.Rules))
	for _, r := range a.Regex.Rules {
		rules = append(rules, fiber.Map{
			"name":    r.Name,
			"pattern": r.Pattern,
		})
	}

	return ctx.JSON(fiber.Map{
		"enable":            a.Enable,
		"mode":              a.Mode,
		"poll_interval":     a.PollInterval,
		"lookback":          a.Lookback,
		"batch_max":         a.BatchMax,
		"signal_max_bytes":  a.SignalMaxBytes,
		"new_service_grace": a.NewServiceGrace,
		"service_patterns":  a.ServicePatterns,
		"sources_path":      a.SourcesPath,
		"sources":           sources,
		"redaction": fiber.Map{
			"enable":              a.Redaction.Enable,
			"redact_ips":          a.Redaction.RedactIPs,
			"extra_pattern_count": len(a.Redaction.ExtraPatterns),
		},
		"catalog": fiber.Map{
			"persist_interval":         a.Catalog.PersistInterval,
			"auto_promote_after":       a.Catalog.AutoPromoteAfter,
			"spike_multiplier":         a.Catalog.SpikeMultiplier,
			"spike_min_frequency":      a.Catalog.SpikeMinFrequency,
			"spike_min_baseline_count": a.Catalog.SpikeMinBaselineCount,
		},
		"miner": fiber.Map{
			"similarity_threshold": a.Miner.SimilarityThreshold,
			"tree_depth":           a.Miner.TreeDepth,
			"max_children":         a.Miner.MaxChildren,
		},
		"regex": fiber.Map{
			"default_pattern": a.Regex.DefaultPattern,
			"rules":           rules,
		},
		"ai": fiber.Map{
			"enable":             a.AI.Enable,
			"model":              a.AI.Model,
			"temperature":        a.AI.Temperature,
			"max_tokens":         a.AI.MaxTokens,
			"max_calls_per_hour": a.AI.MaxCallsPerHour,
			"cache_ttl":          a.AI.CacheTTL,
			"api_key":            secretSet(a.AI.APIKey),
		},
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// secretEither returns "api_key" if the API key is set, otherwise
// "basic" if a username is set, otherwise "" (none).
func secretEither(apiKey, username string) string {
	if apiKey != "" {
		return "api_key"
	}
	if username != "" {
		return "basic"
	}
	return ""
}
