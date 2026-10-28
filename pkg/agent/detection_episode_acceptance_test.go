package agent_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type scriptedDetectionSource struct {
	batches [][]core.Signal
	done    chan struct{}
	once    sync.Once
	next    int
}

func (source *scriptedDetectionSource) Name() string { return "scripted-detection-source" }

func (source *scriptedDetectionSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	if source.next >= len(source.batches) {
		source.once.Do(func() { close(source.done) })
		return nil, time.Now().UTC(), nil
	}
	batch := source.batches[source.next]
	source.next++
	return batch, time.Now().UTC(), nil
}

type fixedDetectionAgent struct {
	calls *atomic.Int64
}

func (fixedDetectionAgent) Name() string          { return "fixed-detection-agent" }
func (fixedDetectionAgent) Kind() core.AITaskKind { return core.AITaskDetect }
func (agent fixedDetectionAgent) Run(context.Context, core.AITask) (*core.AICallResult, error) {
	agent.calls.Add(1)
	return &core.AICallResult{
		Finding: &core.AIFinding{
			Title: "Checkout failures", Summary: "checkout requests are failing", Severity: "medium", Confidence: 1,
		},
		Model: "hermetic-fixed-model",
	}, nil
}

func TestWorkerDetectionEpisodeIsBatchIndependent(t *testing.T) {
	loadDetectionEpisodeConfig(t)

	shapes := []struct {
		name              string
		batches           [][]core.Signal
		wantPipelineCalls int64
	}{
		{name: "one batch", batches: [][]core.Signal{detectionSignals(500)}, wantPipelineCalls: 1},
		{name: "one event per pull", batches: separateDetectionSignals(500), wantPipelineCalls: 100},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			provider := storage.NewMemory()
			previousStore := services.Storage()
			services.SetStorage(provider)
			t.Cleanup(func() { services.SetStorage(previousStore) })

			var notifications atomic.Int64
			var modelCalls atomic.Int64
			var emitterCalls atomic.Int64
			services.SetEmitInterceptor(func(map[string]interface{}, string) services.EmitDecision {
				notifications.Add(1)
				return services.EmitDecision{Action: services.EmitProceed}
			})
			t.Cleanup(func() { services.SetEmitInterceptor(nil) })

			catalog, err := agent.LoadCatalog(provider)
			if err != nil {
				t.Fatalf("LoadCatalog: %v", err)
			}
			matcher, matcherErrors := agent.NewRegexMatcher(config.AgentRegexConfig{DefaultPattern: ".*"})
			if len(matcherErrors) > 0 {
				t.Fatalf("NewRegexMatcher: %v", matcherErrors)
			}
			serviceMatcher, serviceErrors := agent.NewServiceMatcher([]string{`service=(\w+)`})
			if len(serviceErrors) > 0 {
				t.Fatalf("NewServiceMatcher: %v", serviceErrors)
			}

			source := &scriptedDetectionSource{batches: shape.batches, done: make(chan struct{})}
			worker, err := agent.NewWorker(agent.WorkerOptions{
				Cfg: config.AgentConfig{
					Mode: "detect", PollInterval: "1ms",
					Catalog: config.AgentCatalogConfig{AutoPromoteAfter: 100, PersistInterval: "1h"},
				},
				Sources: []core.SignalSource{source}, Matcher: matcher,
				Miner: agent.NewMiner(0.4, 4, 100), Catalog: catalog, Services: serviceMatcher,
				AI: agent.AIBundle{Detect: fixedDetectionAgent{calls: &modelCalls}},
				Emitter: func(finding *core.AIFinding, result core.AgentResult, source, service string) error {
					emitterCalls.Add(1)
					return services.CreateIncidentFromFinding(finding, result, source, service)
				},
				ContinueDetectionEpisode: services.ContinueDetectionEpisode,
			})
			if err != nil {
				t.Fatalf("NewWorker: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				worker.Run(ctx)
			}()
			select {
			case <-source.done:
				cancel()
			case <-time.After(10 * time.Second):
				cancel()
				t.Fatal("worker did not consume the scripted source")
			}
			<-stopped

			incidents, err := provider.ListIncidents(0)
			if err != nil {
				t.Fatalf("ListIncidents: %v", err)
			}
			if len(incidents) != 1 {
				t.Fatalf("incidents=%d, want 1", len(incidents))
			}
			incident := incidents[0]
			t.Logf("shape=%q notifications=%d incident_id=%s occurrence_count=%d", shape.name, notifications.Load(), incident.ID, incident.OccurrenceCount)
			if notifications.Load() != 1 || incident.OccurrenceCount != 500 {
				t.Fatalf("notifications=%d occurrence_count=%d, want 1/500", notifications.Load(), incident.OccurrenceCount)
			}
			if modelCalls.Load() != shape.wantPipelineCalls || emitterCalls.Load() != shape.wantPipelineCalls {
				t.Fatalf("model_calls=%d emitter_calls=%d, want %d/%d", modelCalls.Load(), emitterCalls.Load(), shape.wantPipelineCalls, shape.wantPipelineCalls)
			}
		})
	}
}

func detectionSignals(count int) []core.Signal {
	signals := make([]core.Signal, 0, count)
	for index := 0; index < count; index++ {
		signals = append(signals, core.Signal{
			Source: "scripted-detection-source", Message: "service=checkout request failed status=500",
			Raw: map[string]interface{}{"event_id": fmt.Sprintf("event-%03d", index)},
		})
	}
	return signals
}

func separateDetectionSignals(count int) [][]core.Signal {
	batches := make([][]core.Signal, 0, count)
	for _, signal := range detectionSignals(count) {
		batches = append(batches, []core.Signal{signal})
	}
	return batches
}

func loadDetectionEpisodeConfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("name: detection-episode-acceptance\nalert:\n  slack:\n    enable: false\noncall:\n  enable: false\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}
