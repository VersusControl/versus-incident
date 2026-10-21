package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// CreateIncidentFromKubernetesIssue uses the same detection episode and fan-out as AI findings.
func CreateIncidentFromKubernetesIssue(org string, a kubernetes.IssueAlert) (kubernetes.Outcome, error) {
	return CreateIncidentFromKubernetesIssueContext(context.Background(), org, a)
}

// CreateIncidentFromKubernetesIssueContext checks the capture budget before admission.
func CreateIncidentFromKubernetesIssueContext(ctx context.Context, org string, a kubernetes.IssueAlert) (kubernetes.Outcome, error) {
	org = storage.NormalizeOrgID(org)
	if err := ctx.Err(); err != nil {
		return kubernetes.Outcome{}, err
	}
	if a.Cluster == "" || a.Rule == "" || (a.Severity != "warning" && a.Severity != "critical") {
		return kubernetes.Outcome{}, errors.New("invalid Kubernetes issue alert")
	}
	content := kubernetesIssueContent(a, config.GetConfig().PublicHost)
	emission := &core.DetectionEmissionResult{}
	severity := content["Severity"].(string)
	conditionKey := a.Rule
	if a.RollupCount > 0 {
		conditionKey += ":" + a.BrokeAt.UTC().Format(time.RFC3339Nano)
	}
	err := emitDetection(content, storage.DetectionIdentity{
		AgentKind: "kubernetes", Source: "kubernetes:" + a.Cluster,
		Service:    a.Namespace + "/" + a.WorkloadKind + "/" + a.Workload,
		SignalKind: "kubernetes_issue", ConditionKey: conditionKey,
	}, severity, kubernetesIssueParams(severity), org, 1, emission, storage.DefaultDetectionNotificationLease, ctx)
	return kubernetes.Outcome{IncidentID: emission.IncidentID, NotificationOutcome: emission.NotificationOutcome}, err
}

func kubernetesIssueParams(severity string) map[string]string {
	return map[string]string{"oncall_enable": fmt.Sprint(severity == "critical")}
}

func kubernetesIssueContent(a kubernetes.IssueAlert, publicHost string) map[string]interface{} {
	severity := "medium"
	if a.Severity == "critical" {
		severity = "critical"
	}
	title := a.WorkloadKind + " " + a.Workload + " " + a.RuleLabel
	if a.RollupCount > 0 {
		title = fmt.Sprintf("%d more issues in cluster %s", a.RollupCount, a.Cluster)
	}
	c := map[string]interface{}{
		"AlertKind": "kubernetes_issue", "AlertName": title, "Severity": severity,
		"KubernetesSeverity": a.Severity, "Cluster": a.Cluster, "Namespace": a.Namespace,
		"WorkloadKind": a.WorkloadKind, "Workload": a.Workload, "Rule": a.Rule, "RuleLabel": a.RuleLabel,
		"Affected": fmt.Sprintf("%d of %d pods", a.Count, a.Of), "BrokeAt": a.BrokeAt.UTC().Format(time.RFC3339),
		"BrokeAgo": fmt.Sprintf("%s ago", max(time.Duration(0), time.Since(a.BrokeAt)).Round(time.Second)),
		"Evidence": a.Evidence, "Changes": a.Changes, "HistoryIncomplete": a.HistoryIncomplete,
		"ServiceName": a.Namespace + "/" + a.Workload, "Service": a.Namespace + "/" + a.Workload,
		"Source": "kubernetes:" + a.Cluster, "PatternID": "kubernetes:" + a.Rule, "Status": "firing",
	}
	if a.Changes == nil && !a.HistoryIncomplete {
		// Coalesced observations omit optional enrichment so the stored incident
		// retains the bounded history collected on its first admission.
		delete(c, "Changes")
		delete(c, "HistoryIncomplete")
	}
	scrubber := reportScrubber()
	for key, value := range c {
		switch v := value.(type) {
		case string:
			if len(v) > 512 {
				v = v[:512]
			}
			c[key] = scrubber.Scrub(strings.Join(strings.Fields(v), " "))
		case []string:
			limit := 5
			if key == "Changes" {
				limit = 3
			}
			lines := []string{}
			for _, line := range v[:min(len(v), limit)] {
				if len(line) > 256 {
					line = line[:256]
				}
				lines = append(lines, scrubber.Scrub(strings.Join(strings.Fields(line), " ")))
			}
			c[key] = lines
		}
	}
	u, err := url.Parse(publicHost)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return c
	}
	origin := u.Scheme + "://" + u.Host
	timeline := url.Values{"cluster": {a.Cluster}, "view": {"timeline"}, "root_uid": {a.RootUID}, "namespace": {a.Namespace}, "center": {a.BrokeAt.UTC().Format(time.RFC3339)}}
	agent := url.Values{"provider": {"kubernetes"}, "cluster": {a.Cluster}, "resource_id": {a.ResourceID}, "name": {a.Workload}, "namespace": {a.Namespace}, "uid": {a.RootUID}, "include_changes": {"true"}, "issue": {a.Rule}}
	timelineURL, agentURL := origin+"/agent/kubernetes?"+timeline.Encode(), origin+"/agent/chat?"+agent.Encode()
	links := []map[string]string{{"id": "timeline", "label": "Open timeline", "url": timelineURL}, {"id": "agent", "label": "Investigate with DevOps Agent", "url": agentURL}}
	if a.RollupCount > 0 {
		timelineURL = origin + "/agent/kubernetes?" + url.Values{"cluster": {a.Cluster}, "view": {"issues"}}.Encode()
		links = []map[string]string{{"id": "issues", "label": "Open issues", "url": timelineURL}}
	} else {
		c["AgentURL"] = agentURL
	}
	c["TimelineURL"], c["Links"] = timelineURL, links
	return c
}

// ConfigureKubernetesIssueAlerts attaches an org-aware sink without a Kubernetes/services import cycle.
func ConfigureKubernetesIssueAlerts(registry *kubernetes.ServiceRegistry) {
	agent.ConfigureKubernetesIssueAlerts(registry, CreateIncidentFromKubernetesIssueContext)
}
