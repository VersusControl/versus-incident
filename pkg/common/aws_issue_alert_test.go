package common

import (
	"bytes"
	"encoding/json"
	htmltemplate "html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"unicode/utf16"

	"github.com/slack-go/slack"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// alertOrigin is the public host the AWS issue links point at.
const alertOrigin = "https://versus.example"

// worstLine is a 512-byte line of the characters each channel's escaping
// and encoding grows the most: & (Slack and Telegram entities, a JSON
// escape), < (a JSON escape) and * (Slack keeps it apart with zero-width
// spaces, Markdown with a backslash).
var worstLine = strings.Repeat("&<*_", 128)

// awsAlertContent is the content of an AWS issue alert of a kind, as the
// alert path builds it: marked as built by Versus, with its link.
func awsAlertContent(kind, severity string) map[string]interface{} {
	link := alertOrigin + "/aws/issues/issue-1"
	label := "Open issue"
	switch kind {
	case "coverage":
		link, label = alertOrigin+"/aws/coverage", "Open coverage"
	case "digest":
		link, label = alertOrigin+"/aws/issues", "Open issues"
	}
	content := map[string]interface{}{
		"AlertKind": "aws_issue", "IssueAlert": kind, "Severity": severity, "AlertName": "Root key used",
		"Where": "platform · eu-west-1 · since 2026-11-25 13:02 UTC", "Status": "firing", "IssueURL": link,
		"Links": []map[string]string{{"id": "issue", "label": label, "url": link}},
	}
	utils.MarkInternalAlert(content)
	return content
}

// largestAWSAlert is an alert at every bound the alert path allows: each
// line 512 bytes of the worst characters, five evidence lines, five steps
// each with a command, a likely cause and a reason it reopened, and for a
// coverage notice or a digest the most notes they carry.
func largestAWSAlert(kind string) map[string]interface{} {
	severity := "critical"
	if kind == "digest" {
		severity = "high"
	}
	content := awsAlertContent(kind, severity)
	content["AlertName"] = "Root key used " + worstLine[:498]
	content["Where"] = "platform · " + worstLine[:500]
	switch kind {
	case "coverage", "digest":
		notes := []string{}
		for range 13 {
			notes = append(notes, worstLine)
		}
		content["Notes"] = notes
	default:
		evidence := []string{}
		steps := []map[string]string{}
		for range 5 {
			evidence = append(evidence, worstLine)
			steps = append(steps, map[string]string{"text": worstLine, "command": "aws s3 ls " + worstLine[:502]})
		}
		content["Evidence"], content["Steps"] = evidence, steps
		content["LikelyCause"], content["Reopened"] = worstLine, worstLine
	}
	return content
}

// sentPayloads sends content through every alert channel's real provider
// and returns what each one sent: the request body, or for Slack the
// posted attachment and for email the rendered body.
func sentPayloads(t *testing.T, content map[string]interface{}) map[string]string {
	t.Helper()
	t.Chdir("../..")
	got := map[string]string{}
	capture := func(channel string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if channel == "slack" {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				got[channel] = r.Form.Get("attachments")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"channel":"c","ts":"1"}`))
				return
			}
			raw, _ := io.ReadAll(r.Body)
			got[channel] = string(raw)
			_, _ = w.Write([]byte(`{"ok":true,"status":0}`))
		}))
		t.Cleanup(server.Close)
		return server
	}
	via := func(server *httptest.Server) *http.Client {
		return &http.Client{Transport: rewriteTo(server.URL)}
	}
	slackProvider := NewSlackProvider(config.SlackConfig{})
	slackProvider.publicHost = alertOrigin
	slackProvider.client = slack.New("token", slack.OptionAPIURL(capture("slack").URL+"/"))
	telegram := NewTelegramProvider(config.TelegramConfig{BotToken: "t", ChatID: "c"}, config.ProxyConfig{})
	telegram.client = via(capture("telegram"))
	viber := NewViberProvider(config.ViberConfig{BotToken: "t", UserID: "u"}, config.ProxyConfig{})
	viber.client = via(capture("viber"))
	teams := NewMSTeamsProvider(config.MSTeamsConfig{PowerAutomateURL: capture("msteams").URL})
	lark := NewLarkProvider(config.LarkConfig{WebhookURL: capture("lark").URL}, config.ProxyConfig{})
	for name, provider := range map[string]interface{ SendAlert(*m.Incident) error }{
		"slack": slackProvider, "telegram": telegram, "viber": viber, "msteams": teams, "lark": lark,
	} {
		incident := content
		if err := provider.SendAlert(&m.Incident{ID: "incident-1", Content: &incident}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	email, err := NewEmailProvider(config.EmailConfig{}).renderAlertTemplate(&m.Incident{ID: "incident-1", Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	got["email"] = email
	return got
}

// rewriteTo sends every request to one local server.
type rewriteTo string

func (target rewriteTo) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.URL.Scheme, clone.URL.Host = "http", strings.TrimPrefix(string(target), "http://")
	clone.RequestURI = ""
	return http.DefaultTransport.RoundTrip(clone)
}

func utf16Length(text string) int { return len(utf16.Encode([]rune(text))) }

// slackSection is the text Slack checks against its section limit.
func slackSection(t *testing.T, attachments string) (string, map[string]interface{}) {
	t.Helper()
	var list []map[string]interface{}
	if err := json.Unmarshal([]byte(attachments), &list); err != nil || len(list) != 1 {
		t.Fatalf("slack attachments %q: %v", attachments, err)
	}
	blocks, _ := list[0]["blocks"].([]interface{})
	if len(blocks) == 0 {
		text, _ := list[0]["text"].(string)
		return text, list[0]
	}
	section := blocks[0].(map[string]interface{})["text"].(map[string]interface{})["text"].(string)
	return section, list[0]
}

func jsonField(t *testing.T, body, field string) string {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("payload %q: %v", body, err)
	}
	text, _ := payload[field].(string)
	return text
}

// The largest alert the alert path can build reaches every channel within
// its hard limit: lower-priority lines are dropped and the rest shortened,
// each cut says what is left on the issue page, and the severity, title,
// where and link stay.
func TestLargestAWSAlertFitsEveryChannel(t *testing.T) {
	for _, kind := range []string{"regressed", "coverage", "digest"} {
		t.Run(kind, func(t *testing.T) {
			sent := sentPayloads(t, largestAWSAlert(kind))
			section, _ := slackSection(t, sent["slack"])
			telegram := jsonField(t, sent["telegram"], "text")
			viber := jsonField(t, sent["viber"], "text")
			for channel, check := range map[string]struct {
				size, limit int
				text        string
			}{
				"slack":    {utf16Length(section), 3000, section},
				"telegram": {utf16Length(telegram), 4096, telegram},
				"viber":    {utf16Length(viber), 7000, viber},
				"msteams":  {len(sent["msteams"]), 28000, sent["msteams"]},
				"lark":     {len(sent["lark"]), 30000, sent["lark"]},
			} {
				if check.size > check.limit {
					t.Errorf("%s: %d over its limit of %d", channel, check.size, check.limit)
				}
				for _, want := range []string{"Root key used", "platform", "more "} {
					if !strings.Contains(check.text, want) {
						t.Errorf("%s: the fitted alert lost %q:\n%s", channel, want, check.text)
					}
				}
			}
			for channel, body := range map[string]string{"telegram": telegram, "viber": viber, "msteams": sent["msteams"], "lark": sent["lark"]} {
				if !strings.Contains(body, alertOrigin) {
					t.Errorf("%s: the fitted alert lost its link:\n%s", channel, body)
				}
			}
			if !strings.Contains(sent["slack"], alertOrigin) {
				t.Errorf("slack: the fitted alert lost its link button:\n%s", sent["slack"])
			}
		})
	}
}

// A command in "What to do" reaches Teams as a monospace text run, which
// Teams never reads as Markdown: backticks, asterisks and a link in it
// copy exactly. A command that starts with a fence cannot close its block
// and hide the steps after it.
func TestTeamsAWSAlertCommandIsExactCode(t *testing.T) {
	const command = "aws s3api list-objects-v2 --query 'Contents[?StorageClass==`STANDARD`].Key' | grep -v '*tmp*' [x](https://y.example)"
	content := awsAlertContent("opened", "critical")
	content["Steps"] = []map[string]string{
		{"text": "List the objects", "command": command},
		{"text": "Then this step", "command": "```aws s3 ls"},
		{"text": "And the last step", "command": "aws sts get-caller-identity"},
	}
	card := sentPayloads(t, content)["msteams"]
	for _, want := range []string{command, "```aws s3 ls", "aws sts get-caller-identity"} {
		run, _ := json.Marshal(map[string]string{"fontType": "Monospace", "text": want, "type": "TextRun"})
		if !strings.Contains(card, string(run)) {
			t.Errorf("the card does not show %q as a monospace text run:\n%s", want, card)
		}
	}
	if !strings.Contains(card, "And the last step") {
		t.Fatalf("a command starting with a fence closed its block and hid what follows:\n%s", card)
	}
}

// Lark shows a command in a code block it cannot close, and no text or
// command of an alert can mention everyone.
func TestLarkAWSAlertCommandAndMentions(t *testing.T) {
	content := awsAlertContent("opened", "critical")
	content["AlertName"] = "Root key used <at id=all></at>"
	content["Steps"] = []map[string]string{
		{"text": "Run <at user_id=\"all\">x</at>", "command": "```aws s3 ls"},
		{"text": "And the last step", "command": "echo < at id=all></at>"},
	}
	var payload struct {
		Card struct {
			Elements []struct {
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"card"`
	}
	if err := json.Unmarshal([]byte(sentPayloads(t, content)["lark"]), &payload); err != nil || len(payload.Card.Elements) != 1 {
		t.Fatalf("lark payload: %v", err)
	}
	text := payload.Card.Elements[0].Content
	if !strings.Contains(text, "~~~\n```aws s3 ls\n~~~") {
		t.Errorf("a command starting with backticks is not fenced apart from them:\n%s", text)
	}
	for _, mention := range []string{"<at", "< at"} {
		if strings.Contains(strings.ToLower(text), mention) {
			t.Errorf("a mention tag %q reaches Lark:\n%s", mention, text)
		}
	}
}

// The frame of an AWS issue alert follows what it is: Slack's colour bar,
// Lark's header and Teams' accent are red for a critical issue, orange for
// a high one and a digest of high issues, and grey for a coverage notice.
// Teams shows the link as a button.
func TestAWSAlertFrameFollowsTheAlert(t *testing.T) {
	digest := awsAlertContent("digest", "high")
	digest["AlertName"] = "4 high issues opened"
	for name, tc := range map[string]struct {
		content                  map[string]interface{}
		slack, lark, larkHeading string
		teams, button            string
	}{
		"critical": {awsAlertContent("opened", "critical"), "#C70039", "red", "🔴 Critical AWS issue", "attention", "Open issue"},
		"high":     {awsAlertContent("opened", "high"), "#D99A00", "orange", "🟠 High AWS issue", "warning", "Open issue"},
		"digest":   {digest, "#D99A00", "orange", "🟠 4 high issues", "warning", "Open issues"},
		"coverage": {awsAlertContent("coverage", "medium"), "#9E9E9E", "grey", "⚪ Coverage notice", "emphasis", "Open coverage"},
	} {
		t.Run(name, func(t *testing.T) {
			sent := sentPayloads(t, tc.content)
			if _, attachment := slackSection(t, sent["slack"]); attachment["color"] != tc.slack {
				t.Errorf("slack colour = %v; want %s", attachment["color"], tc.slack)
			}
			var lark struct {
				Card struct {
					Header struct {
						Title    struct{ Content string } `json:"title"`
						Template string                   `json:"template"`
					} `json:"header"`
				} `json:"card"`
			}
			if err := json.Unmarshal([]byte(sent["lark"]), &lark); err != nil {
				t.Fatal(err)
			}
			if lark.Card.Header.Template != tc.lark || lark.Card.Header.Title.Content != tc.larkHeading {
				t.Errorf("lark header = %q %q; want %q %q", lark.Card.Header.Title.Content, lark.Card.Header.Template, tc.larkHeading, tc.lark)
			}
			var card utils.AdaptiveCard
			if err := json.Unmarshal([]byte(sent["msteams"]), &card); err != nil {
				t.Fatal(err)
			}
			heading, _ := card.Body[0].(map[string]interface{})
			if heading["style"] != tc.teams {
				t.Errorf("teams accent = %v; want %s", heading["style"], tc.teams)
			}
			button, _ := json.Marshal(map[string]string{"title": tc.button, "type": "Action.OpenUrl", "url": tc.content["IssueURL"].(string)})
			actions, _ := json.Marshal(card.Actions)
			if !bytes.Contains(actions, button) {
				t.Errorf("teams actions = %s; want the button %s", actions, button)
			}
			if strings.Contains(sent["msteams"], "["+tc.button+"]") {
				t.Errorf("teams still shows the link as text:\n%s", sent["msteams"])
			}
		})
	}
}

// A webhook body that names the AWS or Kubernetes issue kind is not one:
// only content Versus built picks those templates.
func TestWebhookContentCannotPickTheIssueTemplates(t *testing.T) {
	for _, kind := range []string{"aws_issue", "kubernetes_issue"} {
		var content map[string]interface{}
		if err := json.Unmarshal([]byte(`{"AlertKind":"`+kind+`","IssueAlert":"opened","Severity":"critical","AlertName":"spoof","InternalAlert":true}`), &content); err != nil {
			t.Fatal(err)
		}
		for _, channel := range []string{utils.ChannelSlack, utils.ChannelTelegram, utils.ChannelMSTeams, utils.ChannelLark, utils.ChannelViber, utils.ChannelEmail} {
			if got := utils.AlertTemplatePath(content, channel, "own.tmpl", "issue.tmpl"); got != "own.tmpl" {
				t.Errorf("%s from a webhook picks %s on %s; want the channel's own", kind, got, channel)
			}
		}
		built := map[string]interface{}{"AlertKind": kind}
		utils.MarkInternalAlert(built)
		if got := utils.AlertTemplatePath(built, utils.ChannelSlack, "own.tmpl", "issue.tmpl"); got == "own.tmpl" {
			t.Errorf("%s built by Versus lost its template", kind)
		}
		stored, _ := json.Marshal(built)
		if string(stored) != `{"AlertKind":"`+kind+`","InternalAlert":true}` {
			t.Errorf("the mark is stored as %s", stored)
		}
	}
}

// A user's own channel template reads the content the provider has always
// given it: Slack the content map, the others a pointer to it. A template
// that indexes or counts the root renders exactly as it did.
func TestUserTemplatesReadTheRootAsBefore(t *testing.T) {
	const source = `{{ index . "service-name" }} {{ len . }}`
	path := filepath.Join(t.TempDir(), "user.tmpl")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	content := map[string]interface{}{"service-name": "checkout", "Status": "firing"}
	before := func(data any) string {
		tmpl := template.Must(template.New("user.tmpl").Funcs(utils.GetTemplateFuncMaps()).Parse(source))
		var out bytes.Buffer
		if err := tmpl.Execute(&out, data); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	got := map[string]string{}
	receiver := func(channel string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if channel == "slack" {
				_ = r.ParseForm()
				got[channel] = r.Form.Get("attachments")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"channel":"c","ts":"1"}`))
				return
			}
			raw, _ := io.ReadAll(r.Body)
			got[channel] = string(raw)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(server.Close)
		return server
	}
	slackProvider := NewSlackProvider(config.SlackConfig{TemplatePath: path, MessageProperties: config.SlackMessageProperties{DisableButton: true}})
	slackProvider.client = slack.New("token", slack.OptionAPIURL(receiver("slack").URL+"/"))
	telegram := NewTelegramProvider(config.TelegramConfig{TemplatePath: path, BotToken: "t", ChatID: "c"}, config.ProxyConfig{})
	telegram.client = &http.Client{Transport: rewriteTo(receiver("telegram").URL)}
	viber := NewViberProvider(config.ViberConfig{TemplatePath: path, BotToken: "t", UserID: "u"}, config.ProxyConfig{})
	viber.client = &http.Client{Transport: rewriteTo(receiver("viber").URL)}
	teams := NewMSTeamsProvider(config.MSTeamsConfig{TemplatePath: path, PowerAutomateURL: receiver("msteams").URL})
	lark := NewLarkProvider(config.LarkConfig{TemplatePath: path, WebhookURL: receiver("lark").URL}, config.ProxyConfig{})
	for name, provider := range map[string]interface{ SendAlert(*m.Incident) error }{
		"slack": slackProvider, "telegram": telegram, "viber": viber, "msteams": teams, "lark": lark,
	} {
		incident := content
		if err := provider.SendAlert(&m.Incident{ID: "incident-1", Content: &incident}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var attachments []map[string]interface{}
	if err := json.Unmarshal([]byte(got["slack"]), &attachments); err != nil || len(attachments) != 1 {
		t.Fatalf("slack attachments %q: %v", got["slack"], err)
	}
	if text := attachments[0]["text"]; text != "checkout 2" || text != before(content) {
		t.Errorf("slack renders %q; want %q, as before", text, before(content))
	}
	pointer := before(&content)
	for channel, text := range map[string]string{
		"telegram": jsonField(t, got["telegram"], "text"),
		"viber":    jsonField(t, got["viber"], "text"),
		"msteams":  jsonField(t, got["msteams"], "messageText"),
	} {
		if text != pointer {
			t.Errorf("%s renders %q; want %q, as before", channel, text, pointer)
		}
	}
	var card struct {
		Card struct {
			Elements []struct{ Content string } `json:"elements"`
		} `json:"card"`
	}
	if err := json.Unmarshal([]byte(got["lark"]), &card); err != nil || len(card.Card.Elements) != 1 || card.Card.Elements[0].Content != strings.TrimSpace(pointer) {
		t.Errorf("lark renders %s; want %q, as before (%v)", got["lark"], pointer, err)
	}
	email, err := NewEmailProvider(config.EmailConfig{TemplatePath: path}).renderAlertTemplate(&m.Incident{ID: "incident-1", Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	emailTemplate := htmltemplate.Must(htmltemplate.New("user.tmpl").Funcs(htmltemplate.FuncMap(utils.GetTemplateFuncMaps())).Parse(source))
	var emailBefore bytes.Buffer
	if err := emailTemplate.Execute(&emailBefore, &content); err != nil {
		t.Fatal(err)
	}
	if email != emailBefore.String() {
		t.Errorf("email renders %q; want %q, as before", email, emailBefore.String())
	}
}
