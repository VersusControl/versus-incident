package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Memory budget for one capture at the history save limits: the published
// inventory blob may grow by at most this much because of the capture's
// history records, and saving it may need at most this much heap on top of
// what the published generation already holds.
const (
	captureHistoryBlobBudget = 16 << 20
	captureTransientBudget   = 128 << 20
)

// memoryPod is a Pod as the inventory holds it, with the labels, owner,
// node, image and status fields a production Pod carries.
func memoryPod(uid string, index int) kubeindex.Record {
	namespace := fmt.Sprintf("team-%02d", index%60)
	name := fmt.Sprintf("checkout-api-7d9f8b6c5-%05d", index)
	return kubeindex.Record{
		APIVersion: "v1", UID: uid, Kind: "Pod", Namespace: namespace, Name: name, ResourceVersion: fmt.Sprint(1000000 + index),
		Labels:    map[string]string{"app.kubernetes.io/name": "checkout-api", "app.kubernetes.io/instance": "checkout", "pod-template-hash": "7d9f8b6c5", "team": namespace},
		Owners:    []kubeindex.OwnerRef{{UID: "rs-" + namespace, Kind: "ReplicaSet", Name: "checkout-api-7d9f8b6c5", Namespace: namespace, Controller: true, ControllerKnown: true}},
		CreatedAt: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC),
		Phase:     "Running", NodeName: fmt.Sprintf("ip-10-0-%d-%d.ec2.internal", index%250, index%7), Images: []string{"registry.example.com/shop/checkout-api:2026.11.06-1a2b3c4d"},
		Summary:           map[string]string{"phase": "Running", "ready_containers": "1", "total_containers": "1", "restarts": "0", "qos_class": "Burstable"},
		RequestedCPUMilli: 250, RequestedMemoryByte: 512 << 20,
	}
}

// memoryCapture builds a published generation for a capture in which pods
// Pods were replaced, with its history batch prepared the way a capture
// prepares it, plus extra records carrying payload bytes when payload is set.
func memoryCapture(t *testing.T, pods, heavy, heavySize int) inventoryGeneration {
	t.Helper()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	batch := storage.EvidenceBatch{ID: "capture:memory", At: at, Epoch: "memory", Coverage: map[string]storage.EvidenceInterval{"Pod": {From: at.Add(-time.Minute), To: at}}}
	view := kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready", ObservedAt: at, CompletedAt: at}}}
	byUID := map[string]kubeindex.Record{}
	for i := 0; i < pods; i++ {
		old, replacement := memoryPod(fmt.Sprintf("old-%05d", i), i), memoryPod(fmt.Sprintf("new-%05d", i), i)
		view.Records = append(view.Records, replacement)
		byUID[old.UID], byUID[replacement.UID] = old, replacement
		for _, delta := range []kubeindex.Delta{{Old: &old}, {New: &replacement}} {
			for _, change := range kubechanges.DetectAll("memory-cluster", delta, at) {
				if record, ok := byUID[change.UID]; ok {
					change.RootUID = kubebreaks.Root(record, byUID).UID
				}
				raw, err := json.Marshal(change)
				if err != nil {
					t.Fatal(err)
				}
				batch.Records = append(batch.Records, storage.EvidenceRecord{AuthorizationKind: change.Kind, Priority: 1, ID: "change:" + change.ID, Type: "change", At: change.At, ObservedAt: at, Namespace: change.Namespace, Name: change.Name, Kind: change.Kind, UID: change.UID, RootUID: change.RootUID, Rule: string(change.Type), Epoch: batch.Epoch, Payload: raw})
			}
		}
	}
	for i := 0; i < heavy; i++ {
		head := fmt.Sprintf(`{"cluster":"memory-cluster","kind":"ConfigMap","name":"settings-%05d","type":"updated","fill":"`, i)
		raw := json.RawMessage(head + strings.Repeat("x", heavySize-len(head)-2) + `"}`)
		id := fmt.Sprintf("heavy-%05d", i)
		batch.Records = append(batch.Records, storage.EvidenceRecord{AuthorizationKind: "ConfigMap", Priority: 1, ID: "change:" + id, Type: "change", At: at, ObservedAt: at, Namespace: "shop", Name: id, Kind: "ConfigMap", UID: id, RootUID: id, Rule: "updated", Epoch: batch.Epoch, Payload: raw})
	}
	return inventoryGeneration{Revision: "memory", View: view, EvidenceScope: storage.EvidenceScope{OrgID: "default", ClusterID: "memory", ClusterUID: "memory-physical"}, EvidenceBatch: &batch}
}

type captureMemory struct {
	records, payloadBytes    int
	blob, blobWithoutHistory int
	retained                 int64
	// Transient heap above what the generation holds, per step of a capture
	// and for publishing the same generation without its history.
	validate, publish, publishWithoutHistory, save uint64
	// saveDatabase is the save into Postgres, measured when a database is
	// available; zero otherwise.
	saveDatabase uint64
}

