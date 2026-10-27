package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestUnknownResourceTimestampsAreOmittedButKnownTimesSurvive(t *testing.T) {
	unknown := []any{ProjectedResource{}, Condition{}, WorkloadPod{}, kubeindex.ContainerState{}}
	for _, value := range unknown {
		raw, err := json.Marshal(value)
		if err != nil || strings.Contains(string(raw), "0001-") || strings.Contains(string(raw), "_at") {
			t.Fatalf("unknown timestamp escaped: %s %v", raw, err)
		}
	}
	at := time.Now().UTC().Truncate(time.Second)
	known := []any{ProjectedResource{CreatedAt: at}, Condition{TransitionAt: at}, WorkloadPod{CreatedAt: at}, kubeindex.ContainerState{TerminatedAt: at}}
	for _, value := range known {
		raw, err := json.Marshal(value)
		if err != nil || !strings.Contains(string(raw), at.Format(time.RFC3339)) {
			t.Fatalf("known timestamp omitted: %s %v", raw, err)
		}
	}
}

func TestFocusedPodKeepsUnfilteredCapacityMetadata(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	records := workloadStreamRecords("StatefulSet", 25, 1)[1:]
	session := &WorkloadLogSession{service: service, options: WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", PodUIDs: []string{"pod-24"}}, workers: map[string]*workloadSourceWorker{}, resume: map[string]string{}}
	if err := session.reconcile(records); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, worker := range session.workers {
			worker.release()
		}
	}()
	if session.pods[0].Reason != "not_selected" || session.pods[0].AllPodsReason != "" || !session.pods[0].AllPodsAdmitted {
		t.Fatalf("focus mislabeled normal Pod: %+v", session.pods[0])
	}
	last := session.pods[24]
	if !last.Admitted || last.Reason != "" || last.AllPodsReason != "source_limit" || last.AllPodsAdmitted {
		t.Fatalf("focus erased All-pods safety limit: %+v", last)
	}
	session.options.Container = "missing"
	if err := session.reconcile(records); err != nil {
		t.Fatal(err)
	}
	for _, pod := range session.pods {
		if pod.AllPodsReason != "container_unavailable" {
			t.Fatal("missing container counted as capacity", pod)
		}
	}
	capacity := workloadAllPodsCapacity(records, "", 1)
	if capacity["pod-00"] != "" || capacity["pod-01"] != "source_limit" {
		t.Fatal("process-source budget not represented in unfiltered capacity", capacity)
	}
}

func TestWorkloadInitialTailWaitsForConsumerWithoutDroppingSources(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(strconv.FormatBool(previous), func(t *testing.T) {
			records := workloadStreamRecords("StatefulSet", 20, 1)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if serveStreamPodObject(writer, request, records) {
					return
				}
				for i := 0; i < 500; i++ {
					fmt.Fprintf(writer, "2026-10-01T00:00:00Z %d\n", i)
				}
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
			service.SetScrubber(replacingScrubber{})
			seedWorkloadStreamIndex(t, service, records)
			seen := make(map[string]bool)
			stop := errors.New("viewer received initial tail")
			count, statuses := 0, 0
			err := service.StreamWorkloadLogs(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Previous: previous, TailLines: 500}, func(event WorkloadLogStreamEvent) error {
				if event.Event == "source_status" {
					statuses++
				}
				if event.Event != "line" {
					return nil
				}
				count++
				key := event.PodUID + "/" + event.Text
				if seen[key] {
					t.Fatalf("duplicate tail line %s", key)
				}
				seen[key] = true
				if count == 1 {
					time.Sleep(30 * time.Millisecond)
				}
				if count%16 == 0 {
					time.Sleep(100 * time.Microsecond)
				}
				if count == 10000 && !previous {
					return stop
				}
				return nil
			})
			if previous && err != nil || !previous && !errors.Is(err, stop) || count != 10000 || statuses != 0 {
				t.Fatalf("normal tail cut count=%d errors=%d err=%v", count, statuses, err)
			}
		})
	}
}

func TestPreviousWorkloadMissingSourcesStillCompletes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, workloadStreamRecords("StatefulSet", 2, 1))
	ctx, cancel := context.WithTimeout(logStreamTestContext(), time.Second)
	defer cancel()
	var final WorkloadLogStreamEvent
	if err := service.StreamWorkloadLogs(ctx, WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Previous: true}, func(event WorkloadLogStreamEvent) error { final = event; return nil }); err != nil {
		t.Fatal("missing previous source prevented finite completion", err)
	}
	if final.Event != "end" || final.Reason != "complete" {
		t.Fatalf("finite missing source did not end: %+v", final)
	}
}

