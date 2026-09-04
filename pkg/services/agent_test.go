package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/redis/go-redis/v9"
)

type failingDetectionEpisodeStore struct {
	storage.Provider
	err error
}

type transientGetDetectionStore struct {
	storage.Provider
	episodes  storage.DetectionEpisodeStore
	detection storage.DetectionIncidentStore
	failGet   atomic.Bool
}

type blockingDetectionSaveStore struct {
	storage.Provider
	episodes    storage.DetectionEpisodeStore
	detection   storage.DetectionIncidentStore
	blockNext   atomic.Bool
	saveCalls   atomic.Int64
	saveStarted chan struct{}
	releaseSave chan struct{}
}

type transientRenewDetectionStore struct {
	storage.Provider
	episodes     storage.DetectionEpisodeStore
	detection    storage.DetectionIncidentStore
	failNext     atomic.Bool
	renewalCalls atomic.Int64
	renewErr     error
}

type renewalSequenceStore struct {
	storage.DetectionEpisodeStore
	renewErrors []error
	calls       atomic.Int64
}

func newBlockingDetectionSaveStore(provider storage.Provider) *blockingDetectionSaveStore {
	return &blockingDetectionSaveStore{
		Provider: provider, episodes: provider.(storage.DetectionEpisodeStore),
		detection:   provider.(storage.DetectionIncidentStore),
		saveStarted: make(chan struct{}), releaseSave: make(chan struct{}),
	}
}

func (s *blockingDetectionSaveStore) RecordDetectionOccurrence(occurrence storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	return s.episodes.RecordDetectionOccurrence(occurrence)
}

func (s *blockingDetectionSaveStore) RenewDetectionNotificationClaim(renewal storage.DetectionNotificationRenewal) error {
	return s.episodes.RenewDetectionNotificationClaim(renewal)
}

func (s *blockingDetectionSaveStore) CompleteDetectionNotification(completion storage.DetectionNotificationCompletion) error {
	return s.episodes.CompleteDetectionNotification(completion)
}

func (s *blockingDetectionSaveStore) CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error {
	return s.episodes.CloseDetectionEpisodeByIncident(incidentID, closedAt)
}

func (s *blockingDetectionSaveStore) SaveDetectionIncident(rec *storage.IncidentRecord) error {
	s.saveCalls.Add(1)
	if s.blockNext.CompareAndSwap(true, false) {
		close(s.saveStarted)
		<-s.releaseSave
	}
	return s.detection.SaveDetectionIncident(rec)
}

func (s *transientGetDetectionStore) RecordDetectionOccurrence(occurrence storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	return s.episodes.RecordDetectionOccurrence(occurrence)
}

func (s *transientGetDetectionStore) RenewDetectionNotificationClaim(renewal storage.DetectionNotificationRenewal) error {
	return s.episodes.RenewDetectionNotificationClaim(renewal)
}

func (s *transientGetDetectionStore) CompleteDetectionNotification(completion storage.DetectionNotificationCompletion) error {
	return s.episodes.CompleteDetectionNotification(completion)
}

func (s *transientGetDetectionStore) CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error {
	return s.episodes.CloseDetectionEpisodeByIncident(incidentID, closedAt)
}

func (s *transientGetDetectionStore) SaveDetectionIncident(rec *storage.IncidentRecord) error {
	return s.detection.SaveDetectionIncident(rec)
}

func (s *transientRenewDetectionStore) RecordDetectionOccurrence(occurrence storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	return s.episodes.RecordDetectionOccurrence(occurrence)
}

func (s *transientRenewDetectionStore) RenewDetectionNotificationClaim(renewal storage.DetectionNotificationRenewal) error {
	s.renewalCalls.Add(1)
	if s.failNext.CompareAndSwap(true, false) {
		if s.renewErr != nil {
			return s.renewErr
		}
		return errors.New("temporary renewal failure")
	}
	return s.episodes.RenewDetectionNotificationClaim(renewal)
}

func (s *transientRenewDetectionStore) CompleteDetectionNotification(completion storage.DetectionNotificationCompletion) error {
	return s.episodes.CompleteDetectionNotification(completion)
}

func (s *transientRenewDetectionStore) CloseDetectionEpisodeByIncident(incidentID string, closedAt time.Time) error {
	return s.episodes.CloseDetectionEpisodeByIncident(incidentID, closedAt)
}

func (s *transientRenewDetectionStore) SaveDetectionIncident(rec *storage.IncidentRecord) error {
	return s.detection.SaveDetectionIncident(rec)
}

func (s *renewalSequenceStore) RenewDetectionNotificationClaim(renewal storage.DetectionNotificationRenewal) error {
	index := int(s.calls.Add(1)) - 1
	if index < len(s.renewErrors) {
		return s.renewErrors[index]
	}
	return s.DetectionEpisodeStore.RenewDetectionNotificationClaim(renewal)
}

func (s *transientGetDetectionStore) GetIncident(id string) (*storage.IncidentRecord, error) {
	if s.failGet.Load() {
		return nil, errors.New("temporary incident read failure")
	}
	return s.Provider.GetIncident(id)
}

type countingOnCallProvider struct {
	calls atomic.Int64
}

func (p *countingOnCallProvider) TriggerOnCall(context.Context, string, *config.OnCallConfig) error {
	p.calls.Add(1)
	return nil
}

func (s failingDetectionEpisodeStore) RecordDetectionOccurrence(storage.DetectionOccurrence) (storage.DetectionEpisodeDecision, error) {
	return storage.DetectionEpisodeDecision{}, s.err
}

func (s failingDetectionEpisodeStore) RenewDetectionNotificationClaim(storage.DetectionNotificationRenewal) error {
	return s.err
}

func (s failingDetectionEpisodeStore) CompleteDetectionNotification(storage.DetectionNotificationCompletion) error {
	return s.err
}

