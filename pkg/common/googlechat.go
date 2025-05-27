package common

import (
	"bytes"
	"fmt"
	"io/ioutil"
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
	var status interface{}
	if i.Content != nil {
		status = (*i.Content)["status"]
	}
	utils.Log.Infof("GoogleChatProvider: Received alert ID %s, Status: %s", i.ID, status)

	incidentData := make(map[string]interface{})
	if i.Content != nil {
		for k, v := range *i.Content {
			incidentData[k] = v
		}
	}

	var ackURL string
	// Determine if resolved. Safely access status.
	var statusStr string
	if statusVal, ok := incidentData["status"]; ok {
		if str, ok := statusVal.(string); ok {
			statusStr = str
		}
	}
	isResolved := strings.ToLower(statusStr) == "resolved"
	if !isResolved {
		// Process AckURL, similar to SlackProvider.processAckURL
		if ackURLVal, ok := incidentData["AckURL"]; ok {
			ackURL = fmt.Sprintf("%v", ackURLVal)
			delete(incidentData, "AckURL") // Remove from incidentData as it's handled separately
		}
		utils.Log.Debugf("GoogleChatProvider: AckURL for incident %s: %s", i.ID, ackURL)
	}
	payload, err := renderCardPayload(s.templatePath, incidentData)
	if err != nil {
		utils.Log.Errorf("GoogleChatProvider: Error rendering card payload for incident %s: %v", i.ID, err)
		return fmt.Errorf("failed to render Google Chat card payload for incident %s: %w", i.ID, err)
	}
	utils.Log.Debugf("GoogleChatProvider: Payload for incident %s: %s", i.ID, string(payload))

	utils.Log.Infof("GoogleChatProvider: Sending alert %s to Google Chat webhook", i.ID)
	req, err := http.NewRequest("POST", s.webhookURL, bytes.NewBuffer(payload))
	if err != nil {
		utils.Log.Errorf("GoogleChatProvider: Error creating HTTP request for incident %s: %v", i.ID, err)
		return fmt.Errorf("failed to create Google Chat request for incident %s: %w", i.ID, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		utils.Log.Errorf("GoogleChatProvider: Error sending alert %s to Google Chat: %v", i.ID, err)
		return fmt.Errorf("failed to send Google Chat message for incident %s: %w", i.ID, err)
	}
	defer resp.Body.Close()

	responseBodyBytes, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		utils.Log.Errorf("GoogleChatProvider: Failed to read response body for incident %s: %v", i.ID, readErr)
		// Continue to check status code, as the request might have succeeded
	}

	if resp.StatusCode != http.StatusOK {
		utils.Log.Errorf("GoogleChatProvider: Google Chat responded with status %s for incident %s. Body: %s", resp.Status, i.ID, string(responseBodyBytes))
		return fmt.Errorf("failed to send Google Chat message for incident %s: status code %d, response: %s", i.ID, resp.StatusCode, string(responseBodyBytes))
	}

	utils.Log.Infof("GoogleChatProvider: Successfully sent alert %s to Google Chat", i.ID)
	return nil
}

// renderCardPayload generates the JSON payload for a Google Chat Card
func renderCardPayload(templatePath string, incidentData map[string]interface{}) ([]byte, error) {
	if templatePath == "" {
		utils.Log.Errorf("GoogleChatProvider: Template path is not configured.")
		return nil, fmt.Errorf("google chat template path is not configured")
	}

	tmplName := filepath.Base(templatePath)
	tmpl, err := template.New(tmplName).Funcs(utils.GetTemplateFuncMaps()).ParseFiles(templatePath)
	if err != nil {
		utils.Log.Errorf("GoogleChatProvider: Error parsing template %s: %v", templatePath, err)
		return nil, fmt.Errorf("failed to parse Google Chat template %s: %w", templatePath, err)
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, incidentData); err != nil {
		utils.Log.Errorf("GoogleChatProvider: Error executing template %s: %v", templatePath, err)
		return nil, fmt.Errorf("failed to execute Google Chat template %s: %w", templatePath, err)
	}

	// The template is expected to produce a valid JSON.
	// We don't marshal here because the template itself should generate the JSON string.
	return buf.Bytes(), nil
}