func TestWorkloadReopenValidatesLiveUIDAndIncarnation(t *testing.T) {
	for _, change := range []string{"uid", "incarnation", "deleted", "same"} {
		t.Run(change, func(t *testing.T) {
			records := workloadStreamRecords("StatefulSet", 1, 1)
			pod := records[1]
			var gets, logs atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasSuffix(request.URL.Path, "/log") {
					logs.Add(1)
					fmt.Fprintln(writer, "2026-10-01T00:00:00Z valid")
					return
				}
				gets.Add(1)
				if change == "deleted" {
					writer.WriteHeader(404)
					return
				}
				uid := pod.UID
				restarts := 0
				if change == "uid" {
					uid = "replacement"
				}
				if change == "incarnation" {
					restarts = 1
				}
				writeJSON(writer, map[string]any{"kind": "Pod", "metadata": map[string]any{"namespace": "shop", "name": pod.Name, "uid": uid, "ownerReferences": []any{map[string]any{"kind": "StatefulSet", "name": "api", "uid": "root", "controller": true}}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app-0"}}}, "status": map[string]any{"containerStatuses": []any{map[string]any{"name": "app-0", "restartCount": restarts, "state": map[string]any{"running": map[string]any{"startedAt": pod.LogContainers[0].StartedAt.Format(time.RFC3339Nano)}}}}}})
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
			for _, worker := range session.workers {
				worker.started, worker.finished = true, true
				worker.cancel = func() {}
				worker.release()
				close(worker.done)
			}
			if err := session.reconcile(records[1:]); err != nil {
				t.Fatal(err)
			}
			session.startWorkers()
			select {
			case message := <-session.queue:
				if change == "same" {
					if message.event.Event != "line" || logs.Load() != 1 {
						t.Fatalf("validated source not reopened %+v", message)
					}
				} else if !message.finished || !errors.Is(message.err, errWorkloadSourceGone) || logs.Load() != 0 {
					t.Fatalf("replacement read under old identity: %+v logs=%d", message, logs.Load())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("source validation did not finish")
			}
			session.cancel()
			for _, worker := range session.workers {
				<-worker.done
			}
			if gets.Load() != 1 {
				t.Fatal("reopening did not validate live Pod UID")
			}
		})
	}
}

func TestConfirmedGoneWorkloadSourceStaysQuietUntilMembershipChanges(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	record := workloadStreamRecords("StatefulSet", 1, 1)[1]
	options := WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root"}
	session := &WorkloadLogSession{service: service, options: options, workers: map[string]*workloadSourceWorker{}, resume: map[string]string{}}
	if err := session.reconcile([]kubeindex.Record{record}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, worker := range session.workers {
			worker.release()
		}
	}()
	var id string
	for key, worker := range session.workers {
		id = key
		worker.started, worker.finished, worker.terminal = true, true, true
	}
	old := session.workers[id]
	for i := 0; i < 6; i++ {
		if err := session.reconcile([]kubeindex.Record{record}); err != nil {
			t.Fatal(err)
		}
	}
	if session.workers[id] != old {
		t.Fatal("confirmed gone source reopened from stale index")
	}
}

// A viewer that reconnects with a timestamp gives up nothing: every source
// starts at that timestamp backed off by the replay overlap, and a viewer that
// resumes with window bounds gets exactly those bounds.
func TestWorkloadResumeStartsEverySourceAtTheViewerWatermark(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, workloadStreamRecords("Deployment", 2, 1))

	watermark := time.Now().UTC().Add(-time.Minute)
	resumed, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root", SinceTime: watermark})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.uncertain || len(resumed.workers) != 2 {
		t.Fatalf("timestamp resume unsure=%v or incomplete=%d", resumed.uncertain, len(resumed.workers))
	}
	for _, worker := range resumed.workers {
		if !worker.options.SinceTime.Equal(watermark.Add(-sinceTimeOverlap)) || worker.options.SinceSeconds != 0 || worker.options.TailLines != 0 || worker.options.Cursor != "" {
			t.Fatalf("source did not resume at the watermark: %+v", worker.options)
		}
	}

	windowed, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "Deployment", Name: "api", Namespace: "shop", UID: "root", SinceSeconds: 60, TailLines: 123})
	if err != nil {
		t.Fatal(err)
	}
	defer windowed.Close()
	if windowed.uncertain || len(windowed.workers) != 2 {
		t.Fatalf("windowed open unsure=%v", windowed.uncertain)
	}
	for _, worker := range windowed.workers {
		if worker.options.SinceSeconds != 60 || worker.options.TailLines != 123 || !worker.options.SinceTime.IsZero() {
			t.Fatalf("window bounds lost: %+v", worker.options)
		}
	}
}

