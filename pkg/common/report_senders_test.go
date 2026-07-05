package common

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	m "github.com/VersusControl/versus-incident/pkg/models"

	"github.com/slack-go/slack"
)

// Compile-time capability assertions: the image-capable channels implement
// AttachmentSender; the webhook-only channels implement the TextSender
// fallback. If any of these regress the report delivery path degrades.
var (
	_ core.AttachmentSender = (*SlackProvider)(nil)
	_ core.AttachmentSender = (*TelegramProvider)(nil)
	_ core.AttachmentSender = (*EmailProvider)(nil)
	_ core.TextSender       = (*MSTeamsProvider)(nil)
	_ core.TextSender       = (*ViberProvider)(nil)
	_ core.TextSender       = (*LarkProvider)(nil)
)

// TestReportCapabilityDetection mirrors how services.sendReport routes: an
// image-capable channel type-asserts to AttachmentSender; an image-incapable
// one does not (and instead asserts to TextSender).
func TestReportCapabilityDetection(t *testing.T) {
	var slackP core.AlertProvider = &SlackProvider{}
	if _, ok := slackP.(core.AttachmentSender); !ok {
		t.Fatal("slack should implement AttachmentSender")
	}
	var teamsP core.AlertProvider = &MSTeamsProvider{}
	if _, ok := teamsP.(core.AttachmentSender); ok {
		t.Fatal("teams must NOT implement AttachmentSender (webhook can't upload a binary)")
	}
	if _, ok := teamsP.(core.TextSender); !ok {
		t.Fatal("teams should implement the TextSender fallback")
	}
}

// captureRT is a fake http.RoundTripper that records the last request + body
// and returns a canned response.
type captureRT struct {
	lastReq  *http.Request
	lastBody []byte
	status   int
	respBody string
}

