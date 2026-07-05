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

type ViberProvider struct {
	botToken     string
	userID       string
	channelID    string
	templatePath string
	apiType      string // "bot" or "channel"
	client       *http.Client
}

// ViberBotMessage represents a message for Viber Bot API
type ViberBotMessage struct {
	Receiver string                 `json:"receiver"`
	Type     string                 `json:"type"`
	Text     string                 `json:"text"`
	Sender   map[string]interface{} `json:"sender"`
}

// ViberChannelMessage represents a message for Viber Channels Post API
type ViberChannelMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func NewViberProvider(cfg config.ViberConfig, proxyConfig config.ProxyConfig) *ViberProvider {
	apiType := cfg.APIType
	if apiType == "" {
		apiType = "bot" // Default to bot API for backward compatibility
	}

	client := utils.CreateHTTPClient(proxyConfig, cfg.UseProxy)

	return &ViberProvider{
		botToken:     cfg.BotToken,
		userID:       cfg.UserID,
		channelID:    cfg.ChannelID,
		templatePath: cfg.TemplatePath,
		apiType:      apiType,
		client:       client,
	}
}

// Name implements core.AlertProvider.
func (v *ViberProvider) Name() string { return "viber" }

func (v *ViberProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tplPath := v.templatePath
	if i.Content != nil && utils.IsAgentIncident(*i.Content) {
		tplPath = utils.AgentViberTemplatePath
	}

	tmpl, err := template.New(filepath.Base(tplPath)).Funcs(funcMaps).ParseFiles(tplPath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var message bytes.Buffer
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	if v.apiType == "channel" {
		return v.sendChannelMessage(message.String())
	}

	return v.sendBotMessage(message.String())
}

// SendText implements core.TextSender: the image-fallback path for Viber,
// which posts the already-redacted report caption + note as a text message
// through the configured (bot or channel) API. No raw incident content is
// referenced.
func (v *ViberProvider) SendText(i *m.Incident, text string) error {
	if v.apiType == "channel" {
		return v.sendChannelMessage(text)
	}
	return v.sendBotMessage(text)
}

// sendBotMessage sends a message using Viber Bot API
func (v *ViberProvider) sendBotMessage(text string) error {
	viberMsg := ViberBotMessage{
		Receiver: v.userID,
		Type:     "text",
		Text:     text,
		Sender: map[string]interface{}{
			"name":   "Versus Incident",
			"avatar": "",
		},
	}

	jsonData, err := json.Marshal(viberMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal bot message: %w", err)
	}

	return v.makeAPIRequest("https://chatapi.viber.com/pa/send_message", jsonData)
}

// sendChannelMessage sends a message using Viber Channels Post API
func (v *ViberProvider) sendChannelMessage(text string) error {
	viberMsg := ViberChannelMessage{
		Type: "text",
		Text: text,
	}

	jsonData, err := json.Marshal(viberMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal channel message: %w", err)
	}

	url := fmt.Sprintf("https://chatapi.viber.com/pa/post_to_channel/%s", v.channelID)
	return v.makeAPIRequest(url, jsonData)
}

// makeAPIRequest makes the HTTP request to Viber API
func (v *ViberProvider) makeAPIRequest(url string, jsonData []byte) error {
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Viber-Auth-Token", v.botToken)

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close()

	// Read and print response body
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("viber API returned non-200 status code: %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}
