package utils

const (
	KubernetesIssueSlackTemplatePath    = "config/kubernetes_issue_slack.tmpl"
	KubernetesIssueTelegramTemplatePath = "config/kubernetes_issue_telegram.tmpl"
	KubernetesIssueMSTeamsTemplatePath  = "config/kubernetes_issue_msteams.tmpl"
	KubernetesIssueLarkTemplatePath     = "config/kubernetes_issue_lark.tmpl"
	KubernetesIssueViberTemplatePath    = "config/kubernetes_issue_viber.tmpl"
	KubernetesIssueEmailTemplatePath    = "config/kubernetes_issue_email.tmpl"
)

// IsKubernetesIssueIncident reports whether the content is a Kubernetes
// issue that Versus built itself. A webhook body that names the kind is
// not one.
func IsKubernetesIssueIncident(content map[string]interface{}) bool {
	return content != nil && content["AlertKind"] == "kubernetes_issue" && IsInternalAlert(content)
}

func KubernetesIssueTemplatePath(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}
