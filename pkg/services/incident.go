package services

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/common"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/utils"

	m "github.com/VersusControl/versus-incident/pkg/models"
)

func CreateIncident(teamID string, content *map[string]interface{}, params ...*map[string]string) error {
	var cfg *config.Config

	// Resolve the effective config for THIS incident with runtime-override →
	// YAML → default precedence, on a per-request clone (never mutating global
	// config). The runtime channel override (nil-inert in OSS) overlays a
	// channel's credentials + enable; the existing per-incident routing params
	// still apply on top. CreateIncident has no request ctx today, so the
	// single-org resolver uses its boot-pinned org via context.Background — no
	// signature change to the emission path. Providers are rebuilt from this
	// clone every call (no sender cache), so a runtime channel change takes
	// effect on the NEXT incident with no restart (read-through hot-reload).
	var p *map[string]string
	if len(params) > 0 {
		p = params[0]
	}
	cfg = config.GetConfigForAlert(context.Background(), p)

	// Initialization of providers and alert
	factory := common.NewAlertProviderFactory(cfg)
	providers, err := factory.CreateProviders()
	if err != nil {
		return fmt.Errorf("failed to create providers: %v", err)
	}

	alert := core.NewAlert(providers...)

	// Skip AckURL and On-Call if resolved alert
	resolved := isResolved(*content)

	// Webhook auto-resolve: an incident that arrived via the PUBLIC webhook
	// intake — and is NOT already resolved by its payload — runs the FULL
	// normal unresolved webhook flow (AckURL injection when on-call is enabled,
	// persist, fan out, start on-call escalation), and is THEN stamped resolved
	// in backend storage as a follow-up write. The only delta versus a normal
	// webhook incident is the persisted resolved / resolved_at: alerting,
	// AckURL, and on-call are identical. It is default ON and operator-
	// toggleable, and scoped STRICTLY to the webhook origin (durable source ==
	// "webhook"), so SNS/SQS transports and agent-emitted incidents are never
	// auto-resolved. The intake blob is read once per request, mirroring how
	// effective config is resolved above.
	hint := sourceHint(params...)
	autoResolve := shouldAutoResolveWebhook(resolved, resolveSource(*content, hint), LoadIntakeSettings(store))

	incident := m.NewIncident(teamID, content, resolved)

	// Dereference the Pointer and add AckURL if needed. Auto-resolve does NOT
	// gate this: only a payload-resolved incident skips the AckURL, exactly as
	// a normal incident would.
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
	// downstream channel later fails. Failures here are non-fatal. The record
	// is persisted with its live resolved state (payload-resolved only); a
	// webhook auto-resolve is applied as a follow-up write AFTER fan-out and
	// on-call, so it never gates them.
	var rec *storage.IncidentRecord
	if store != nil {
		rec = buildIncidentRecord(incident, cfg, contentClone, resolved, hint)
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
		if !core.IsOnCallWorkflowInitialized() {
			// On-call was requested (globally or via a per-incident
			// override) but the workflow singleton was never initialized
			// at boot — escalation is simply skipped so the incident still
			// persists and the caller keeps running instead of panicking.
			log.Printf("incident: on-call requested for %s but workflow not initialized; skipping on-call", incident.ID)
			if store != nil && rec != nil && rec.OnCallTriggered {
				rec.OnCallTriggered = false
				if err := store.SaveIncident(rec); err != nil {
					log.Printf("incident: persist oncall status warning: %v", err)
				}
			}
		} else {
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
	}

	// Webhook auto-resolve finalize: the full unresolved flow above (AckURL,
	// fan-out, on-call) has already run byte-for-byte as a normal incident.
	// The ONLY additional effect is to stamp the stored record resolved so it
	// leaves the open list — the same load / set resolved+resolved_at / save
	// the admin resolve endpoint performs.
	if autoResolve && store != nil && rec != nil {
		now := time.Now().UTC()
		rec.Resolved = true
		rec.ResolvedAt = &now
		if err := store.SaveIncident(rec); err != nil {
			log.Printf("incident: persist auto-resolve warning: %v", err)
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
	// Origin is the coarse classifier the UI uses to separate the
	// AI-detected feed from the inbound-alert firehose. Agent-originated
	// incidents (built by CreateIncidentFromFinding) classify as
	// OriginAIDetect; every ingress path — the public webhook plus the
	// SNS/SQS hints — classifies as OriginWebhook.
	origin := storage.OriginWebhook
	if utils.IsAgentIncident(content) {
		origin = storage.OriginAIDetect
	}
	rec := &storage.IncidentRecord{
		ID:              incident.ID,
		OrgID:           storage.DefaultOrgID,
		TeamID:          incident.TeamID,
		Title:           firstString(content, "title", "alertname", "summary", "subject", "name"),
		Service:         firstString(content, "service", "service_name", "app", "component"),
		Source:          resolveSource(content, hint),
		Origin:          origin,
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

// webhookSource is the durable Source label resolveSource assigns to the plain
// public-webhook intake path (no transport hint, not agent-emitted). The
// webhook auto-resolve gate keys on it, so only true webhook-origin incidents
// are affected — SNS/SQS transports and agent emits carry a different Source.
const webhookSource = "webhook"

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
	return webhookSource
}

// shouldAutoResolveWebhook decides whether an incident should be auto-resolved
// on intake: it must have arrived via the PUBLIC webhook (durable source ==
// "webhook"), must NOT already be resolved by its payload, and the operator
// must have the auto-resolve toggle enabled. It is the single decision point
// that keeps SNS/SQS transports and agent-emitted incidents (whose source is
// never "webhook") out of the auto-resolve path.
func shouldAutoResolveWebhook(alreadyResolved bool, source string, settings IntakeSettings) bool {
	if alreadyResolved {
		return false
	}
	if !settings.AutoResolveWebhook {
		return false
	}
	return source == webhookSource
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
