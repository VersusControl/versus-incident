package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/scheduler"
	"github.com/VersusControl/versus-incident/pkg/servicehealth"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

type healthRecordingSource struct {
	name  string
	pulls int
}

type pullCountingSignalSource struct {
	core.SignalSource
	pulls int
}

func (source *pullCountingSignalSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	source.pulls++
	return source.SignalSource.Pull(ctx, since)
}

func (source *healthRecordingSource) Name() string { return source.name }
func (source *healthRecordingSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	source.pulls++
	now := time.Now().UTC()
	return []core.Signal{{Source: source.name, Timestamp: now, Severity: "error", Message: "service=api password=secret failure 42", Raw: map[string]interface{}{"cursor": "secret"}}}, now, nil
}

type capturingHealthRecorder struct {
	logs    []core.LogHealthObservation
	sources []core.SourceHealthObservation
	logErr  error
}

func (recorder *capturingHealthRecorder) RecordLogHealth(_ context.Context, observation core.LogHealthObservation) error {
	recorder.logs = append(recorder.logs, observation)
	return recorder.logErr
}

func TestWorkerHealthBufferFullDoesNotBlockCursorAndPreservesOrg(t *testing.T) {
	source := &healthRecordingSource{name: "loki"}
	recorder := &capturingHealthRecorder{logErr: servicehealth.ErrBufferFull}
	worker := newSeamWorker(t, "training", source, AIBundle{}, nil, "org-a")
	worker.healthRecorder = recorder
	worker.kindByName[source.name] = signalsources.KindLogs
	worker.cursors = NewCursorStore(nil)

	worker.tickSource(context.Background(), source, "training")

	if len(recorder.logs) != 1 || recorder.logs[0].OrgID != "org-a" {
		t.Fatalf("observations = %#v", recorder.logs)
	}
	if len(recorder.sources) != 2 || recorder.sources[1].Succeeded || recorder.sources[1].ErrorClass != "unavailable" {
		t.Fatalf("source health = %#v", recorder.sources)
	}
	if _, ok := worker.cursors.Get(context.Background(), source.name); !ok {
		t.Fatal("cursor did not advance after non-critical health projection failure")
	}
}

func TestWorkerNonOwnerSkipsHealthProjectionWithoutBlockingCursor(t *testing.T) {
	scheduler.SetOwnership(func(name string) bool { return name != "service-health" })
	t.Cleanup(func() { scheduler.SetOwnership(nil) })
	source := &healthRecordingSource{name: "loki"}
	recorder := &capturingHealthRecorder{}
	worker := newSeamWorker(t, "training", source, AIBundle{}, nil, "org-a")
	worker.healthRecorder = recorder
	worker.kindByName[source.name] = signalsources.KindLogs
	worker.cursors = NewCursorStore(nil)

	worker.tickSource(context.Background(), source, "training")

	if len(recorder.logs) != 0 || len(recorder.sources) != 0 {
		t.Fatalf("non-owner recorded health projection: logs=%#v sources=%#v", recorder.logs, recorder.sources)
	}
	if _, ok := worker.cursors.Get(context.Background(), source.name); !ok {
		t.Fatal("non-owner cursor did not advance")
	}
}

func (recorder *capturingHealthRecorder) RecordSourceHealth(_ context.Context, observation core.SourceHealthObservation) error {
	recorder.sources = append(recorder.sources, observation)
	return nil
}

type bufferFullHealthRecorder struct{ manager *servicehealth.Manager }

func (recorder bufferFullHealthRecorder) RecordLogHealth(context.Context, core.LogHealthObservation) error {
	return servicehealth.ErrBufferFull
}

func (recorder bufferFullHealthRecorder) RecordSourceHealth(ctx context.Context, observation core.SourceHealthObservation) error {
	return recorder.manager.RecordSourceHealth(ctx, observation)
}

