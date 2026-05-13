package services

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

func CreateIncident(teamID string, content *map[string]interface{}, params ...*map[string]string) error {
	var cfg *config.Config

	if len(params) > 0 {
		cfg = config.GetConfigWitParamsOverwrite(params[0])
	} else {
		cfg = config.GetConfig()
	}

	// Initialization of providers and alert
	factory := common.NewAlertProviderFactory(cfg)
	providers, err := factory.CreateProviders()
	if err != nil {
		return fmt.Errorf("failed to create providers: %v", err)
	}

	alert := core.NewAlert(providers...)

	// Skip AckURL and On-Call if resolved alert
	resolved := isResolved(*content)

	incident := m.NewIncident(teamID, content, resolved)

	// Dereference the Pointer and add AckURL if needed
	contentClone := make(map[string]interface{})
	for k, v := range *content {
		contentClone[k] = v
	}

	if !resolved && cfg.OnCall.Enable {
		ackURL := fmt.Sprintf("%s/api/ack/%s", cfg.PublicHost, incident.ID)
		contentClone["AckURL"] = ackURL

		incident.Content = &contentClone
	}

	// Persist FIRST so every received alert is recorded, even if a
	// downstream channel later fails. Failures here are non-fatal.
	var rec *storage.IncidentRecord
	if store != nil {
		rec = buildIncidentRecord(incident, cfg, contentClone, resolved)
		rec.NotifyStatus = "pending"
		if err := store.SaveIncident(rec); err != nil {
			log.Printf("incident: persist warning: %v", err)
		}
	}

	// Fan out to every enabled channel. SendAllAlerts (unlike the
	// legacy SendAlert) does NOT short-circuit on the first failure —
	// a broken Slack must not silently mute Telegram or Email.
	fanOut := alert.SendAllAlerts(incident)
	sendErr := fanOut.Err

	// Stamp the final fan-out outcome so the UI can show whether the
	// alert actually reached its channels. ChannelsNotified is now
	// the list of providers that SUCCEEDED, not the list of providers
	// that were enabled in config.
	if store != nil && rec != nil {
		rec.ChannelsNotified = fanOut.Succeeded
		switch {
		case sendErr == nil:
			rec.NotifyStatus = "sent"
			rec.NotifyError = ""
		case len(fanOut.Succeeded) == 0:
			rec.NotifyStatus = "failed"
			rec.NotifyError = sendErr.Error()
		default:
			rec.NotifyStatus = "partial"
			rec.NotifyError = sendErr.Error()
		}
		if err := store.SaveIncident(rec); err != nil {
			log.Printf("incident: persist status warning: %v", err)
		}
	}

	// On-call escalation. We still kick this off even when *some*
	// channels failed — partial delivery is exactly the case where an
	// escalation matters most. Only skip when no channel succeeded
	// AND there are no channels configured at all (nothing to escalate
	// off of).
	var oncallErr error
	if !resolved && cfg.OnCall.Enable {
		workflow := core.GetOnCallWorkflow()
		if err := workflow.Start(incident.ID, cfg.OnCall); err != nil {
			oncallErr = err
			// Walk back the optimistic OnCallTriggered flag set at
			// build time so the UI does not lie. Best-effort
			// persistence; the alert outcome above is the source of
			// truth.
			if store != nil && rec != nil {
				rec.OnCallTriggered = false
				rec.OnCallError = err.Error()
				if err := store.SaveIncident(rec); err != nil {
					log.Printf("incident: persist oncall status warning: %v", err)
				}
			}
		}
	}

	switch {
	case sendErr != nil && oncallErr != nil:
		return fmt.Errorf("send: %w; oncall: %v", sendErr, oncallErr)
	case sendErr != nil:
		return sendErr
	case oncallErr != nil:
		return oncallErr
	}
	return nil
}

// buildIncidentRecord copies the alert into a durable IncidentRecord.
// ChannelsEnabled snapshots the channels configured at fire time;
// ChannelsNotified stays empty here and is filled in after the
// fan-out so it reflects channels that ACTUALLY succeeded.
// OnCallTriggered likewise starts at the optimistic value and is
// flipped back to false if workflow.Start fails.
func buildIncidentRecord(incident *m.Incident, cfg *config.Config, content map[string]interface{}, resolved bool) *storage.IncidentRecord {
	rec := &storage.IncidentRecord{
		ID:              incident.ID,
		TeamID:          incident.TeamID,
		Title:           firstString(content, "title", "alertname", "summary", "subject", "name"),
		Service:         firstString(content, "service", "service_name", "app", "component"),
		Source:          "http",
		Resolved:        resolved,
		ChannelsEnabled: enabledChannels(cfg),
		OnCallTriggered: !resolved && cfg.OnCall.Enable,
		CreatedAt:       time.Now().UTC(),
		Content:         content,
	}
	return rec
}

// enabledChannels returns the names of every alert channel currently
// enabled in cfg. Order is stable for snapshot diffs.
func enabledChannels(cfg *config.Config) []string {
	var out []string
	if cfg.Alert.Slack.Enable {
		out = append(out, "slack")
	}
	if cfg.Alert.Telegram.Enable {
		out = append(out, "telegram")
	}
	if cfg.Alert.Viber.Enable {
		out = append(out, "viber")
	}
	if cfg.Alert.Email.Enable {
		out = append(out, "email")
	}
	if cfg.Alert.MSTeams.Enable {
		out = append(out, "msteams")
	}
	if cfg.Alert.Lark.Enable {
		out = append(out, "lark")
	}
	return out
}

// firstString returns the first non-empty string value found at any of
// the given keys (case-insensitive on the key match). Used to derive a
// human-friendly title from a free-form alert payload.
func firstString(content map[string]interface{}, keys ...string) string {
	lower := make(map[string]interface{}, len(content))
	for k, v := range content {
		lower[strings.ToLower(k)] = v
	}
	for _, k := range keys {
		if v, ok := lower[strings.ToLower(k)]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// isResolved checks if the alert is resolved by checking common status fields
func isResolved(content map[string]interface{}) bool {
	// List of common field names that might indicate status
	statusFields := []string{"status", "state", "alertState"}

	for _, field := range statusFields {
		if val, ok := content[field]; ok {
			if strVal, isString := val.(string); isString {
				// Case-insensitive check for "resolved"
				return strings.EqualFold(strVal, "resolved")
			}
		}
	}

	// Not resolved (trigger On-Call)
	return false
}
