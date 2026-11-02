package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func TestRunningInitCompletionContinuesCheckpointWithoutReplayingLines(t *testing.T) {
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ended := time.Now().UTC().Truncate(time.Second)
	at := ended.Add(-5 * time.Second)
	running := workloadStreamRecords("StatefulSet", 1, 1)
	running[1].LogContainers = append(running[1].LogContainers, kubeindex.ContainerState{Name: "setup", Init: true, StateKnown: true, Running: true, StartedAt: started})
	finished := append([]kubeindex.Record(nil), running...)
	finished[1].LogContainers = append([]kubeindex.ContainerState(nil), running[1].LogContainers...)
	finished[1].LogContainers[1].Running = false
	finished[1].LogContainers[1].CurrentTermination = true
	finished[1].LogContainers[1].TerminatedAt = ended
	finished[1].LogContainers[1].TerminatedReason = "Completed"
	var completed atomic.Bool
	var follows, finite atomic.Int32
	var finiteSince atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/log") {
			record := running[1]
			if completed.Load() {
				record = finished[1]
			}
			object := streamPodObject(record)
			object["spec"].(map[string]any)["containers"] = []any{map[string]any{"name": "app-0"}}
			object["spec"].(map[string]any)["initContainers"] = []any{map[string]any{"name": "setup"}}
			state := map[string]any{"running": map[string]any{"startedAt": started.Format(time.RFC3339Nano)}}
			if completed.Load() {
				state = map[string]any{"terminated": map[string]any{"startedAt": started.Format(time.RFC3339Nano), "finishedAt": ended.Format(time.RFC3339Nano), "reason": "Completed", "exitCode": 0}}
			}
			object["status"].(map[string]any)["initContainerStatuses"] = []any{map[string]any{"name": "setup", "restartCount": 0, "state": state}}
			writeJSON(writer, object)
			return
		}
		count := 4
		if request.URL.Query().Get("follow") == "true" {
			follows.Add(1)
			count = 3
		} else {
			finite.Add(1)
			finiteSince.Store(request.URL.Query().Get("sinceTime"))
		}
		for i := 1; i <= count; i++ {
			fmt.Fprintf(writer, "%s line-%d\n", at.Format(time.RFC3339Nano), i)
		}
		writer.(http.Flusher).Flush()
		if count == 3 {
			<-request.Context().Done()
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, running)
	ctx, cancel := context.WithTimeout(logStreamTestContext(), 8*time.Second)
	defer cancel()
	counts := make(map[string]int)
	lines, gaps := 0, 0
	err := service.StreamWorkloadLogs(ctx, WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Container: "setup", SinceSeconds: 60, TailLines: 123}, func(event WorkloadLogStreamEvent) error {
		if event.ReplayUncertain {
			gaps++
		}
		if event.Event != "line" {
			return nil
		}
		lines++
		counts[event.Text]++
		if event.Text == "line-3" && completed.CompareAndSwap(false, true) {
			seedWorkloadStreamIndex(t, service, finished)
		}
		return nil
	})
	if err != nil || lines != 4 || len(counts) != 4 || gaps != 0 || follows.Load() != 1 || finite.Load() != 1 {
		t.Fatalf("same init run replayed during completion: counts=%v lines=%d gaps=%d follows=%d finite=%d err=%v", counts, lines, gaps, follows.Load(), finite.Load(), err)
	}
	if since, ok := finiteSince.Load().(string); !ok || since != at.Add(-time.Nanosecond).Format(time.RFC3339Nano) {
		t.Fatalf("completed finite read did not use prior checkpoint: %v", finiteSince.Load())
	}
}

