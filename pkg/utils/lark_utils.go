package utils

import (
	"regexp"
	"strings"
)

type LarkMessage struct {
	MsgType string         `json:"msg_type"`
	Content LarkMsgContent `json:"content"`
}

type LarkMsgContent struct {
	Text string    `json:"text,omitempty"`
	Post *LarkPost `json:"post,omitempty"`
}

type LarkPost struct {
	ZhCn *LarkPostContent `json:"zh_cn,omitempty"`
	EnUs *LarkPostContent `json:"en_us,omitempty"`
}

type LarkPostContent struct {
	Title   string              `json:"title"`
	Content [][]LarkPostElement `json:"content"`
}

type LarkPostElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text,omitempty"`
	UnEscape bool   `json:"un_escape,omitempty"`
	Href     string `json:"href,omitempty"`      // For links
	UserID   string `json:"user_id,omitempty"`   // For @mentions
	ImageKey string `json:"image_key,omitempty"` // For images
}

// LarkContainsMarkdownSyntax checks if the message contains any Markdown formatting
func LarkContainsMarkdownSyntax(text string) bool {
	// Check for common Markdown syntax
	return strings.Contains(text, "**") || // Bold
		strings.Contains(text, "```") || // Code block
		strings.Contains(text, "# ") || // Heading
		strings.Contains(text, "## ") || // Heading
		strings.Contains(text, "### ") || // Heading
		strings.Contains(text, "[") && strings.Contains(text, "](") || // Links
		strings.Contains(text, "- ") || // Lists
		strings.Contains(text, "* ") || // Lists
		strings.Contains(text, "1. ") // Ordered lists
}

// ConvertToLarkMarkdown converts markdown text to Lark's post format
func ConvertToLarkMarkdown(text string, resolved bool) *LarkPost {
	lines := strings.Split(text, "\n")

	// Extract title from the first line
	title := ""
	content := make([][]LarkPostElement, 0)

	var codeBlock bool
	var codeBlockText strings.Builder
	var listItems []LarkPostElement
	var inList bool
	var isOrderedList bool

	// Link pattern for Markdown links: [text](url)
	linkRegex := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	// Pre-compile the ordered list regex pattern for better performance
	orderedListRegex := regexp.MustCompile(`^\d+\.\s`)

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			// If we're in a list, finish the list when encountering an empty line
			if inList && len(listItems) > 0 {
				content = append(content, listItems)
				listItems = nil
				inList = false
			}

			// Add an empty line unless it's the first line
			if i > 0 {
				content = append(content, []LarkPostElement{{Tag: "text", Text: " "}})
			}
			continue
		}

		// Use first non-empty line as title
		if title == "" {
			// Remove leading Markdown heading characters (# ) if present
			title = strings.TrimSpace(strings.TrimLeft(line, "# "))
			continue
		}

		// Handle code blocks
		if strings.HasPrefix(line, "```") {
			if codeBlock {
				// End of code block
				codeBlock = false
				elements := []LarkPostElement{
					{
						Tag:      "text",
						Text:     codeBlockText.String(),
						UnEscape: true,
					},
				}
				content = append(content, elements)
				codeBlockText.Reset()
			} else {
				// Start of code block
				codeBlock = true
			}
			continue
		}

		if codeBlock {
			// Inside code block, collect text
			codeBlockText.WriteString(line + "\n")
			continue
		}

		// Check for list items
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			if !inList || isOrderedList {
				// Start a new unordered list
				if inList {
					// Finish the previous list
					content = append(content, listItems)
					listItems = nil
				}
				inList = true
				isOrderedList = false
				listItems = make([]LarkPostElement, 0)
			}

			listItem := LarkPostElement{
				Tag:      "text",
				Text:     "• " + strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* "),
				UnEscape: true,
			}

			// Process for any links in the list item
			if linkRegex.MatchString(line) {
				processedLine := processMarkdownLinks(line, linkRegex)
				listItem.Text = processedLine
			}

			listItems = append(listItems, listItem)
			continue
		}

		// Check for ordered list items
		orderedListMatch := orderedListRegex.MatchString(line)
		if orderedListMatch {
			if !inList || !isOrderedList {
				// Start a new ordered list
				if inList {
					// Finish the previous list
					content = append(content, listItems)
					listItems = nil
				}
				inList = true
				isOrderedList = true
				listItems = make([]LarkPostElement, 0)
			}

			// Extract number and text
			parts := strings.SplitN(line, ". ", 2)
			if len(parts) == 2 {
				listItem := LarkPostElement{
					Tag:      "text",
					Text:     parts[0] + ". " + parts[1],
					UnEscape: true,
				}

				// Process for any links in the list item
				if linkRegex.MatchString(line) {
					processedLine := processMarkdownLinks(parts[1], linkRegex)
					listItem.Text = parts[0] + ". " + processedLine
				}

				listItems = append(listItems, listItem)
			}
			continue
		}

		// If we were in a list but current line is not a list item, finish the list
		if inList && !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") && !orderedListMatch {
			content = append(content, listItems)
			listItems = nil
			inList = false
		}

		// Handle headings (already processed first heading as title)
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			text := line
			if strings.HasPrefix(line, "# ") {
				text = "**" + strings.TrimPrefix(line, "# ") + "**" // Bold for h1
			} else if strings.HasPrefix(line, "## ") {
				text = "**" + strings.TrimPrefix(line, "## ") + "**" // Bold for h2
			} else if strings.HasPrefix(line, "### ") {
				text = "*" + strings.TrimPrefix(line, "### ") + "*" // Italics for h3
			}

			elements := []LarkPostElement{
				{
					Tag:      "text",
					Text:     text,
					UnEscape: true,
				},
			}
			content = append(content, elements)
			continue
		}

		// Handle bold text (e.g., **text**)
		if strings.Contains(line, "**") {
			elements := []LarkPostElement{
				{
					Tag:      "text",
					Text:     line,
					UnEscape: true,
				},
			}
			content = append(content, elements)
			continue
		}

		// Handle links
		if linkRegex.MatchString(line) {
			processedLine := processMarkdownLinks(line, linkRegex)

			elements := []LarkPostElement{
				{
					Tag:      "text",
					Text:     processedLine,
					UnEscape: true,
				},
			}
			content = append(content, elements)
			continue
		}

		// Default: Treat as regular text
		elements := []LarkPostElement{
			{
				Tag:      "text",
				Text:     line,
				UnEscape: true,
			},
		}
		content = append(content, elements)
	}

	// Check if we need to finalize a list
	if inList && len(listItems) > 0 {
		content = append(content, listItems)
	}

	// If no title was found, use a default
	if title == "" {
		if resolved {
			title = "🟢 Incident Resolved"
		} else {
			title = "🔴 Incident Alert"
		}
	} else {
		// Add resolved/alert emoji to the extracted title
		if resolved {
			title = "🟢 " + title
		} else {
			title = "🔴 " + title
		}
	}

	return &LarkPost{
		EnUs: &LarkPostContent{
			Title:   title,
			Content: content,
		},
	}
}

