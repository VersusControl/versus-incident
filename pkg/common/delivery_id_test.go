package common

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
)

// Webhook channels send a notification's delivery ID as the Idempotency-Key
// header, and a custom template can show it. Without one there is no header,
// and a template that names the field still renders.
func TestWebhookChannelsSendTheDeliveryIDAsIdempotencyKey(t *testing.T) {
	template := filepath.Join(t.TempDir(), "issue.tmpl")
	if err := os.WriteFile(template, []byte(`{{.Workload}} delivery={{.DeliveryID}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	type request struct {
		header  string
		present bool
		body    string
	}
	requests := make(chan request, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_, present := r.Header["Idempotency-Key"]
		requests <- request{header: r.Header.Get("Idempotency-Key"), present: present, body: string(raw)}
	}))
	defer receiver.Close()
	providers := map[string]func() interface{ SendAlert(*m.Incident) error }{
		"msteams": func() interface{ SendAlert(*m.Incident) error } {
			return NewMSTeamsProvider(config.MSTeamsConfig{PowerAutomateURL: receiver.URL, TemplatePath: template, KubernetesIssueTemplatePath: template})
		},
		"lark": func() interface{ SendAlert(*m.Incident) error } {
			return NewLarkProvider(config.LarkConfig{WebhookURL: receiver.URL, TemplatePath: template, KubernetesIssueTemplatePath: template}, config.ProxyConfig{})
		},
	}
	for name, build := range providers {
		t.Run(name, func(t *testing.T) {
			content := map[string]interface{}{"AlertKind": "kubernetes_issue", "Workload": "api", "DeliveryID": "episode-1"}
			if err := build().SendAlert(&m.Incident{ID: "incident", Content: &content, DeliveryID: "episode-1"}); err != nil {
				t.Fatal(err)
			}
			got := <-requests
			if got.header != "episode-1" || !strings.Contains(got.body, "delivery=episode-1") {
				t.Fatalf("with a delivery ID: header=%q body=%s", got.header, got.body)
			}
			plain := map[string]interface{}{"AlertKind": "kubernetes_issue", "Workload": "api"}
			if err := build().SendAlert(&m.Incident{ID: "incident", Content: &plain}); err != nil {
				t.Fatalf("a template naming the field failed without it: %v", err)
			}
			if got := <-requests; got.present || !strings.Contains(got.body, "api") {
				t.Fatalf("without a delivery ID: header present=%t body=%s", got.present, got.body)
			}
		})
	}
}