func TestInitCompletionKeepsCheckpointWhenProcessSlotIsContended(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	records := workloadStreamRecords("StatefulSet", 1, 1)
	started := time.Now().UTC().Add(-time.Hour)
	records[1].LogContainers = append(records[1].LogContainers, kubeindex.ContainerState{Name: "setup", Init: true, Running: true, StateKnown: true, StartedAt: started})
	seedWorkloadStreamIndex(t, service, records)
	session, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Container: "setup"})
	if err != nil {
		t.Fatal(err)
	}
	reservations := []func(){}
	defer func() {
		session.Close()
		for _, release := range reservations {
			release()
		}
	}()
	for i := 0; i < MaxProcessLogReads-1; i++ {
		release, err := acquireLogRead()
		if err != nil {
			t.Fatal(err)
		}
		reservations = append(reservations, release)
	}
	var oldID string
	var competitor func()
	for id, worker := range session.workers {
		oldID = id
		raw, _ := json.Marshal(logResumeCursor{Target: service.logTarget(worker.options), Timestamp: time.Now().UTC().Add(-time.Second), Ordinal: 2})
		session.resume[id] = base64.RawURLEncoding.EncodeToString(raw)
		release := worker.release
		var once sync.Once
		worker.release = func() {
			once.Do(func() {
				release()
				var err error
				competitor, err = acquireLogRead()
				if err != nil {
					t.Error(err)
				}
			})
		}
	}
	defer func() {
		if competitor != nil {
			competitor()
		}
	}()
	current := records[1]
	current.LogContainers = append([]kubeindex.ContainerState(nil), records[1].LogContainers...)
	current.LogContainers[1].Running = false
	current.LogContainers[1].StartedAt = time.Time{}
	current.LogContainers[1].CurrentTermination = true
	current.LogContainers[1].TerminatedAt = time.Now().UTC()
	if err := session.reconcile([]kubeindex.Record{current}); err != nil {
		t.Fatal(err)
	}
	anchor := session.workers[oldID]
	if anchor == nil || !anchor.retiredForCompletion || session.resume[oldID] == "" {
		t.Fatal("contended transition lost its bounded checkpoint anchor")
	}
	session.startWorkers()
	if anchor.started {
		t.Fatal("stopped running source was reopened under contention")
	}
	competitor()
	if err := session.reconcile([]kubeindex.Record{current}); err != nil {
		t.Fatal(err)
	}
	if len(session.workers) != 1 || session.workers[oldID] != nil {
		t.Fatal("completion did not replace its stopped anchor")
	}
	for _, worker := range session.workers {
		cursor, err := service.logCursor(worker.options)
		if err != nil || cursor.Ordinal != 2 {
			t.Fatal("checkpoint lost after read capacity recovered", err)
		}
	}
}

func TestInitCompletionCannotBorrowOtherUIDOrRestartCheckpoint(t *testing.T) {
	for _, change := range []string{"completion", "restart", "recreated", "different-start", "running-new-start", "unknown-start"} {
		t.Run(change, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
			service.SetScrubber(replacingScrubber{})
			started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			records := workloadStreamRecords("StatefulSet", 1, 1)
			records[1].LogContainers = append(records[1].LogContainers, kubeindex.ContainerState{Name: "setup", Init: true, StateKnown: true, Running: true, StartedAt: started})
			if change == "unknown-start" {
				records[1].LogContainers[1].StartedAt = time.Time{}
			}
			seedWorkloadStreamIndex(t, service, records)
			session, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Container: "setup"})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			for id, worker := range session.workers {
				raw, _ := json.Marshal(logResumeCursor{Target: service.logTarget(worker.options), Timestamp: time.Now().UTC().Add(-time.Second), Ordinal: 2})
				session.resume[id] = base64.RawURLEncoding.EncodeToString(raw)
			}
			current := records[1]
			current.LogContainers = append([]kubeindex.ContainerState(nil), records[1].LogContainers...)
			current.LogContainers[1].Running = false
			current.LogContainers[1].StartedAt = time.Time{}
			current.LogContainers[1].CurrentTermination = true
			current.LogContainers[1].TerminatedAt = time.Now().UTC()
			current.LogContainers[1].TerminatedReason = "Completed"
			switch change {
			case "restart":
				current.LogContainers[1].RestartCount = 1
			case "recreated":
				current.UID = "replacement"
			case "different-start":
				current.LogContainers[1].StartedAt = started.Add(time.Minute)
			case "running-new-start":
				current.LogContainers[1].Running = true
				current.LogContainers[1].CurrentTermination = false
				current.LogContainers[1].StartedAt = started.Add(time.Minute)
			}
			if err := session.reconcile([]kubeindex.Record{current}); err != nil {
				t.Fatal(err)
			}
			for _, worker := range session.workers {
				if change == "completion" {
					cursor, err := service.logCursor(worker.options)
					if err != nil || cursor.Ordinal != 2 {
						t.Fatalf("proven same-run completion lost checkpoint: %+v %v", cursor, err)
					}
				} else if worker.options.Cursor != "" {
					t.Fatal("different or unproven init run inherited checkpoint")
				}
			}
		})
	}
}
