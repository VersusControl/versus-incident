package utils

import (
	"strings"
)

// LarkMessage represents the structure of a Lark message
type LarkMessage struct {
	MsgType string    `json:"msg_type"`
	Card    *LarkCard `json:"card"`
}

// LarkCard represents an interactive card in Lark
type LarkCard struct {
	Header   LarkCardHeader    `json:"header"`
	Elements []LarkCardElement `json:"elements"`
}

// LarkCardHeader represents the header of a Lark card
type LarkCardHeader struct {
	Title LarkCardTitle `json:"title"`
}

// LarkCardTitle represents the title component of a Lark card header
type LarkCardTitle struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

// LarkCardElement represents an element in a Lark card
type LarkCardElement struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

// CreateLarkMessage creates a Lark message with interactive card format
// This format uses "msg_type": "interactive" with a card structure
func CreateLarkMessage(content string, isResolved bool) *LarkMessage {
	// Extract first line for title and remove it from content
	title := "Incident Alert"
	remainingContent := content

	if len(content) > 0 {
		lines := strings.SplitN(content, "\n", 2)
		if len(lines) > 0 && len(lines[0]) > 0 {
			// Use first line as title
			title = strings.ReplaceAll(lines[0], "**", "")
			title = strings.TrimSpace(title)

			// Remove the first line from content
			if len(lines) > 1 {
				remainingContent = strings.TrimSpace(lines[1])
			} else {
				// If there was only one line, it's now in the title
				remainingContent = ""
			}
		}
	}

	// Add status indicator to title
	if isResolved {
		title = "🟢 " + title
	} else {
		title = "🔴 " + title
	}

	// Create the interactive card message
	return &LarkMessage{
		MsgType: "interactive",
		Card: &LarkCard{
			Header: LarkCardHeader{
				Title: LarkCardTitle{
					Tag:     "plain_text",
					Content: title,
				},
			},
			Elements: []LarkCardElement{
				{
					Tag:     "markdown",
					Content: remainingContent,
				},
			},
		},
	}
}
