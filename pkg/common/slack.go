package common

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"

	"github.com/slack-go/slack"
)

type SlackProvider struct {
	client       *slack.Client
	channelID    string
	templatePath string
}

func NewSlackProvider(cfg config.SlackConfig) *SlackProvider {
	return &SlackProvider{
		client:       slack.New(cfg.Token),
		channelID:    cfg.ChannelID,
		templatePath: cfg.TemplatePath,
	}
}

func (s *SlackProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tmpl, err := template.New(filepath.Base(s.templatePath)).Funcs(funcMaps).ParseFiles(s.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var message strings.Builder
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	color := "#C70039" // Red
	if i.Resolved {
		color = "#36A64F" // Green
	}

	_, _, err = s.client.PostMessage(
		s.channelID,
		slack.MsgOptionAttachments(slack.Attachment{
			Text:  message.String(),
			Color: color,
		}),
	)

	if err != nil {
		return fmt.Errorf("failed to post message: %w", err)
	}

	return nil
}
