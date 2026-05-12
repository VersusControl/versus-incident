package core

import (
	"errors"
	"strings"
	"testing"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

type stubProvider struct {
	name string
	err  error
	hits int
}

func (s *stubProvider) Name() string                  { return s.name }
func (s *stubProvider) SendAlert(_ *m.Incident) error { s.hits++; return s.err }

// TestSendAllAlerts_TriesEveryProvider catches the regression where
// the legacy SendAlert short-circuited on the first error and never
// invoked subsequent providers. The whole point of v1.4.x is
// multi-channel: one broken Slack must not silently mute Telegram
// and Email.
func TestSendAllAlerts_TriesEveryProvider(t *testing.T) {
	slack := &stubProvider{name: "slack", err: errors.New("slack down")}
	tg := &stubProvider{name: "telegram"} // ok
	email := &stubProvider{name: "email", err: errors.New("smtp closed")}

	a := NewAlert(slack, tg, email)
	res := a.SendAllAlerts(&m.Incident{})

	if slack.hits != 1 || tg.hits != 1 || email.hits != 1 {
		t.Fatalf("each provider must be called once; got slack=%d tg=%d email=%d",
			slack.hits, tg.hits, email.hits)
	}
	if got := len(res.Succeeded); got != 1 || res.Succeeded[0] != "telegram" {
		t.Fatalf("Succeeded=%v want [telegram]", res.Succeeded)
	}
	if _, ok := res.Failed["slack"]; !ok {
		t.Fatal("Failed map missing slack")
	}
	if _, ok := res.Failed["email"]; !ok {
		t.Fatal("Failed map missing email")
	}
	if res.Err == nil {
		t.Fatal("Err should be non-nil when any provider fails")
	}
	if !strings.Contains(res.Err.Error(), "slack down") || !strings.Contains(res.Err.Error(), "smtp closed") {
		t.Fatalf("joined err missing details: %v", res.Err)
	}
}

func TestSendAllAlerts_AllSucceed(t *testing.T) {
	a := NewAlert(&stubProvider{name: "a"}, &stubProvider{name: "b"})
	res := a.SendAllAlerts(&m.Incident{})
	if res.Err != nil {
		t.Fatalf("Err should be nil when all succeed: %v", res.Err)
	}
	if got := len(res.Succeeded); got != 2 {
		t.Fatalf("Succeeded len=%d want 2", got)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed=%v want empty", res.Failed)
	}
}

func TestSendAllAlerts_AllFail(t *testing.T) {
	a := NewAlert(
		&stubProvider{name: "a", err: errors.New("boom1")},
		&stubProvider{name: "b", err: errors.New("boom2")},
	)
	res := a.SendAllAlerts(&m.Incident{})
	if len(res.Succeeded) != 0 {
		t.Fatalf("Succeeded=%v want empty", res.Succeeded)
	}
	if len(res.Failed) != 2 {
		t.Fatalf("Failed len=%d want 2", len(res.Failed))
	}
	if res.Err == nil {
		t.Fatal("Err must be non-nil")
	}
}

func TestSendAlertLegacy_ReturnsJoinedErr(t *testing.T) {
	a := NewAlert(
		&stubProvider{name: "a"},
		&stubProvider{name: "b", err: errors.New("boom")},
	)
	if err := a.SendAlert(&m.Incident{}); err == nil {
		t.Fatal("legacy SendAlert must surface the joined error")
	}
}