func (c *captureRT) RoundTrip(r *http.Request) (*http.Response, error) {
	c.lastReq = r
	if r.Body != nil {
		c.lastBody, _ = io.ReadAll(r.Body)
	}
	st := c.status
	if st == 0 {
		st = http.StatusOK
	}
	body := c.respBody
	if body == "" {
		body = `{"ok":true}`
	}
	return &http.Response{
		StatusCode: st,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

func pngAttachment() core.Attachment {
	return core.Attachment{
		Filename: "incident-abcdef01.png",
		MIME:     "image/png",
		Data:     []byte("\x89PNG\r\n\x1a\nFAKEPNGDATA"),
		Caption:  "Incident report: Pool exhausted\nservice payments · severity critical",
	}
}

// --- Slack (files.upload v2 external flow) ---------------------------------

func TestSlackProvider_SendAttachment(t *testing.T) {
	var uploadedBody []byte
	var completeForm string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/files.getUploadURLExternal"):
			w.Header().Set("Content-Type", "application/json")
			// upload_url points back at this server's /upload route.
			_, _ = io.WriteString(w, `{"ok":true,"upload_url":"`+srv.URL+`/upload","file_id":"F1"}`)
		case strings.HasSuffix(r.URL.Path, "/upload"):
			uploadedBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/files.completeUploadExternal"):
			b, _ := io.ReadAll(r.Body)
			completeForm = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true,"files":[{"id":"F1","title":"incident-abcdef01.png"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := slack.New("xoxb-test", slack.OptionAPIURL(srv.URL+"/"))
	p := &SlackProvider{client: client, channelID: "C123"}

	if err := p.SendAttachment(&m.Incident{}, pngAttachment()); err != nil {
		t.Fatalf("SendAttachment: %v", err)
	}
	if !strings.Contains(string(uploadedBody), "FAKEPNGDATA") {
		t.Fatalf("upload did not carry the PNG bytes; got %q", uploadedBody)
	}
	if !strings.Contains(completeForm, "initial_comment") {
		t.Fatalf("complete step missing caption/initial_comment: %q", completeForm)
	}
}

// --- Telegram (sendPhoto multipart) ----------------------------------------

func TestTelegramProvider_SendAttachment(t *testing.T) {
	rt := &captureRT{}
	p := &TelegramProvider{botToken: "T", chatID: "chat-1", client: &http.Client{Transport: rt}}

	if err := p.SendAttachment(&m.Incident{}, pngAttachment()); err != nil {
		t.Fatalf("SendAttachment: %v", err)
	}
	if !strings.HasSuffix(rt.lastReq.URL.Path, "/sendPhoto") {
		t.Fatalf("expected sendPhoto, got %s", rt.lastReq.URL.Path)
	}
	// Parse the multipart body and assert the photo + caption fields.
	mt, params, err := mime.ParseMediaType(rt.lastReq.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		t.Fatalf("content-type = %q (err %v)", rt.lastReq.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(strings.NewReader(string(rt.lastBody)), params["boundary"])
	var sawPhoto, sawCaption, sawChat bool
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(part)
		switch part.FormName() {
		case "photo":
			if strings.Contains(string(body), "FAKEPNGDATA") {
				sawPhoto = true
			}
		case "caption":
			if strings.Contains(string(body), "Pool exhausted") {
				sawCaption = true
			}
		case "chat_id":
			if string(body) == "chat-1" {
				sawChat = true
			}
		}
	}
	if !sawPhoto || !sawCaption || !sawChat {
		t.Fatalf("multipart missing fields: photo=%v caption=%v chat=%v", sawPhoto, sawCaption, sawChat)
	}
}

// --- Email (multipart/related MIME) ----------------------------------------

func TestBuildReportMIME(t *testing.T) {
	att := pngAttachment()
	msg := string(buildReportMIME("from@x.com", "to@y.com", "Incident report", att))

	for _, want := range []string{
		"From: from@x.com",
		"To: to@y.com",
		"Subject: Incident report",
		"Content-Type: multipart/related;",
		"Content-Type: image/png",
		"Content-ID: <report>",
		"cid:report",
		"Pool exhausted", // caption in the HTML body
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("MIME message missing %q\n---\n%s", want, msg)
		}
	}
	// The PNG must be base64-encoded, never raw.
	if strings.Contains(msg, "FAKEPNGDATA") {
		t.Fatal("raw PNG bytes should not appear un-encoded in the MIME message")
	}
}

func TestEmailProvider_SendAttachment_NoRecipients(t *testing.T) {
	p := &EmailProvider{to: ""}
	if err := p.SendAttachment(&m.Incident{}, pngAttachment()); err == nil {
		t.Fatal("expected error with no recipients")
	}
}

// --- Teams / Viber / Lark text fallback ------------------------------------

func TestMSTeamsProvider_SendText(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &MSTeamsProvider{powerAutomateURL: srv.URL}
	if err := p.SendText(&m.Incident{}, "plain fallback text for teams"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if !strings.Contains(got, "plain fallback text for teams") {
		t.Fatalf("teams payload missing text: %q", got)
	}
}

func TestViberProvider_SendText(t *testing.T) {
	rt := &captureRT{}
	p := &ViberProvider{apiType: "channel", channelID: "chan-1", botToken: "tok", client: &http.Client{Transport: rt}}
	if err := p.SendText(&m.Incident{}, "viber fallback text"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	var payload struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rt.lastBody, &payload); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rt.lastBody)
	}
	if payload.Type != "text" || payload.Text != "viber fallback text" {
		t.Fatalf("viber payload = %+v", payload)
	}
}

func TestLarkProvider_SendText(t *testing.T) {
	rt := &captureRT{}
	p := &LarkProvider{webhookURL: "https://open.larksuite.com/hook", client: &http.Client{Transport: rt}}
	if err := p.SendText(&m.Incident{Resolved: false}, "lark fallback text"); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if !strings.Contains(string(rt.lastBody), "lark fallback text") {
		t.Fatalf("lark payload missing text: %q", rt.lastBody)
	}
}
