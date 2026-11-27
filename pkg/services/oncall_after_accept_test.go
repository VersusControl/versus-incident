package services

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/redis/go-redis/v9"
)

// errStorageGone is every write of a process that has died.
var errStorageGone = errors.New("the process died")

// dyingStore is a memory store that stops taking writes once dead, as the
// storage of a process that died mid-send: what it held stays, nothing
// after the death reaches it.
type dyingStore struct {
	storage.Provider
	episodes  storage.DetectionEpisodeStore
	detection storage.DetectionIncidentStore
	dead      atomic.Bool
}

func newDyingStore() *dyingStore {
	base := storage.NewMemory()
	return &dyingStore{Provider: base, episodes: base.(storage.DetectionEpisodeStore), detection: base.(storage.DetectionIncidentStore)}
}

func (s *dyingStore) SaveIncident(rec *storage.IncidentRecord) error {
	if s.dead.Load() {
		return errStorageGone
	}
	return s.Provider.SaveIncident(rec)
}

func (s *dyingStore) SaveDetectionIncident(rec *storage.IncidentRecord) error {
	if s.dead.Load() {
		return errStorageGone
	}
	return s.detection.SaveDetectionIncident(rec)
}

func (s *dyingStore) RecordDetectionOccurrence(occurrence storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	if s.dead.Load() {
		return storage.DetectionEpisodeDecision{}, errStorageGone
	}
	return s.episodes.RecordDetectionOccurrence(occurrence)
}

func (s *dyingStore) RenewDetectionNotificationClaim(renewal storage.DetectionNotificationRenewal) error {
	if s.dead.Load() {
		return errStorageGone
	}
	return s.episodes.RenewDetectionNotificationClaim(renewal)
}

func (s *dyingStore) CompleteDetectionNotification(completion storage.DetectionNotificationCompletion) error {
	if s.dead.Load() {
		return errStorageGone
	}
	return s.episodes.CompleteDetectionNotification(completion)
}

func (s *dyingStore) CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error {
	if s.dead.Load() {
		return errStorageGone
	}
	return s.episodes.CloseDetectionEpisodeByIncident(incidentID, closedAt)
}

// watchingOnCall is an on-call provider that records, on each call, whether
// the stored incident already said on-call had started. With die set, its
// first call kills the process before the provider answers.
type watchingOnCall struct {
	store        *dyingStore
	die          bool
	calls, paged atomic.Int32
	flagBefore   atomic.Bool
}

func (p *watchingOnCall) TriggerOnCall(_ context.Context, incidentID string, _ *config.OnCallConfig) error {
	if rec, err := p.store.GetIncident(incidentID); err == nil && rec.OnCallTriggered {
		p.flagBefore.Store(true)
	}
	if p.calls.Add(1) == 1 && p.die {
		p.store.dead.Store(true)
		return errors.New("pagerduty: connection reset")
	}
	p.paged.Add(1)
	return nil
}

func useWatchingOnCall(t *testing.T, die bool) *watchingOnCall {
	t.Helper()
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	previous := cfg.OnCall
	t.Cleanup(func() { cfg.OnCall = previous })
	cfg.OnCall.Enable, cfg.OnCall.WaitMinutes = true, 0
	store := newDyingStore()
	previousStore := Storage()
	SetStorage(store)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"}) // never dialled: escalation is immediate
	provider := &watchingOnCall{store: store, die: die}
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(rdb, provider))
	t.Cleanup(func() {
		core.SetOnCallWorkflow(nil)
		_ = rdb.Close()
		SetStorage(previousStore)
	})
	return provider
}

// Plain, agent and Kubernetes incidents each page once, and the stored
// incident says on-call started only after the provider accepted: while
// the provider is being called it still says not started.
func TestOnCallIsRecordedOnlyAfterTheProviderAccepts(t *testing.T) {
	for name, send := range map[string]func() error{
		"plain": func() error {
			content := map[string]interface{}{"AlertName": "disk full", "Status": "firing"}
			return CreateIncident("", &content)
		},
		"agent": func() error {
			return CreateIncidentFromFinding(&core.AIFinding{Title: "db pool exhausted", Severity: "critical"},
				core.AgentResult{Verdict: core.VerdictSpike, PatternID: "db-pool", Frequency: 1}, "agent:file:app", "db-pool")
		},
		"kubernetes": func() error {
			_, err := CreateIncidentFromKubernetesIssue("", kubernetes.IssueAlert{Cluster: "prod", Namespace: "shop", WorkloadKind: "Deployment",
				Workload: "checkout", RootUID: "root", Rule: "pod.crash_loop", RuleLabel: "crash looping", Severity: "critical", BrokeAt: time.Now()})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider := useWatchingOnCall(t, false)
			if err := send(); err != nil {
				t.Fatalf("send: %v", err)
			}
			if provider.paged.Load() != 1 {
				t.Fatalf("pages = %d; want one", provider.paged.Load())
			}
			if provider.flagBefore.Load() {
				t.Fatal("the incident said on-call had started before the provider accepted the page")
			}
			if rec := onlyStoredIncident(t, provider.store); !rec.OnCallTriggered || rec.OnCallError != "" {
				t.Fatalf("stored on-call triggered=%v error=%q; want started", rec.OnCallTriggered, rec.OnCallError)
			}
		})
	}
}

// A process that dies while the on-call provider is being called leaves
// the incident saying on-call did not start, so the retry of the
// notification pages; it pages exactly once, and a later repeat does not.
func TestDeathDuringTheOnCallCallIsPagedByTheRetry(t *testing.T) {
	provider := useWatchingOnCall(t, true)
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	previousClock := detectionNow
	detectionNow = func() time.Time { return now }
	t.Cleanup(func() { detectionNow = previousClock })
	finding := &core.AIFinding{Title: "db pool exhausted", Severity: "critical"}
	result := core.AgentResult{Verdict: core.VerdictSpike, PatternID: "db-pool", Frequency: 1}

	_ = CreateIncidentFromFinding(finding, result, "agent:file:app", "db-pool")
	if provider.calls.Load() != 1 || provider.paged.Load() != 0 {
		t.Fatalf("calls %d, pages %d; want the one call that died", provider.calls.Load(), provider.paged.Load())
	}
	if rec := onlyStoredIncident(t, provider.store); rec.OnCallTriggered {
		t.Fatal("the dead attempt left the incident saying on-call started")
	}

	// The process restarts; the retry comes after the dead attempt's claim
	// ran out.
	provider.store.dead.Store(false)
	now = now.Add(2 * storage.DefaultDetectionNotificationLease)
	if err := CreateIncidentFromFinding(finding, result, "agent:file:app", "db-pool"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	now = now.Add(time.Minute)
	if err := CreateIncidentFromFinding(finding, result, "agent:file:app", "db-pool"); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if provider.paged.Load() != 1 {
		t.Fatalf("pages = %d; want the retry to page once", provider.paged.Load())
	}
	if rec := onlyStoredIncident(t, provider.store); !rec.OnCallTriggered {
		t.Fatal("the paged retry is not recorded as on-call started")
	}
}
