package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"text/template"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

type GoogleChatProvider struct {
	webhookURL     string
	otherButtons   map[string]string
	displayButtons []string
	templatePath   string
}

func NewGoogleChatProvider(cfg config.GoogleChatConfig) *GoogleChatProvider {
	return &GoogleChatProvider{
		webhookURL:     cfg.WebhookURL,
		otherButtons:   cfg.OtherButtons,
		displayButtons: cfg.DisplayButtons,
		templatePath:   cfg.TemplatePath,
	}
}

func (g *GoogleChatProvider) SendAlert(i *m.Incident) error {

	funcMaps := utils.GetTemplateFuncMaps()

	tmpl, err := template.New(filepath.Base(g.templatePath)).Funcs(funcMaps).ParseFiles(g.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var message bytes.Buffer
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Create interactive card message
	ggChatCardMsg := utils.CreateGoogleChatMessage(message.String(), g.otherButtons, g.displayButtons, i.Resolved)

	jsonData, err := json.Marshal(ggChatCardMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	resp, err := http.Post(g.webhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("google chat API returned non-200 status code: %d, body: %s", resp.StatusCode, string(body))
	}
	return nil
}
