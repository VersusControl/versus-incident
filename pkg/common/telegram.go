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

type TelegramProvider struct {
	botToken     string
	chatID       string
	templatePath string
	client       *http.Client
}

type TelegramMessage struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

func NewTelegramProvider(cfg config.TelegramConfig, proxyConfig config.ProxyConfig) *TelegramProvider {
	client := utils.CreateHTTPClient(proxyConfig, cfg.UseProxy)

	return &TelegramProvider{
		botToken:     cfg.BotToken,
		chatID:       cfg.ChatID,
		templatePath: cfg.TemplatePath,
		client:       client,
	}
}

func (t *TelegramProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tmpl, err := template.New(filepath.Base(t.templatePath)).Funcs(funcMaps).ParseFiles(t.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var message bytes.Buffer
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	telegramMsg := TelegramMessage{
		ChatID:    t.chatID,
		Text:      message.String(),
		ParseMode: "HTML",
	}

	jsonData, err := json.Marshal(telegramMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.botToken)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close()

	// Read and print response body
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned non-200 status code: %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}