func TestWorkerHealthBufferFullSurfacesPartialCoverage(t *testing.T) {
	store := storage.NewMemory()
	manager := servicehealth.NewManager(store)
	source := &healthRecordingSource{name: "loki"}
	worker := newSeamWorker(t, "training", source, AIBundle{}, nil, "org-a")
	worker.healthRecorder = bufferFullHealthRecorder{manager: manager}
	worker.kindByName[source.name] = signalsources.KindLogs
	worker.cursors = NewCursorStore(nil)

	worker.tickSource(context.Background(), source, "training")

	collector := servicehealth.NewCollector(servicehealth.CollectorOptions{
		Manager: manager, Store: store, SourceIDs: []string{source.name},
		Services: func() []servicehealth.ServiceMetadata {
			return []servicehealth.ServiceMetadata{{OrgID: "org-a", Name: "api"}}
		},
	})
	snapshot, err := collector.Collect(context.Background(), "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Services) != 1 || !snapshot.Coverage.Partial || snapshot.Services[0].Availability["logs"].State != core.HealthPartial {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, ok := worker.cursors.Get(context.Background(), source.name); !ok {
		t.Fatal("cursor did not advance")
	}
}

type countingDetector struct {
	core.SignalDetector
	calls int
}

func (detector *countingDetector) Classify(observation core.Observation, mean, std float64, confident bool) core.TypedVerdict {
	detector.calls++
	return detector.SignalDetector.Classify(observation, mean, std, confident)
}

func TestWorkerProjectsNormalizedLogShapeWithoutAnotherPull(t *testing.T) {
	for _, sourceName := range []string{"configured-log", "registered-log-stub"} {
		t.Run(sourceName, func(t *testing.T) {
			source := &healthRecordingSource{name: sourceName}
			recorder := &capturingHealthRecorder{}
			worker := newSeamWorker(t, "training", source, AIBundle{}, nil)
			worker.healthRecorder = recorder
			worker.kindByName[sourceName] = signalsources.KindLogs

			worker.tickSource(context.Background(), source, "training")

			if source.pulls != 1 {
				t.Fatalf("Pull calls = %d, want 1", source.pulls)
			}
			if len(recorder.logs) != 1 || len(recorder.sources) != 1 {
				t.Fatalf("projection logs=%d sources=%d", len(recorder.logs), len(recorder.sources))
			}
			observation := recorder.logs[0]
			if observation.SourceID != sourceName || observation.Service != "api" || observation.Frequency != 1 || observation.LifecycleClassified {
				t.Fatalf("projection = %#v", observation)
			}
		})
	}
}

func TestWorkerProjectsBuiltInFileSourceThroughTickSource(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/app.log"
	if err := os.WriteFile(logPath, []byte("2026-09-10T12:34:56Z service=api error request failed 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileSource, err := signalsources.NewFileSource("health", config.AgentFileSourceConfig{
		Path: logPath, CursorPath: dir + "/cursor", FromBeginning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := &pullCountingSignalSource{SignalSource: fileSource}
	recorder := &capturingHealthRecorder{}
	worker := newSeamWorker(t, "training", source, AIBundle{}, nil)
	worker.healthRecorder = recorder
	worker.kindByName[source.Name()] = signalsources.KindOf("file")

	started := time.Now().UTC().Add(-time.Second)
	worker.tickSource(context.Background(), source, "training")
	finished := time.Now().UTC().Add(time.Second)

	if source.pulls != 1 {
		t.Fatalf("Pull calls = %d, want 1", source.pulls)
	}
	if len(recorder.sources) != 1 || !recorder.sources[0].Succeeded {
		t.Fatalf("source observations = %#v", recorder.sources)
	}
	if len(recorder.logs) != 1 {
		t.Fatalf("log observations = %#v", recorder.logs)
	}
	observation := recorder.logs[0]
	if observation.SourceID != "file:health" || observation.Service != "api" || observation.PatternID == "" || observation.Frequency != 1 || observation.ObservedAt.Before(started) || observation.ObservedAt.After(finished) || observation.LifecycleClassified {
		t.Fatalf("normalized projection = %#v", observation)
	}
}

func TestWorkerHealthProjectionUsesSingleExistingClassification(t *testing.T) {
	source := &healthRecordingSource{name: "loki"}
	recorder := &capturingHealthRecorder{}
	worker := newSeamWorker(t, "shadow", source, AIBundle{}, nil)
	worker.healthRecorder = recorder
	learner, detector := worker.brainFor(source.Name())
	counting := &countingDetector{SignalDetector: detector}
	worker.brains[source.Name()] = typedBrain{learner: learner, detector: counting}

	worker.tickSource(context.Background(), source, "shadow")

	if counting.calls != 1 {
		t.Fatalf("Classify calls = %d, want 1", counting.calls)
	}
	if len(recorder.logs) != 1 || !recorder.logs[0].LifecycleClassified {
		t.Fatalf("projection = %#v", recorder.logs)
	}
}
