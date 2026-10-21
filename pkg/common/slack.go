package common

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	token                       string
	publicHost                  string
	kubernetesIssueTemplatePath string
	client                      *slack.Client
	channelID                   string
	templatePath                string
	msgProps                    config.SlackMessageProperties
}

func NewSlackProvider(cfg config.SlackConfig) *SlackProvider {
	return &SlackProvider{
		token:                       cfg.Token,
		kubernetesIssueTemplatePath: cfg.KubernetesIssueTemplatePath,
		client:                      slack.New(cfg.Token),
		channelID:                   cfg.ChannelID,
		templatePath:                cfg.TemplatePath,
		msgProps:                    cfg.MessageProperties,
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
	validLinks := []slackURLAction{}
	if i.Content != nil {
		validLinks = validIssueActions((*i.Content)["Links"], s.publicHost)
	}
	safeContent := make(map[string]interface{}, len(contentToUse))
	for key, value := range contentToUse {
		safeContent[key] = value
	}
	if i.Content != nil && (*i.Content)["Links"] != nil {
		links := []map[string]string{}
		for _, link := range validLinks {
			links = append(links, map[string]string{"id": link.ID, "label": link.Label, "url": link.URL})
		}
		safeContent["Links"] = links
		for _, key := range []string{"TimelineURL", "AgentURL"} {
			value, _ := safeContent[key].(string)
			valid := false
			for _, link := range validLinks {
				if value == link.URL {
					valid = true
				}
			}
			if !valid {
				delete(safeContent, key)
			}
		}
	}
	if utils.IsKubernetesIssueIncident(safeContent) && ackURL != "" {
		if len(validIssueActions([]map[string]string{{"id": "ack_incident", "label": "Acknowledge", "url": ackURL}}, s.publicHost)) == 0 {
			ackURL = ""
			delete(safeContent, "AckURL")
		}
	}
	contentToUse = safeContent
	actions := []slackURLAction{}
	if !s.msgProps.DisableButton {
		if ackURL != "" {
			label := s.msgProps.ButtonText
			if label == "" {
				label = "Acknowledge Alert"
			}
			actions = append(actions, slackURLAction{"ack_incident", label, ackURL})
		}
		actions = append(actions, validLinks...)
		if len(actions) > 3 {
			actions = actions[:3]
		}
		if len(actions) > 0 {
			copyContent := make(map[string]interface{}, len(contentToUse))
			for k, v := range contentToUse {
				copyContent[k] = v
			}
			for _, k := range []string{"Links", "TimelineURL", "AgentURL", "AckURL"} {
				delete(copyContent, k)
			}
			contentToUse = copyContent
		}
	}

	// Render the template with the processed content
	messageText, err := s.renderTemplateWithContent(contentToUse)
	if err != nil {
		return err
	}

	// Red color for unresolved incidents
	color := "#C70039"
	if utils.IsKubernetesIssueIncident(contentToUse) && contentToUse["Severity"] != "critical" {
		color = "#D99A00"
	}

	// Determine whether to use button or standard message format
	if !s.msgProps.DisableButton && len(actions) > 0 {
		// Send message with interactive button
		return s.sendMessageWithActions(messageText, color, actions, i.ID)
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
	if utils.IsKubernetesIssueIncident(content) {
		tplPath = utils.KubernetesIssueTemplatePath(s.kubernetesIssueTemplatePath, utils.KubernetesIssueSlackTemplatePath)
	} else if utils.IsAgentIncident(content) {
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
	label := s.msgProps.ButtonText
	if label == "" {
		label = "Acknowledge Alert"
	}
	return s.sendMessageWithActions(messageText, color, []slackURLAction{{"ack_incident", label, ackURL}}, incidentID)
}

type slackURLAction struct{ ID, Label, URL string }

func validIssueActions(value interface{}, publicHost string) []slackURLAction {
	origin, err := url.Parse(publicHost)
	if err != nil || origin.Host == "" || origin.User != nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var links []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		URL   string `json:"url"`
	}
	if json.Unmarshal(encoded, &links) != nil {
		return nil
	}
	actions := []slackURLAction{}
	seen := map[string]bool{}
	for _, link := range links {
		u, err := url.Parse(link.URL)
		if err != nil || len(link.URL) > 3000 || len(link.Label) == 0 || len(link.Label) > 75 || len(link.ID) == 0 || len(link.ID) > 100 || seen[link.ID] || u.User != nil || !strings.EqualFold(u.Scheme, origin.Scheme) || !strings.EqualFold(u.Host, origin.Host) || strings.ContainsAny(link.URL, "\\\r\n") {
			continue
		}
		actions = append(actions, slackURLAction{link.ID, link.Label, link.URL})
		seen[link.ID] = true
		if len(actions) == 3 {
			break
		}
	}
	return actions
}

func (s *SlackProvider) sendMessageWithActions(messageText, color string, actions []slackURLAction, incidentID string) error {
	// Create text block for the main message content
	headerText := slack.NewTextBlockObject("mrkdwn", messageText, false, false)
	headerSection := slack.NewSectionBlock(headerText, nil, nil)

	elements := []slack.BlockElement{}
	for _, action := range actions {
		btn := slack.NewButtonBlockElement(action.ID, incidentID, slack.NewTextBlockObject("plain_text", action.Label, false, false))
		btn.URL = action.URL
		if action.ID == "ack_incident" {
			btn.Style = slack.Style(s.msgProps.ButtonStyle)
			if btn.Style == "" {
				btn.Style = "primary"
			}
		}
		elements = append(elements, btn)
	}
	actionBlock := slack.NewActionBlock("incident_actions", elements...)

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
