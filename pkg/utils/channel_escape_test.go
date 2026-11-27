package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

// Each channel's escaper keeps user markup from rendering, keeps ordinary
// text (apostrophes, dates, Regions, commands) as typed, and adds no HTML
// entity a channel would show as text.
func TestChannelEscapers(t *testing.T) {
	const typed = "<b>x</b> *bold* _it_ ~gone~ `code` ```fence``` [link](https://evil.example) # > | can't eu-west-1 2026-11-25 13:02 a&b"
	for _, tc := range []struct {
		channel string
		absent  []string
		present []string
	}{
		{ChannelMSTeams, []string{"<b>", "*bold*", "[link](", "_it_", "`code`", "```"}, []string{`\<b\>`, `\*bold\*`, `\[link\]\(https://evil.example\)`, "can't", "eu-west-1", "2026-11-25 13:02", `a\&b`, "\\`code\\`", "\\`\\`\\`fence\\`\\`\\`"}},
		{ChannelLark, []string{"<b>", "*bold*", "[link](", "`code`", "```"}, []string{`\*bold\*`, "can't", "\\`code\\`", "\\`\\`\\`fence\\`\\`\\`"}},
		{ChannelSlack, []string{"<b>", "*bold*", "_it_", "~gone~", "`code`", "```", "&#"}, []string{"&lt;b&gt;", "bold", "can't", "eu-west-1", "a&amp;b"}},
		{ChannelTelegram, []string{"<b>", "&#"}, []string{"&lt;b&gt;", "*bold*", "can't", "a&amp;b"}},
		{ChannelViber, nil, []string{typed}},
		{ChannelEmail, nil, []string{typed}},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			got := EscapeForChannel(tc.channel, typed)
			for _, markup := range tc.absent {
				if strings.Contains(got, markup) {
					t.Errorf("%q survives in %q", markup, got)
				}
			}
			for _, text := range tc.present {
				if !strings.Contains(got, text) {
					t.Errorf("%q is missing from %q", text, got)
				}
			}
		})
	}
	if got := EscapeMarkdown("plain text, can't"); got != "plain text, can't" {
		t.Fatalf("plain text changed: %q", got)
	}
}

// Text that starts a line, or follows a list marker, never opens a block:
// a list item, a numbered item, a rule or a heading underline at its
// start, on any of its lines, stays text. The same characters inside the
// text are left alone.
func TestEscapeMarkdownLineStart(t *testing.T) {
	for typed, want := range map[string]string{
		"1. prod · eu-west-1":     `1\. prod · eu-west-1`,
		"12) prod":                `12\) prod`,
		"- prod":                  `\- prod`,
		"+ prod":                  `\+ prod`,
		"---":                     `\---`,
		"===":                     `\===`,
		"  - prod":                `  \- prod`,
		"prod\n2. staging":        "prod\n" + `2\. staging`,
		"prod-1 v2.0 a=b c+d":     "prod-1 v2.0 a=b c+d",
		"2026-11-25 13:02 UTC":    "2026-11-25 13:02 UTC",
		"eu-west-1 · since 13:02": "eu-west-1 · since 13:02",
	} {
		if got := EscapeMarkdown(typed); got != want {
			t.Errorf("EscapeMarkdown(%q) = %q; want %q", typed, got, want)
		}
	}
}

// Code forms show a command as typed: Slack inline code (a code block when
// the command holds a backtick), a fenced block the command cannot close,
// and Telegram HTML code; only &, < and > are encoded where the channel
// decodes them.
func TestChannelCodeForms(t *testing.T) {
	const command = `aws s3 ls s3://a_b/*/ --x \ [y] && z <w>`
	if got, want := SlackCode(command), "`aws s3 ls s3://a_b/*/ --x \\ [y] &amp;&amp; z &lt;w&gt;`"; got != want {
		t.Errorf("SlackCode = %q; want %q", got, want)
	}
	for typed, want := range map[string]string{
		// A backtick makes it a block, its fences on their own lines, so
		// a backtick at the start or the end stays part of the command.
		"echo `id`": "```\necho `id`\n```",
		"`id` | wc": "```\n`id` | wc\n```",
		"`a b`":     "```\n`a b`\n```",
		// A fence inside cannot end the block early and let what follows
		// render: every third backtick of a run is set apart.
		"a ```*bold*``` b": "```\na ``\u200b`*bold*``\u200b` b\n```",
		"x ``````":         "```\nx ``\u200b``\u200b``\n```",
	} {
		got := SlackCode(typed)
		if got != want {
			t.Errorf("SlackCode(%q) = %q; want %q", typed, got, want)
		}
		if inner := strings.TrimSuffix(strings.TrimPrefix(got, "```\n"), "\n```"); strings.Contains(inner, "```") {
			t.Errorf("SlackCode(%q) holds a fence inside the block: %q", typed, got)
		}
	}
	if got, want := MarkdownCodeBlock(command), "```\n"+command+"\n```"; got != want {
		t.Errorf("MarkdownCodeBlock = %q; want %q", got, want)
	}
	if got, want := MarkdownCodeBlock("a ``` b"), "````\na ``` b\n````"; got != want {
		t.Errorf("MarkdownCodeBlock with a fence inside = %q; want %q", got, want)
	}
	if got, want := TelegramCode(command), "<code>aws s3 ls s3://a_b/*/ --x \\ [y] &amp;&amp; z &lt;w&gt;</code>"; got != want {
		t.Errorf("TelegramCode = %q; want %q", got, want)
	}
}

// Lark: a command is fenced with a character it does not use, so it
// cannot close its own block; no < of a text or a command opens a mention.
func TestLarkCodeAndMentions(t *testing.T) {
	for typed, want := range map[string]string{
		"aws s3 ls":             "```\naws s3 ls\n```",
		"```aws s3 ls":          "~~~\n```aws s3 ls\n~~~",
		"a ` b ~~~ c":           "~~~~\na ` b ~~~ c\n~~~~",
		"echo <at id=all></at>": "```\necho <​at id=all></at>\n```",
		"echo < AT id=all>":     "```\necho <​ AT id=all>\n```",
		"cat <file":             "```\ncat <file\n```",
	} {
		if got := LarkCodeBlock(typed); got != want {
			t.Errorf("LarkCodeBlock(%q) = %q; want %q", typed, got, want)
		}
	}
	if got := EscapeLark("hi <at id=all></at>"); strings.Contains(got, "<at") || strings.Contains(got, "<\\/at") {
		t.Errorf("EscapeLark leaves a mention: %q", got)
	}
}

// The Teams card shows a fenced block as a monospace text run, kept as
// written (indentation included), and only a line of the block's own fence
// closes it.
func TestTeamsCardCodeBlocks(t *testing.T) {
	card := ConvertMarkdownToAdaptiveCard("**Steps**\n- first\n````\n```not a close\n  indented *x* [a](b)\n````\nafter")
	encoded, _ := json.Marshal(card)
	run, _ := json.Marshal(map[string]string{"fontType": "Monospace", "text": "```not a close\n  indented *x* [a](b)", "type": "TextRun"})
	if !strings.Contains(string(encoded), string(run)) || !strings.Contains(string(encoded), `"after"`) {
		t.Fatalf("card = %s", encoded)
	}
	if first, code := strings.Index(string(encoded), "first"), strings.Index(string(encoded), "RichTextBlock"); first < 0 || code < first {
		t.Fatalf("the code block is not after the list item before it: %s", encoded)
	}
}
