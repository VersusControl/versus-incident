package utils

import (
	"net/url"
	"strings"
)

// An AWS issue alert's frame (Slack's colour bar, Lark's card header,
// Teams' accent) follows what the alert is, as its body does: a critical
// issue is red, a high issue and a digest of high issues are orange, and a
// coverage notice, which is not an incident, is grey.

// awsIssueTone is what an AWS issue alert's frame shows.
type awsIssueTone int

const (
	awsToneCritical awsIssueTone = iota
	awsToneHigh
	awsToneNotice
)

func awsIssueToneOf(content map[string]interface{}) awsIssueTone {
	switch {
	case content["IssueAlert"] == "coverage":
		return awsToneNotice
	case content["IssueAlert"] == "digest", content["Severity"] != "critical":
		return awsToneHigh
	}
	return awsToneCritical
}

// AWSIssueSlackColor is the Slack colour bar of an AWS issue alert.
func AWSIssueSlackColor(content map[string]interface{}) string {
	switch awsIssueToneOf(content) {
	case awsToneNotice:
		return "#9E9E9E"
	case awsToneHigh:
		return "#D99A00"
	}
	return "#C70039"
}

// AWSIssueLarkHeader is the Lark card header of an AWS issue alert: its
// title and colour template.
func AWSIssueLarkHeader(content map[string]interface{}) (string, string) {
	switch awsIssueToneOf(content) {
	case awsToneNotice:
		return "⚪ Coverage notice", "grey"
	case awsToneHigh:
		if content["IssueAlert"] == "digest" {
			title, _ := content["AlertName"].(string)
			return "🟠 " + strings.TrimSuffix(cmpString(title, "High issues"), " opened"), "orange"
		}
		return "🟠 High AWS issue", "orange"
	}
	return "🔴 Critical AWS issue", "red"
}

// awsIssueTeamsStyle is the Teams container style that accents the card.
func awsIssueTeamsStyle(content map[string]interface{}) string {
	switch awsIssueToneOf(content) {
	case awsToneNotice:
		return "emphasis"
	case awsToneHigh:
		return "warning"
	}
	return "attention"
}

// AWSIssueTeamsCard is the Adaptive Card of an AWS issue alert: the body
// the template rendered, its heading accented by what the alert is, and
// its links (open the issue, acknowledge) as buttons. The template renders
// without the links, which this adds as actions.
func AWSIssueTeamsCard(markdown string, content map[string]interface{}) AdaptiveCard {
	card := ConvertMarkdownToAdaptiveCard(markdown)
	if len(card.Body) > 0 {
		card.Body[0] = map[string]interface{}{
			"type": "Container", "style": awsIssueTeamsStyle(content), "bleed": true,
			"items": []interface{}{card.Body[0]},
		}
	}
	card.Actions = AWSIssueTeamsActions(content)
	return card
}

// AWSIssueTeamsActions are the buttons of an AWS issue alert on Teams:
// acknowledge when on-call waits for it, then each link of the alert.
// Only plain http(s) links become buttons.
func AWSIssueTeamsActions(content map[string]interface{}) []interface{} {
	actions := []interface{}{}
	add := func(label, link string) {
		u, err := url.Parse(link)
		if label == "" || err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return
		}
		actions = append(actions, map[string]interface{}{"type": "Action.OpenUrl", "title": label, "url": link})
	}
	if ack, ok := content["AckURL"].(string); ok {
		add("Acknowledge", ack)
	}
	switch links := content["Links"].(type) {
	case []map[string]string:
		for _, link := range links {
			add(link["label"], link["url"])
		}
	case []interface{}:
		for _, item := range links {
			link, _ := item.(map[string]interface{})
			label, _ := link["label"].(string)
			target, _ := link["url"].(string)
			add(label, target)
		}
	}
	return actions
}

// WithoutAlertLinks is content without its links and acknowledge link, for
// a channel that shows them as buttons instead.
func WithoutAlertLinks(content map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(content))
	for key, value := range content {
		out[key] = value
	}
	delete(out, "Links")
	delete(out, "AckURL")
	delete(out, "IssueURL")
	return out
}

func cmpString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
