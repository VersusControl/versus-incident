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
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

type GoogleChatProvider struct {
	webhookURL     string
	thread         string
	otherButtons   map[string]string
	displayButtons []string
	templatePath   string
}

type ThreadInfo struct {
	ThreadKey string `json:"threadKey"`
}

func NewGoogleChatProvider(cfg config.GoogleChatConfig) *GoogleChatProvider {
	return &GoogleChatProvider{
		webhookURL:     cfg.WebhookURL,
		thread:         cfg.Thread,
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

	fmt.Println("thread", g.thread)
	var thread = g.thread
	if thread != "" {
		thread = strings.ReplaceAll(g.thread, "__DATE__", time.Now().Format("20060102"))
	}

	// Create interactive card message
	ggChatCardMsg := utils.CreateGoogleChatMessage(message.String(), thread, g.otherButtons, g.displayButtons, i.Resolved)

	jsonData, err := json.Marshal(ggChatCardMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	var webhookURL = g.webhookURL
	if thread != "" {
		webhookURL += "&messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
	}
	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(jsonData))
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
