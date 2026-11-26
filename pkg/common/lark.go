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

type LarkProvider struct {
	kubernetesIssueTemplatePath string
	webhookURL                  string
	templatePath                string
	client                      *http.Client
}

func NewLarkProvider(cfg config.LarkConfig, proxyConfig config.ProxyConfig) *LarkProvider {
	client := utils.CreateHTTPClient(proxyConfig, cfg.UseProxy)
	return &LarkProvider{
		kubernetesIssueTemplatePath: cfg.KubernetesIssueTemplatePath,
		webhookURL:                  cfg.WebhookURL,
		templatePath:                cfg.TemplatePath,
		client:                      client,
	}
}

// Name implements core.AlertProvider.
func (l *LarkProvider) Name() string { return "lark" }

func (l *LarkProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tplPath := l.templatePath
	if i.Content != nil {
		tplPath = utils.AlertTemplatePath(*i.Content, utils.ChannelLark, tplPath, l.kubernetesIssueTemplatePath)
	}

	tmpl, err := template.New(filepath.Base(tplPath)).Funcs(funcMaps).ParseFiles(tplPath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	// An AWS issue alert is fitted to the card limit and framed by what
	// it is; any other alert keeps the firing or resolved header.
	message, err := utils.RenderAlert(tmpl, i.Content, utils.LarkAlertBudget, utils.JSONLength)
	if err != nil {
		return err
	}

	// Create interactive card message
	larkMsg := utils.CreateLarkMessage(message, i.Resolved)
	if i.Content != nil && utils.IsAWSIssueIncident(*i.Content) && !i.Resolved {
		title, colour := utils.AWSIssueLarkHeader(*i.Content)
		larkMsg = utils.CreateLarkCard(message, title, colour)
	}

	jsonData, err := json.Marshal(larkMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	req, err := http.NewRequest("POST", l.webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	setIdempotencyKey(req, i)

	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lark API returned non-200 status code: %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}

// SendText implements core.TextSender: the image-fallback path for Lark,
// which cannot upload a binary over a custom-bot webhook. It posts the
// already-redacted report caption + note as an interactive text card. No
// raw incident content is referenced.
func (l *LarkProvider) SendText(i *m.Incident, text string) error {
	larkMsg := utils.CreateLarkMessage(text, i.Resolved)

	jsonData, err := json.Marshal(larkMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal text message: %w", err)
	}

	req, err := http.NewRequest("POST", l.webhookURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send text: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("lark API returned non-200 status code: %d, body: %s", resp.StatusCode, string(body))
	}
	return nil
}
