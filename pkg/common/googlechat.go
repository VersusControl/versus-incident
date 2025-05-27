package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// GoogleChatMessageProperties holds configuration for Google Chat message buttons
type GoogleChatMessageProperties struct {
	ButtonText string
}

// GoogleChatProvider holds the configuration for the Google Chat alert provider
type GoogleChatProvider struct {
	webhookURL   string
	templatePath string
	msgProps     GoogleChatMessageProperties
	httpClient   *http.Client
}

// NewGoogleChatProvider initializes a new GoogleChatProvider
func NewGoogleChatProvider(cfg config.GoogleChatConfig) *GoogleChatProvider {
	return &GoogleChatProvider{
		webhookURL:   cfg.WebhookURL,
		templatePath: cfg.TemplatePath,
		msgProps: GoogleChatMessageProperties{
			ButtonText: cfg.MessageProperties.ButtonText,
		},
		httpClient: &http.Client{Timeout: 10 * time.Second}, // Using standard http.Client as utils.NewHTTPClient is hypothetical
	}
}

// SendAlert sends an alert to Google Chat
func (s *GoogleChatProvider) SendAlert(i *m.Incident) error {
	incidentData := make(map[string]interface{})
	if i.Content != nil {
		for k, v := range *i.Content {
			incidentData[k] = v
		}
	}

	var ackURL string
	isResolved := strings.ToLower(incidentData["status"].(string)) == "resolved"

	if !isResolved {
		// Process AckURL, similar to SlackProvider.processAckURL
		if ackURLVal, ok := incidentData["ackurl"]; ok {
			ackURL = fmt.Sprintf("%v", ackURLVal)
			delete(incidentData, "ackurl") // Remove from incidentData as it's handled separately
		} else if ackURLVal, ok := incidentData["ack_url"]; ok {
			ackURL = fmt.Sprintf("%v", ackURLVal)
			delete(incidentData, "ack_url")
		}
	}
	
	payload, err := renderCardPayload(s.templatePath, incidentData, ackURL, s.msgProps.ButtonText, isResolved)
	if err != nil {
		return fmt.Errorf("failed to render Google Chat card payload: %w", err)
	}

	req, err := http.NewRequest("POST", s.webhookURL, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create Google Chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send Google Chat message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Attempt to read body for more details, but don't fail if it's not possible
		var responseBody strings.Builder
		_, err := responseBody.ReadFrom(resp.Body)
		if err != nil {
			utils.Logger.Errorf("Failed to read Google Chat error response body: %v", err)
		}
		return fmt.Errorf("failed to send Google Chat message: status code %d, response: %s", resp.StatusCode, responseBody.String())
	}

	utils.Logger.Infof("Google Chat alert sent successfully for incident: %s", i.ID)
	return nil
}

// renderCardPayload generates the JSON payload for a Google Chat Card
func renderCardPayload(templatePath string, incidentData map[string]interface{}, ackURL string, buttonText string, isResolved bool) ([]byte, error) {
	if templatePath == "" {
		return nil, fmt.Errorf("google chat template path is not configured")
	}

	tmplName := filepath.Base(templatePath)
	tmpl, err := template.New(tmplName).Funcs(utils.GetTemplateFuncMaps()).ParseFiles(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Google Chat template: %w", err)
	}

	var buf bytes.Buffer
	templateData := map[string]interface{}{
		"Incident":   incidentData,
		"AckURL":     ackURL,
		"ButtonText": buttonText,
		"IsResolved": isResolved,
	}

	if err := tmpl.Execute(&buf, templateData); err != nil {
		return nil, fmt.Errorf("failed to execute Google Chat template: %w", err)
	}

	// The template is expected to produce a valid JSON.
	// We don't marshal here because the template itself should generate the JSON string.
	return buf.Bytes(), nil
}
