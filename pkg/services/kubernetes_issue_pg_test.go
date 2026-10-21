package services

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Independent emitters share real durable episode claims. Capture ownership
// and downstream delivery receipts are separate live acceptance requirements.
func TestPostgresKubernetesIssueConcurrentEmissionClaims(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN required")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal("open dedicated Postgres database failed")
	}
	t.Cleanup(func() { _ = provider.Close() })
	loadAgentTestConfig(t)
	previous := Storage()
	SetStorage(provider)
	SetEmitInterceptor(nil)
	t.Cleanup(func() { SetStorage(previous); SetEmitInterceptor(nil) })
	alert := kubernetes.IssueAlert{
		Cluster: "acceptance", Namespace: "shop", WorkloadKind: "Deployment",
		Workload: "checkout", RootUID: "checkout-root", Rule: "pod.crash_loop",
		RuleLabel: "Crash looping", Severity: "warning", BrokeAt: time.Now().UTC(),
	}
	var incidentID string
	for _, severity := range []string{"warning", "critical"} {
		t.Run(severity, func(t *testing.T) {
			alert.Severity = severity
			const emitters = 16
			type result struct {
				outcome kubernetes.Outcome
				err     error
			}
			start := make(chan struct{})
			results := make(chan result, emitters)
			var wg sync.WaitGroup
			for n := 0; n < emitters; n++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					outcome, err := CreateIncidentFromKubernetesIssue("acceptance-org", alert)
					results <- result{outcome, err}
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			claims := 0
			for result := range results {
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.outcome.IncidentID != "" {
					if incidentID == "" {
						incidentID = result.outcome.IncidentID
					}
					if incidentID != result.outcome.IncidentID {
						t.Fatal("concurrent emitters created distinct incidents")
					}
				}
				if result.outcome.NotificationOutcome == "sent" {
					claims++
				}
			}
			if claims != 1 || incidentID == "" {
				t.Fatalf("completed emission claims=%d, incident=%q", claims, incidentID)
			}
			record, err := provider.GetIncident(incidentID)
			if err != nil {
				t.Fatal(err)
			}
			want := "medium"
			if severity == "critical" {
				want = "critical"
			}
			if record.OrgID != "acceptance-org" || record.Content["Severity"] != want {
				t.Fatalf("durable attribution/severity mismatch: org=%q severity=%v", record.OrgID, record.Content["Severity"])
			}
		})
	}
}
