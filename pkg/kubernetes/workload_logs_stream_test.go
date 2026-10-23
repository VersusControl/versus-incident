package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

func workloadStreamRecords(kind string, count, containers int) []kubeindex.Record {
	records := []kubeindex.Record{{Kind: kind, UID: "root", Name: "api", Namespace: "shop"}}
	owner := kubeindex.OwnerRef{Kind: kind, UID: "root", Name: "api", Namespace: "shop", Controller: true, ControllerKnown: true}
	if kind == "Deployment" {
		records = append(records, kubeindex.Record{Kind: "ReplicaSet", UID: "rs", Name: "api-rs", Namespace: "shop", Owners: []kubeindex.OwnerRef{owner}})
		owner = kubeindex.OwnerRef{Kind: "ReplicaSet", UID: "rs", Name: "api-rs", Namespace: "shop", Controller: true, ControllerKnown: true}
	}
	for i := 0; i < count; i++ {
		pod := kubeindex.Record{Kind: "Pod", UID: fmt.Sprintf("pod-%02d", i), Name: fmt.Sprintf("api-%02d", i), Namespace: "shop", MainContainer: "app-0", Owners: []kubeindex.OwnerRef{owner}}
		for j := 0; j < containers; j++ {
			pod.LogContainers = append(pod.LogContainers, kubeindex.ContainerState{Name: fmt.Sprintf("app-%d", j), Running: true, StateKnown: true, StartedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)})
		}
		records = append(records, pod)
	}
	return records
}

func seedWorkloadStreamIndex(t *testing.T, service *Service, records []kubeindex.Record) {
	t.Helper()
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Deployment", "StatefulSet", "ReplicaSet", "Pod"} {
		var items []kubeindex.Record
		for _, item := range records {
			if item.Kind == kind {
				items = append(items, item)
			}
		}
		if err := index.Refresh(context.Background(), kind, 0, time.Now().UTC(), func(context.Context) ([]kubeindex.Record, bool, error) { return items, true, nil }); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkloadLogOwnershipUsesUIDChainAndFreshness(t *testing.T) {
	for _, kind := range []string{"Deployment", "StatefulSet"} {
		t.Run(kind, func(t *testing.T) {
			records := workloadStreamRecords(kind, 2, 1)
			unrelated := records[len(records)-1]
			unrelated.UID, unrelated.Name = "unrelated", "same-label"
			unrelated.Owners = []kubeindex.OwnerRef{{Kind: kind, UID: "other-root", Namespace: "shop", Controller: true, ControllerKnown: true}}
			records = append(records, unrelated)
			snapshot := kubeindex.Snapshot{Records: records, Kinds: map[string]kubeindex.KindStatus{}}
			for _, name := range []string{kind, "Pod", "ReplicaSet"} {
				snapshot.Kinds[name] = kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC()}
			}
			options := WorkloadLogStreamOptions{Kind: kind, Name: "api", Namespace: "shop", UID: "root"}
			pods, err := workloadLogPods(snapshot, options, true)
			if err != nil || len(pods) != 2 {
				t.Fatalf("ownership: %+v %v", pods, err)
			}
			options.PodUIDs = []string{"unrelated"}
			if _, err := workloadLogPods(snapshot, options, true); !errors.Is(err, ErrInvalidArguments) {
				t.Fatal("unrelated focus accepted", err)
			}
			options.PodUIDs = nil
			options.UID = "replacement"
			if _, err := workloadLogPods(snapshot, options, true); !errors.Is(err, ErrNotFound) {
				t.Fatal("UID replacement accepted", err)
			}
			options.UID = "root"
			snapshot.Kinds["Pod"] = kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().Add(-3 * time.Minute)}
			if _, err := workloadLogPods(snapshot, options, true); !errors.Is(err, ErrForbidden) {
				t.Fatal("stale membership accepted", err)
			}
		})
	}
}

