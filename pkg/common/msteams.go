package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"text/template"
	m "versus-incident/pkg/models"
)

type MSTeamsProvider struct {
	webhookURL   string
	templatePath string
}

type MSTeamsMessage struct {
	Text string `json:"text"`
}

func NewMSTeamsProvider(cfg MSTeamsConfig) *MSTeamsProvider {
	return &MSTeamsProvider{
		webhookURL:   cfg.WebhookURL,
		templatePath: cfg.TemplatePath,
	}
}

func (m *MSTeamsProvider) SendAlert(i *m.Incident) error {
	funcMaps := GetTemplateFuncMaps()

	tmpl, err := template.New(filepath.Base(m.templatePath)).Funcs(funcMaps).ParseFiles(m.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template
	var message bytes.Buffer
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Create MS Teams message payload
	msTeamsMsg := MSTeamsMessage{
		Text: message.String(),
	}

	jsonData, err := json.Marshal(msTeamsMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Send to Teams webhook
	resp, err := http.Post(m.webhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("MS Teams API returned %d status code: %s", resp.StatusCode, string(body))
	}

	return nil
}
