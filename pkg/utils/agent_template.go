package utils

// Hardcoded per-channel template paths used for incidents emitted by
// the AI SRE Agent (detect mode). Each channel has its own file
// because the markup syntax differs per channel (mrkdwn, HTML,
// Markdown, plain text). Providers render agent-emitted incidents
// through these files instead of their per-channel default templates.
const (
	AgentSlackTemplatePath    = "config/agent_slack.tmpl"
	AgentTelegramTemplatePath = "config/agent_telegram.tmpl"
	AgentMSTeamsTemplatePath  = "config/agent_msteams.tmpl"
	AgentLarkTemplatePath     = "config/agent_lark.tmpl"
	AgentViberTemplatePath    = "config/agent_viber.tmpl"
	AgentEmailTemplatePath    = "config/agent_email.tmpl"
)

// IsAgentIncident returns true when the incident content map was built
// by services.CreateIncidentFromFinding, i.e. originated from the AI
// SRE Agent. Detection is based on stable fields stamped by the agent
// path: a non-empty PatternID OR a Source that begins with "agent:".
func IsAgentIncident(content map[string]interface{}) bool {
	if content == nil {
		return false
	}
	if v, ok := content["PatternID"].(string); ok && v != "" {
		return true
	}
	if v, ok := content["Source"].(string); ok && len(v) >= 6 && v[:6] == "agent:" {
		return true
	}
	return false
}