func TestWorkloadLogAdmissionDefaultsBoundsAndFocus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	records := workloadStreamRecords("Deployment", 25, 3)
	seedWorkloadStreamIndex(t, service, records)
	options := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}
	session, err := service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.workers) != 20 || len(session.pods) != 25 {
		t.Fatalf("default bounds: %d %d", len(session.workers), len(session.pods))
	}
	for _, worker := range session.workers {
		if worker.options.Container != "app-0" {
			t.Fatal("main container default lost")
		}
	}
	if session.pods[24].Admitted {
		t.Fatal("extra Pod silently admitted")
	}
	session.Close()
	options.Container = "*"
	session, err = service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.workers) != 40 {
		t.Fatalf("all-container ceiling: %d", len(session.workers))
	}
	session.Close()
	options.PodUIDs = []string{"pod-24"}
	session, err = service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.workers) != 3 {
		t.Fatal("extra Pod focus could not be admitted")
	}
	for _, worker := range session.workers {
		if worker.source.PodUID != "pod-24" {
			t.Fatal("focus escaped")
		}
	}
	session.Close()
	session.Close()
}

func TestWorkloadPreviousFanInAndResumeIsolation(t *testing.T) {
	var follows atomic.Int32
	records := workloadStreamRecords("StatefulSet", 2, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveStreamPodObject(writer, request, records) {
			return
		}
		if request.URL.Query().Get("follow") != "false" {
			follows.Add(1)
		}
		if strings.Contains(request.URL.Path, "api-01") {
			writer.WriteHeader(403)
			return
		}
		fmt.Fprintln(writer, "2026-10-01T00:00:00Z token=secret")
		fmt.Fprintln(writer, "2026-10-01T00:00:01Z second")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, records)
	options := WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Previous: true}
	var events []WorkloadLogStreamEvent
	err := service.StreamWorkloadLogs(logStreamTestContext(), options, func(event WorkloadLogStreamEvent) error { events = append(events, event); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var lines, statuses int
	cursor := ""
	for _, event := range events {
		if event.Event == "line" {
			lines++
			if event.PodUID != "pod-00" || event.SourceID == "" || event.Incarnation == "" || strings.Contains(event.Text, "secret") {
				t.Fatalf("unsafe source identity or redaction: %+v", event)
			}
		}
		if event.Event == "source_status" && event.PodUID == "pod-01" {
			statuses++
		}
		if event.Cursor != "" {
			cursor = event.Cursor
		}
	}
	if lines != 2 || statuses != 1 || follows.Load() != 0 || cursor == "" {
		t.Fatalf("finite fan-in lines=%d status=%d follows=%d cursor=%t", lines, statuses, follows.Load(), cursor != "")
	}
	options.Cursor = cursor
	events = nil
	if err := service.StreamWorkloadLogs(logStreamTestContext(), options, func(event WorkloadLogStreamEvent) error { events = append(events, event); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Event == "line" {
			t.Fatal("resumed retained line duplicated")
		}
	}
	options.UID = "other"
	if _, err := service.workloadCursor(options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("cross-workload cursor accepted", err)
	}
	options.UID = "root"
	options.Container = "app-0"
	if _, err := service.workloadCursor(options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("cross-container cursor accepted", err)
	}
	options.Container = ""
	options.PodUIDs = []string{"pod-00"}
	if _, err := service.workloadCursor(options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("cross-focus cursor accepted", err)
	}
}

func TestWorkloadReconcileMembershipAndIncarnation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	records := workloadStreamRecords("Deployment", 1, 1)
	seedWorkloadStreamIndex(t, service, records)
	session, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	old := ""
	for id := range session.workers {
		old = id
	}
	newRecords := workloadStreamRecords("Deployment", 2, 1)
	newRecords[2].LogContainers[0].RestartCount = 1
	if err := session.reconcile(newRecords[2:]); err != nil {
		t.Fatal(err)
	}
	if len(session.workers) != 2 || session.workers[old] != nil {
		t.Fatal("restart/join did not replace old source")
	}
	if err := session.reconcile(newRecords[3:]); err != nil {
		t.Fatal(err)
	}
	if len(session.workers) != 1 {
		t.Fatal("deleted Pod worker retained")
	}
}

func TestWorkloadIdleAuthorizationRevokesAllReads(t *testing.T) {
	var opened, closed atomic.Int32
	records := workloadStreamRecords("Deployment", 2, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveStreamPodObject(writer, request, records) {
			return
		}
		opened.Add(1)
		writer.WriteHeader(200)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		closed.Add(1)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, records)
	var revoked atomic.Bool
	ctx := WithLogStreamAuthorization(logStreamTestContext(), func(context.Context) error {
		if revoked.Load() {
			return ErrForbidden
		}
		return nil
	})
	done := make(chan error, 1)
	go func() {
		done <- service.StreamWorkloadLogs(ctx, WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}, func(WorkloadLogStreamEvent) error { return nil })
	}()
	deadline := time.Now().Add(3 * time.Second)
	for opened.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if opened.Load() != 2 {
		t.Fatal("workers did not open")
	}
	revoked.Store(true)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle authorization did not cancel fan-in")
	}
	deadline = time.Now().Add(time.Second)
	for closed.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closed.Load() != 2 {
		t.Fatal("revoked upstream read survived")
	}
}

