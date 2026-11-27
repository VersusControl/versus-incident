package services

import (
	"encoding/json"
	"html"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// Markup a user can put in the text of an AWS issue (a resource name, a
// tag, an alarm name, a reject reason): an HTML tag, emphasis and a link.
const (
	userTag      = "<b>tagged</b>"
	userEmphasis = "*bold*"
	userLink     = "[link](https://evil.example)"
	userMarkup   = userTag + " " + userEmphasis + " " + userLink
)

// markupIssue is the attack issue with user markup in every text a user
// can set: the title, the evidence, the likely cause and the steps.
func markupIssue() issues.Issue {
	issue := awsAttackIssue()
	issue.Title = "Bucket " + userMarkup + " is public"
	issue.Evidence = []issues.Evidence{{Text: "Policy names " + userMarkup, Status: issues.EvidenceProven}}
	issue.Cause = &issues.Cause{Status: "found", Text: "Tag set to " + userMarkup}
	issue.Steps = []issues.Step{{Text: "Remove " + userMarkup, Command: "aws s3api delete-bucket-policy --bucket " + userMarkup}}
	return issue
}

// markupAlerts are the four AWS issue alert kinds, each carrying user
// markup, with the parts each must still show.
func markupAlerts() map[string]struct {
	content map[string]interface{}
	want    []string
} {
	host := "https://versus.example"
	opened := issues.Alert{Kind: issues.AlertOpened, Issue: markupIssue(), Key: "opened:1", AccountName: "team " + userMarkup}
	regressed := issues.Alert{Kind: issues.AlertRegressed, Issue: markupIssue(), Key: "regressed:2", AccountName: "platform",
		Detail: "rejected issue reopened: " + userMarkup}
	coverage := issues.Alert{Kind: issues.AlertCoverage, Issue: markupIssue(), Key: "coverage:1", AccountName: "team " + userMarkup,
		Unread: []issues.Unread{{Source: "guardduty", Reason: model.GapDenied, LastGoodAt: time.Date(2026, 11, 25, 14, 12, 0, 0, time.UTC),
			Fix: []string{"guardduty:ListFindings"}}}}
	high := markupIssue()
	high.Severity = issues.SeverityHigh
	digest := []issues.Alert{{Kind: issues.AlertOpened, Issue: high, Key: "opened:3"}, {Kind: issues.AlertOpened, Issue: high, Key: "opened:4"}}
	return map[string]struct {
		content map[string]interface{}
		want    []string
	}{
		"opened": {awsIssueContent(opened, host), []string{"Critical", "Bucket", "is public", "eu-west-1", "since 2026-11-25 13:02 UTC",
			"Policy names", "Likely cause", "Tag set to", "What to do", "Remove", "delete-bucket-policy", "Open issue"}},
		"regressed": {awsIssueContent(regressed, host), []string{"Critical", "Reopened: Bucket", "platform · eu-west-1",
			"rejected issue reopened", "Policy names", "Likely cause", "What to do", "Open issue"}},
		"coverage": {awsIssueContent(coverage, host), []string{"Not read", "Versus can no longer read guardduty",
			"last good read 2026-11-25 14:12 UTC", "is public", "guardduty:ListFindings", "Open coverage"}},
		"digest": {awsIssueDigestContent(digest, host), []string{"High", "2 high issues opened", "Bucket", "Open issues"}},
	}
}

// User markup in an AWS issue alert is shown as text on every channel,
// never as markup: no HTML tag on the HTML and HTML-like channels (Slack
// mrkdwn treats <...> as a link or mention), no emphasis or link on the
// Markdown channels. Viber shows plain text, so the text stays as it was
// typed. The escaped text is still there to read.
func TestAWSIssueAlertUserMarkupIsTextOnEveryChannel(t *testing.T) {
	loadAgentTestConfig(t)
	// raw is the user markup a channel would render as markup.
	raw := map[string][]string{
		utils.ChannelSlack:    {userTag, userEmphasis},
		utils.ChannelTelegram: {userTag},
		utils.ChannelEmail:    {userTag},
		utils.ChannelLark:     {userTag, userEmphasis, userLink},
		utils.ChannelMSTeams:  {userTag, userEmphasis, userLink},
		utils.ChannelViber:    nil,
	}
	alerts := markupAlerts()
	for _, name := range slices.Sorted(maps.Keys(alerts)) {
		tc := alerts[name]
		for _, channel := range slices.Sorted(maps.Keys(raw)) {
			active := raw[channel]
			t.Run(name+"/"+channel, func(t *testing.T) {
				message := renderFor(t, channel, tc.content)
				if strings.Contains(message, "%!") || strings.Contains(message, "<no value>") || strings.Contains(message, "Versus Agent") {
					t.Fatalf("message carries a format error or the agent header:\n%s", message)
				}
				for _, want := range tc.want {
					if !strings.Contains(message, want) {
						t.Errorf("message misses %q:\n%s", want, message)
					}
				}
				for _, markup := range active {
					// A command is shown as code, where emphasis and links
					// are not markup; an HTML tag stays escaped even there,
					// since Slack, Telegram and email still read it.
					shown := outsideCode(message)
					if markup == userTag && channel != utils.ChannelLark && channel != utils.ChannelMSTeams {
						shown = message
					}
					if strings.Contains(shown, markup) {
						t.Errorf("user markup %q reaches %s as markup:\n%s", markup, channel, message)
					}
				}
				if !strings.Contains(message, "tagged") || !strings.Contains(message, "bold") || !strings.Contains(message, "link") {
					t.Errorf("the user text is gone instead of escaped:\n%s", message)
				}
				if channel == utils.ChannelViber && !strings.Contains(message, userMarkup) {
					t.Errorf("viber is plain text and shows the user text as typed:\n%s", message)
				}
			})
		}
	}
}

// codeForm matches the code forms a command is shown in: a fenced block,
// Slack inline code, and HTML code.
var codeForm = regexp.MustCompile("(?s)```+.*?```+|`[^`\n]*`|<code>.*?</code>")

// outsideCode is the message without its code, the text a channel renders
// as markup.
func outsideCode(message string) string {
	return codeForm.ReplaceAllString(message, "")
}

// Each channel picks the AWS issue template only for an AWS issue alert:
// a plain incident keeps the channel's own template, an agent detection
// the agent template, and a Kubernetes issue its own.
func TestAlertTemplatePathKeepsOtherAlertsOnTheirTemplates(t *testing.T) {
	agent := map[string]interface{}{"AlertName": "Error spike", "Source": "agent:elasticsearch:prod", "PatternID": "p-1"}
	aws := awsIssueContent(issues.Alert{Kind: issues.AlertOpened, Issue: awsAttackIssue(), Key: "opened:1"}, "")
	plain := map[string]interface{}{"AlertName": "Disk full", "Logs": "disk /var is 98% full"}
	for _, channel := range slices.Sorted(maps.Keys(channelTemplates)) {
		own := channelTemplates[channel]
		if got := utils.AlertTemplatePath(plain, channel, own, ""); got != own {
			t.Errorf("%s: a plain incident renders with %s; want its own %s", channel, got, own)
		}
		if got := utils.AlertTemplatePath(nil, channel, own, ""); got != own {
			t.Errorf("%s: no content renders with %s; want its own %s", channel, got, own)
		}
		if got := utils.AlertTemplatePath(agent, channel, own, ""); !strings.HasPrefix(got, "config/agent_") {
			t.Errorf("%s: an agent detection renders with %s; want the agent template", channel, got)
		}
		if got := utils.AlertTemplatePath(aws, channel, own, ""); got != "config/aws_issue_"+channel+".tmpl" {
			t.Errorf("%s: an AWS issue renders with %s; want its AWS issue template", channel, got)
		}
	}
	if got := utils.AlertTemplatePath(aws, "pagerduty", "custom.tmpl", ""); got != "custom.tmpl" {
		t.Errorf("a channel without an AWS issue template renders with %s; want its own", got)
	}
}

// A plain incident renders through each channel's own template as it did
// before AWS issue alerts: no format error, and no AWS issue or agent
// header.
func TestPlainIncidentRendersOnEveryChannel(t *testing.T) {
	content := map[string]interface{}{
		"alertname": "DiskFull", "summary": "disk /var on db-1 is 98% full", "status": "firing",
		"severity": "critical", "instance": "db-1",
	}
	for _, channel := range slices.Sorted(maps.Keys(channelTemplates)) {
		t.Run(channel, func(t *testing.T) {
			message := renderFor(t, channel, content)
			if strings.TrimSpace(message) == "" || strings.Contains(message, "%!") || strings.Contains(message, "<no value>") ||
				strings.Contains(message, "Versus Agent") || strings.Contains(message, "Not read") || strings.Contains(message, "What to do") {
				t.Fatalf("plain incident:\n%s", message)
			}
		})
	}
}

// A command in "What to do" is shown as code on every channel and copies
// exactly as written: underscores, asterisks, backslashes and brackets
// are not escaped into it. Only what the code form itself needs is
// encoded (Slack's and HTML's &, < and >), which the client decodes.
func TestAWSIssueAlertCommandCopiesExactlyOnEveryChannel(t *testing.T) {
	loadAgentTestConfig(t)
	const command = `aws ec2 describe-instances --filters Name=tag:deploy_role,Values=a*b* --query Reservations[*].Instances[0].InstanceId --output text \ && echo done`
	issue := awsAttackIssue()
	issue.Steps = []issues.Step{{Text: "List the instances of the deploy role", Command: command}}
	content := awsIssueContent(issues.Alert{Kind: issues.AlertOpened, Issue: issue, Key: "opened:1"}, "")
	code := map[string]*regexp.Regexp{
		utils.ChannelSlack:    regexp.MustCompile("\n`([^`\n]*)`\n"),
		utils.ChannelTelegram: regexp.MustCompile(`<code>(.*?)</code>`),
		utils.ChannelEmail:    regexp.MustCompile(`<code>(.*?)</code>`),
		utils.ChannelMSTeams:  regexp.MustCompile("\n```\n([^\n]*)\n```\n"),
		utils.ChannelLark:     regexp.MustCompile("\n```\n([^\n]*)\n```\n"),
		utils.ChannelViber:    regexp.MustCompile("\n(aws [^\n]*)\n"),
	}
	decode := map[string]func(string) string{
		utils.ChannelSlack:    strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace,
		utils.ChannelTelegram: html.UnescapeString,
		utils.ChannelEmail:    html.UnescapeString,
	}
	for _, channel := range slices.Sorted(maps.Keys(code)) {
		t.Run(channel, func(t *testing.T) {
			message := renderFor(t, channel, content)
			match := code[channel].FindStringSubmatch(message)
			if match == nil {
				t.Fatalf("no command shown as code:\n%s", message)
			}
			copied := match[1]
			if unescape := decode[channel]; unescape != nil {
				copied = unescape(copied)
			}
			if copied != command {
				t.Fatalf("the command copies as\n%q\nwant\n%q\nin:\n%s", copied, command, message)
			}
			if channel == utils.ChannelMSTeams {
				// A Power Automate flow gets an Adaptive Card: the command
				// is a monospace text run, which is never read as Markdown,
				// after the step it belongs to.
				card, _ := json.Marshal(utils.ConvertMarkdownToAdaptiveCard(message))
				step, block := strings.Index(string(card), "List the instances"), strings.Index(string(card), `"RichTextBlock"`)
				if step < 0 || block < step || !strings.Contains(string(card), `{"fontType":"Monospace","text":`+jsonString(command)+`,"type":"TextRun"}`) {
					t.Fatalf("the card does not show the command as a block after its step:\n%s", card)
				}
			}
		})
	}
}

func jsonString(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

// An account display name starts the "where" line of the Markdown
// channels; one that looks like a list item or a rule stays text.
func TestAWSIssueAlertAccountNameNeverOpensABlock(t *testing.T) {
	loadAgentTestConfig(t)
	for name, escaped := range map[string]string{
		"1. prod": `1\. prod`, "7) prod": `7\) prod`, "- prod": `\- prod`, "+ prod": `\+ prod`, "---": `\---`,
	} {
		content := awsIssueContent(issues.Alert{Kind: issues.AlertOpened, Issue: awsAttackIssue(), Key: "opened:1", AccountName: name}, "")
		for _, channel := range []string{utils.ChannelMSTeams, utils.ChannelLark} {
			message := renderFor(t, channel, content)
			if !strings.Contains(message, "\n"+escaped+" · eu-west-1 · ") {
				t.Errorf("%s: account %q starts a block:\n%s", channel, name, message)
			}
		}
	}
}

// userBackticks is user text with inline code and a fence, which a
// Markdown or Slack channel would render as code and could use to break
// out of the message's own code.
const userBackticks = "```*fenced*``` `x`"

// Backticks and a fence in a title or evidence a user controls stay text
// on every channel: Teams and Lark escape each backtick, Slack keeps each
// apart from its neighbours, and the text is still there to read. The
// command's own code is the only code in the message.
func TestAWSIssueAlertUserBackticksAreTextOnEveryChannel(t *testing.T) {
	loadAgentTestConfig(t)
	issue := awsAttackIssue()
	issue.Title = "Bucket " + userBackticks + " is public"
	issue.Evidence = []issues.Evidence{{Text: "Policy names " + userBackticks, Status: issues.EvidenceProven}}
	command := issue.Steps[0].Command
	content := awsIssueContent(issues.Alert{Kind: issues.AlertOpened, Issue: issue, Key: "opened:1", AccountName: "team " + userBackticks}, "")
	ownCode := map[string]string{
		utils.ChannelSlack: utils.SlackCode(command), utils.ChannelMSTeams: utils.MarkdownCodeBlock(command),
		utils.ChannelLark: utils.MarkdownCodeBlock(command),
	}
	for _, channel := range slices.Sorted(maps.Keys(channelTemplates)) {
		t.Run(channel, func(t *testing.T) {
			message := renderFor(t, channel, content)
			if strings.Count(message, "fenced") != 3 {
				t.Fatalf("the user text is gone instead of escaped:\n%s", message)
			}
			own, markup := ownCode[channel]
			if !markup {
				return
			}
			if strings.Count(message, own) != 1 {
				t.Fatalf("the command's code is missing:\n%s", message)
			}
			rest := []rune(strings.Replace(message, own, "", 1))
			for index, r := range rest {
				if r != '`' {
					continue
				}
				var kept bool
				if channel == utils.ChannelSlack {
					kept = index > 0 && index+1 < len(rest) && rest[index-1] == '\u200b' && rest[index+1] == '\u200b'
				} else {
					kept = index > 0 && rest[index-1] == '\\'
				}
				if !kept {
					t.Fatalf("a user backtick reaches %s as markup at rune %d:\n%s", channel, index, message)
				}
			}
			if strings.Contains(string(rest), "*fenced*") {
				t.Fatalf("emphasis inside the user fence reaches %s as markup:\n%s", channel, message)
			}
		})
	}
}
