package services

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/metrics"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/utils"

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
		rec = buildIncidentRecord(incident, cfg, contentClone, resolved, sourceHint(params...))
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
	// Notification delivery status — drives both the persisted record and
	// the incidents_total metric (counted even when storage is disabled).
	status := "sent"
	switch {
	case sendErr == nil:
		status = "sent"
	case len(fanOut.Succeeded) == 0:
		status = "failed"
	default:
		status = "partial"
	}
	metrics.IncidentsTotal.WithLabelValues(status).Inc()

	if store != nil && rec != nil {
		rec.ChannelsNotified = fanOut.Succeeded
		rec.NotifyStatus = status
		if sendErr != nil {
			rec.NotifyError = sendErr.Error()
		} else {
			rec.NotifyError = ""
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
// flipped back to false if workflow.Start fails. hint is the ingress
// source hint ("sns"/"sqs"/...) supplied by the calling adapter; it is
// ignored for agent-originated incidents, which carry their own Source.
func buildIncidentRecord(incident *m.Incident, cfg *config.Config, content map[string]interface{}, resolved bool, hint string) *storage.IncidentRecord {
	rec := &storage.IncidentRecord{
		ID:              incident.ID,
		OrgID:           storage.DefaultOrgID,
		TeamID:          incident.TeamID,
		Title:           firstString(content, "title", "alertname", "summary", "subject", "name"),
		Service:         firstString(content, "service", "service_name", "app", "component"),
		Source:          resolveSource(content, hint),
		Resolved:        resolved,
		ChannelsEnabled: enabledChannels(cfg),
		OnCallTriggered: !resolved && cfg.OnCall.Enable,
		CreatedAt:       time.Now().UTC(),
		Content:         content,
	}
	return rec
}

// sourceHintKey is the reserved params key used by ingress adapters
// (SNS, SQS) to tell buildIncidentRecord which transport delivered the
// alert. It is NOT a config-overwrite key: GetConfigWitParamsOverwrite
// ignores it. The public HTTP webhook strips any caller-supplied value
// so it cannot be spoofed via query string.
const sourceHintKey = "incident_source"

// sourceHint extracts the ingress source hint from the optional params
// overwrite map. Empty when no hint was supplied.
func sourceHint(params ...*map[string]string) string {
	if len(params) == 0 || params[0] == nil {
		return ""
	}
	return strings.TrimSpace((*params[0])[sourceHintKey])
}

// resolveSource decides the durable Source label for an incident.
// Agent-originated incidents are self-describing: they carry a Source
// like "agent:elasticsearch:prod-app" in their content, so that value
// wins. Otherwise the ingress hint ("sns"/"sqs") is used, falling back
// to "webhook" for the standard webhook path.
func resolveSource(content map[string]interface{}, hint string) string {
	if utils.IsAgentIncident(content) {
		if s, ok := content["Source"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		return "agent"
	}
	if hint != "" {
		return hint
	}
	return "webhook"
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