// LarkStripMarkdown removes common markdown syntax from text
func LarkStripMarkdown(text string) string {
	// Remove heading markers
	text = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(
		text, "# "), "## "), "### ")

	// Remove bold markers
	text = strings.ReplaceAll(text, "**", "")
	// Remove italic markers
	text = strings.ReplaceAll(text, "*", "")

	// Remove link syntax, keeping just the text
	linkRegex := regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	text = linkRegex.ReplaceAllString(text, "$1")

	return text
}

// LarkConvertMarkdownToPlainText converts markdown to plain text for fallback
func LarkConvertMarkdownToPlainText(markdown string) string {
	lines := strings.Split(markdown, "\n")
	var result strings.Builder
	inCodeBlock := false

	for _, line := range lines {
		// Handle code blocks
		if strings.HasPrefix(line, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}

		if !inCodeBlock {
			// Process normal markdown lines
			line = LarkStripMarkdown(line)

			// Convert list markers
			if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
				line = "• " + strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* ")
			}

			// Handle numbered lists
			numberedListRegex := regexp.MustCompile(`^\d+\.\s+(.*)$`)
			if matches := numberedListRegex.FindStringSubmatch(line); len(matches) > 1 {
				line = matches[0] // Keep the original numbered item
			}
		}

		result.WriteString(line + "\n")
	}

	return result.String()
}

// processMarkdownLinks processes Markdown links and returns a string with appropriate Lark formatting
func processMarkdownLinks(text string, linkRegex *regexp.Regexp) string {
	return linkRegex.ReplaceAllString(text, "$1")
}

// CreateLarkTextMessage creates a simple text message for Lark
func CreateLarkTextMessage(text string) LarkMessage {
	return LarkMessage{
		MsgType: "text",
		Content: LarkMsgContent{
			Text: text,
		},
	}
}
