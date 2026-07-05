package core

import (
	"errors"
	"fmt"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

// AlertProvider is implemented by every notification channel (Slack,
// Telegram, MS Teams, Lark, Viber, Email, …). Name() identifies the
// channel in failure reports so operators can tell which one failed
// without parsing the wrapped error.
type AlertProvider interface {
	Name() string
	SendAlert(incident *m.Incident) error
}

type Alert struct {
	providers []AlertProvider
}

func NewAlert(providers ...AlertProvider) *Alert {
	return &Alert{providers: providers}
}

// AlertResult captures the outcome of SendAllAlerts: which providers
// succeeded, which failed (with their error), and a joined error
// containing all failures (nil when every provider succeeded). The
// list of Succeeded channel names is what the incident persistence
// layer should store as ChannelsNotified — it is the truth about
// what actually reached its destination, not a list of what was
// enabled in config.
type AlertResult struct {
	Succeeded []string
	Failed    map[string]error
	Err       error // joined errors from Failed, nil when none
}

// SendAllAlerts tries every configured provider regardless of earlier
// failures. A single broken channel never prevents the others from
// firing — this is the multi-channel promise. The returned Err is
// non-nil iff at least one provider failed.
func (a *Alert) SendAllAlerts(incident *m.Incident) AlertResult {
	res := AlertResult{Failed: map[string]error{}}
	var errs []error
	for _, p := range a.providers {
		if err := p.SendAlert(incident); err != nil {
			res.Failed[p.Name()] = err
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
			continue
		}
		res.Succeeded = append(res.Succeeded, p.Name())
	}
	if len(errs) > 0 {
		res.Err = errors.Join(errs...)
	}
	return res
}

// SendAlert is the legacy "first error wins" API. New code should use
// SendAllAlerts so a flaky Slack does not silently mute Telegram and
// Email. Kept here so external callers (and existing tests) compile
// without churn; internally we now delegate to SendAllAlerts and
// surface only the joined error.
func (a *Alert) SendAlert(incident *m.Incident) error {
	return a.SendAllAlerts(incident).Err
}

// Attachment is one rendered artifact (today, an incident report image)
// bound for a channel. Caption is the short text summary that image-capable
// channels attach alongside the binary AND that image-incapable channels
// fall back to. The Data bytes are already the final encoded image; the
// text is already redacted by the report assembler.
type Attachment struct {
	Filename string // "incident-<shortid>.png"
	MIME     string // "image/png"
	Data     []byte // the rendered image bytes
	Caption  string // short redacted text summary (also the fallback body)
}

// AttachmentSender is an OPTIONAL capability a notification channel may
// implement on top of the mandatory AlertProvider — exactly like
// storage.Searcher / storage.Lifecycle. A channel that can upload a binary
// image (Slack, Telegram, Email) implements it; a channel that cannot
// (Teams, Viber, Lark webhooks) simply does not, and the report delivery
// path detects the difference with a type assertion. The interface is
// deliberately generic: no per-channel special-casing leaks into the
// caller.
type AttachmentSender interface {
	SendAttachment(incident *m.Incident, att Attachment) error
}

// TextSender is the OPTIONAL text-fallback sibling of AttachmentSender. A
// channel that cannot upload a binary but CAN post text (Teams, Viber, Lark
// webhooks) implements it so the report delivery path can still deliver the
// already-redacted caption + a short note to that channel. Like
// AttachmentSender it is generic and detected via type assertion; a channel
// that implements neither simply receives no report and the caller records
// a graceful fallback outcome.
type TextSender interface {
	SendText(incident *m.Incident, text string) error
}
