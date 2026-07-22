package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
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

// Name implements core.AlertProvider.
func (s *SlackProvider) Name() string { return "slack" }

// SendAttachment implements core.AttachmentSender: it uploads the report
// PNG to the configured channel with the caption as the message. It uses
// the slack-go three-step external-upload flow (files.getUploadURLExternal
// → upload → files.completeUploadExternal), which is the modern replacement
// for the retired files.upload endpoint.
//
// Unlike alerts (which post a message via chat.postMessage and only require
// the bot to be able to post to a public channel), sharing a file into a
// channel requires the bot to be a *member*. So before uploading we make a
// best-effort attempt to join the channel — this self-heals the common
// public-channel case when the bot has the channels:join scope, and is
// harmless when it can't (private channel / missing scope), in which case we
// still try the upload and surface an actionable error if it's rejected.
func (s *SlackProvider) SendAttachment(i *m.Incident, att core.Attachment) error {
	if len(att.Data) == 0 {
		return fmt.Errorf("slack: empty attachment")
	}
	ctx := context.Background()

	// Best-effort auto-join: ignore the error. Joining only works for public
	// channels when the bot holds the channels:join scope; it legitimately
	// fails otherwise, and we still attempt the upload below.
	_, _, _, _ = s.client.JoinConversationContext(ctx, s.channelID)

	_, err := s.client.UploadFileContext(ctx, slack.UploadFileParameters{
		Reader:         bytes.NewReader(att.Data),
		FileSize:       len(att.Data),
		Filename:       att.Filename,
		Title:          att.Filename,
		InitialComment: att.Caption,
		Channel:        s.channelID,
	})
	if err != nil {
		if isNotInChannel(err) {
			return fmt.Errorf("slack upload failed: the bot is not a member of channel %s — invite it in Slack (\"/invite @<bot>\") or grant the channels:join scope so it can upload report files (alerts post messages and don't need this)", s.channelID)
		}
		return fmt.Errorf("slack upload: %w", err)
	}
	return nil
}

// isNotInChannel reports whether a Slack API error is the not_in_channel
// rejection returned when the bot tries to share a file into a channel it
// hasn't joined. It inspects the typed slack.SlackErrorResponse first and
// falls back to a substring match on the error string.
func isNotInChannel(err error) bool {
	if err == nil {
		return false
	}
	var slackErr slack.SlackErrorResponse
	if errors.As(err, &slackErr) && slackErr.Err == "not_in_channel" {
		return true
	}
	return strings.Contains(err.Error(), "not_in_channel")
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
	if !s.msgProps.DisableButton && ackURL != "" {
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

	// Extract and remove AckURL from content
	if !s.msgProps.DisableButton {
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

	// If button is disabled, just return the original content and extract AckURL
	if urlVal, ok := (*i.Content)["AckURL"]; ok {
		if urlStr, ok := urlVal.(string); ok {
			ackURL = urlStr
		}
	}

	return *i.Content, ackURL
}

// renderTemplateWithContent renders the template with the given content map.
// Agent-emitted incidents (detect mode) are rendered through the shared
// agent template instead of the per-channel one.
func (s *SlackProvider) renderTemplateWithContent(content map[string]interface{}) (string, error) {
	funcMaps := utils.GetTemplateFuncMaps()

	tplPath := s.templatePath
	if utils.IsAgentIncident(content) {
		tplPath = utils.AgentSlackTemplatePath
	}

	tmpl, err := template.New(filepath.Base(tplPath)).Funcs(funcMaps).ParseFiles(tplPath)
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
