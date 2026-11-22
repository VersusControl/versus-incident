package services

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// A storm roll-up is keyed by its window start. Two senders that
// report the same roll-up window while its count drifts between
// ticks notify once over one incident; a roll-up for the next window is a new
// episode.
func TestKubernetesIssueRollupIsOneEpisodePerWindowAcrossTicksAndSenders(t *testing.T) {
	loadAgentTestConfig(t)
	cfg := config.GetConfig()
	previousOnCall := cfg.OnCall
	t.Cleanup(func() { cfg.OnCall = previousOnCall })
	cfg.OnCall.Enable, cfg.OnCall.InitializedOnly = false, false
	old := store
	SetStorage(storage.NewMemory())
	t.Cleanup(func() { SetStorage(old) })

	windowStart := time.Now().Add(-time.Minute).UTC()
	rollup := func(start time.Time, tick int) kubernetes.IssueAlert {
		return kubernetes.IssueAlert{OrgID: "default", Cluster: "alpha", Rule: "storm_rollup", RuleLabel: "More issues", Severity: "warning", BrokeAt: start, RollupCount: 3 + tick%5}
	}
	var notified atomic.Int32
	incidents := sync.Map{}
	var senders sync.WaitGroup
	for sender := range 2 {
		senders.Add(1)
		go func() {
			defer senders.Done()
			for tick := range 50 {
				outcome, err := CreateIncidentFromKubernetesIssue("default", rollup(windowStart, tick+sender))
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
		t.Fatalf("one roll-up window notified %d times over %d incidents, want once over one", notified.Load(), count)
	}

	next, err := CreateIncidentFromKubernetesIssue("default", rollup(windowStart.Add(10*time.Minute), 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, same := incidents.Load(next.IncidentID); same || next.NotificationOutcome == "not_applicable" {
		t.Fatalf("the next roll-up window joined the previous episode: %+v", next)
	}
}
