package utils

const (
	KubernetesIssueSlackTemplatePath    = "config/kubernetes_issue_slack.tmpl"
	KubernetesIssueTelegramTemplatePath = "config/kubernetes_issue_telegram.tmpl"
	KubernetesIssueMSTeamsTemplatePath  = "config/kubernetes_issue_msteams.tmpl"
	KubernetesIssueLarkTemplatePath     = "config/kubernetes_issue_lark.tmpl"
	KubernetesIssueViberTemplatePath    = "config/kubernetes_issue_viber.tmpl"
	KubernetesIssueEmailTemplatePath    = "config/kubernetes_issue_email.tmpl"
)

func IsKubernetesIssueIncident(content map[string]interface{}) bool {
	return content != nil && content["AlertKind"] == "kubernetes_issue"
}

func KubernetesIssueTemplatePath(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}
