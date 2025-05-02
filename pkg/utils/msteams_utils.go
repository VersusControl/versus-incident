// Package utils provides utility functions for the application
package utils

import (
	"encoding/json"
	"regexp"
	"strings"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

// MSTeamsMessage represents a basic message for MS Teams
type MSTeamsMessage struct {
	Text string `json:"text"`
}

// AdaptiveCard represents a basic Adaptive Card structure for MS Teams
type AdaptiveCard struct {
	Type    string        `json:"type"`
	Body    []interface{} `json:"body"`
	Schema  string        `json:"$schema"`
	Version string        `json:"version"`
	// Required MS Teams fields
	Text    string `json:"text,omitempty"`    // Fallback text for clients that don't support cards
	Summary string `json:"summary,omitempty"` // Required by MS Teams for notification
}

// ConvertToTeamsPayload takes the rendered template message and converts it to the
// appropriate payload format for Power Automate.
func ConvertToTeamsPayload(powerAutomateURL, messageText string, incident *m.Incident) ([]byte, error) {
	// Check if the power_automate_url is an Office 365 Connector webhook URL
	// These usually start with https://<domain>.webhook.office.com/
	isLegacyWebhook := strings.Contains(powerAutomateURL, "webhook.office.com")

	// For legacy Office 365 Connector webhooks
	if isLegacyWebhook {
		// Legacy webhooks expect a simple format with "text" field
		legacyPayload := MSTeamsMessage{
			Text: messageText,
		}
		return json.Marshal(legacyPayload)
	}

	// For new Power Automate workflows
	if ContainsMarkdownSyntax(messageText) {
		// Convert template output to Adaptive Card format
		adaptiveCard := ConvertMarkdownToAdaptiveCard(messageText)
		return json.Marshal(adaptiveCard)
	}

	// For Power Automate with regular text, create a dynamic payload
	payload := make(map[string]interface{})

	// Always include the rendered template as messageText
	payload["messageText"] = messageText

	// Pass through original content fields
	if incident.Content != nil {
		// Add all top-level fields to the payload
		for key, value := range *incident.Content {
			// Skip adding duplicates
			if key != "messageText" {
				payload[key] = value
			}
		}
	}

	return json.Marshal(payload)
}

// ContainsMarkdownSyntax checks if the message contains any Markdown formatting
func ContainsMarkdownSyntax(text string) bool {
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

// ConvertMarkdownToAdaptiveCard converts Markdown-like text to an Adaptive Card
func ConvertMarkdownToAdaptiveCard(markdown string) AdaptiveCard {
	lines := strings.Split(markdown, "\n")
	var body []interface{}
	var codeBlock bool
	var codeBlockText strings.Builder
	var listItems []map[string]interface{}
	var isOrderedList bool
	var inList bool

	// Variables to extract summary
	var summary string
	var firstHeading string
	var firstLine string

	linkRegex := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	// Pre-compile the ordered list regex pattern for better performance
	orderedListRegex := regexp.MustCompile(`^\d+\.\s`)

	for lineNum, line := range lines {
		line = strings.TrimSpace(line)

		// Save first non-empty line for summary fallback
		if lineNum == 0 || (firstLine == "" && line != "") {
			firstLine = line
		}

		if line == "" {
			// If we're in a list, finish the list when encountering an empty line
			if inList && len(listItems) > 0 {
				body = append(body, createListContainer(listItems, isOrderedList))
				listItems = nil
				inList = false
			}
			continue
		}

		// Extract first heading for summary
		if firstHeading == "" {
			if strings.HasPrefix(line, "# ") {
				firstHeading = strings.TrimPrefix(line, "# ")
			} else if strings.HasPrefix(line, "## ") {
				firstHeading = strings.TrimPrefix(line, "## ")
			} else if strings.HasPrefix(line, "### ") {
				firstHeading = strings.TrimPrefix(line, "### ")
			}
		}

		// Handle code blocks
		if strings.HasPrefix(line, "```") {
			if codeBlock {
				// End of code block
				codeBlock = false
				body = append(body, map[string]interface{}{
					"type":  "Container",
					"style": "emphasis",
					"items": []interface{}{
						map[string]interface{}{
							"type":     "TextBlock",
							"text":     codeBlockText.String(),
							"wrap":     true,
							"fontType": "Monospace",
						},
					},
				})
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
					body = append(body, createListContainer(listItems, isOrderedList))
					listItems = nil
				}
				inList = true
				isOrderedList = false
			}
			listItems = append(listItems, map[string]interface{}{
				"type": "TextBlock",
				"text": "• " + strings.TrimPrefix(strings.TrimPrefix(line, "- "), "* "),
				"wrap": true,
			})
			continue
		}

		orderedListMatch := orderedListRegex.MatchString(line)
		if orderedListMatch {
			if !inList || !isOrderedList {
				// Start a new ordered list
				if inList {
					// Finish the previous list
					body = append(body, createListContainer(listItems, isOrderedList))
					listItems = nil
				}
				inList = true
				isOrderedList = true
			}
			// Extract number and text
			parts := strings.SplitN(line, ". ", 2)
			if len(parts) == 2 {
				listItems = append(listItems, map[string]interface{}{
					"type": "TextBlock",
					"text": parts[0] + ". " + parts[1],
					"wrap": true,
				})
			}
			continue
		}

		// If we were in a list but current line is not a list item, finish the list
		if inList && !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "* ") && !orderedListMatch {
			body = append(body, createListContainer(listItems, isOrderedList))
			listItems = nil
			inList = false
		}

		// Handle headings
		if strings.HasPrefix(line, "# ") {
			body = append(body, map[string]interface{}{
				"type":   "TextBlock",
				"text":   strings.TrimPrefix(line, "# "),
				"size":   "ExtraLarge",
				"weight": "Bolder",
				"wrap":   true,
			})
			continue
		}

		if strings.HasPrefix(line, "## ") {
			body = append(body, map[string]interface{}{
				"type":   "TextBlock",
				"text":   strings.TrimPrefix(line, "## "),
				"size":   "Large",
				"weight": "Bolder",
				"wrap":   true,
			})
			continue
		}

		if strings.HasPrefix(line, "### ") {
			body = append(body, map[string]interface{}{
				"type":   "TextBlock",
				"text":   strings.TrimPrefix(line, "### "),
				"size":   "Medium",
				"weight": "Bolder",
				"wrap":   true,
			})
			continue
		}

		// Handle bold text (e.g., **text**)
		if strings.Contains(line, "**") {
			// We will process the entire line, but check if it's purely a bold line
			if strings.HasPrefix(line, "**") && strings.HasSuffix(line, "**") && strings.Count(line, "**") == 2 {
				text := strings.TrimPrefix(strings.TrimSuffix(line, "**"), "**")
				body = append(body, map[string]interface{}{
					"type":   "TextBlock",
					"text":   text,
					"weight": "Bolder",
					"wrap":   true,
				})
				continue
			}
		}

		// Handle links
		if linkRegex.MatchString(line) {
			// Convert markdown links to Adaptive Card action links
			processedLine := linkRegex.ReplaceAllStringFunc(line, func(match string) string {
				parts := linkRegex.FindStringSubmatch(match)
				if len(parts) == 3 {
					return parts[1] // Just return the link text for display
				}
				return match
			})

			// Add the text block with the link text
			textBlock := map[string]interface{}{
				"type": "TextBlock",
				"text": processedLine,
				"wrap": true,
			}

			// Extract the link info for actions
			links := linkRegex.FindAllStringSubmatch(line, -1)
			if len(links) > 0 {
				actions := make([]interface{}, 0, len(links))
				for _, link := range links {
					if len(link) == 3 {
						actions = append(actions, map[string]interface{}{
							"type":  "Action.OpenUrl",
							"title": link[1],
							"url":   link[2],
						})
					}
				}

				// Create a container with the text and action buttons
				container := map[string]interface{}{
					"type":  "Container",
					"items": []interface{}{textBlock},
					"selectAction": map[string]interface{}{
						"type": "Action.OpenUrl",
						"url":  links[0][2], // Use the first link as default action
					},
				}

				body = append(body, container)
				continue
			}
		}

		// Default: Treat as regular text
		body = append(body, map[string]interface{}{
			"type": "TextBlock",
			"text": line,
			"wrap": true,
		})
	}

	// Check if we need to finalize a list
	if inList && len(listItems) > 0 {
		body = append(body, createListContainer(listItems, isOrderedList))
	}

	// Use the first heading as summary if available, otherwise use first line
	if firstHeading != "" {
		summary = firstHeading
	} else {
		// Clean up any markdown in the first line for the summary
		summary = StripMarkdown(firstLine)
	}

	// Create the fallback plain text version from the markdown
	plainText := ConvertMarkdownToPlainText(markdown)

	return AdaptiveCard{
		Type:    "AdaptiveCard",
		Body:    body,
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Version: "1.5",
		Summary: summary,   // Required by MS Teams
		Text:    plainText, // Fallback for clients that don't support cards
	}
}

// StripMarkdown removes common markdown syntax from text
func StripMarkdown(text string) string {
	// Remove heading markers
	text = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(
		text, "# "), "## "), "### ")

	// Remove bold markers
	text = strings.ReplaceAll(text, "**", "")

	// Remove link syntax, keeping just the text
	linkRegex := regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	text = linkRegex.ReplaceAllString(text, "$1")

	return text
}

// ConvertMarkdownToPlainText converts markdown to plain text for fallback
func ConvertMarkdownToPlainText(markdown string) string {
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
			line = StripMarkdown(line)

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

// createListContainer creates a container for list items
func createListContainer(items []map[string]interface{}, ordered bool) map[string]interface{} {
	container := map[string]interface{}{
		"type":  "Container",
		"items": items,
	}

	// Add a style property if it's an ordered list to visually distinguish it
	if ordered {
		container["style"] = "default"
	}

	return container
}