func heapInUse() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// transientPeak runs step and reports the most heap it held above the heap in
// use when it started, garbage not yet collected included, at the default
// collection pace: the memory a process actually needs for the step. The heap
// is read continuously without stopping the world.
func transientPeak(step func()) uint64 {
	runtime.GC()
	start := heapInUse()
	var peak atomic.Uint64
	peak.Store(start)
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			if current := heapInUse(); current > peak.Load() {
				peak.Store(current)
			}
			select {
			case <-stop:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	step()
	if current := heapInUse(); current > peak.Load() {
		peak.Store(current)
	}
	close(stop)
	sampler.Wait()
	return peak.Load() - start
}

// measureCapture reports a capture's published blob size, the heap its
// generation retains, and the most heap each of the capture's steps needs
// beyond that: validating the batch, publishing the generation, and saving
// it the way the journal commit does (a projected copy of the records handed
// to the store, here the in-process store).
func measureCapture(t *testing.T, build func() inventoryGeneration) captureMemory {
	t.Helper()
	runtime.GC()
	runtime.GC()
	baseline := heapInUse()
	generation := build()
	runtime.GC()
	result := captureMemory{records: len(generation.EvidenceBatch.Records), retained: int64(heapInUse()) - int64(baseline)}
	for _, record := range generation.EvidenceBatch.Records {
		result.payloadBytes += len(record.Payload)
	}
	result.validate = transientPeak(func() {
		if err := generation.EvidenceBatch.Validate(); err != nil {
			t.Fatalf("a capture at the limits does not validate: %v", err)
		}
	})
	result.publish = transientPeak(func() {
		payload, err := json.Marshal(generation)
		if err != nil {
			t.Fatal(err)
		}
		result.blob = len(payload)
	})
	withoutHistory := generation
	withoutHistory.EvidenceBatch = nil
	result.publishWithoutHistory = transientPeak(func() {
		payload, _ := json.Marshal(withoutHistory)
		result.blobWithoutHistory = len(payload)
	})
	result.save = transientPeak(func() {
		provider := storage.NewMemory()
		defer provider.Close()
		batch := *generation.EvidenceBatch
		batch.Records = append([]storage.EvidenceRecord(nil), batch.Records...)
		if err := provider.(storage.EvidenceStore).CommitEvidence(context.Background(), generation.EvidenceScope, batch); err != nil {
			t.Fatalf("saving a capture at the limits: %v", err)
		}
	})
	if dsn := os.Getenv("TEST_POSTGRES_DSN"); dsn != "" {
		database, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		scope := generation.EvidenceScope
		scope.ClusterID = fmt.Sprintf("memory-%d", time.Now().UnixNano())
		result.saveDatabase = transientPeak(func() {
			batch := *generation.EvidenceBatch
			batch.Records = append([]storage.EvidenceRecord(nil), batch.Records...)
			if err := database.(storage.EvidenceStore).CommitEvidence(context.Background(), scope, batch); err != nil {
				t.Fatalf("saving a capture at the limits to Postgres: %v", err)
			}
		})
	}
	runtime.KeepAlive(generation)
	return result
}

// TestCaptureMemoryAtTheHistorySaveLimits measures one capture at each history
// save limit: the most records (Pods replaced in bulk, the realistic way to
// reach it) and the most payload bytes. The capture's history share of the
// published inventory blob and the transient heap of validating, publishing
// and saving it must stay within budget.
func TestCaptureMemoryAtTheHistorySaveLimits(t *testing.T) {
	if testing.Short() {
		t.Skip("builds captures at the history save limits")
	}
	recordsPerPod := len(memoryCapture(t, 1, 0, 0).EvidenceBatch.Records)
	heavySize := 64 << 10
	for _, shape := range []struct {
		name  string
		build func() inventoryGeneration
	}{
		{"most records", func() inventoryGeneration {
			return memoryCapture(t, storage.MaxEvidenceBatchRecords/recordsPerPod, 0, 0)
		}},
		{"most payload bytes", func() inventoryGeneration {
			return memoryCapture(t, 0, storage.MaxEvidenceBatchBytes/heavySize, heavySize)
		}},
	} {
		measured := measureCapture(t, shape.build)
		history := measured.blob - measured.blobWithoutHistory
		publishHistory := int64(measured.publish) - int64(measured.publishWithoutHistory)
		t.Logf("%s: %d records, %.1f MiB payload; published blob %.1f MiB, of which history %.1f MiB; generation retains %.1f MiB; transient: validate %.1f MiB, publish %.1f MiB (%.1f MiB without history), save to Postgres %.1f MiB, save to the in-process store %.1f MiB",
			shape.name, measured.records, float64(measured.payloadBytes)/(1<<20), float64(measured.blob)/(1<<20), float64(history)/(1<<20),
			float64(measured.retained)/(1<<20), float64(measured.validate)/(1<<20), float64(measured.publish)/(1<<20), float64(measured.publishWithoutHistory)/(1<<20),
			float64(measured.saveDatabase)/(1<<20), float64(measured.save)/(1<<20))
		if history > captureHistoryBlobBudget {
			t.Errorf("%s: the capture's history adds %.1f MiB to the published blob, over the %d MiB budget", shape.name, float64(history)/(1<<20), captureHistoryBlobBudget>>20)
		}
		// The steps run one after another, so the largest bounds the peak:
		// validating the batch, what the history adds to publishing, and the
		// save into Postgres, the store a cluster of this size runs on. The
		// in-process store keeps its whole history in one document and is
		// reported, not budgeted.
		for step, used := range map[string]int64{"validating": int64(measured.validate), "publishing": publishHistory, "saving to Postgres": int64(measured.saveDatabase)} {
			if used > captureTransientBudget {
				t.Errorf("%s: %s the capture's history needed %.1f MiB of transient heap, over the %d MiB budget", shape.name, step, float64(used)/(1<<20), captureTransientBudget>>20)
			}
		}
	}
}
