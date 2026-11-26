package utils

import "strings"

// Text a user controls (a resource or account name, a tag, a reason, a
// title a rule builds from them) is escaped for the channel it is sent to,
// so it shows as typed and never as markup. Each escaper leaves ordinary
// text, apostrophes and quotes included, as it is: no HTML entity reaches
// a channel that shows it literally.

// markdownSpecial are the characters Markdown (Microsoft Teams, Lark
// cards) reads as emphasis, code, links, headings, quotes, tables or raw
// HTML. Each is escaped with a backslash, which the renderer drops. The
// hyphen, plus, equals sign, dot and closing parenthesis only mean
// something at the start of a line (a list item, a rule, a heading
// underline, a numbered item), so they are escaped only there.
const markdownSpecial = "\\`*_~[]()#<>|!{}&"

// EscapeMarkdown escapes text for a Markdown channel. Text can start a
// line, or follow a list marker: a value such as "1. prod", "- prod" or
// "---" at the start of any of its lines is escaped so it stays text and
// does not become a list item or a rule.
func EscapeMarkdown(text string) string {
	if !strings.ContainsAny(text, markdownSpecial+"-+=.)") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text) + 8)
	for _, line := range strings.SplitAfter(text, "\n") {
		out.WriteString(escapeMarkdownLine(line))
	}
	return out.String()
}

// escapeMarkdownLine escapes one line: the block marker it starts with,
// then every inline special character.
func escapeMarkdownLine(line string) string {
	var out strings.Builder
	rest := strings.TrimLeft(line, " \t")
	out.WriteString(line[:len(line)-len(rest)])
	switch {
	case strings.HasPrefix(rest, "-"), strings.HasPrefix(rest, "+"), strings.HasPrefix(rest, "="):
		out.WriteByte('\\')
		out.WriteByte(rest[0])
		rest = rest[1:]
	default:
		digits := 0
		for digits < len(rest) && digits < 10 && rest[digits] >= '0' && rest[digits] <= '9' {
			digits++
		}
		if digits > 0 && digits < len(rest) && (rest[digits] == '.' || rest[digits] == ')') {
			out.WriteString(rest[:digits])
			out.WriteByte('\\')
			out.WriteByte(rest[digits])
			rest = rest[digits+1:]
		}
	}
	for _, r := range rest {
		if r < 128 && strings.ContainsRune(markdownSpecial, r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// slackFormatting are the characters Slack mrkdwn reads as bold, italic,
// strike and code. Slack has no escape for them; a zero-width space on
// each side keeps them from touching a word, which is what formatting
// needs, and they show as the characters they are.
const slackFormatting = "*_~`"

// EscapeSlack escapes text for Slack mrkdwn: &, < and > as Slack's own
// entities (it decodes exactly these three), and the formatting characters
// kept apart from the words around them.
func EscapeSlack(text string) string {
	text = escapeAngles(text)
	if !strings.ContainsAny(text, slackFormatting) {
		return text
	}
	var out strings.Builder
	out.Grow(len(text) + 16)
	for _, r := range text {
		if strings.ContainsRune(slackFormatting, r) {
			out.WriteString("​")
			out.WriteRune(r)
			out.WriteString("​")
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// EscapeTelegramHTML escapes text for Telegram's HTML parse mode, which
// needs only &, < and > escaped in text.
func EscapeTelegramHTML(text string) string {
	return escapeAngles(text)
}

func escapeAngles(text string) string {
	if !strings.ContainsAny(text, "&<>") {
		return text
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

// SlackCode shows text as Slack code, so it reads and copies as typed:
// formatting characters inside code are not formatting. Slack still
// decodes &, < and >, so those are its entities. Text without a backtick
// is inline code. Text with one is a code block, its fences on lines of
// their own so a backtick at either end of the text cannot join them.
// Slack has no escape inside code, so a run of three or more backticks,
// which would end the block, is broken with a zero-width space: only
// that rare command copies with an invisible character in it.
func SlackCode(text string) string {
	text = escapeAngles(text)
	if !strings.Contains(text, "`") {
		return "`" + text + "`"
	}
	return "```\n" + breakBacktickRuns(text) + "\n```"
}

// breakBacktickRuns puts a zero-width space after every second backtick
// of a run, so no three backticks stand together.
func breakBacktickRuns(text string) string {
	if !strings.Contains(text, "```") {
		return text
	}
	var out strings.Builder
	run := 0
	for _, r := range text {
		if r != '`' {
			run = 0
			out.WriteRune(r)
			continue
		}
		if run == 2 {
			out.WriteString("\u200b")
			run = 0
		}
		run++
		out.WriteRune(r)
	}
	return out.String()
}

// MarkdownCodeBlock shows text as a fenced code block on its own lines,
// for Microsoft Teams and Lark: nothing inside a code block is markup, so
// the text is not escaped and copies as typed. The fence is longer than
// any run of backticks in the text, so the text cannot close it.
func MarkdownCodeBlock(text string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "\n" + text + "\n" + fence
}

// TelegramCode shows text as code in Telegram's HTML parse mode, which
// needs only &, < and > escaped inside it.
func TelegramCode(text string) string {
	return "<code>" + escapeAngles(text) + "</code>"
}

// EscapeForChannel escapes text for one alert channel. Email templates
// rely on html/template, and Viber shows plain text, so both return text
// as it is.
func EscapeForChannel(channel, text string) string {
	switch channel {
	case ChannelMSTeams, ChannelLark:
		return EscapeMarkdown(text)
	case ChannelSlack:
		return EscapeSlack(text)
	case ChannelTelegram:
		return EscapeTelegramHTML(text)
	}
	return text
}

// EscapeLark escapes text for a Lark card's Markdown: as for any Markdown
// channel, and every < is kept from opening a Lark tag, so text such as
// <at id=all></at> can never become a mention of everyone. The zero-width
// space it adds does not show.
func EscapeLark(text string) string {
	return breakLarkTags(EscapeMarkdown(text))
}

// LarkCodeBlock shows a command as a Lark code block that copies as
// written. The fence uses a character the command does not start a line
// with: backticks for a command without one, tildes otherwise, so a
// command that begins with backticks can never close its own block. A
// < that would open a Lark tag (a mention) is the one thing changed.
func LarkCodeBlock(text string) string {
	text = breakLarkMentions(text)
	if !strings.Contains(text, "`") {
		return MarkdownCodeBlock(text)
	}
	fence := "~~~"
	for strings.Contains(text, fence) {
		fence += "~"
	}
	return fence + "\n" + text + "\n" + fence
}

// breakLarkTags puts a zero-width space after every <.
func breakLarkTags(text string) string {
	return strings.ReplaceAll(text, "<", "<​")
}

// breakLarkMentions puts a zero-width space after a < that starts a Lark
// at tag (a mention), whatever its case or spacing.
func breakLarkMentions(text string) string {
	if !strings.Contains(text, "<") {
		return text
	}
	var out strings.Builder
	for index := 0; index < len(text); index++ {
		out.WriteByte(text[index])
		if text[index] != '<' {
			continue
		}
		rest := strings.TrimLeft(text[index+1:], " \t")
		if len(rest) >= 2 && strings.EqualFold(rest[:2], "at") {
			out.WriteString("​")
		}
	}
	return out.String()
}
