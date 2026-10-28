package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestKubernetesIssueContentAllowlistSeverityLinksAndRedaction(t *testing.T) {
	a := kubernetes.IssueAlert{Cluster: "prod & east", Namespace: "shop", WorkloadKind: "Deployment", Workload: "checkout", RootUID: "root&uid", ResourceID: "deployments", Rule: "pod.crash_loop", RuleLabel: "crash looping", Severity: "warning", BrokeAt: time.Now(), Evidence: []string{"password=secret-value", "12 restarts"}, Changes: []string{"Image changed 4m before break"}}
	c := kubernetesIssueContent(a, "https://versus.invalid")
	if c["Severity"] != "medium" || c["KubernetesSeverity"] != "warning" {
		t.Fatal(c)
	}
	if kubernetesIssueParams(c["Severity"].(string))["oncall_enable"] != "false" {
		t.Fatal("warning would page")
	}
	encoded, _ := json.Marshal(c)
	if strings.Contains(string(encoded), "secret-value") || strings.Contains(string(encoded), `"Logs"`) || strings.Contains(string(encoded), "description") || !strings.Contains(c["TimelineURL"].(string), "root_uid=root%26uid") {
		t.Fatal(string(encoded))
	}
	if _, ok := kubernetesIssueContent(a, "")["Links"]; ok {
		t.Fatal("links without origin")
	}
	a.Severity = "critical"
	if c := kubernetesIssueContent(a, ""); c["Severity"] != "critical" {
		t.Fatal(c)
	}
	if kubernetesIssueParams("critical")["oncall_enable"] != "true" {
		t.Fatal("critical would not page")
	}
	a.RollupCount = 4
	c = kubernetesIssueContent(a, "https://versus.invalid")
	if !strings.Contains(c["AlertName"].(string), "4 more issues") || strings.Contains(c["TimelineURL"].(string), "view=timeline") {
		t.Fatal(c)
	}
}

func TestKubernetesIssueCoalescedHistoryAndRollupCriticalEscalation(t *testing.T) {
	loadAgentTestConfig(t)
	previous := store
	SetStorage(storage.NewMemory())
	defer SetStorage(previous)
	var severities []string
	SetEmitInterceptor(func(content map[string]interface{}, _ string) EmitDecision {
		severities = append(severities, content["Severity"].(string))
		return EmitDecision{Action: EmitProceed}
	})
	defer SetEmitInterceptor(nil)
	a := kubernetes.IssueAlert{Cluster: "prod", Namespace: "shop", WorkloadKind: "Deployment", Workload: "api", Rule: "pod.failed", Severity: "warning", BrokeAt: time.Now(), Changes: []string{"Image changed 1m before break"}, HistoryIncomplete: true}
	first, err := CreateIncidentFromKubernetesIssue("org", a)
	if err != nil {
		t.Fatal(err)
	}
	a.Changes, a.HistoryIncomplete = nil, false
	again, err := CreateIncidentFromKubernetesIssue("org", a)
	if err != nil || again.IncidentID != first.IncidentID {
		t.Fatal(again, err)
	}
	record, err := store.GetIncident(first.IncidentID)
	if err != nil || record.Content["HistoryIncomplete"] != true {
		t.Fatal("coalescing cleared history completeness", record, err)
	}
	encoded, _ := json.Marshal(record.Content["Changes"])
	if !strings.Contains(string(encoded), "Image changed") {
		t.Fatal("coalescing cleared changes", record.Content)
	}
	a = kubernetes.IssueAlert{Cluster: "prod", Rule: "storm_rollup", Severity: "warning", BrokeAt: time.Now(), RollupCount: 2}
	warning, err := CreateIncidentFromKubernetesIssue("org", a)
	if err != nil {
		t.Fatal(err)
	}
	a.Severity = "critical"
	critical, err := CreateIncidentFromKubernetesIssue("org", a)
	if err != nil || critical.IncidentID != warning.IncidentID || critical.NotificationOutcome != "sent" {
		t.Fatal(critical, err)
	}
	a.RollupCount = 50
	repeated, err := CreateIncidentFromKubernetesIssue("org", a)
	if err != nil || repeated.IncidentID != warning.IncidentID || repeated.NotificationOutcome != "not_applicable" {
		t.Fatal(repeated, err)
	}
	if len(severities) != 3 || severities[1] != "medium" || severities[2] != "critical" || kubernetesIssueParams(severities[2])["oncall_enable"] != "true" {
		t.Fatal("unbounded or missing critical rollup escalation", severities)
	}
}

