package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"text/template"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
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

// Name implements core.AlertProvider.
func (t *TelegramProvider) Name() string { return "telegram" }

// SendAttachment implements core.AttachmentSender: it uploads the report
// PNG via the Bot API sendPhoto multipart endpoint with the caption.
func (t *TelegramProvider) SendAttachment(i *m.Incident, att core.Attachment) error {
	if len(att.Data) == 0 {
		return fmt.Errorf("telegram: empty attachment")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("chat_id", t.chatID); err != nil {
		return fmt.Errorf("telegram: form field: %w", err)
	}
	if att.Caption != "" {
		if err := w.WriteField("caption", att.Caption); err != nil {
			return fmt.Errorf("telegram: form field: %w", err)
		}
	}
	fw, err := w.CreateFormFile("photo", att.Filename)
	if err != nil {
		return fmt.Errorf("telegram: form file: %w", err)
	}
	if _, err := fw.Write(att.Data); err != nil {
		return fmt.Errorf("telegram: write photo: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("telegram: close writer: %w", err)
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendPhoto", t.botToken)
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return fmt.Errorf("telegram: create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: sendPhoto: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram sendPhoto returned non-200 status code: %d, body: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (t *TelegramProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tplPath := t.templatePath
	if i.Content != nil && utils.IsAgentIncident(*i.Content) {
		tplPath = utils.AgentTelegramTemplatePath
	}

	tmpl, err := template.New(filepath.Base(tplPath)).Funcs(funcMaps).ParseFiles(tplPath)
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
