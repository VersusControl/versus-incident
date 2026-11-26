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

type MSTeamsProvider struct {
	client                      *http.Client
	kubernetesIssueTemplatePath string
	powerAutomateURL            string
	templatePath                string
}

func NewMSTeamsProvider(cfg config.MSTeamsConfig) *MSTeamsProvider {
	return &MSTeamsProvider{
		client:                      http.DefaultClient,
		kubernetesIssueTemplatePath: cfg.KubernetesIssueTemplatePath,
		powerAutomateURL:            cfg.PowerAutomateURL,
		templatePath:                cfg.TemplatePath,
	}
}

// Name implements core.AlertProvider.
func (m *MSTeamsProvider) Name() string { return "msteams" }

func (m *MSTeamsProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tplPath := m.templatePath
	if i.Content != nil {
		tplPath = utils.AlertTemplatePath(*i.Content, utils.ChannelMSTeams, tplPath, m.kubernetesIssueTemplatePath)
	}

	tmpl, err := template.New(filepath.Base(tplPath)).Funcs(funcMaps).ParseFiles(tplPath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// An AWS issue alert sent to a workflow is its own card: the links
	// become buttons, the heading is accented by what the alert is, and
	// the whole card is fitted to the message limit. An Office 365
	// connector shows the Markdown text, links included.
	if i.Content != nil && utils.IsAWSIssueIncident(*i.Content) && !utils.IsTeamsConnectorURL(m.powerAutomateURL) {
		content := *i.Content
		body := utils.WithoutAlertLinks(content)
		card := func(text string) []byte {
			encoded, _ := json.Marshal(utils.AWSIssueTeamsCard(text, content))
			return encoded
		}
		message, err := utils.RenderAlert(tmpl, &body, utils.MSTeamsAlertBudget, func(text string) int { return len(card(text)) })
		if err != nil {
			return err
		}
		return m.post(i, card(message))
	}

	// Execute template - this preserves the existing template format
	message, err := utils.RenderAlert(tmpl, i.Content, utils.MSTeamsAlertBudget, utils.JSONLength)
	if err != nil {
		return err
	}

	// Convert the message to the appropriate payload format
	jsonData, err := utils.ConvertToTeamsPayload(m.powerAutomateURL, message, i)
	if err != nil {
		return fmt.Errorf("failed to prepare message payload: %w", err)
	}
	return m.post(i, jsonData)
}

// post sends one payload to the configured Teams URL.
func (m *MSTeamsProvider) post(i *m.Incident, jsonData []byte) error {
	// Send to Power Automate
	client := m.client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequest(http.MethodPost, m.powerAutomateURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	setIdempotencyKey(req, i)
	resp, err := client.Do(req)
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
	client := m.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Post(m.powerAutomateURL, "application/json", bytes.NewBuffer(payload))
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
