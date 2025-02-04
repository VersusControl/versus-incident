package common

import (
	"fmt"
	"strings"
	"text/template"
	m "versus-incident/pkg/models"

	"github.com/slack-go/slack"
)

type SlackProvider struct {
	client       *slack.Client
	channelID    string
	templatePath string
}

func NewSlackProvider(cfg SlackConfig) *SlackProvider {
	return &SlackProvider{
		client:       slack.New(cfg.Token),
		channelID:    cfg.ChannelID,
		templatePath: cfg.TemplatePath,
	}
}

func (s *SlackProvider) SendAlert(i *m.Incident) error {
	tmpl, err := template.ParseFiles(s.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var message strings.Builder
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	_, _, err = s.client.PostMessage(
		s.channelID,
		slack.MsgOptionAttachments(slack.Attachment{
			Text:  message.String(),
			Color: "#C70039",
		}),
	)

	if err != nil {
		return fmt.Errorf("failed to post message: %w", err)
	}

	return nil
}