func TestWorkloadMembershipOnlyEmitsChanges(t *testing.T) {
	session := &WorkloadLogSession{workers: map[string]*workloadSourceWorker{"one": {source: WorkloadLogSource{SourceID: "one", PodUID: "pod", Admitted: true}}}, pods: []WorkloadLogPod{{UID: "pod", Admitted: true}}}
	if _, changed := session.membershipChange(); !changed {
		t.Fatal("missing initial catalog")
	}
	for i := 0; i < 100; i++ {
		if _, changed := session.membershipChange(); changed {
			t.Fatal("unchanged catalog resent")
		}
	}
	session.pods[0].Reason = "source_limit"
	if _, changed := session.membershipChange(); !changed {
		t.Fatal("changed admission catalog suppressed")
	}
}

func TestIdlePodRotationReturnsBoundedQuietResumeCursor(t *testing.T) {
	queries := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries <- request.URL.RawQuery
		writer.WriteHeader(200)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	options := PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "app", SinceSeconds: 60, TailLines: 123}
	var events []PodLogStreamEvent
	collect := func(event PodLogStreamEvent) error { events = append(events, event); return nil }
	if err := service.streamPodLogs(context.Background(), options, collect, 30*time.Millisecond, time.Hour, time.Hour, 8192); err != nil {
		t.Fatal(err)
	}
	<-queries
	if len(events) != 2 || events[0].Event != "limit" || events[0].Cursor == "" || events[1].Cursor != events[0].Cursor {
		t.Fatalf("idle rotation cursor missing: %+v", events)
	}
	options.Cursor, options.SinceSeconds, options.TailLines = events[0].Cursor, 0, 0
	cursor, err := service.logCursor(options)
	if err != nil || !cursor.Quiet || cursor.SinceSeconds != 60 || cursor.TailLines != 123 {
		t.Fatalf("quiet bounds lost: %+v %v", cursor, err)
	}
	events = nil
	if err := service.streamPodLogs(context.Background(), options, collect, 30*time.Millisecond, time.Hour, time.Hour, 8192); err != nil {
		t.Fatal(err)
	}
	query := <-queries
	if !strings.Contains(query, "sinceTime=") || !strings.Contains(query, "tailLines=123") || events[0].ReplayUncertain {
		t.Fatalf("quiet rotation changed read bounds %s %+v", query, events)
	}
}

