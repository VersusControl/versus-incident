package core

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

type urlFailingProvider struct{ name, rawURL string }

func (p urlFailingProvider) Name() string { return p.name }
func (p urlFailingProvider) SendAlert(*m.Incident) error {
	return errors.Join(errors.New("failed to send message"),
		&url.Error{Op: "Post", URL: p.rawURL, Err: errors.New("connection refused")})
}

// A channel whose transport error carries its credential in the URL (the
// Telegram bot token in the path, the Teams signature in the query) reaches
// the fan-out result with the URL cut to scheme://host.
func TestSendAllAlertsRedactsCredentialURLs(t *testing.T) {
	a := &Alert{providers: []AlertProvider{
		urlFailingProvider{"telegram", "http://127.0.0.1:1/botFAKE123:SECRETTOKEN/sendMessage"},
		urlFailingProvider{"msteams", "https://prod.logic.azure.com/workflows/x/invoke?api-version=1&sig=SECRETSIG"},
	}}
	res := a.SendAllAlerts(&m.Incident{})
	if res.Err == nil || len(res.Failed) != 2 {
		t.Fatalf("want two failures, got %+v", res)
	}
	texts := []string{res.Err.Error()}
	for _, err := range res.Failed {
		texts = append(texts, err.Error())
	}
	for _, s := range texts {
		if strings.Contains(s, "SECRET") {
			t.Fatalf("credential leaked: %s", s)
		}
	}
	if !strings.Contains(res.Err.Error(), `"http://127.0.0.1:1"`) || !strings.Contains(res.Err.Error(), `"https://prod.logic.azure.com"`) {
		t.Fatalf("hosts should remain for diagnosis: %s", res.Err.Error())
	}
}
