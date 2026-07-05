package common

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"text/template"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

type MSTeamsProvider struct {
	powerAutomateURL string
	templatePath     string
}

func NewMSTeamsProvider(cfg config.MSTeamsConfig) *MSTeamsProvider {
	return &MSTeamsProvider{
		powerAutomateURL: cfg.PowerAutomateURL,
		templatePath:     cfg.TemplatePath,
	}
}

// Name implements core.AlertProvider.
func (m *MSTeamsProvider) Name() string { return "msteams" }

func (m *MSTeamsProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tplPath := m.templatePath
	if i.Content != nil && utils.IsAgentIncident(*i.Content) {
		tplPath = utils.AgentMSTeamsTemplatePath
	}

	tmpl, err := template.New(filepath.Base(tplPath)).Funcs(funcMaps).ParseFiles(tplPath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template - this preserves the existing template format
	var message bytes.Buffer
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Convert the message to the appropriate payload format
	jsonData, err := utils.ConvertToTeamsPayload(m.powerAutomateURL, message.String(), i)
	if err != nil {
		return fmt.Errorf("failed to prepare message payload: %w", err)
	}

	// Send to Power Automate
	resp, err := http.Post(m.powerAutomateURL, "application/json", bytes.NewBuffer(jsonData))
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

// SendText implements core.TextSender: the image-fallback path for Teams,
// which cannot upload a binary over its Power Automate webhook. It posts the
// already-redacted text (report caption + note) as a Teams message. The
// incident carrier has no raw content, so nothing unredacted is passed
// through ConvertToTeamsPayload.
func (m *MSTeamsProvider) SendText(i *m.Incident, text string) error {
	payload, err := utils.ConvertToTeamsPayload(m.powerAutomateURL, text, i)
	if err != nil {
		return fmt.Errorf("failed to prepare text payload: %w", err)
	}
	resp, err := http.Post(m.powerAutomateURL, "application/json", bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to send text: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("MS Teams API returned %d status code: %s", resp.StatusCode, string(body))
	}
	return nil
}