func TestLogReadBudgetSharedWithPodAndWorkload(t *testing.T) {
	releases := []func(){}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < MaxProcessLogReads; i++ {
		release, err := acquireLogRead()
		if err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	if _, err := acquireLogRead(); !errors.Is(err, ErrLogStreamBusy) {
		t.Fatal("process read cap exceeded")
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("read opened above process limit") }))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	if err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "shop", Pod: "api"}, func(PodLogStreamEvent) error { return nil }); !errors.Is(err, ErrLogStreamBusy) {
		t.Fatal("Pod bypassed shared read budget", err)
	}
	if _, err := service.PodLogs(context.Background(), "shop", "api", "app", false, 60, 10); !errors.Is(err, ErrLogStreamBusy) {
		t.Fatal("snapshot bypassed shared process read budget", err)
	}
	seedWorkloadStreamIndex(t, service, workloadStreamRecords("Deployment", 1, 1))
	if _, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}); !errors.Is(err, ErrLogStreamBusy) {
		t.Fatal("workload bypassed initial read budget", err)
	}
}

func TestWorkloadCursorRejectsOversizeAndCrossEpoch(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	options := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}
	cursor, _ := service.workloadCursor(options)
	encoded, _ := json.Marshal(cursor)
	options.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
	if _, err := service.workloadCursor(options); err != nil {
		t.Fatal(err)
	}

	other := service.Scoped(Scope{OrgID: "default", ClusterID: "other"})
	if _, err := other.workloadCursor(options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("cross-cluster cursor accepted", err)
	}
	options.Cursor = strings.Repeat("x", maxWorkloadCursorBytes+1)
	if _, err := service.workloadCursor(options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatal("oversized cursor accepted", err)
	}
}

func TestWorkloadBackpressureWaitIsCanceledAndJoinsWorker(t *testing.T) {
	records := workloadStreamRecords("StatefulSet", 1, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveStreamPodObject(writer, request, records) {
			return
		}
		for i := 0; i < 500; i++ {
			fmt.Fprintln(writer, "2026-10-01T00:00:00Z safe")
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, records)
	session, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.queue = make(chan workloadSourceMessage, 1)
	session.startWorkers()
	deadline := time.Now().Add(time.Second)
	for len(session.queue) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(session.queue) != 1 {
		t.Fatal("test did not fill bounded queue")
	}
	session.cancel()
	for _, worker := range session.workers {
		select {
		case <-worker.done:
		case <-time.After(time.Second):
			t.Fatal("backpressured worker survived viewer end")
		}
	}
}

func TestDeploymentLogOwnershipIncludesBothRolloutReplicaSets(t *testing.T) {
	records := workloadStreamRecords("Deployment", 1, 1)
	oldSet := records[1]
	oldSet.UID, oldSet.Name = "old-rs", "api-old"
	oldPod := records[2]
	oldPod.UID, oldPod.Name = "old-pod", "api-old-0"
	oldPod.Owners = []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "old-rs", Name: "api-old", Namespace: "shop", Controller: true, ControllerKnown: true}}
	records = append(records, oldSet, oldPod)
	snapshot := kubeindex.Snapshot{Records: records, Kinds: map[string]kubeindex.KindStatus{}}
	for _, kind := range []string{"Deployment", "ReplicaSet", "Pod"} {
		snapshot.Kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: time.Now()}
	}
	pods, err := workloadLogPods(snapshot, WorkloadLogStreamOptions{Kind: "Deployment", Namespace: "shop", Name: "api", UID: "root"}, true)
	if err != nil || len(pods) != 2 {
		t.Fatalf("rollout ReplicaSet omitted: %+v %v", pods, err)
	}
}
