package services

import (
	"github.com/VersusControl/versus-incident/pkg/core"
)

// CreateIncidentFromFinding maps an AI SRE finding into the standard
// content map and delegates to CreateIncident so all existing channels
// (Slack, Telegram, MS Teams, Lark, Email, Viber) and the on-call
// workflow trigger with no per-channel changes.
//
// Source identifies the agent + signal source (e.g. "agent:elasticsearch:prod-app").
// Service is the operator-facing service name extracted from the log
// pattern; "_unknown" when no service_pattern matched.
//
// All keys added here are stable and additive — existing template files
// keep working unchanged. Operators who want to surface AI-specific
// detail can reference {{ .Summary }}, {{ .Suggestions }},
// {{ .Confidence }}, {{ .PatternID }}, {{ .Verdict }}.
func CreateIncidentFromFinding(f *core.AIFinding, r core.AgentResult, source, service string) error {
	if f == nil {
		return nil
	}
	if service == "" {
		service = "_unknown"
	}

	// First sample message, if any, becomes Logs/Description so the
	// existing channel templates that key off .Logs or .description
	// render useful context out of the box.
	var firstSample string
	if len(r.SampleSignals) > 0 {
		firstSample = r.SampleSignals[0].Message
	}

	content := map[string]interface{}{
		// AI-supplied
		"AlertName":   f.Title,
		"Summary":     f.Summary,
		"Severity":    f.Severity,
		"Category":    f.Category,
		"Confidence":  f.Confidence,
		"Suggestions": f.Suggestions,

		// Pattern context
		"PatternID":       r.PatternID,
		"PatternTemplate": r.Template,
		"Frequency":       r.Frequency,
		"Baseline":        r.Baseline,
		"Verdict":         r.Verdict.String(),

		// Provenance
		"ServiceName": service,
		"Service":     service, // alias used by some default templates
		"Source":      source,

		// Default-template friendliness
		"Logs":        firstSample,
		"description": f.Summary,

		// "firing" so isResolved returns false → AckURL injected, on-call
		// escalation triggers when configured.
		"Status": "firing",
	}

	// Escalate to on-call for high/critical severity findings when
	// on-call is configured. Lower severities skip on-call even if it
	// is globally enabled — agent medium/low findings are informational
	// and should not page operators.
	params := map[string]string{}
	switch f.Severity {
	case "critical", "high":
		params["oncall_enable"] = "true"
	default:
		params["oncall_enable"] = "false"
	}

	return CreateIncident("", &content, &params)
}
