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
	webhookURL   string
	templatePath string
}

func NewLarkProvider(cfg config.LarkConfig) *LarkProvider {
	return &LarkProvider{
		webhookURL:   cfg.WebhookURL,
		templatePath: cfg.TemplatePath,
	}
}

func (l *LarkProvider) SendAlert(i *m.Incident) error {
	funcMaps := utils.GetTemplateFuncMaps()

	tmpl, err := template.New(filepath.Base(l.templatePath)).Funcs(funcMaps).ParseFiles(l.templatePath)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}

	var message bytes.Buffer
	if err := tmpl.Execute(&message, i.Content); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}

	// Create Lark message format using utility function
	larkMsg := utils.LarkMessage{
		MsgType: "post",
		Content: utils.LarkMsgContent{
			Post: utils.ConvertToLarkMarkdown(message.String(), i.Resolved),
		},
	}

	jsonData, err := json.Marshal(larkMsg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	resp, err := http.Post(l.webhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Lark API returned non-200 status code: %d, body: %s", resp.StatusCode, string(body))
	}

	return nil
}