func TestKubernetesIssueSharedEpisodeCoalesceEscalateAndOncall(t *testing.T) {
	loadAgentTestConfig(t)
	old := store
	SetStorage(storage.NewMemory())
	t.Cleanup(func() { SetStorage(old); SetEmitInterceptor(nil) })
	var severities []string
	SetEmitInterceptor(func(c map[string]interface{}, _ string) EmitDecision {
		severities = append(severities, c["Severity"].(string))
		return EmitDecision{Action: EmitProceed}
	})
	a := kubernetes.IssueAlert{Cluster: "prod", Namespace: "shop", WorkloadKind: "Deployment", Workload: "checkout", RootUID: "root", Rule: "pod.crash_loop", RuleLabel: "crash looping", Severity: "warning", BrokeAt: time.Now()}
	first, err := CreateIncidentFromKubernetesIssue("org-a", a)
	if err != nil || first.IncidentID == "" || first.NotificationOutcome != "sent" {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := CreateIncidentFromKubernetesIssue("org-a", a)
	if err != nil || second.IncidentID != first.IncidentID || second.NotificationOutcome != "not_applicable" {
		t.Fatalf("%+v %v", second, err)
	}
	a.Severity = "critical"
	third, err := CreateIncidentFromKubernetesIssue("org-a", a)
	if err != nil || third.IncidentID != first.IncidentID {
		t.Fatalf("%+v %v", third, err)
	}
	rec, err := store.GetIncident(first.IncidentID)
	if err != nil || rec.OrgID != "org-a" || rec.Content["Severity"] != "critical" || len(severities) != 2 || severities[0] != "medium" || severities[1] != "critical" {
		t.Fatalf("%+v %+v %v", rec, severities, err)
	}
	other, err := CreateIncidentFromKubernetesIssue("org-b", a)
	if err != nil || other.IncidentID == first.IncidentID || other.IncidentID == "" {
		t.Fatalf("org collision: %+v %v", other, err)
	}
	otherRecord, _ := store.GetIncident(other.IncidentID)
	if otherRecord == nil || otherRecord.OrgID != "org-b" {
		t.Fatal(otherRecord)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateIncidentFromKubernetesIssueContext(ctx, "org-a", a); err == nil {
		t.Fatal("cancellation ignored")
	}
}

// A scoped adapter spy proves claim, heartbeat and completion all carry the org.
type scopedIssueStore struct {
	storage.Provider
	storage.DetectionEpisodeStore
	orgs []string
}

func (s *scopedIssueStore) RecordDetectionOccurrenceForOrg(org string, o storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	s.orgs = append(s.orgs, org)
	return s.RecordDetectionOccurrence(o)
}
func (s *scopedIssueStore) RenewDetectionNotificationClaimForOrg(org string, r storage.DetectionNotificationRenewal) error {
	s.orgs = append(s.orgs, org)
	return s.RenewDetectionNotificationClaim(r)
}
func (s *scopedIssueStore) CompleteDetectionNotificationForOrg(org string, c storage.DetectionNotificationCompletion) error {
	s.orgs = append(s.orgs, org)
	return s.CompleteDetectionNotification(c)
}
func (s *scopedIssueStore) CloseDetectionEpisodeByIncidentForOrg(org, id string, at time.Time) error {
	return s.CloseDetectionEpisodeByIncident(id, at)
}

func TestKubernetesIssueUsesScopedEpisodeStore(t *testing.T) {
	loadAgentTestConfig(t)
	base := storage.NewMemory()
	spy := &scopedIssueStore{Provider: base, DetectionEpisodeStore: base.(storage.DetectionEpisodeStore)}
	old := store
	SetStorage(spy)
	defer SetStorage(old)
	a := kubernetes.IssueAlert{Cluster: "prod", Rule: "pod.failed", Severity: "warning", BrokeAt: time.Now()}
	_, err := CreateIncidentFromKubernetesIssue("org-b", a)
	if err != nil {
		t.Fatal(err)
	}
	if len(spy.orgs) < 2 {
		t.Fatal(spy.orgs)
	}
	for _, org := range spy.orgs {
		if org != "org-b" {
			t.Fatal(spy.orgs)
		}
	}
}
