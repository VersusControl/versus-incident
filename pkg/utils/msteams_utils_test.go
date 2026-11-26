package utils

import (
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

func TestContainsMarkdownSyntax(t *testing.T) {
	cases := map[string]bool{
		"plain text":                  false,
		"with **bold**":               true,
		"```code```":                  true,
		"# heading":                   true,
		"## h2":                       true,
		"### h3":                      true,
		"see [docs](https://x.io)":    true,
		"- item":                      true,
		"* item":                      true,
		"1. item":                     true,
		"link text [no closing paren": false,
		"link](only)":                 false,
	}
	for in, want := range cases {
		if got := ContainsMarkdownSyntax(in); got != want {
			t.Errorf("ContainsMarkdownSyntax(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStripMarkdown(t *testing.T) {
	cases := map[string]string{
		"# Heading":             "Heading",
		"## h2":                 "h2",
		"### h3":                "h3",
		"**bold**":              "bold",
		"text **mid** more":     "text mid more",
		"see [docs](https://x)": "see docs",
		"plain":                 "plain",
	}
	for in, want := range cases {
		if got := StripMarkdown(in); got != want {
			t.Errorf("StripMarkdown(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertMarkdownToPlainText(t *testing.T) {
	in := "# Title\n**bold**\n- one\n* two\n1. first\n```\ncode lines\n```\nend"
	out := ConvertMarkdownToPlainText(in)
	// Headings + bold stripped.
	if !strings.Contains(out, "Title") || strings.Contains(out, "#") {
		t.Errorf("heading not stripped: %q", out)
	}
	if strings.Contains(out, "**") {
		t.Errorf("bold markers not stripped: %q", out)
	}
	// Fence markers are dropped but code block content is preserved verbatim.
	if strings.Contains(out, "```") {
		t.Errorf("fence not removed: %q", out)
	}
	if !strings.Contains(out, "code lines") {
		t.Errorf("code-block content should be preserved: %q", out)
	}
	// Bullet conversion.
	if !strings.Contains(out, "• one") || !strings.Contains(out, "• two") {
		t.Errorf("bullets not converted: %q", out)
	}
	if !strings.Contains(out, "1. first") {
		t.Errorf("ordered list missing: %q", out)
	}
	if !strings.Contains(out, "end") {
		t.Errorf("tail missing: %q", out)
	}
}

func TestConvertMarkdownToAdaptiveCard_Basics(t *testing.T) {
	in := "# H1\n## H2\n### H3\n**bold line**\nplain text\n- u1\n- u2\n\n1. a\n2. b\n\n[click](https://example.com)\n```\ncode\n```"
	card := ConvertMarkdownToAdaptiveCard(in)
	if card.Type != "AdaptiveCard" {
		t.Errorf("Type = %q", card.Type)
	}
	if card.Version == "" || card.Schema == "" {
		t.Errorf("missing version/schema")
	}
	if card.Summary != "H1" {
		t.Errorf("Summary = %q, want H1", card.Summary)
	}
	if card.Text == "" {
		t.Error("Text fallback should be set")
	}
	if len(card.Body) < 6 {
		t.Errorf("expected several body blocks, got %d", len(card.Body))
	}
	// Find text blocks and verify a few expected entries.
	found := map[string]bool{}
	for _, item := range card.Body {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if text, _ := obj["text"].(string); text != "" {
			found[text] = true
		}
	}
	for _, want := range []string{"H1", "H2", "H3", "bold line", "plain text"} {
		if !found[want] {
			t.Errorf("body missing %q; found=%v", want, found)
		}
	}
}

func TestConvertMarkdownToAdaptiveCard_LinksKeepFirstTarget(t *testing.T) {
	card := ConvertMarkdownToAdaptiveCard("See [docs](https://example.com/docs) and [status](https://example.com/status)")
	if len(card.Body) != 1 {
		t.Fatalf("body has %d blocks, want 1", len(card.Body))
	}
	container := card.Body[0].(map[string]interface{})
	items := container["items"].([]interface{})
	if text := items[0].(map[string]interface{})["text"]; text != "See docs and status" {
		t.Fatalf("link text = %v", text)
	}
	action := container["selectAction"].(map[string]interface{})
	if action["type"] != "Action.OpenUrl" || action["url"] != "https://example.com/docs" {
		t.Fatalf("link action = %v", action)
	}
}

func TestConvertMarkdownToAdaptiveCard_SummaryFallback(t *testing.T) {
	// No heading — summary falls back to the first non-empty line.
	card := ConvertMarkdownToAdaptiveCard("first line\nsecond\n")
	if card.Summary != "first line" {
		t.Errorf("Summary = %q, want first line", card.Summary)
	}
}

func TestConvertToTeamsPayload_LegacyOffice365(t *testing.T) {
	inc := m.NewIncident("", &map[string]interface{}{}, false)
	body, err := ConvertToTeamsPayload("https://outlook.webhook.office.com/abc", "hello", inc)
	if err != nil {
		t.Fatal(err)
	}
	var got MSTeamsMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello" {
		t.Errorf("Text = %q, want hello", got.Text)
	}
}

func TestConvertToTeamsPayload_PowerAutomateAdaptiveCard(t *testing.T) {
	inc := m.NewIncident("", &map[string]interface{}{}, false)
	body, err := ConvertToTeamsPayload("https://prod-1.westus.logic.azure.com/x", "# Heading\n**bold**", inc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["type"] != "AdaptiveCard" {
		t.Errorf("type = %v, want AdaptiveCard", got["type"])
	}
}

func TestConvertToTeamsPayload_PowerAutomatePlainPassthrough(t *testing.T) {
	content := map[string]interface{}{"AlertName": "X", "Severity": "high"}
	inc := m.NewIncident("", &content, false)
	body, err := ConvertToTeamsPayload("https://prod-1.westus.logic.azure.com/x", "plain text", inc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["messageText"] != "plain text" {
		t.Errorf("messageText = %v", got["messageText"])
	}
	if got["AlertName"] != "X" {
		t.Errorf("missing pass-through AlertName, got %v", got)
	}
	if got["Severity"] != "high" {
		t.Errorf("missing pass-through Severity, got %v", got)
	}
}

func TestConvertToTeamsPayload_PowerAutomateMessageTextNotOverwritten(t *testing.T) {
	// `messageText` key in content must not overwrite the rendered template.
	content := map[string]interface{}{"messageText": "from-content"}
	inc := m.NewIncident("", &content, false)
	body, _ := ConvertToTeamsPayload("https://prod-1.westus.logic.azure.com/x", "rendered", inc)
	var got map[string]interface{}
	_ = json.Unmarshal(body, &got)
	if got["messageText"] != "rendered" {
		t.Errorf("messageText = %v, want rendered", got["messageText"])
	}
}

// cardBody is the card's body as JSON, for asserting on its elements.
func cardBody(card AdaptiveCard) string {
	encoded, _ := json.Marshal(card.Body)
	return string(encoded)
}

// A Python traceback marks the failing expression with a line of tildes.
// Through the default Teams template it stays text: the card keeps every
// element after it, the error, the time, the Kubernetes metadata and the
// acknowledge link.
func TestTeamsCardKeepsATracebackAndWhatFollows(t *testing.T) {
	traceback := "ERROR checkout failed\nTraceback (most recent call last):\n  File \"/app/pay.py\", line 41, in charge\n    total = price(cart)[\"amount\"] / cart.count\n            ~~~~~~~~~~~~~~~~~~~~~~^~~~~~~~~~~~\nZeroDivisionError: division by zero"
	content := map[string]interface{}{"log": traceback, "time": "2026-11-26 02:50:00",
		"kubernetes": map[string]interface{}{"pod_name": "checkout-1", "namespace_name": "shop", "container_name": "app", "labels": map[string]interface{}{"app": "checkout"}},
		"AckURL":     "https://versus.example/api/ack/1"}
	tmpl, err := template.New("msteams_message.tmpl").Funcs(GetTemplateFuncMaps()).ParseFiles("../../config/msteams_message.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	var message strings.Builder
	if err := tmpl.Execute(&message, content); err != nil {
		t.Fatal(err)
	}
	card := ConvertMarkdownToAdaptiveCard(message.String())
	body := cardBody(card)
	if len(card.Body) != 14 {
		t.Errorf("card has %d elements; want the 14 the template renders:\n%s", len(card.Body), body)
	}
	for _, want := range []string{"~~~~~~^~~~", "ZeroDivisionError", "Time:", "Namespace: shop", "versus.example/api/ack/1"} {
		if !strings.Contains(body, want) {
			t.Errorf("card lost %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "RichTextBlock") {
		t.Errorf("a line of tildes opened a code block:\n%s", body)
	}
}

// Fences the converter has always read keep their meaning, and whatever
// follows a block still renders: a line of tildes is text, a closing fence
// may carry text after it, a longer fence closes on a shorter line of
// backticks, and a block never closed is shown as code, not dropped.
func TestTeamsCardRendersWhatFollowsAFence(t *testing.T) {
	for name, tc := range map[string]struct {
		markdown string
		code     string // the code shown, or empty for none
		after    []string
	}{
		"tilde rule":                       {"**Deploy**\n~~~~~~~~\nservice api\n- step one\n[Open](https://versus.example/x)", "", []string{"~~~~~~~~", "service api", "• step one", "https://versus.example/x"}},
		"tilde block":                      {"**Deploy**\n~~~\ncode here\n~~~\nafter", "", []string{"code here", "after"}},
		"closing fence with trailing text": {"**Deploy**\n```\ncode\n``` done\nafter\n- item", "code", []string{"after", "• item"}},
		"fence closed by a shorter one":    {"**Deploy**\n````\ncode\n```\nafter", "code", []string{"after"}},
		"fence never closed":               {"**Deploy**\n```\ncode\nmore\n[Open](https://versus.example/x)", "code\nmore\n[Open](https://versus.example/x)", nil},
	} {
		t.Run(name, func(t *testing.T) {
			body := cardBody(ConvertMarkdownToAdaptiveCard(tc.markdown))
			if tc.code == "" && strings.Contains(body, "RichTextBlock") {
				t.Errorf("no code block expected:\n%s", body)
			}
			if tc.code != "" {
				run, _ := json.Marshal(map[string]string{"fontType": "Monospace", "text": tc.code, "type": "TextRun"})
				if !strings.Contains(body, string(run)) {
					t.Errorf("code %q not shown as a text run:\n%s", tc.code, body)
				}
			}
			for _, want := range tc.after {
				if !strings.Contains(body, want) {
					t.Errorf("lost %q after the fence:\n%s", want, body)
				}
			}
			plain := ConvertMarkdownToPlainText(tc.markdown)
			for _, want := range tc.after {
				if !strings.Contains(plain, StripMarkdown(strings.TrimPrefix(want, "• "))) && !strings.Contains(want, "https://") {
					t.Errorf("fallback text lost %q:\n%s", want, plain)
				}
			}
		})
	}
}
