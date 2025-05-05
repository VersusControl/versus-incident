package common

import (
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/VersusControl/versus-incident/pkg/config"
	m "github.com/VersusControl/versus-incident/pkg/models"
	"github.com/VersusControl/versus-incident/pkg/utils"

	"github.com/slack-go/slack"
)

type SlackProvider struct {
	client       *slack.Client
	channelID    string
	templatePath string
	msgProps     config.SlackMessageProperties
}

func NewSlackProvider(cfg config.SlackConfig) *SlackProvider {
	return &SlackProvider{
		client:       slack.New(cfg.Token),
		channelID:    cfg.ChannelID,
		templatePath: cfg.TemplatePath,
		msgProps:     cfg.MessageProperties,
	}
}

// SendAlert determines whether to process a resolved or unresolved incident
func (s *SlackProvider) SendAlert(i *m.Incident) error {
	if i.Resolved {
		return s.sendResolvedAlert(i)
	} else {
		return s.sendUnresolvedAlert(i)
	}
}

// sendResolvedAlert handles messaging for resolved incidents
func (s *SlackProvider) sendResolvedAlert(i *m.Incident) error {
	// Render the template with the original content
	messageText, err := s.renderTemplateWithContent(*i.Content)
	if err != nil {
		return err
	}

	// Use green color for resolved incidents
	color := "#36A64F"

	// Send standard message
	return s.sendStandardMessage(messageText, color)
}

// sendUnresolvedAlert handles messaging for unresolved incidents
func (s *SlackProvider) sendUnresolvedAlert(i *m.Incident) error {
	// Extract and remove AckURL from content if button acknowledgment is enabled
	contentToUse, ackURL := s.processAckURL(i)

	// Render the template with the processed content
	messageText, err := s.renderTemplateWithContent(contentToUse)
	if err != nil {
		return err
	}

	// Red color for unresolved incidents
	color := "#C70039"

	// Determine whether to use button or standard message format
	if s.msgProps.UseButtonAck && ackURL != "" {
		// Send message with interactive button
		return s.sendMessageWithButton(messageText, color, ackURL, i.ID)
	} else {
		// Send standard message
		return s.sendStandardMessage(messageText, color)
	}
}

// processAckURL extracts and optionally removes the AckURL from incident content
func (s *SlackProvider) processAckURL(i *m.Incident) (map[string]interface{}, string) {
	var ackURL string

	// If content is nil, return empty values
	if i.Content == nil {
		return nil, ""
	}

	// If button ACK is enabled, extract and remove AckURL from content
	if s.msgProps.UseButtonAck {
		// Create a clone of the content to avoid modifying the original
		contentClone := make(map[string]interface{})
		for k, v := range *i.Content {
			contentClone[k] = v
		}

		// Extract AckURL from content if it exists
		if urlVal, ok := contentClone["AckURL"]; ok {
			if urlStr, ok := urlVal.(string); ok {
				ackURL = urlStr
				// Remove AckURL from the content clone so it won't show in the template
				delete(contentClone, "AckURL")
			}
		}

		return contentClone, ackURL
	}

	// If button ACK is not enabled, just return the original content and extract AckURL
	if urlVal, ok := (*i.Content)["AckURL"]; ok {
		if urlStr, ok := urlVal.(string); ok {
			ackURL = urlStr
		}
	}

	return *i.Content, ackURL
}

// renderTemplateWithContent renders the template with the given content map
func (s *SlackProvider) renderTemplateWithContent(content map[string]interface{}) (string, error) {
	funcMaps := utils.GetTemplateFuncMaps()

	tmpl, err := template.New(filepath.Base(s.templatePath)).Funcs(funcMaps).ParseFiles(s.templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var message strings.Builder
	if err := tmpl.Execute(&message, content); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return message.String(), nil
}

// sendMessageWithButton sends a message with an interactive button for acknowledgment
func (s *SlackProvider) sendMessageWithButton(messageText, color, ackURL, incidentID string) error {
	// Create text block for the main message content
	headerText := slack.NewTextBlockObject("mrkdwn", messageText, false, false)
	headerSection := slack.NewSectionBlock(headerText, nil, nil)

	// Get button text from config or use default
	buttonText := s.msgProps.ButtonText
	if buttonText == "" {
		buttonText = "Acknowledge Alert"
	}

	// Create button for acknowledgment
	btnText := slack.NewTextBlockObject("plain_text", buttonText, false, false)
	btnElement := slack.NewButtonBlockElement("ack_incident", incidentID, btnText)
	btnElement.URL = ackURL // Use URL for direct navigation on click

	// Set button style if specified in config
	buttonStyle := s.msgProps.ButtonStyle
	if buttonStyle != "" {
		btnElement.Style = slack.Style(buttonStyle)
	} else {
		btnElement.Style = "primary" // Default to primary style
	}

	actionBlock := slack.NewActionBlock("incident_actions", btnElement)

	// Build the message with blocks
	_, _, err := s.client.PostMessage(
		s.channelID,
		slack.MsgOptionAttachments(slack.Attachment{
			Color: color,
			Blocks: slack.Blocks{
				BlockSet: []slack.Block{headerSection, actionBlock},
			},
		}),
	)

	if err != nil {
		return fmt.Errorf("failed to post message with button: %w", err)
	}

	return nil
}

// sendStandardMessage sends a message using standard Slack attachments
func (s *SlackProvider) sendStandardMessage(messageText, color string) error {
	_, _, err := s.client.PostMessage(
		s.channelID,
		slack.MsgOptionAttachments(slack.Attachment{
			Text:  messageText,
			Color: color,
		}),
	)

	if err != nil {
		return fmt.Errorf("failed to post standard message: %w", err)
	}

	return nil
}
