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
	// Template is the header colour (red, orange, grey, ...); empty keeps
	// Lark's default.
	Template string `json:"template,omitempty"`
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
	title := "Alert"

	// Add status indicator to title
	if isResolved {
		title = "🟢 Resolved " + title
	} else {
		title = "🔴 Firing " + title
	}

	// Trim whitespace (including newlines) from the beginning and end of content
	content = strings.TrimSpace(content)

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
					Content: content,
				},
			},
		},
	}
}

// CreateLarkCard is a Lark interactive card with the given header title
// and colour template, for alerts whose frame follows what they are.
func CreateLarkCard(content, title, colour string) *LarkMessage {
	message := CreateLarkMessage(content, false)
	message.Card.Header.Title.Content = title
	message.Card.Header.Template = colour
	return message
}