func (s failingDetectionEpisodeStore) CloseDetectionEpisodeByIncident(string, time.Time) error {
	return s.err
}

// loadAgentTestConfig writes a minimal config to a temp dir and loads it
// into the global singleton. On-call is globally disabled and the workflow
// is never initialized — exactly the default that crashed the emit path before the fix.
func loadAgentTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `name: test
host: 0.0.0.0
port: 3000
public_host: http://localhost:3000
alert:
  slack:
    enable: false
oncall:
  enable: false
  initialized_only: false
redis:
  host: localhost
  port: 6379
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}

// TestCreateIncidentFromFinding_HighSeverityNoOnCall reproduces the crash: a
// high-severity AI finding force-sets oncall_enable=true, but with the
// on-call workflow uninitialized the emit path must persist the incident
// and return without panicking (on-call simply skipped).
func TestCreateIncidentFromFinding_HighSeverityNoOnCall(t *testing.T) {
	loadAgentTestConfig(t)

	mem := storage.NewMemory()
	prev := Storage()
	SetStorage(mem)
	t.Cleanup(func() { SetStorage(prev) })

	f := &core.AIFinding{
		Title:      "EC2 CPU saturation",
		Summary:    "Sustained CPU spike on ec2-i-0abcd1234",
		Severity:   "high",
		Category:   "compute",
		Confidence: 0.92,
	}
	r := core.AgentResult{Verdict: core.VerdictSpike, PatternID: "p-ec2-cpu"}

	// Would panic before the fix (GetOnCallWorkflow on a nil singleton).
	if err := CreateIncidentFromFinding(f, r, "agent:metrics:ec2-i-0abcd1234", "ec2-i-0abcd1234"); err != nil {
		t.Fatalf("CreateIncidentFromFinding returned error: %v", err)
	}

	got, err := mem.ListIncidents(10)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted incident, got %d", len(got))
	}
	if got[0].Source != "agent:metrics:ec2-i-0abcd1234" {
		t.Fatalf("Source = %q, want agent:metrics:ec2-i-0abcd1234", got[0].Source)
	}
	if got[0].Origin != storage.OriginAIDetect {
		t.Fatalf("Origin = %q, want %q (AI detect emit)", got[0].Origin, storage.OriginAIDetect)
	}
	if got[0].OnCallTriggered {
		t.Fatal("OnCallTriggered should be false when the on-call workflow is not initialized")
	}
}

func TestCreateIncidentFromFinding_BatchIndependentCoalescing(t *testing.T) {
	loadAgentTestConfig(t)
	shapes := []struct {
		name             string
		calls            int
		frequency        int
		wantAction       string
		wantNotification string
	}{
		{name: "one batch", calls: 1, frequency: 500, wantAction: "opened", wantNotification: "sent"},
		{name: "one occurrence per call", calls: 500, frequency: 1, wantAction: "coalesced", wantNotification: "not_applicable"},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			mem := storage.NewMemory()
			previousStore := Storage()
			SetStorage(mem)
			defer SetStorage(previousStore)

			var orchestrations atomic.Int64
			SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
				orchestrations.Add(1)
				return EmitDecision{Action: EmitProceed}
			})
			defer SetEmitInterceptor(nil)

			finding := &core.AIFinding{Title: "Checkout failures", Summary: "checkout failed", Severity: "medium"}
			result := core.AgentResult{
				Verdict: core.VerdictUnknown, PatternID: "checkout-failure", Frequency: shape.frequency,
				AgentKind: "detect", SignalKind: "logs", DetectionEmission: &core.DetectionEmissionResult{},
			}
			for index := 0; index < shape.calls; index++ {
				if err := CreateIncidentFromFinding(finding, result, "loki-prod", "checkout"); err != nil {
					t.Fatalf("CreateIncidentFromFinding(%d): %v", index, err)
				}
			}

			incidents, err := mem.ListIncidents(0)
			if err != nil {
				t.Fatalf("ListIncidents: %v", err)
			}
			if len(incidents) != 1 {
				t.Fatalf("incidents=%d, want 1", len(incidents))
			}
			if incidents[0].OccurrenceCount != 500 || incidents[0].Content["Frequency"] != int64(500) {
				t.Fatalf("cumulative incident = %+v content=%v", incidents[0], incidents[0].Content)
			}
			if orchestrations.Load() != 1 {
				t.Fatalf("orchestrations=%d, want one initial notification", orchestrations.Load())
			}
			if result.DetectionEmission.EpisodeAction != shape.wantAction ||
				result.DetectionEmission.NotificationOutcome != shape.wantNotification ||
				result.DetectionEmission.OccurrenceCount != 500 || result.DetectionEmission.EpisodeID == "" {
				t.Fatalf("emission metadata = %+v", result.DetectionEmission)
			}
		})
	}
}

func TestContinueDetectionEpisode_ExistingOnlyWithoutFanout(t *testing.T) {
	loadAgentTestConfig(t)
	provider := storage.NewMemory()
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })

	var orchestrations atomic.Int64
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		orchestrations.Add(1)
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	occurrence := storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "loki-prod", Service: "checkout",
			SignalKind: "logs", ConditionKey: "checkout-failure",
		},
		Frequency: 3, Severity: "medium", ReceivedAt: time.Now().UTC().Add(time.Minute),
	}
	if _, err := ContinueDetectionEpisode(occurrence); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("ContinueDetectionEpisode(without active episode) = %v, want ErrNotFound", err)
	}
	incidents, err := provider.ListIncidents(0)
	if err != nil || len(incidents) != 0 || orchestrations.Load() != 0 {
		t.Fatalf("known no-episode state: incidents=%d orchestrations=%d err=%v", len(incidents), orchestrations.Load(), err)
	}

	finding := &core.AIFinding{Title: "Checkout failures", Summary: "checkout failed", Severity: "medium"}
	result := core.AgentResult{
		Verdict: core.VerdictUnknown, PatternID: "checkout-failure", Frequency: 2,
		AgentKind: "detect", SignalKind: "logs", DetectionEmission: &core.DetectionEmissionResult{},
	}
	if err := CreateIncidentFromFinding(finding, result, "loki-prod", "checkout"); err != nil {
		t.Fatalf("CreateIncidentFromFinding: %v", err)
	}
	decision, err := ContinueDetectionEpisode(occurrence)
	if err != nil {
		t.Fatalf("ContinueDetectionEpisode(active): %v", err)
	}
	if decision.Action != storage.DetectionEpisodeCoalesced || decision.NotificationClaimed || decision.OccurrenceCount != 5 {
		t.Fatalf("continuation decision = %+v", decision)
	}
	incidents, err = provider.ListIncidents(0)
	if err != nil || len(incidents) != 1 || incidents[0].OccurrenceCount != 5 ||
		incidents[0].Content["Frequency"] != int64(5) || orchestrations.Load() != 1 {
		t.Fatalf("continued incident=%+v orchestrations=%d err=%v", incidents, orchestrations.Load(), err)
	}
}

func TestCreateIncidentFromFinding_EscalatesSameIncidentOnce(t *testing.T) {
	loadAgentTestConfig(t)
	mem := storage.NewMemory()
	previousStore := Storage()
	SetStorage(mem)
	t.Cleanup(func() { SetStorage(previousStore) })

	var orchestrations atomic.Int64
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		orchestrations.Add(1)
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })
	result := core.AgentResult{
		Verdict: core.VerdictUnknown, PatternID: "database-timeout", Frequency: 1,
		AgentKind: "detect", SignalKind: "logs",
	}
	for _, severity := range []string{"low", "low", "critical", "critical", "low"} {
		finding := &core.AIFinding{Title: "Database timeout", Summary: "query timed out", Severity: severity}
		if err := CreateIncidentFromFinding(finding, result, "elasticsearch-prod", "billing"); err != nil {
			t.Fatalf("CreateIncidentFromFinding(%s): %v", severity, err)
		}
	}
	incidents, err := mem.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(incidents) != 1 {
		t.Fatalf("incidents=%d, want 1", len(incidents))
	}
	incident := incidents[0]
	if incident.OccurrenceCount != 5 || incident.HighestObservedSeverity != "critical" || incident.HighestNotifiedSeverity != "critical" ||
		incident.Content["Severity"] != "critical" {
		t.Fatalf("escalated incident = %+v", incident)
	}
	if orchestrations.Load() != 2 {
		t.Fatalf("orchestrations=%d, want initial plus one escalation", orchestrations.Load())
	}
}

func TestCreateIncidentFromFinding_RenewsLiveClaimDuringFanout(t *testing.T) {
	for _, testCase := range []struct {
		name               string
		initialSeverity    string
		blockedSeverity    string
		wantOrchestrations int64
	}{
		{name: "initial", initialSeverity: "low", blockedSeverity: "low", wantOrchestrations: 1},
		{name: "escalation", initialSeverity: "low", blockedSeverity: "critical", wantOrchestrations: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			loadAgentTestConfig(t)
			provider := storage.NewMemory()
			previousStore := Storage()
			SetStorage(provider)
			t.Cleanup(func() { SetStorage(previousStore) })

			var orchestrations atomic.Int64
			blocked := make(chan struct{})
			release := make(chan struct{})
			blockNext := atomic.Bool{}
			SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
				orchestrations.Add(1)
				if blockNext.CompareAndSwap(true, false) {
					close(blocked)
					<-release
				}
				return EmitDecision{Action: EmitProceed}
			})
			t.Cleanup(func() { SetEmitInterceptor(nil) })

			result := core.AgentResult{
				Verdict: core.VerdictUnknown, PatternID: "live-claim-" + testCase.name, Frequency: 1,
				AgentKind: "detect", SignalKind: "logs",
			}
			if testCase.name == "escalation" {
				finding := &core.AIFinding{Title: "Live claim", Severity: testCase.initialSeverity}
				if err := createIncidentFromFindingWithLease(finding, result, "loki", "checkout", 30*time.Millisecond); err != nil {
					t.Fatalf("initial CreateIncidentFromFinding: %v", err)
				}
			}
			blockNext.Store(true)
			ownerDone := make(chan error, 1)
			go func() {
				finding := &core.AIFinding{Title: "Live claim", Severity: testCase.blockedSeverity}
				ownerDone <- createIncidentFromFindingWithLease(finding, result, "loki", "checkout", 30*time.Millisecond)
			}()
			<-blocked
			time.Sleep(80 * time.Millisecond)
			later := &core.AIFinding{Title: "Live claim", Severity: "low"}
			if err := createIncidentFromFindingWithLease(later, result, "loki", "checkout", 30*time.Millisecond); err != nil {
				t.Fatalf("later CreateIncidentFromFinding: %v", err)
			}
			if got := orchestrations.Load(); got != testCase.wantOrchestrations {
				t.Fatalf("orchestrations while owner alive=%d, want %d", got, testCase.wantOrchestrations)
			}
			close(release)
			if err := <-ownerDone; err != nil {
				t.Fatalf("owner CreateIncidentFromFinding: %v", err)
			}
			incidents, err := provider.ListIncidents(0)
			if err != nil || len(incidents) != 1 {
				t.Fatalf("incidents=%+v err=%v", incidents, err)
			}
			if testCase.name == "escalation" && (incidents[0].HighestObservedSeverity != "critical" || incidents[0].Content["Severity"] != "critical") {
				t.Fatalf("escalated severity downgraded: %+v", incidents[0])
			}
		})
	}
}

func TestCreateIncidentFromFinding_RenewalErrorStillCompletesClaim(t *testing.T) {
	loadAgentTestConfig(t)
	base := storage.NewMemory()
	provider := &transientRenewDetectionStore{
		Provider: base, episodes: base.(storage.DetectionEpisodeStore), detection: base.(storage.DetectionIncidentStore),
	}
	provider.failNext.Store(true)
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })

	var orchestrations atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		if orchestrations.Add(1) == 1 {
			close(started)
			<-release
		}
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	emission := &core.DetectionEmissionResult{}
	result := core.AgentResult{PatternID: "renewal-error", Frequency: 1, AgentKind: "detect", SignalKind: "logs", DetectionEmission: emission}
	finding := &core.AIFinding{Title: "Renewal error", Severity: "low"}
	done := make(chan error, 1)
	go func() {
		done <- createIncidentFromFindingWithLease(finding, result, "loki", "checkout", 30*time.Millisecond)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("recovered renewal error = %v", err)
	}
	if emission.NotificationOutcome != "sent" {
		t.Fatalf("notification outcome = %q, want sent", emission.NotificationOutcome)
	}
	if err := createIncidentFromFindingWithLease(finding, result, "loki", "checkout", 30*time.Millisecond); err != nil {
		t.Fatalf("post-completion occurrence: %v", err)
	}
	incidents, err := base.ListIncidents(0)
	if err != nil || len(incidents) != 1 || incidents[0].HighestNotifiedSeverity != "low" {
		t.Fatalf("completed incident=%+v err=%v", incidents, err)
	}
	if orchestrations.Load() != 1 {
		t.Fatalf("orchestrations=%d, want one despite renewal error", orchestrations.Load())
	}
}

func TestCreateIncidentFromFinding_ClaimLostRenewalRemainsError(t *testing.T) {
	loadAgentTestConfig(t)
	base := storage.NewMemory()
	provider := &transientRenewDetectionStore{
		Provider: base, episodes: base.(storage.DetectionEpisodeStore), detection: base.(storage.DetectionIncidentStore),
		renewErr: storage.ErrDetectionClaimLost,
	}
	provider.failNext.Store(true)
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })

	started := make(chan struct{})
	release := make(chan struct{})
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		close(started)
		<-release
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	result := core.AgentResult{PatternID: "renewal-claim-lost", Frequency: 1, AgentKind: "detect", SignalKind: "logs"}
	done := make(chan error, 1)
	go func() {
		done <- createIncidentFromFindingWithLease(
			&core.AIFinding{Title: "Renewal claim lost", Severity: "low"},
			result, "loki", "checkout", 30*time.Millisecond,
		)
	}()
	<-started
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-done; !errors.Is(err, storage.ErrDetectionClaimLost) {
		t.Fatalf("claim-lost renewal error = %v, want ErrDetectionClaimLost", err)
	}
}

func TestDetectionClaimHeartbeatStopsRenewing(t *testing.T) {
	base := storage.NewMemory()
	provider := &transientRenewDetectionStore{
		Provider: base, episodes: base.(storage.DetectionEpisodeStore), detection: base.(storage.DetectionIncidentStore),
	}
	lease := 30 * time.Millisecond
	decision, err := provider.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "loki", Service: "checkout", SignalKind: "logs", ConditionKey: "heartbeat-stop",
		},
		Frequency: 1, Severity: "low", ReceivedAt: time.Now().UTC(), ClaimLease: lease,
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence: %v", err)
	}
	heartbeat := startDetectionClaimHeartbeat(provider, decision, lease)
	deadline := time.Now().Add(time.Second)
	for provider.renewalCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if provider.renewalCalls.Load() == 0 {
		t.Fatal("heartbeat did not renew before deadline")
	}
	if err := heartbeat.stop(); err != nil {
		t.Fatalf("stop heartbeat: %v", err)
	}
	callsAfterStop := provider.renewalCalls.Load()
	time.Sleep(3 * lease)
	if calls := provider.renewalCalls.Load(); calls != callsAfterStop {
		t.Fatalf("renewal calls after stop = %d, want %d", calls, callsAfterStop)
	}
	if err := heartbeat.stop(); err != nil {
		t.Fatalf("stop heartbeat again: %v", err)
	}
}

func TestDetectionClaimHeartbeatPreservesClaimLossAfterTransientError(t *testing.T) {
	base := storage.NewMemory().(storage.DetectionEpisodeStore)
	lease := 30 * time.Millisecond
	decision, err := base.RecordDetectionOccurrence(storage.DetectionOccurrence{
		Identity: storage.DetectionIdentity{
			AgentKind: "detect", Source: "loki", Service: "checkout", SignalKind: "logs", ConditionKey: "heartbeat-loss",
		},
		Frequency: 1, Severity: "low", ReceivedAt: time.Now().UTC(), ClaimLease: lease,
	})
	if err != nil {
		t.Fatalf("RecordDetectionOccurrence: %v", err)
	}
	store := &renewalSequenceStore{
		DetectionEpisodeStore: base,
		renewErrors:           []error{errors.New("temporary renewal failure"), storage.ErrDetectionClaimLost},
	}
	heartbeat := startDetectionClaimHeartbeat(store, decision, lease)
	deadline := time.Now().Add(time.Second)
	for store.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if store.calls.Load() < 2 {
		t.Fatal("heartbeat did not reach claim-loss renewal before deadline")
	}
	err = heartbeat.stop()
	if !errors.Is(err, storage.ErrDetectionClaimLost) || !strings.Contains(err.Error(), "temporary renewal failure") {
		t.Fatalf("heartbeat error = %v, want transient context and ErrDetectionClaimLost", err)
	}
}

func TestCreateIncidentFromFinding_PostgresLiveOwnerExactlyOnce(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping postgres live-owner test")
	}
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	cfg.OnCall.Enable = true
	cfg.OnCall.WaitMinutes = 0
	onCallProvider := &countingOnCallProvider{}
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(redisClient, onCallProvider))
	t.Cleanup(func() {
		core.SetOnCallWorkflow(nil)
		_ = redisClient.Close()
	})

	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })

	var notifications atomic.Int64
	blockEscalation := atomic.Bool{}
	escalationStarted := make(chan struct{})
	releaseEscalation := make(chan struct{})
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		notifications.Add(1)
		if blockEscalation.CompareAndSwap(true, false) {
			close(escalationStarted)
			<-releaseEscalation
		}
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	condition := fmt.Sprintf("postgres-live-owner-%d", time.Now().UTC().UnixNano())
	result := core.AgentResult{PatternID: condition, Frequency: 1, AgentKind: "detect", SignalKind: "logs"}
	initial := &core.AIFinding{Title: condition, Severity: "low"}
	const workers = 24
	const claimLease = 300 * time.Millisecond
	runConcurrent := func(finding *core.AIFinding) {
		t.Helper()
		errorsCh := make(chan error, workers)
		var wait sync.WaitGroup
		for index := 0; index < workers; index++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				if err := createIncidentFromFindingWithLease(finding, result, "postgres-review", "checkout", claimLease); err != nil {
					errorsCh <- err
				}
			}()
		}
		wait.Wait()
		close(errorsCh)
		for err := range errorsCh {
			t.Errorf("concurrent occurrence: %v", err)
		}
	}
	runConcurrent(initial)
	if notifications.Load() != 1 {
		t.Fatalf("initial notifications=%d, want one", notifications.Load())
	}

	blockEscalation.Store(true)
	escalationDone := make(chan error, 1)
	go func() {
		escalationDone <- createIncidentFromFindingWithLease(
			&core.AIFinding{Title: condition, Severity: "critical"}, result,
			"postgres-review", "checkout", claimLease,
		)
	}()
	<-escalationStarted
	time.Sleep(800 * time.Millisecond)
	runConcurrent(initial)
	if notifications.Load() != 2 {
		t.Fatalf("notifications while escalation owner alive=%d, want two total", notifications.Load())
	}
	close(releaseEscalation)
	if err := <-escalationDone; err != nil {
		t.Fatalf("escalation owner: %v", err)
	}
	if onCallProvider.calls.Load() != 1 {
		t.Fatalf("on-call triggers=%d, want one", onCallProvider.calls.Load())
	}
	incidents, err := provider.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	for _, incident := range incidents {
		if incident.Title != condition {
			continue
		}
		if incident.OccurrenceCount != workers*2+1 || incident.HighestObservedSeverity != "critical" ||
			incident.HighestNotifiedSeverity != "critical" || incident.Content["Severity"] != "critical" {
			t.Fatalf("postgres live-owner incident=%+v", incident)
		}
		return
	}
	t.Fatalf("postgres live-owner incident %q not found", condition)
}

func TestCreateIncidentFromFinding_EscalationDoesNotRestartOnCall(t *testing.T) {
	tests := []struct {
		name            string
		initialSeverity string
		escalation      string
		wantOnCallCalls int64
	}{
		{name: "low to high", initialSeverity: "low", escalation: "high", wantOnCallCalls: 1},
		{name: "high to critical", initialSeverity: "high", escalation: "critical", wantOnCallCalls: 1},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			loadAgentTestConfig(t)
			cfg := config.GetConfig()
			cfg.OnCall.Enable = true
			cfg.OnCall.WaitMinutes = 0

			provider := &countingOnCallProvider{}
			redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
			core.SetOnCallWorkflow(core.NewOnCallWorkflow(redisClient, provider))
			t.Cleanup(func() {
				core.SetOnCallWorkflow(nil)
				_ = redisClient.Close()
			})

			store := storage.NewMemory()
			previousStore := Storage()
			SetStorage(store)
			t.Cleanup(func() { SetStorage(previousStore) })

			var notifications atomic.Int64
			SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
				notifications.Add(1)
				return EmitDecision{Action: EmitProceed}
			})
			t.Cleanup(func() { SetEmitInterceptor(nil) })

			result := core.AgentResult{
				Verdict: core.VerdictUnknown, PatternID: "oncall-escalation", Frequency: 1,
				AgentKind: "detect", SignalKind: "logs",
			}
			finding := &core.AIFinding{Title: "Checkout failures", Summary: "requests failed", Severity: testCase.initialSeverity}
			if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
				t.Fatalf("initial CreateIncidentFromFinding: %v", err)
			}
			initial, err := store.ListIncidents(0)
			if err != nil || len(initial) != 1 {
				t.Fatalf("initial incidents=%d err=%v", len(initial), err)
			}

			finding.Severity = testCase.escalation
			if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
				t.Fatalf("escalation CreateIncidentFromFinding: %v", err)
			}
			incidents, err := store.ListIncidents(0)
			if err != nil || len(incidents) != 1 {
				t.Fatalf("escalated incidents=%d err=%v", len(incidents), err)
			}
			if incidents[0].ID != initial[0].ID || incidents[0].OccurrenceCount != 2 {
				t.Fatalf("escalation changed incident or count: initial=%+v current=%+v", initial[0], incidents[0])
			}
			if notifications.Load() != 2 {
				t.Fatalf("notifications=%d, want initial plus one escalation", notifications.Load())
			}
			if provider.calls.Load() != testCase.wantOnCallCalls {
				t.Fatalf("on-call starts=%d, want %d", provider.calls.Load(), testCase.wantOnCallCalls)
			}
			if !incidents[0].OnCallTriggered {
				t.Fatal("escalated incident should retain successful on-call state")
			}
		})
	}
}

func TestCreateIncidentFromFinding_DelayRemainsRetryable(t *testing.T) {
	loadAgentTestConfig(t)
	provider := storage.NewMemory()
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })

	var attempts atomic.Int64
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		if attempts.Add(1) == 1 {
			return EmitDecision{Action: EmitDelay}
		}
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })

	result := core.AgentResult{
		Verdict: core.VerdictUnknown, PatternID: "delayed-condition", Frequency: 1,
		AgentKind: "detect", SignalKind: "logs", DetectionEmission: &core.DetectionEmissionResult{},
	}
	finding := &core.AIFinding{Title: "Delayed condition", Severity: "medium"}
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("delayed CreateIncidentFromFinding: %v", err)
	}
	if result.DetectionEmission.NotificationOutcome != "delayed" {
		t.Fatalf("delay outcome = %q", result.DetectionEmission.NotificationOutcome)
	}
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("retry CreateIncidentFromFinding: %v", err)
	}
	if attempts.Load() != 2 || result.DetectionEmission.NotificationOutcome != "sent" {
		t.Fatalf("attempts=%d outcome=%q, want retry sent", attempts.Load(), result.DetectionEmission.NotificationOutcome)
	}
}

func TestCreateIncidentFromFinding_ConcurrentEscalationClaimsOnce(t *testing.T) {
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	cfg.OnCall.Enable = true
	cfg.OnCall.WaitMinutes = 0
	onCallProvider := &countingOnCallProvider{}
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(redisClient, onCallProvider))
	t.Cleanup(func() {
		core.SetOnCallWorkflow(nil)
		_ = redisClient.Close()
	})
	provider := storage.NewMemory()
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })
	var orchestrations atomic.Int64
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		orchestrations.Add(1)
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })
	result := core.AgentResult{
		Verdict: core.VerdictUnknown, PatternID: "concurrent-timeout", Frequency: 1,
		AgentKind: "detect", SignalKind: "logs",
	}
	low := &core.AIFinding{Title: "Concurrent timeout", Summary: "timeout", Severity: "low"}
	if err := CreateIncidentFromFinding(low, result, "loki", "checkout"); err != nil {
		t.Fatalf("initial CreateIncidentFromFinding: %v", err)
	}
	initial, err := provider.ListIncidents(0)
	if err != nil || len(initial) != 1 {
		t.Fatalf("initial incidents=%d err=%v", len(initial), err)
	}
	incidentID := initial[0].ID

	const repeats = 100
	high := &core.AIFinding{Title: "Concurrent timeout", Summary: "timeout", Severity: "high"}
	var wait sync.WaitGroup
	errorsCh := make(chan error, repeats)
	for index := 0; index < repeats; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := CreateIncidentFromFinding(high, result, "loki", "checkout"); err != nil {
				errorsCh <- err
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent CreateIncidentFromFinding: %v", err)
	}
	incidents, err := provider.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(incidents) != 1 || incidents[0].ID != incidentID {
		t.Fatalf("incidents=%+v, want one stable id %s", incidents, incidentID)
	}
	if incidents[0].OccurrenceCount != repeats+1 || incidents[0].HighestNotifiedSeverity != "high" {
		t.Fatalf("concurrent incident=%+v", incidents[0])
	}
	if orchestrations.Load() != 2 {
		t.Fatalf("orchestrations=%d, want initial plus one escalation", orchestrations.Load())
	}
	if onCallProvider.calls.Load() != 1 || !incidents[0].OnCallTriggered {
		t.Fatalf("on-call starts=%d triggered=%v, want one/true", onCallProvider.calls.Load(), incidents[0].OnCallTriggered)
	}
}

func TestCreateIncidentFromFinding_ConcurrentFirstOwnerExactCount(t *testing.T) {
	loadAgentTestConfig(t)
	provider := storage.NewMemory()
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })
	var orchestrations atomic.Int64
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		orchestrations.Add(1)
		return EmitDecision{Action: EmitProceed}
	})
	t.Cleanup(func() { SetEmitInterceptor(nil) })
	result := core.AgentResult{Verdict: core.VerdictUnknown, PatternID: "first-race", Frequency: 1, AgentKind: "detect", SignalKind: "logs"}
	finding := &core.AIFinding{Title: "First race", Summary: "same condition", Severity: "medium"}
	const occurrences = 100
	var wait sync.WaitGroup
	errorsCh := make(chan error, occurrences)
	for index := 0; index < occurrences; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
				errorsCh <- err
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("CreateIncidentFromFinding: %v", err)
	}
	incidents, err := provider.ListIncidents(0)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(incidents) != 1 || incidents[0].OccurrenceCount != occurrences || incidents[0].Content["Frequency"] != int64(occurrences) {
		t.Fatalf("concurrent first incident=%+v", incidents)
	}
	if orchestrations.Load() != 1 {
		t.Fatalf("orchestrations=%d, want 1", orchestrations.Load())
	}
}

func TestCreateIncidentFromFinding_CoalescedSavePreservesConcurrentOperatorState(t *testing.T) {
	providers := []struct {
		name string
		open func(*testing.T) storage.Provider
	}{
		{name: "memory", open: func(*testing.T) storage.Provider { return storage.NewMemory() }},
		{name: "file", open: func(t *testing.T) storage.Provider {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: t.TempDir()})
			if err != nil {
				t.Fatalf("NewFile: %v", err)
			}
			return provider
		}},
		{name: "postgres", open: func(t *testing.T) storage.Provider {
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("TEST_POSTGRES_DSN not set; skipping postgres tests")
			}
			provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			if err != nil {
				t.Fatalf("NewPostgres: %v", err)
			}
			return provider
		}},
	}
	for _, testCase := range providers {
		t.Run(testCase.name, func(t *testing.T) {
			loadAgentTestConfig(t)
			base := testCase.open(t)
			t.Cleanup(func() { _ = base.Close() })
			provider := newBlockingDetectionSaveStore(base)
			previousStore := Storage()
			SetStorage(provider)
			SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
				return EmitDecision{Action: EmitProceed}
			})
			t.Cleanup(func() {
				SetEmitInterceptor(nil)
				SetStorage(previousStore)
			})

			condition := "operator-race-" + strings.ReplaceAll(t.Name(), "/", "-") + time.Now().UTC().Format("150405.000000000")
			emission := &core.DetectionEmissionResult{}
			result := core.AgentResult{
				PatternID: condition, Frequency: 1, AgentKind: "detect", SignalKind: "logs", DetectionEmission: emission,
			}
			finding := &core.AIFinding{Title: "Operator race", Severity: "medium"}
			if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
				t.Fatalf("CreateIncidentFromFinding(initial): %v", err)
			}
			incidentID := emission.IncidentID
			if incidentID == "" {
				t.Fatal("initial emission did not report an incident ID")
			}
			savesBefore := provider.saveCalls.Load()
			continued, err := ContinueDetectionEpisode(storage.DetectionOccurrence{
				Identity: storage.DetectionIdentity{
					AgentKind: "detect", Source: "loki", Service: "checkout",
					SignalKind: "logs", ConditionKey: condition,
				},
				Frequency: 1, Severity: "medium", ReceivedAt: time.Now().UTC(),
			})
			if err != nil || continued.OccurrenceCount != 2 {
				t.Fatalf("ContinueDetectionEpisode(active)=%+v err=%v", continued, err)
			}
			if provider.saveCalls.Load() != savesBefore {
				t.Fatalf("ExistingOnly performed a second service save: before=%d after=%d", savesBefore, provider.saveCalls.Load())
			}
			provider.blockNext.Store(true)
			emitDone := make(chan error, 1)
			go func() {
				emitDone <- CreateIncidentFromFinding(finding, result, "loki", "checkout")
			}()
			<-provider.saveStarted

			operator, err := base.GetIncident(incidentID)
			if err != nil {
				t.Fatalf("GetIncident(operator): %v", err)
			}
			now := time.Now().UTC()
			operator.Resolved = true
			operator.ResolvedAt = &now
			operator.AckedAt = &now
			operator.AssignedTeamID = "sre"
			operator.AssignedMemberIDs = []string{"operator"}
			if err := base.SaveIncident(operator); err != nil {
				t.Fatalf("SaveIncident(operator): %v", err)
			}
			close(provider.releaseSave)
			if err := <-emitDone; err != nil {
				t.Fatalf("CreateIncidentFromFinding(coalesced): %v", err)
			}
			got, err := base.GetIncident(incidentID)
			if err != nil {
				t.Fatalf("GetIncident(final): %v", err)
			}
			if !got.Resolved || got.ResolvedAt == nil || got.AckedAt == nil ||
				got.AssignedTeamID != "sre" || len(got.AssignedMemberIDs) != 1 ||
				got.AssignedMemberIDs[0] != "operator" || got.OccurrenceCount != 3 {
				t.Fatalf("coalesced save lost operator state: %+v", got)
			}
		})
	}
}

func TestCreateIncidentFromFinding_EpisodeErrorFailsOpen(t *testing.T) {
	loadAgentTestConfig(t)
	base := storage.NewMemory()
	previousStore := Storage()
	SetStorage(failingDetectionEpisodeStore{Provider: base, err: errors.New(strings.Repeat("x", 400))})
	t.Cleanup(func() { SetStorage(previousStore) })
	result := core.AgentResult{
		Verdict: core.VerdictUnknown, PatternID: "new-anomaly", Frequency: 1,
		AgentKind: "detect", SignalKind: "logs", DetectionEmission: &core.DetectionEmissionResult{},
	}
	finding := &core.AIFinding{Title: "New anomaly", Summary: "failed open", Severity: "medium"}
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("CreateIncidentFromFinding: %v", err)
	}
	incidents, err := base.ListIncidents(0)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("fail-open incidents=%d err=%v", len(incidents), err)
	}
	if result.DetectionEmission.EpisodeAction != "episode_error" ||
		result.DetectionEmission.NotificationOutcome != "sent" ||
		len(result.DetectionEmission.EpisodeError) != 256 {
		t.Fatalf("fail-open audit = %+v", result.DetectionEmission)
	}
}

func TestCreateIncidentFromFinding_UnsupportedEpisodeCapabilityIsSilent(t *testing.T) {
	base := storage.NewMemory()
	previousStore := Storage()
	SetStorage(failingDetectionEpisodeStore{Provider: base, err: storage.ErrUnsupported})
	t.Cleanup(func() {
		SetEmitInterceptor(nil)
		SetStorage(previousStore)
	})
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		return EmitDecision{Action: EmitGroup}
	})
	var logs bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	result := core.AgentResult{
		Verdict: core.VerdictUnknown, PatternID: "unsupported-condition", Frequency: 1,
		AgentKind: "detect", SignalKind: "logs", DetectionEmission: &core.DetectionEmissionResult{},
	}
	finding := &core.AIFinding{Title: "Unsupported episode backend", Severity: "medium"}
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("CreateIncidentFromFinding: %v", err)
	}
	if result.DetectionEmission.EpisodeAction != "" || result.DetectionEmission.EpisodeError != "" ||
		result.DetectionEmission.NotificationOutcome != "grouped" {
		t.Fatalf("unsupported capability audit=%+v", result.DetectionEmission)
	}
	if strings.Contains(logs.String(), "episode_error") {
		t.Fatalf("unsupported capability emitted episode error log: %q", logs.String())
	}
}

func TestCreateIncidentFromFinding_InterceptorCompletionIsTerminal(t *testing.T) {
	loadAgentTestConfig(t)
	cases := []struct {
		name   string
		action EmitAction
		want   string
	}{
		{name: "suppress", action: EmitSuppress, want: "suppressed"},
		{name: "group", action: EmitGroup, want: "grouped"},
		{name: "divert", action: EmitDivert, want: "diverted"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := storage.NewMemory()
			previousStore := Storage()
			SetStorage(provider)
			defer SetStorage(previousStore)
			var calls atomic.Int64
			SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
				calls.Add(1)
				return EmitDecision{Action: testCase.action}
			})
			defer SetEmitInterceptor(nil)
			result := core.AgentResult{
				Verdict: core.VerdictUnknown, PatternID: "same-condition", Frequency: 1,
				AgentKind: "detect", SignalKind: "logs", DetectionEmission: &core.DetectionEmissionResult{},
			}
			finding := &core.AIFinding{Title: "Same condition", Summary: "repeat", Severity: "medium"}
			for index := 0; index < 2; index++ {
				if err := CreateIncidentFromFinding(finding, result, "source", "service"); err != nil {
					t.Fatalf("CreateIncidentFromFinding(%d): %v", index, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("interceptor calls=%d, want 1", calls.Load())
			}
			if result.DetectionEmission.NotificationOutcome != "not_applicable" {
				t.Fatalf("repeat metadata=%+v, first outcome should have completed as %q", result.DetectionEmission, testCase.want)
			}
			if testCase.action == EmitSuppress || testCase.action == EmitGroup {
				incidents, err := provider.ListIncidents(0)
				if err != nil || len(incidents) != 1 {
					t.Fatalf("terminal incidents=%+v err=%v", incidents, err)
				}
				record := incidents[0]
				if record.NotifyStatus != testCase.want || record.OccurrenceCount != 2 ||
					record.DetectionEpisodeID == "" || record.DetectionFingerprint == "" ||
					record.Content["Frequency"] != int64(2) || len(record.ChannelsEnabled) != 0 ||
					len(record.ChannelsNotified) != 0 || record.OnCallTriggered {
					t.Fatalf("terminal detection incident=%+v", record)
				}
				if _, ok := record.Content["AckURL"]; ok {
					t.Fatalf("terminal detection incident has AckURL: %+v", record.Content)
				}
			}
		})
	}
}

func TestCreateIncidentFromFinding_TransientEscalationReadDoesNotRestartOnCall(t *testing.T) {
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	cfg.OnCall.Enable = true
	cfg.OnCall.WaitMinutes = 0
	onCall := &countingOnCallProvider{}
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	core.SetOnCallWorkflow(core.NewOnCallWorkflow(redisClient, onCall))
	t.Cleanup(func() {
		core.SetOnCallWorkflow(nil)
		_ = redisClient.Close()
	})

	base := storage.NewMemory()
	provider := &transientGetDetectionStore{
		Provider:  base,
		episodes:  base.(storage.DetectionEpisodeStore),
		detection: base.(storage.DetectionIncidentStore),
	}
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() {
		SetEmitInterceptor(nil)
		SetStorage(previousStore)
	})
	var notifications atomic.Int64
	SetEmitInterceptor(func(map[string]interface{}, string) EmitDecision {
		notifications.Add(1)
		return EmitDecision{Action: EmitProceed}
	})
	result := core.AgentResult{PatternID: "read-failure", Frequency: 1, AgentKind: "detect", SignalKind: "logs"}
	finding := &core.AIFinding{Title: "Read failure", Severity: "high"}
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("initial CreateIncidentFromFinding: %v", err)
	}
	provider.failGet.Store(true)
	finding.Severity = "critical"
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err == nil ||
		!strings.Contains(err.Error(), "temporary incident read failure") {
		t.Fatalf("transient escalation error=%v", err)
	}
	if onCall.calls.Load() != 1 || notifications.Load() != 1 {
		t.Fatalf("failed read restarted delivery: on-call=%d notifications=%d", onCall.calls.Load(), notifications.Load())
	}
	provider.failGet.Store(false)
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("retry CreateIncidentFromFinding: %v", err)
	}
	if onCall.calls.Load() != 1 || notifications.Load() != 2 {
		t.Fatalf("retry delivery state: on-call=%d notifications=%d", onCall.calls.Load(), notifications.Load())
	}
}

func TestCreateIncidentFromFinding_RawSampleAbsentFromEpisodeState(t *testing.T) {
	loadAgentTestConfig(t)
	dir := t.TempDir()
	provider, err := storage.NewFile(storage.FileOptions{DataDir: dir})
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	defer provider.Close()
	previousStore := Storage()
	SetStorage(provider)
	t.Cleanup(func() { SetStorage(previousStore) })
	const rawSample = "raw-payload-marker-should-not-enter-episode"
	result := core.AgentResult{
		Verdict: core.VerdictUnknown, PatternID: "safe-condition-key", Frequency: 1,
		AgentKind: "detect", SignalKind: "logs", SampleSignals: []core.Signal{{Message: rawSample}},
	}
	finding := &core.AIFinding{Title: "Safe identity", Summary: "sample remains incident evidence", Severity: "medium"}
	if err := CreateIncidentFromFinding(finding, result, "loki", "checkout"); err != nil {
		t.Fatalf("CreateIncidentFromFinding: %v", err)
	}
	episodeData, err := os.ReadFile(filepath.Join(dir, "detection_episodes.json"))
	if err != nil {
		t.Fatalf("read detection episode state: %v", err)
	}
	if strings.Contains(string(episodeData), rawSample) {
		t.Fatalf("raw sample leaked into detection episode state: %s", episodeData)
	}
}
