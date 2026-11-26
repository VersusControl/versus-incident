package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Message budgets of an AWS issue alert on each channel, set below the
// channel's hard limit so the escaped, rendered message is never refused.
const (
	// Slack refuses a section text over 3,000 characters.
	SlackAlertBudget = 2900
	// Telegram refuses a message over 4,096 characters (UTF-16 units).
	TelegramAlertBudget = 4000
	// Microsoft Teams refuses a message over about 28 KB; the budget is
	// bytes of the JSON payload.
	MSTeamsAlertBudget = 20000
	// Lark refuses a card request over 30 KB; the budget is bytes of the
	// JSON-encoded card text.
	LarkAlertBudget = 20000
	// Viber refuses a text message over 7,000 characters.
	ViberAlertBudget = 6500
	// Email has no small limit; the budget only stops a runaway body.
	EmailAlertBudget = 200000
)

// UTF16Length counts text the way Slack and Telegram count their limits;
// an emoji outside the basic plane counts twice.
func UTF16Length(text string) int {
	return len(utf16.Encode([]rune(text)))
}

// JSONLength is the size of text once encoded as a JSON string, which is
// what a card channel's request limit counts.
func JSONLength(text string) int {
	encoded, err := json.Marshal(text)
	if err != nil {
		return len(text)
	}
	return len(encoded)
}

// FitAWSIssueAlert renders an AWS issue alert so it fits its channel's
// budget. A message within the budget is rendered as it is. Otherwise the
// lower-priority parts go first: evidence lines past the first, then steps
// past the first, then notes past the first; then every remaining line is
// shortened; then the reason it reopened and the likely cause, the rest of
// the evidence, steps and notes; and last the title and the "where" line
// are shortened. Severity, title, where and the links always stay. Each
// list says how many of its lines are left for the issue page. Content
// that is not an AWS issue alert is rendered unchanged.
func FitAWSIssueAlert(content map[string]interface{}, budget int, measure func(string) int, render func(map[string]interface{}) (string, error)) (string, error) {
	message, err := render(content)
	if err != nil || !IsAWSIssueIncident(content) || measure(message) <= budget {
		return message, err
	}
	original := alertParts(content)
	for _, fit := range fitLadder(original) {
		message, err = render(fit.apply(content, original))
		if err != nil || measure(message) <= budget {
			return message, err
		}
	}
	return message, nil
}

// alertLists are the parts of an alert that can be cut, as rendered.
type alertLists struct {
	evidence, notes []string
	steps           []map[string]string
}

func alertParts(content map[string]interface{}) alertLists {
	return alertLists{evidence: stringList(content["Evidence"]), notes: stringList(content["Notes"]), steps: stepList(content["Steps"])}
}

// fitStep is one way of cutting an alert down.
type fitStep struct {
	evidence, steps, notes int // lines kept of each list
	lineCap                int // runes kept of each line; 0 keeps them whole
	dropDetail             bool
	headCap                int // runes kept of the title and the "where" line
}

// fitLadder lists the cuts from the smallest to the largest.
func fitLadder(parts alertLists) []fitStep {
	step := fitStep{evidence: len(parts.evidence), steps: len(parts.steps), notes: len(parts.notes)}
	var ladder []fitStep
	for _, count := range []*int{&step.evidence, &step.steps, &step.notes} {
		for *count > 1 {
			*count--
			ladder = append(ladder, step)
		}
	}
	for _, limit := range []int{256, 128, 64} {
		step.lineCap = limit
		ladder = append(ladder, step)
	}
	step.dropDetail = true
	ladder = append(ladder, step)
	step.evidence, step.steps, step.notes = 0, 0, 0
	ladder = append(ladder, step)
	step.headCap = 120
	ladder = append(ladder, step)
	step.headCap = 60
	return append(ladder, step)
}

