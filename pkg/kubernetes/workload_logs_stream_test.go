package kubernetes

import (
	"context"
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
			_, err = workloadLogPods(snapshot, options, true)
			// A stale kind is a reason to wait, never a permission answer.
			if !errors.Is(err, ErrInventoryNotReady) || errors.Is(err, ErrForbidden) {
				t.Fatalf("stale membership not reported as retryable: %v", err)
			}
			if detail := DiagnoseError(err); detail.Code != "inventory_not_ready" || !detail.Retryable || !strings.Contains(detail.Message, "Pod inventory is 3 min old") {
				t.Fatalf("stale membership diagnosis = %+v", detail)
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
		base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
		fmt.Fprintf(writer, "%s token=secret\n", base.Format(time.RFC3339Nano))
		fmt.Fprintf(writer, "%s second\n", base.Add(time.Second).Format(time.RFC3339Nano))
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
	var watermark time.Time
	for _, event := range events {
		if event.Event == "line" {
			lines++
			if event.PodUID != "pod-00" || event.SourceID == "" || event.Incarnation == "" || strings.Contains(event.Text, "secret") {
				t.Fatalf("unsafe source identity or redaction: %+v", event)
			}
			if event.Timestamp == "" || event.Cursor != "" {
				t.Fatalf("line resume state: %+v", event)
			}
			watermark, _ = time.Parse(time.RFC3339Nano, event.Timestamp)
		}
		if event.Event == "source_status" && event.PodUID == "pod-01" {
			statuses++
		}
	}
	if lines != 2 || statuses != 1 || follows.Load() != 0 || watermark.IsZero() {
		t.Fatalf("finite fan-in lines=%d status=%d follows=%d watermark=%s", lines, statuses, follows.Load(), watermark)
	}
	// A reconnect carries the viewer's timestamp, and the finite selection is
	// re-read from it rather than resumed from saved state.
	options.SinceTime = watermark
	events = nil
	if err := service.StreamWorkloadLogs(logStreamTestContext(), options, func(event WorkloadLogStreamEvent) error { events = append(events, event); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || follows.Load() != 0 {
		t.Fatalf("resumed finite fan-in events=%d follows=%d", len(events), follows.Load())
	}
	for _, event := range events {
		if event.Event == "line" && (event.Timestamp == "" || event.Cursor != "" || event.PodUID != "pod-00") {
			t.Fatalf("resumed line carried state or changed source: %+v", event)
		}
	}
	// The resume is a timestamp, so it cannot cross a workload, container, or
	// focus boundary the way a cursor used to. Only a timestamp beyond the
	// tolerated clock skew is refused; a slightly fast log clock resumes.
	options.SinceTime = time.Now().Add(time.Hour)
	if err := service.ValidateWorkloadLogStream(logStreamTestContext(), options); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("far future resume accepted: %v", err)
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

// A stale, incomplete, or unlisted inventory is a reason to wait. Reported as
// cluster_permission_denied it sends an operator to change RBAC that was never
// the cause, which is exactly what the founder hit.
func TestWorkloadLogInventoryWaitIsRetryableNotDenied(t *testing.T) {
	now := time.Now().UTC()
	options := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root"}
	records := workloadStreamRecords("Deployment", 1, 1)
	for _, test := range []struct {
		name   string
		status kubeindex.KindStatus
		want   string
	}{
		{name: "stale", status: kubeindex.KindStatus{State: "ready", ObservedAt: now.Add(-4*time.Minute - 30*time.Second), Records: 1}, want: "ReplicaSet inventory is 4 min old"},
		{name: "partial", status: kubeindex.KindStatus{State: "partial", Partial: true, Error: "record_rejected", Skipped: 1, ObservedAt: now}, want: "ReplicaSet inventory is under a minute old and incomplete"},
		{name: "unlisted", status: kubeindex.KindStatus{}, want: "ReplicaSet inventory has not been listed yet"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := kubeindex.Snapshot{Records: records, Kinds: map[string]kubeindex.KindStatus{
				"Deployment": {State: "ready", ObservedAt: now, Records: 1},
				"Pod":        {State: "ready", ObservedAt: now, Records: 1},
				"ReplicaSet": test.status,
			}}
			_, err := workloadLogPods(snapshot, options, true)
			if !errors.Is(err, ErrInventoryNotReady) {
				t.Fatalf("inventory wait error = %v", err)
			}
			if errors.Is(err, ErrForbidden) {
				t.Fatal("a freshness problem was reported as a permission problem")
			}
			detail := DiagnoseError(err)
			if detail.Code != "inventory_not_ready" || detail.Code == "cluster_permission_denied" || !detail.Retryable || !strings.Contains(detail.Message, test.want) {
				t.Fatalf("diagnosis = %+v", detail)
			}
			if hint := InventoryRetryAfter(err); hint <= 0 || hint > time.Minute {
				t.Fatalf("retry hint = %s", hint)
			}
		})
	}
	// A real cluster 403 keeps the permission answer and never becomes a wait.
	denied := &permissionDeniedError{Verb: "get", Resource: "pods/log", Namespace: "shop", Principal: "a service account"}
	if detail := DiagnoseError(denied); detail.Code != "cluster_permission_denied" || detail.Retryable || errors.Is(denied, ErrInventoryNotReady) {
		t.Fatalf("real denial diagnosis = %+v", detail)
	}
}

// A viewer that reconnects after a dropped stream resumes every source from its
// own timestamp, the way kubectl logs -f does. Nothing about the resume travels
// in the response, a reconnect never needs a request that grows with the number
// of sources, and no line is lost: the overlap the server re-reads is dropped by
// line identity.
func TestWorkloadStreamResumesAcrossReconnectsWithoutGapOrDuplicate(t *testing.T) {
	const pods, linesPerPod = 60, 4
	records := workloadStreamRecords("Deployment", pods, 1)
	base := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	stamps := make([]time.Time, linesPerPod)
	for line := range stamps {
		stamps[line] = base.Add(time.Duration(line) * time.Second)
	}
	var maxQuery, resumeReads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		for {
			size, previous := int64(len(request.URL.RawQuery)), maxQuery.Load()
			if size <= previous || maxQuery.CompareAndSwap(previous, size) {
				break
			}
		}
		if serveStreamPodObject(writer, request, records) {
			return
		}
		var since time.Time
		if raw := request.URL.Query().Get("sinceTime"); raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				t.Errorf("resume timestamp %q: %v", raw, err)
				return
			}
			since = parsed
			resumeReads.Add(1)
		} else if request.URL.Query().Get("sinceSeconds") == "" {
			t.Error("source read without a resume window")
		}
		name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/v1/namespaces/shop/pods/"), "/log")
		if name == "" || strings.Contains(name, "/") {
			t.Errorf("unexpected log path %q", request.URL.Path)
			return
		}
		for line, at := range stamps {
			if !since.IsZero() && at.Before(since) {
				continue
			}
			fmt.Fprintf(writer, "%s %s-line-%d\n", at.Format(time.RFC3339Nano), name, line)
		}
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, records)

	paused := errors.New("viewer reconnecting")
	view := map[string]string{}
	admitted, limited := 0, 0
	// The viewer resumes from the oldest line it has shown, because a source's
	// unread lines are always newer than that source's own last line and never
	// older than the slowest source's. That is what makes one timestamp a safe
	// resume point for every source at once.
	shown := map[string]time.Time{}
	watermark := func() time.Time {
		oldest := time.Time{}
		for _, at := range shown {
			if oldest.IsZero() || at.Before(oldest) {
				oldest = at
			}
		}
		return oldest
	}
	pauseAt := -1
	round, delivered := 0, 0
	collect := func(event WorkloadLogStreamEvent) error {
		if event.Event == "sources" {
			admitted, limited = 0, 0
			for _, pod := range event.Pods {
				if pod.Admitted {
					admitted++
				} else if pod.Reason == "source_limit" {
					limited++
				}
			}
		}
		if event.Event != "line" {
			return nil
		}
		if event.Cursor != "" {
			t.Fatalf("workload line carried resume state: %+v", event)
		}
		at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
		if err != nil {
			t.Fatalf("line timestamp: %v", err)
		}
		shown[event.SourceID] = at
		delivered++
		view[event.Pod+"|"+event.Timestamp] = event.Text
		round++
		if pauseAt > 0 && round == pauseAt {
			return paused
		}
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		options := WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root", Timestamps: true}
		if resumeFrom := watermark(); resumeFrom.IsZero() {
			options.SinceSeconds, options.TailLines = 3600, 500
		} else {
			options.SinceTime = resumeFrom
		}
		pauseAt = round + 5
		if err := service.StreamWorkloadLogs(logStreamTestContext(), options, collect); !errors.Is(err, paused) {
			t.Fatalf("round %d did not reconnect: %v", attempt, err)
		}
	}
	// Drain with the last watermark the viewer rendered and no pause.
	pauseAt = -1
	ctx, cancel := context.WithTimeout(logStreamTestContext(), 3*time.Second)
	defer cancel()
	if err := service.StreamWorkloadLogs(ctx, WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root", Timestamps: true, SinceTime: watermark()}, collect); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain=%v", err)
	}

	if admitted != MaxWorkloadLogPods || limited != pods-MaxWorkloadLogPods {
		t.Fatalf("admission reported %d admitted and %d limited of %d Pods", admitted, limited, pods)
	}
	// Every admitted source is complete after the client's own dedupe, and no
	// identity appeared twice with different content.
	if len(view) != admitted*linesPerPod {
		t.Fatalf("deduped lines=%d want %d", len(view), admitted*linesPerPod)
	}
	for pod := 0; pod < admitted; pod++ {
		name := fmt.Sprintf("api-%02d", pod)
		for line, at := range stamps {
			key := name + "|" + at.Format(time.RFC3339Nano)
			want := fmt.Sprintf("%s %s-line-%d", at.Format(time.RFC3339Nano), name, line)
			if view[key] != want {
				t.Fatalf("missing or changed line %s=%q want %q", key, view[key], want)
			}
		}
	}
	if maxQuery.Load() == 0 || maxQuery.Load() >= MaxStreamRequestQueryBytes {
		t.Fatalf("largest stream request query is %d bytes", maxQuery.Load())
	}
	if resumeReads.Load() == 0 {
		t.Fatal("no source resumed from the viewer timestamp")
	}
	t.Logf("%d Pods: %d admitted sources complete and deduped across 4 connections; %d lines delivered for %d unique; largest request query %d bytes; %d reads resumed by sinceTime",
		pods, admitted, delivered, len(view), maxQuery.Load(), resumeReads.Load())
}
