package common

import (
	"github.com/VersusControl/versus-incident/pkg/config"
	"testing"
)

func TestKubernetesIssueFactoryPreservesAllTemplateOverrides(t *testing.T) {
	cfg := &config.Config{PublicHost: "https://versus.invalid", Alert: config.AlertConfig{
		Slack:    config.SlackConfig{Enable: true, Token: "test", ChannelID: "test", TemplatePath: "base", KubernetesIssueTemplatePath: "slack"},
		Telegram: config.TelegramConfig{Enable: true, BotToken: "test", ChatID: "test", TemplatePath: "base", KubernetesIssueTemplatePath: "telegram"},
		Viber:    config.ViberConfig{Enable: true, BotToken: "test", APIType: "bot", UserID: "test", TemplatePath: "base", KubernetesIssueTemplatePath: "viber"},
		Email:    config.EmailConfig{Enable: true, SMTPHost: "test.invalid", Username: "test", Password: "test", To: "test@example.invalid", TemplatePath: "base", KubernetesIssueTemplatePath: "email"},
		MSTeams:  config.MSTeamsConfig{Enable: true, PowerAutomateURL: "https://example.invalid", TemplatePath: "base", KubernetesIssueTemplatePath: "teams"},
		Lark:     config.LarkConfig{Enable: true, WebhookURL: "https://example.invalid", TemplatePath: "base", KubernetesIssueTemplatePath: "lark"},
	}}
	factory := NewAlertProviderFactory(cfg)
	factory.SetContext(t.Context())
	providers, err := factory.CreateProviders()
	if err != nil || len(providers) != 6 {
		t.Fatalf("%d %v", len(providers), err)
	}
	for _, provider := range providers {
		switch p := provider.(type) {
		case *SlackProvider:
			if p.kubernetesIssueTemplatePath != "slack" || p.publicHost != cfg.PublicHost {
				t.Fatal(p)
			}
		case *TelegramProvider:
			if p.kubernetesIssueTemplatePath != "telegram" {
				t.Fatal(p)
			}
		case *ViberProvider:
			if p.kubernetesIssueTemplatePath != "viber" {
				t.Fatal(p)
			}
		case *EmailProvider:
			if p.kubernetesIssueTemplatePath != "email" || p.context == nil {
				t.Fatal(p)
			}
		case *MSTeamsProvider:
			if p.kubernetesIssueTemplatePath != "teams" {
				t.Fatal(p)
			}
		case *LarkProvider:
			if p.kubernetesIssueTemplatePath != "lark" {
				t.Fatal(p)
			}
		default:
			t.Fatalf("unknown provider %T", provider)
		}
	}
}
