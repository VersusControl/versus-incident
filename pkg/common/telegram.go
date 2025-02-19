package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"text/template"
	m "versus-incident/pkg/models"
)

type TelegramProvider struct {
	botToken     string
	chatID       string
	templatePath string
}

type TelegramMessage struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

func NewTelegramProvider(cfg TelegramConfig) *TelegramProvider {
	return &TelegramProvider{
		botToken:     cfg.BotToken,
		chatID:       cfg.ChatID,
		templatePath: cfg.TemplatePath,
	}
}

func (t *TelegramProvider) SendAlert(i *m.Incident) error {
	funcMap := template.FuncMap{
		"replaceAll": strings.ReplaceAll,
	}

	funcMapContains := template.FuncMap{
		"contains": strings.Contains,
	}

	tmpl, err := template.New(filepath.Base(t.templatePath)).Funcs(funcMap).Funcs(funcMapContains).ParseFiles(t.templatePath)
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
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
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