func TestReplayUncertaintyIsAnOccurrenceNotStickyState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintln(writer, strings.Repeat("x", MaxLogStreamLineBytes+100))
		fmt.Fprintln(writer, "2026-10-01T00:00:00Z ordinary")
		fmt.Fprintln(writer, strings.Repeat("y", MaxLogStreamLineBytes+100))
		fmt.Fprintln(writer, "2026-10-01T00:00:01Z ordinary again")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	var flags []bool
	if err := service.StreamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "shop", Pod: "api", Previous: true}, func(event PodLogStreamEvent) error { flags = append(flags, event.ReplayUncertain); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(flags) != 5 || !flags[0] || flags[1] || !flags[2] || flags[3] || flags[4] {
		t.Fatalf("gap notice leaked into ordinary events: %v", flags)
	}
	session := &WorkloadLogSession{ctx: context.Background(), cancel: func() {}, release: func() {}, options: WorkloadLogStreamOptions{UID: "root", Previous: true}, workers: map[string]*workloadSourceWorker{}, resume: map[string]string{}, uncertain: true}
	flags = nil
	if err := session.Stream(func(event WorkloadLogStreamEvent) error { flags = append(flags, event.ReplayUncertain); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(flags) != 2 || !flags[0] || flags[1] {
		t.Fatalf("workload inherited gap forever: %v", flags)
	}
}

func TestInventoryCorePublicationDoesNotWaitForUsageCollection(t *testing.T) {
	metrics := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/apis/metrics.k8s.io/") {
			once.Do(func() { close(metrics) })
			<-r.Context().Done()
			return
		}
		writeJSON(w, map[string]any{"items": []any{}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	seedInventoryDiscovery(service)
	entry := service.cache[service.cacheKey()]
	entry.discovery.Resources = append(entry.discovery.Resources, ResourceDefinition{ID: "metrics.k8s.io~v1beta1~pods", Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods", Kind: "PodMetrics", Namespaced: true, Available: true})
	service.cache[service.cacheKey()] = entry
	service.SetChangeStorage(storage.NewMemory())
	service.startInventory(t.Context())
	defer service.StopCapture()
	select {
	case <-metrics:
	case <-time.After(time.Second):
		t.Fatal("usage collection did not start")
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		_, status, err := service.CachedIndexSnapshot(t.Context(), "Pod")
		if err == nil && status.State == "ready" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed inventory remained blocked behind usage: %+v %v", status, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCaptureProgressDoesNotWaitForMetricsSampler(t *testing.T) {
	metricsStarted := make(chan struct{})
	podRead := make(chan struct{}, 1)
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/apis/metrics.k8s.io/") {
			once.Do(func() { close(metricsStarted) })
			<-request.Context().Done()
			return
		}
		if request.URL.Path == "/api/v1/pods" {
			select {
			case podRead <- struct{}{}:
			default:
			}
			writeJSON(writer, map[string]any{"items": []any{}})
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	now := time.Now().UTC()
	service.cache[service.cacheKey()] = discoveryCacheEntry{expires: now.Add(time.Hour), discovery: Discovery{Resources: []ResourceDefinition{
		{ID: "metrics.k8s.io~v1beta1~pods", Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods", Kind: "PodMetrics", Namespaced: true, Available: true, Verbs: []string{"get", "list"}},
		{ID: "core~v1~pods", Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true, Available: true, Verbs: []string{"get", "list"}},
	}}}
	records := workloadStreamRecords("StatefulSet", 1, 1)
	seedWorkloadStreamIndex(t, service, records)
	index, _ := service.indexes.ForScope(service.indexScope())
	if err := index.Refresh(context.Background(), "Pod", 0, now.Add(-time.Minute), func(context.Context) ([]kubeindex.Record, bool, error) { return records[1:], true, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UsageHistory(logStreamTestContext(), "shop", "api-00"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.stopUsageInterestWorker)
	waitUsageInterestCount(t, service, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.startInventory(ctx)
	loop := &captureLoop{ctx: ctx, owner: "sampler-owner", next: map[string]time.Time{}, success: map[string]time.Time{}, completed: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: CaptureStatus{Mode: "continuous"}}
	for kind := range captureCadences {
		loop.next[kind] = now.Add(time.Hour)
	}
	service.changes.captures[service.indexScope()] = loop
	store := service.changeStore()
	if err := store.ClaimLease(loop.owner, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.captureOwned(loop, store, time.Minute, 20*time.Millisecond, now.Add(time.Minute), ticks)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owner sampler did not join on cancellation")
		}
		service.changes.mu.Lock()
		inventory := service.changes.inventories[service.indexScope()]
		service.changes.mu.Unlock()
		select {
		case <-inventory.done:
		case <-time.After(3 * time.Second):
			t.Error("inventory sampler did not join")
		}
	}()
	select {
	case <-metricsStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("metrics sampler never started")
	}
	deadline := time.Now().Add(time.Second)
	for {
		view, _, err := service.CachedIndexSnapshot(context.WithValue(t.Context(), cachedCaptureContextKey{}, true), "Pod")
		if err == nil && !view.Kinds["Pod"].ObservedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("inventory publication waited for slow usage")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-podRead:
	default:
	}
	loop.mu.Lock()
	loop.next["Pod"] = now.Add(-time.Second)
	loop.mu.Unlock()
	ticks <- time.Now()
	deadline = time.Now().Add(time.Second)
	for {
		loop.mu.Lock()
		completed := !loop.completed["Pod"].IsZero()
		loop.mu.Unlock()
		if completed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slow metrics API blocked cached capture coverage")
		}
		time.Sleep(time.Millisecond)
	}
	page, err := store.ReadWindow(now.Add(-time.Minute), time.Now().UTC(), time.Now().UTC())
	if err != nil || page.Coverage["Pod"].To.IsZero() {
		t.Fatalf("main capture did not commit coverage: %+v %v", page, err)
	}
	select {
	case <-podRead:
		t.Fatal("cached capture duplicated upstream inventory read")
	default:
	}
}

func TestUsageBridgeManifestAdmissionBoundsStoredSnapshots(t *testing.T) {
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	now := time.Now().UTC()
	updates := make([]usageInterest, 0, usageHistoryPods)
	for i := 0; i < usageHistoryPods; i++ {
		updates = append(updates, usageInterest{Namespace: "shop", Pod: "api-" + strconv.Itoa(i), UID: "pod-" + strconv.Itoa(i), Opened: now, Touched: now})
	}
	if err := service.updateUsageManifest(context.Background(), provider, updates, now); err != nil {
		t.Fatal(err)
	}
	service.publishUsageSamples(context.Background(), map[string]usagePodHistory{"shop/extra": {uid: "extra", since: now, opened: now, samples: []UsageSample{{Timestamp: now, CPUMilli: 1}}}}, now)
	raw, err := provider.ReadBlob(usageRecordKey(service.cacheKey(), "shop", "extra"))
	if err != nil || len(raw) != 0 {
		t.Fatal("unadmitted snapshot escaped manifest bound", err)
	}
	entries, _, err := readUsageManifest(provider, usageBridgePrefix(service.cacheKey())+"interest")
	if err != nil || len(entries) != usageHistoryPods {
		t.Fatal("manifest Pod bound changed", err)
	}
}

func TestUsageBridgeFailoverRetainsPriorOwnerSamples(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		owner := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
		owner.SetChangeStorage(provider)
		at := now.Add(time.Duration(i-1) * time.Minute)
		owner.publishUsageSamples(context.Background(), map[string]usagePodHistory{"shop/api": {uid: "pod", since: at, opened: now, samples: []UsageSample{{Timestamp: at, CPUMilli: float64(i + 1)}}}}, now)
	}
	record, _, err := readUsageBridgeRecord(provider, "default\x00cluster\x00", "shop", "api", now)
	if err != nil || len(record.Samples) != 2 || record.Samples[0].CPUMilli != 1 || record.Samples[1].CPUMilli != 2 {
		t.Fatalf("failover overwrote history: %+v %v", record, err)
	}
}

func TestUsageHistoryBridgeSamplesHealthyPodOpenedOnAnotherReplica(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writeJSON(writer, map[string]any{"items": []any{map[string]any{"kind": "PodMetrics", "metadata": map[string]any{"namespace": "shop", "name": "api-00"}, "timestamp": now.Format(time.RFC3339Nano), "window": "30s", "containers": []any{map[string]any{"usage": map[string]any{"cpu": "250m", "memory": "64Mi"}}}}}})
	}))
	defer server.Close()
	provider := storage.NewMemory()
	owner := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	viewer := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	for _, service := range []*Service{owner, viewer} {
		service.SetChangeStorage(provider)
		t.Cleanup(service.StopCapture)
		service.cache[service.cacheKey()] = discoveryCacheEntry{expires: now.Add(time.Hour), discovery: Discovery{Resources: []ResourceDefinition{{ID: "metrics.k8s.io~v1beta1~pods", Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods", Kind: "PodMetrics", Namespaced: true, Available: true, Verbs: []string{"get", "list"}}}}}
		seedWorkloadStreamIndex(t, service, workloadStreamRecords("StatefulSet", 1, 1))
	}
	if owner.usageHistory == viewer.usageHistory {
		t.Fatal("test did not model independent replica memory")
	}
	if _, err := viewer.UsageHistory(logStreamTestContext(), "shop", "api-00"); err != nil {
		t.Fatal(err)
	}
	waitUsageInterestCount(t, viewer, 1)
	owner.sampleUsageHistory(context.Background(), now)
	history, err := viewer.UsageHistory(logStreamTestContext(), "shop", "api-00")
	deadline := time.Now().Add(2 * time.Second)
	for err == nil && len(history.Samples) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		history, err = viewer.UsageHistory(logStreamTestContext(), "shop", "api-00")
	}
	if err != nil || calls.Load() != 1 || len(history.Samples) != 1 || history.Samples[0].CPUMilli != 250 {
		t.Fatalf("non-owner drawer never got owner samples: %+v calls=%d err=%v", history, calls.Load(), err)
	}
	foreign := viewer.Scoped(Scope{OrgID: "foreign", ClusterID: "cluster"})
	t.Cleanup(foreign.StopCapture)
	denied, err := foreign.UsageHistory(logStreamTestContext(), "shop", "api-00")
	if err != nil || len(denied.Samples) != 0 {
		t.Fatal("bridge crossed an untrusted org")
	}
}