// apply builds the content of one cut: a copy, so the incident keeps what
// was recorded.
func (step fitStep) apply(content map[string]interface{}, parts alertLists) map[string]interface{} {
	out := make(map[string]interface{}, len(content))
	for key, value := range content {
		out[key] = value
	}
	page := "issue page"
	switch content["IssueAlert"] {
	case "digest":
		page = "issues page"
	case "coverage":
		page = "coverage page"
	}
	if _, ok := content["Evidence"]; ok {
		out["Evidence"] = cutLines(parts.evidence, step.evidence, step.lineCap, "evidence lines", page)
	}
	if _, ok := content["Notes"]; ok {
		out["Notes"] = cutLines(parts.notes, step.notes, step.lineCap, "lines", page)
	}
	if _, ok := content["Steps"]; ok {
		out["Steps"] = cutSteps(parts.steps, step.steps, step.lineCap, page)
	}
	for _, key := range []string{"LikelyCause", "Reopened"} {
		text, ok := content[key].(string)
		switch {
		case !ok:
		case step.dropDetail:
			delete(out, key)
		default:
			out[key] = shorten(text, step.lineCap)
		}
	}
	for _, key := range []string{"AlertName", "Where"} {
		if text, ok := content[key].(string); ok {
			out[key] = shorten(text, step.headCap)
		}
	}
	return out
}

func cutLines(lines []string, keep, lineCap int, what, page string) []string {
	out := make([]string, 0, keep+1)
	for _, line := range lines[:min(keep, len(lines))] {
		out = append(out, shorten(line, lineCap))
	}
	if left := len(lines) - len(out); left > 0 {
		out = append(out, fmt.Sprintf("… %d more %s on the %s", left, what, page))
	}
	return out
}

func cutSteps(steps []map[string]string, keep, lineCap int, page string) []map[string]string {
	out := make([]map[string]string, 0, keep+1)
	for _, step := range steps[:min(keep, len(steps))] {
		text, command := shorten(step["text"], lineCap), step["command"]
		if lineCap > 0 && utf8.RuneCountInString(command) > lineCap {
			// A shortened command is a different command: leave it to the
			// issue page rather than show one that runs something else.
			text, command = text+" (command on the "+page+")", ""
		}
		out = append(out, map[string]string{"text": text, "command": command})
	}
	if left := len(steps) - len(out); left > 0 {
		out = append(out, map[string]string{"text": fmt.Sprintf("… %d more steps on the %s", left, page), "command": ""})
	}
	return out
}

// shorten keeps the first limit runes of text and marks the cut; zero
// keeps it whole.
func shorten(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimRight(string(runes[:limit]), " ") + "…"
}

func stringList(value interface{}) []string {
	switch list := value.(type) {
	case []string:
		return list
	case []interface{}:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, fmt.Sprint(item))
		}
		return out
	}
	return nil
}

func stepList(value interface{}) []map[string]string {
	switch list := value.(type) {
	case []map[string]string:
		return list
	case []interface{}:
		out := make([]map[string]string, 0, len(list))
		for _, item := range list {
			step, _ := item.(map[string]interface{})
			text, _ := step["text"].(string)
			command, _ := step["command"].(string)
			out = append(out, map[string]string{"text": text, "command": command})
		}
		return out
	}
	return nil
}

// AlertTemplate is a parsed channel template, text or HTML.
type AlertTemplate interface {
	Execute(w io.Writer, data any) error
}

// RenderAlert executes a channel template over the data a provider has
// always given it: the content map, or a pointer to it. The data is passed
// as it is, so a user template that reads the root with index or len sees
// what it did before. An AWS issue alert is fitted to the channel's
// budget, each cut rendered in the same form; anything else renders as it
// always has.
func RenderAlert(tmpl AlertTemplate, data any, budget int, measure func(string) int) (string, error) {
	execute := func(data any) (string, error) {
		var out bytes.Buffer
		if err := tmpl.Execute(&out, data); err != nil {
			return "", fmt.Errorf("failed to execute template: %w", err)
		}
		return out.String(), nil
	}
	var content map[string]interface{}
	pointer := false
	switch value := data.(type) {
	case map[string]interface{}:
		content = value
	case *map[string]interface{}:
		if value != nil {
			content, pointer = *value, true
		}
	}
	if !IsAWSIssueIncident(content) {
		return execute(data)
	}
	return FitAWSIssueAlert(content, budget, measure, func(fitted map[string]interface{}) (string, error) {
		if pointer {
			return execute(&fitted)
		}
		return execute(fitted)
	})
}
