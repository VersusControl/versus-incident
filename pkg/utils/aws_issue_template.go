package utils

// Per-channel templates of the AWS issue alerts: an issue that opened or
// regressed, a digest of high issues, and a coverage notice. Like the agent
// templates they are fixed files shipped with the config directory.
const (
	AWSIssueSlackTemplatePath    = "config/aws_issue_slack.tmpl"
	AWSIssueTelegramTemplatePath = "config/aws_issue_telegram.tmpl"
	AWSIssueMSTeamsTemplatePath  = "config/aws_issue_msteams.tmpl"
	AWSIssueLarkTemplatePath     = "config/aws_issue_lark.tmpl"
	AWSIssueViberTemplatePath    = "config/aws_issue_viber.tmpl"
	AWSIssueEmailTemplatePath    = "config/aws_issue_email.tmpl"
)

// IsAWSIssueIncident reports whether the content is an AWS issue alert
// that Versus built itself. A webhook body that names the kind is not one.
func IsAWSIssueIncident(content map[string]interface{}) bool {
	return content != nil && content["AlertKind"] == "aws_issue" && IsInternalAlert(content)
}

// internalAlertKey marks content an internal emission built. Its value
// has a type only Go code can make: content decoded from a webhook body
// holds a string, number, bool, list or map there, never this. So a
// webhook caller cannot pick the AWS or Kubernetes issue templates, which
// trust their content to be built and escaped by Versus.
const internalAlertKey = "InternalAlert"

type internalAlertMark struct{}

// MarshalJSON stores the mark as true, so a stored incident reads cleanly.
func (internalAlertMark) MarshalJSON() ([]byte, error) { return []byte("true"), nil }

// MarkInternalAlert marks content built by Versus itself: an AWS issue
// alert or a Kubernetes issue.
func MarkInternalAlert(content map[string]interface{}) {
	if content != nil {
		content[internalAlertKey] = internalAlertMark{}
	}
}

// IsInternalAlert reports whether Versus built the content itself.
func IsInternalAlert(content map[string]interface{}) bool {
	_, ok := content[internalAlertKey].(internalAlertMark)
	return ok
}

// Alert channels a template is chosen for.
const (
	ChannelSlack    = "slack"
	ChannelTelegram = "telegram"
	ChannelMSTeams  = "msteams"
	ChannelLark     = "lark"
	ChannelViber    = "viber"
	ChannelEmail    = "email"
)

var (
	awsIssueTemplates = map[string]string{
		ChannelSlack: AWSIssueSlackTemplatePath, ChannelTelegram: AWSIssueTelegramTemplatePath, ChannelMSTeams: AWSIssueMSTeamsTemplatePath,
		ChannelLark: AWSIssueLarkTemplatePath, ChannelViber: AWSIssueViberTemplatePath, ChannelEmail: AWSIssueEmailTemplatePath,
	}
	kubernetesIssueTemplates = map[string]string{
		ChannelSlack: KubernetesIssueSlackTemplatePath, ChannelTelegram: KubernetesIssueTelegramTemplatePath, ChannelMSTeams: KubernetesIssueMSTeamsTemplatePath,
		ChannelLark: KubernetesIssueLarkTemplatePath, ChannelViber: KubernetesIssueViberTemplatePath, ChannelEmail: KubernetesIssueEmailTemplatePath,
	}
	agentTemplates = map[string]string{
		ChannelSlack: AgentSlackTemplatePath, ChannelTelegram: AgentTelegramTemplatePath, ChannelMSTeams: AgentMSTeamsTemplatePath,
		ChannelLark: AgentLarkTemplatePath, ChannelViber: AgentViberTemplatePath, ChannelEmail: AgentEmailTemplatePath,
	}
)

// AlertTemplatePath is the template one channel renders an incident with:
// the AWS issue template for an AWS issue alert, the Kubernetes issue
// template (or its configured override) for a Kubernetes issue, the agent
// template for any other detection, and the channel's own template
// otherwise.
func AlertTemplatePath(content map[string]interface{}, channel, channelTemplate, kubernetesIssueOverride string) string {
	switch {
	case IsAWSIssueIncident(content):
		if path, ok := awsIssueTemplates[channel]; ok {
			return path
		}
	case IsKubernetesIssueIncident(content):
		if path, ok := kubernetesIssueTemplates[channel]; ok {
			return KubernetesIssueTemplatePath(kubernetesIssueOverride, path)
		}
	case IsAgentIncident(content):
		if path, ok := agentTemplates[channel]; ok {
			return path
		}
	}
	return channelTemplate
}
