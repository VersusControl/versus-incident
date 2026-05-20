package utils

import (
	"encoding/json"
	"strings"
	"testing"

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
