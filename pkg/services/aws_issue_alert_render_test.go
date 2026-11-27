package services

import (
	"bytes"
	"html"
	htmltemplate "html/template"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// channelTemplates are the channels and their own default templates.
var channelTemplates = map[string]string{
	utils.ChannelSlack: "config/slack_message.tmpl", utils.ChannelTelegram: "config/telegram_message.tmpl",
	utils.ChannelMSTeams: "config/msteams_message.tmpl", utils.ChannelLark: "config/lark_message.tmpl",
	utils.ChannelViber: "config/viber_message.tmpl", utils.ChannelEmail: "config/email_message.tmpl",
}

// renderFor renders content the way a channel's provider does: through the
// template it picks for the content, html/template for email.
func renderFor(t *testing.T, channel string, content map[string]interface{}) string {
	t.Helper()
	path := utils.AlertTemplatePath(content, channel, channelTemplates[channel], "")
	var tpl interface{ Execute(io.Writer, any) error }
	var err error
	if channel == utils.ChannelEmail {
		tpl, err = htmltemplate.New(filepath.Base(path)).Funcs(htmltemplate.FuncMap(utils.GetTemplateFuncMaps())).ParseFiles(filepath.Join("../..", path))
	} else {
		tpl, err = template.New(filepath.Base(path)).Funcs(utils.GetTemplateFuncMaps()).ParseFiles(filepath.Join("../..", path))
	}
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, content); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Every channel renders an AWS issue alert with what the mock-up shows:
// severity, title, account and Region, since, the likely cause when one
// was found, the evidence, what to do and a link to the issue; a coverage
// notice says what is not read and how to fix it; a digest lists its
// issues. No channel renders it as an agent detection, and nothing prints
// a format error.
func TestAWSIssueAlertsRenderOnEveryChannel(t *testing.T) {
	loadAgentTestConfig(t)
	opened := issues.Alert{Kind: issues.AlertOpened, Issue: awsAttackIssue(), Key: "opened:1", AccountName: "platform"}
	regressed := issues.Alert{Kind: issues.AlertRegressed, Issue: awsAttackIssue(), Key: "regressed:2", AccountName: "platform",
		Detail: "rejected issue reopened: its material evidence changed"}
	coverage := issues.Alert{Kind: issues.AlertCoverage, Issue: awsAttackIssue(), Key: "coverage:1", AccountName: "platform",
		Unread: []issues.Unread{{Source: "guardduty", Reason: model.GapDenied, LastGoodAt: time.Date(2026, 11, 25, 14, 12, 0, 0, time.UTC),
			Fix: []string{"guardduty:ListFindings"}}}}
	high := awsAttackIssue()
	high.Severity, high.Title = issues.SeverityHigh, "Old access key in use by ci"
	digest := []issues.Alert{{Kind: issues.AlertOpened, Issue: high, Key: "opened:3"}, {Kind: issues.AlertOpened, Issue: high, Key: "opened:4"}}
	cases := map[string]struct {
		content map[string]interface{}
		want    []string
	}{
		"opened": {awsIssueContent(opened, "https://versus.example"), []string{"Critical", "Attack sequence detected on cluster ci-eks",
			"platform · eu-west-1 · cluster ci-eks · since 2026-11-25 13:02 UTC", "GuardDuty attack sequence, 141 runtime signals",
			"Likely cause", "Runner image updated by ci-deployer, 2 h before", "What to do", "Roll the runners back to the image before 11:02",
			"kubectl -n ci rollout undo deployment/ci-runner", "Open issue"}},
		"regressed": {awsIssueContent(regressed, "https://versus.example"), []string{"Critical", "Reopened: Attack sequence detected",
			"platform · eu-west-1", "its material evidence changed", "Likely cause", "What to do", "Open issue"}},
		"coverage": {awsIssueContent(coverage, "https://versus.example"), []string{"Not read", "Versus can no longer read guardduty: permission removed",
			"last good read 2026-11-25 14:12 UTC", "1 open issue can", "guardduty:ListFindings", "Open coverage"}},
		"digest": {awsIssueDigestContent(digest, "https://versus.example"), []string{"High", "2 high issues opened", "Old access key in use by ci",
			"Open issues"}},
	}
	if title := cases["digest"].content["AlertName"]; title != "2 high issues opened" {
		t.Fatalf("digest title = %q; want the count of the issues it holds", title)
	}
	for name, tc := range cases {
		for channel := range channelTemplates {
			t.Run(name+"/"+channel, func(t *testing.T) {
				message := renderFor(t, channel, tc.content)
				if strings.Contains(message, "%!") || strings.Contains(message, "<no value>") || strings.Contains(message, "Versus Agent") {
					t.Fatalf("message carries a format error or the agent header:\n%s", message)
				}
				for _, want := range tc.want {
					if !strings.Contains(message, want) && !strings.Contains(message, htmltemplate.HTMLEscapeString(want)) {
						t.Fatalf("message misses %q:\n%s", want, message)
					}
				}
			})
		}
	}
}

// An agent detection shows its baseline whenever it carries one, zero
// included, and renders cleanly without one: no format error and no
// baseline text.
func TestAgentDetectionBaselineRendersWhenPresent(t *testing.T) {
	content := map[string]interface{}{
		"AlertName": "Error spike in checkout", "Summary": "500s rose", "Severity": "high", "ServiceName": "checkout",
		"Source": "agent:elasticsearch:prod", "PatternID": "p-1", "Frequency": int64(12), "Status": "firing",
	}
	for _, channel := range slices.Sorted(maps.Keys(channelTemplates)) {
		t.Run(channel+"/missing", func(t *testing.T) {
			message := renderFor(t, channel, content)
			if strings.Contains(message, "%!") || !strings.Contains(message, "Error spike in checkout") || strings.Contains(message, "baseline") {
				t.Fatalf("agent detection without a baseline:\n%s", message)
			}
		})
	}
	for baseline, want := range map[float64]string{0: "(baseline 0.00)", 2.5: "(baseline 2.50)"} {
		withBaseline := maps.Clone(content)
		withBaseline["Baseline"] = baseline
		for _, channel := range slices.Sorted(maps.Keys(channelTemplates)) {
			t.Run(channel+"/"+want, func(t *testing.T) {
				if message := renderFor(t, channel, withBaseline); !strings.Contains(message, want) || strings.Contains(message, "%!") {
					t.Fatalf("a baseline of %v is shown as %q:\n%s", baseline, want, message)
				}
			})
		}
	}
}

// An apostrophe shows as an apostrophe on every channel: "can't" in the
// coverage notice and in a title, never an HTML entity where the channel
// shows entities as text. Email is HTML, where an entity is decoded.
func TestAWSIssueAlertApostrophesRenderAsTyped(t *testing.T) {
	loadAgentTestConfig(t)
	issue := awsAttackIssue()
	issue.Title = "Bucket can't be read by the owner's role"
	coverage := issues.Alert{Kind: issues.AlertCoverage, Issue: issue, Key: "coverage:1", AccountName: "platform",
		Unread: []issues.Unread{{Source: "guardduty", Reason: model.GapDenied}}}
	opened := issues.Alert{Kind: issues.AlertOpened, Issue: issue, Key: "opened:1", AccountName: "platform"}
	for name, content := range map[string]map[string]interface{}{
		"coverage": awsIssueContent(coverage, "https://versus.example"),
		"opened":   awsIssueContent(opened, "https://versus.example"),
	} {
		for channel := range channelTemplates {
			t.Run(name+"/"+channel, func(t *testing.T) {
				message := renderFor(t, channel, content)
				if channel == utils.ChannelEmail {
					message = html.UnescapeString(message)
				} else if strings.Contains(message, "&#") || strings.Contains(message, "&apos;") {
					t.Fatalf("an HTML entity reaches %s, which shows it as text:\n%s", channel, message)
				}
				if !strings.Contains(message, "can't") {
					t.Fatalf("\"can't\" does not render as typed:\n%s", message)
				}
			})
		}
	}
}
