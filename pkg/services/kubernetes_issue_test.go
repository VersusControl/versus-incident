package services

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/VersusControl/versus-incident/pkg/agent"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestKubernetesIssueContentAllowlistSeverityLinksAndRedaction(t *testing.T) {
	loadAgentTestConfig(t)
	// The config is loaded once per process, so pin on-call off (no provider
	// set up) rather than inherit what an earlier test left.
	cfg := config.GetConfig()
	prev := cfg.OnCall
	t.Cleanup(func() { cfg.OnCall = prev })
	cfg.OnCall.Enable, cfg.OnCall.InitializedOnly = false, false
	a := kubernetes.IssueAlert{Cluster: "prod & east", Namespace: "shop", WorkloadKind: "Deployment", Workload: "checkout", RootUID: "root&uid", ResourceID: "deployments", Rule: "pod.crash_loop", RuleLabel: "crash looping", Severity: "warning", BrokeAt: time.Now(), Evidence: []string{"password=secret-value", "12 restarts"}, Changes: []string{"Image changed 4m before break"}}
	c := kubernetesIssueContent(a, "https://versus.invalid")
	if c["Severity"] != "medium" || c["KubernetesSeverity"] != "warning" {
		t.Fatal(c)
	}
	if kubernetesIssueParams(context.Background(), c["Severity"].(string))["oncall_enable"] != "false" {
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
	if _, forced := kubernetesIssueParams(context.Background(), "critical")["oncall_enable"]; forced {
		t.Fatal("with no on-call provider set up a critical issue must not opt in to on-call")
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
	if err != nil || critical.IncidentID != warning.IncidentID || critical.NotificationOutcome != "none" {
		t.Fatal(critical, err)
	}
	a.RollupCount = 50
	repeated, err := CreateIncidentFromKubernetesIssue("org", a)
	if err != nil || repeated.IncidentID != warning.IncidentID || repeated.NotificationOutcome != "not_applicable" {
		t.Fatal(repeated, err)
	}
	if len(severities) != 3 || severities[1] != "medium" || severities[2] != "critical" || kubernetesIssueParams(context.Background(), severities[2])["oncall_enable"] == "false" {
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
	if err != nil || first.IncidentID == "" || first.NotificationOutcome != "none" {
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

// A sender that dies after the channel accepted its notification leaves the
// claim to expire, and the next sender sends again. Both attempts carry the
// same delivery ID, in the template data and as the webhook's
// Idempotency-Key, so the receiver can drop the repeat. The ID is not stored
// on the incident.
func TestKubernetesIssueReplayCarriesTheDeliveryIDOfTheDeadSender(t *testing.T) {
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	previousTeams, previousOnCall := cfg.Alert.MSTeams, cfg.OnCall
	t.Cleanup(func() { cfg.Alert.MSTeams, cfg.OnCall = previousTeams, previousOnCall })
	cfg.OnCall.Enable, cfg.OnCall.InitializedOnly = false, false
	type delivery struct{ header, body string }
	deliveries := make(chan delivery, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		deliveries <- delivery{header: r.Header.Get("Idempotency-Key"), body: string(raw)}
	}))
	defer receiver.Close()
	template := filepath.Join(t.TempDir(), "issue.tmpl")
	if err := os.WriteFile(template, []byte(`{{.Workload}} delivery={{.DeliveryID}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Alert.MSTeams = config.MSTeamsConfig{Enable: true, PowerAutomateURL: receiver.URL, KubernetesIssueTemplatePath: template, TemplatePath: template}
	old := store
	memory := storage.NewMemory()
	SetStorage(memory)
	t.Cleanup(func() { SetStorage(old) })

	a := kubernetes.IssueAlert{Cluster: "alpha", Namespace: "shop", WorkloadKind: "Deployment", Workload: "ambiguous", RootUID: "root", Rule: "pod.failed", RuleLabel: "has failed", Severity: "warning", BrokeAt: time.Now().Add(-time.Hour)}
	// The dead sender claimed the notification a lease ago and never finished.
	dead, err := memory.(storage.DetectionEpisodeStore).RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity:  storage.DetectionIdentity{AgentKind: "kubernetes", Source: "kubernetes:alpha", Service: "shop/Deployment/ambiguous", SignalKind: "kubernetes_issue", ConditionKey: "pod.failed"},
		Frequency: 1, Severity: "medium", ReceivedAt: time.Now().Add(-2 * storage.DefaultDetectionNotificationLease), ClaimLease: storage.DefaultDetectionNotificationLease,
	})
	if err != nil || dead.DeliveryID() == "" {
		t.Fatalf("dead sender claim = %q, %v", dead.DeliveryID(), err)
	}
	outcome, err := CreateIncidentFromKubernetesIssue("default", a)
	if err != nil || outcome.NotificationOutcome != "sent" || outcome.IncidentID != dead.IncidentID {
		t.Fatalf("replay = %+v, %v", outcome, err)
	}
	got := <-deliveries
	if got.header != dead.DeliveryID() || !strings.Contains(got.body, "delivery="+dead.DeliveryID()) {
		t.Fatalf("replay delivery header=%q body=%s, want delivery ID %q", got.header, got.body, dead.DeliveryID())
	}
	again, err := CreateIncidentFromKubernetesIssue("default", a)
	if err != nil || again.NotificationOutcome != "not_applicable" || len(deliveries) != 0 {
		t.Fatalf("an unchanged break notified again: %+v, %v", again, err)
	}
	record, err := memory.GetIncident(dead.IncidentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := record.Content["DeliveryID"]; stored {
		t.Fatal("the delivery ID was stored on the incident")
	}
}

// One break of one workload is one episode, whichever process reports it and
// however its details drift between ticks: two senders reporting the same
// break 50 times each notify once.
func TestKubernetesIssueOneBreakIsOneEpisodeAcrossTicksAndSenders(t *testing.T) {
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	previousOnCall := cfg.OnCall
	t.Cleanup(func() { cfg.OnCall = previousOnCall })
	cfg.OnCall.Enable, cfg.OnCall.InitializedOnly = false, false
	backends := map[string]func(t *testing.T) storage.Provider{
		"memory": func(*testing.T) storage.Provider { return storage.NewMemory() },
		"file": func(t *testing.T) storage.Provider {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			return provider
		},
	}
	for name, build := range backends {
		t.Run(name, func(t *testing.T) {
			old := store
			SetStorage(build(t))
			t.Cleanup(func() { SetStorage(old) })
			brokeAt := time.Now().Add(-time.Minute).UTC()
			var notified atomic.Int32
			incidents := sync.Map{}
			var senders sync.WaitGroup
			for sender := range 2 {
				senders.Add(1)
				go func() {
					defer senders.Done()
					for tick := range 50 {
						a := kubernetes.IssueAlert{OrgID: "default", Cluster: "alpha", Namespace: "shop", WorkloadKind: "Deployment", Workload: "api", RootUID: "root", Rule: "pod.crash_loop", RuleLabel: "is crash looping", Severity: "critical", Count: 1 + tick%3, Of: 3, BrokeAt: brokeAt, Evidence: []string{fmt.Sprintf("%d restarts", tick+sender)}}
						outcome, err := CreateIncidentFromKubernetesIssue("default", a)
						if err != nil {
							t.Errorf("sender %d tick %d: %v", sender, tick, err)
							return
						}
						incidents.Store(outcome.IncidentID, true)
						if outcome.NotificationOutcome != "not_applicable" {
							notified.Add(1)
						}
					}
				}()
			}
			senders.Wait()
			count := 0
			incidents.Range(func(any, any) bool { count++; return true })
			if notified.Load() != 1 || count != 1 {
				t.Fatalf("one break notified %d times over %d incidents, want once over one", notified.Load(), count)
			}
		})
	}
}

// TestScrubIssueContentReplacesValuesUnderSecretFieldNames covers the
// scrubbing of an issue's template content: a value under a field named like
// a secret is replaced, in strings and in lists, and the other fields are
// scrubbed and bounded as before.
func TestScrubIssueContentReplacesValuesUnderSecretFieldNames(t *testing.T) {
	scrubber, errs := agent.NewRedactor(false, nil)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	content := map[string]interface{}{
		"Cluster":       "prod",
		"Password":      "hunter2",
		"Credentials":   []string{"a", "b"},
		"TokenCount":    "5",
		"Evidence":      []string{"login  db_passwd=hunter2\nfailed", "two", "three", "four", "five", "six"},
		"RollupCount":   3,
		"HistoryIsDone": true,
	}
	scrubIssueContent(content, scrubber)
	want := map[string]interface{}{
		"Cluster":       "prod",
		"Password":      "<REDACTED:secret>",
		"Credentials":   []string{"<REDACTED:secret>", "<REDACTED:secret>"},
		"TokenCount":    "5",
		"Evidence":      []string{"login db_passwd=<REDACTED:secret> failed", "two", "three", "four", "five"},
		"RollupCount":   3,
		"HistoryIsDone": true,
	}
	if !reflect.DeepEqual(content, want) {
		t.Fatalf("content = %#v, want %#v", content, want)
	}
}
