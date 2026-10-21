package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"golang.org/x/time/rate"
)

type breakLogScrubber struct{}

func (breakLogScrubber) Scrub(value string) string {
	return strings.ReplaceAll(value, "MESSAGE_CANARY", "[redacted]")
}

func breakLogFixture(now time.Time, pods, containers int) kubeindex.Snapshot {
	records := []kubeindex.Record{{Kind: "Deployment", APIVersion: "apps/v1", UID: "root", Name: "api", Namespace: "shop", Replicas: int32(pods), Available: 0, Summary: map[string]string{"available_replicas": "0"}}}
	for position := 0; position < pods; position++ {
		pod := kubeindex.Record{Kind: "Pod", APIVersion: "v1", UID: fmt.Sprintf("pod-%d", position), Name: fmt.Sprintf("api-%d", position), Namespace: "shop", CreatedAt: now.Add(time.Duration(position) * time.Second), Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: "root", Name: "api", Namespace: "shop", Controller: true, ControllerKnown: true}}}
		for container := 0; container < containers; container++ {
			pod.Containers = append(pod.Containers, kubeindex.ContainerState{Name: fmt.Sprintf("app-%d", container), Init: container == 1, RestartCount: 2, WaitingReason: "CrashLoopBackOff", TerminatedAt: now.Add(-time.Minute), TerminatedReason: "Error", ExitCode: 1})
		}
		records = append(records, pod)
	}
	return kubeindex.Snapshot{Records: records, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}
}

func TestBreakLogCaptureBoundsDedupNewTerminationAndDeletedPersistence(t *testing.T) {
	now := time.Now().UTC()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/log") {
			reads.Add(1)
			query := request.URL.Query()
			if query.Get("previous") != "true" || query.Get("timestamps") != "true" || query.Get("tailLines") != "500" || query.Get("limitBytes") != "65536" || query.Has("sinceSeconds") {
				t.Errorf("query: %v", query)
			}
			fmt.Fprintf(writer, "%s ERROR MESSAGE_CANARY\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
			return
		}
		switch request.URL.Path {
		case "/api":
			writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		case "/apis":
			writeJSON(writer, map[string]any{"groups": []any{}})
		case "/api/v1":
			writeJSON(writer, map[string]any{"resources": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	provider := storage.NewMemory()
	service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(provider)
	service.client.indexLimiter = rate.NewLimiter(rate.Inf, 100)
	snapshot := breakLogFixture(now, 6, 4)
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != 15 {
		t.Fatalf("reads=%d want 15", reads.Load())
	}
	state, err := service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Snapshots) != 15 || len(state.Gaps) == 0 {
		t.Fatal("capture", len(state.Snapshots), err)
	}
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), "MESSAGE_CANARY") {
		t.Fatal("stored canary")
	}
	afterRestart := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	afterRestart.SetScrubber(breakLogScrubber{})
	afterRestart.SetChangeStorage(provider)
	defer afterRestart.StopCapture()
	warming := breakLogFixture(now, 6, 4)
	warming.Records = warming.Records[1:]
	for position := range warming.Records {
		warming.Records[position].Owners = []kubeindex.OwnerRef{{Kind: "ReplicaSet", UID: "replica", Name: "api-replica", Namespace: "shop", Controller: true, ControllerKnown: true}}
	}
	afterRestart.captureBreakLogs(t.Context(), warming, "")
	warming.Records = append(warming.Records, kubeindex.Record{Kind: "ReplicaSet", UID: "replica", Name: "api-replica", Namespace: "shop", Owners: []kubeindex.OwnerRef{{Kind: "Deployment", UID: "root", Name: "api", Namespace: "shop", Controller: true, ControllerKnown: true}}})
	afterRestart.captureBreakLogs(t.Context(), warming, "")
	warming.Records = append(warming.Records, snapshot.Records[0])
	afterRestart.captureBreakLogs(t.Context(), warming, "")
	if reads.Load() != 15 {
		t.Fatalf("warming restart repeated capped reads: %d", reads.Load())
	}
	state, err = afterRestart.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Jobs) != 1 {
		t.Fatalf("warming index persisted transient jobs: %+v err=%v", state.Jobs, err)
	}
	for _, gap := range state.Gaps {
		if gap.RootUID != "root" {
			t.Fatalf("warming index persisted wrong-root gap: %+v", gap)
		}
	}
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != 15 {
		t.Fatalf("unchanged capped job should not retry, reads=%d", reads.Load())
	}
	snapshot = breakLogFixture(now, 1, 1)
	service.captureBreakLogs(t.Context(), snapshot, "")
	before := reads.Load()
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != before {
		t.Fatal("identical trigger reread")
	}
	snapshot.Records[1].Containers[0].RestartCount++
	snapshot.Records[1].Containers[0].TerminatedAt = now.Add(-30 * time.Second)
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != before+1 {
		t.Fatal("new termination not captured")
	}
	restarted := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
	restarted.SetChangeStorage(provider)
	restarted.SetScrubber(breakLogScrubber{})
	page, err := restarted.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", From: now.Add(-2 * time.Minute), To: now, Limit: 500})
	if err != nil || len(page.Lines) == 0 {
		t.Fatal("deleted persistence", err)
	}
	encoded, _ = json.Marshal(page)
	if strings.Contains(string(encoded), "MESSAGE_CANARY") {
		t.Fatal("returned canary")
	}
}

func TestBreakLogCaptureOwnerKindCoverage(t *testing.T) {
	for _, test := range []struct {
		name         string
		kind         string
		intermediate string
		status       kubeindex.KindStatus
		capture      bool
	}{
		{name: "custom rollout", kind: "Rollout", capture: true},
		{name: "rollout replica set", kind: "Rollout", intermediate: "ReplicaSet", capture: true},
		{name: "operator stateful set", kind: "Database", intermediate: "StatefulSet", capture: true},
		{name: "cloud native postgres", kind: "Cluster", capture: true},
		{name: "forbidden owner list", kind: "StatefulSet", status: kubeindex.KindStatus{State: "error", Error: "forbidden", Partial: true}, capture: true},
		{name: "complete absent owner", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC()}, capture: true},
		{name: "complete stale owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready", ObservedAt: time.Now().UTC().Add(-time.Hour)}},
		{name: "complete owner list unknown freshness", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready"}},
		{name: "unstarted owner list", kind: "ReplicaSet"},
		{name: "warming owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "warming"}},
		{name: "incomplete owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "partial", Partial: true}},
		{name: "partial ready owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "ready", Partial: true}},
		{name: "failed owner list", kind: "ReplicaSet", status: kubeindex.KindStatus{State: "error", Error: "timeout", Partial: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if !strings.HasSuffix(request.URL.Path, "/log") {
					http.NotFound(writer, request)
					return
				}
				reads.Add(1)
				fmt.Fprintf(writer, "%s failure\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "org", ClusterID: "cluster"})
			service.SetScrubber(breakLogScrubber{})
			service.SetChangeStorage(storage.NewMemory())
			defer service.StopCapture()
			snapshot := breakLogFixture(now, 1, 1)
			snapshot.Records = snapshot.Records[1:]
			snapshot.Kinds["Pod"] = kubeindex.KindStatus{State: "ready", CompletedAt: now}
			snapshot.Records[0].OwnerObservedAt = now
			owner := kubeindex.OwnerRef{Kind: test.kind, UID: "root", Name: "api", Namespace: "shop", Controller: true, ControllerKnown: true}
			snapshot.Records[0].Owners = []kubeindex.OwnerRef{owner}
			if test.intermediate != "" {
				snapshot.Records[0].Owners = []kubeindex.OwnerRef{{Kind: test.intermediate, UID: "intermediate", Name: "api-child", Namespace: "shop", Controller: true, ControllerKnown: true}}
				snapshot.Records = append(snapshot.Records, kubeindex.Record{Kind: test.intermediate, UID: "intermediate", Name: "api-child", Namespace: "shop", Owners: []kubeindex.OwnerRef{owner}})
				snapshot.Kinds[test.intermediate] = kubeindex.KindStatus{State: "ready"}
			}
			if test.status.State != "" {
				if test.capture && test.status.State == "ready" {
					test.status.ObservedAt = now.Add(time.Second)
				}
				snapshot.Kinds[test.kind] = test.status
			}
			service.captureBreakLogs(t.Context(), snapshot, "")
			if !test.capture {
				state, err := service.changeStore().BreakLogs(now)
				if err != nil || reads.Load() != 0 || len(state.Jobs) != 0 || len(state.Snapshots) != 0 || len(state.Gaps) != 0 {
					t.Fatalf("incomplete warmup captured evidence: reads=%d state=%+v err=%v", reads.Load(), state, err)
				}
				snapshot.Kinds[test.kind] = kubeindex.KindStatus{State: "ready", ObservedAt: now.Add(time.Second)}
				service.captureBreakLogs(t.Context(), snapshot, "")
			}
			service.captureBreakLogs(t.Context(), snapshot, "")
			state, err := service.changeStore().BreakLogs(time.Now().UTC())
			if err != nil || reads.Load() != 1 || len(state.Jobs) != 1 || len(state.Snapshots) != 1 || len(state.Gaps) != 0 {
				t.Fatalf("owner coverage capture: reads=%d state=%+v err=%v", reads.Load(), state, err)
			}
			if state.Snapshots[0].RootUID != "root" {
				t.Fatalf("wrong root: %+v", state.Snapshots[0])
			}
		})
	}
}

func TestBreakLogCaptureLostLeaseAndCredentialFence(t *testing.T) {
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { reads.Add(1) }))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.SetScrubber(breakLogScrubber{})
	now := time.Now().UTC()
	if err := service.changeStore().ClaimLease("current", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	service.captureBreakLogs(t.Context(), breakLogFixture(now, 1, 1), "stale")
	if reads.Load() != 0 {
		t.Fatal("lost lease read upstream")
	}
	service.identity.cancel()
	service.captureBreakLogs(context.Background(), breakLogFixture(now, 1, 1), "current")
	if reads.Load() != 0 {
		t.Fatal("retired credential read upstream")
	}
}

func TestBreakLogCaptureRetriesTransientBudgetWithoutRepeatingFinalCaps(t *testing.T) {
	now := time.Now().UTC()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/log") {
			switch request.URL.Path {
			case "/api":
				writeJSON(writer, map[string]any{"versions": []string{"v1"}})
			case "/apis":
				writeJSON(writer, map[string]any{"groups": []any{}})
			case "/api/v1":
				writeJSON(writer, map[string]any{"resources": []any{}})
			default:
				http.NotFound(writer, request)
			}
			return
		}
		if reads.Add(1) == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprintf(writer, "%s safe\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.SetScrubber(breakLogScrubber{})
	service.client.indexLimiter = rate.NewLimiter(rate.Inf, 100)
	snapshot := breakLogFixture(now, 1, 1)
	service.captureBreakLogs(t.Context(), snapshot, "")
	state, err := service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Jobs) != 0 || len(state.Gaps) != 1 || state.Gaps[0].Reason != "budget" {
		t.Fatalf("transient budget marked complete: %+v err=%v", state, err)
	}
	service.client.backoffMu.Lock()
	service.client.backoffUntil = time.Time{}
	service.client.backoffMu.Unlock()
	service.captureBreakLogs(t.Context(), snapshot, "")
	service.captureBreakLogs(t.Context(), snapshot, "")
	if reads.Load() != 2 {
		t.Fatalf("transient job should retry once then deduplicate: %d", reads.Load())
	}
	state, err = service.changeStore().BreakLogs(time.Now().UTC())
	if err != nil || len(state.Snapshots) != 1 || len(state.Gaps) != 0 {
		t.Fatalf("successful retry retained budget failure: %+v err=%v", state, err)
	}
	item := state.Snapshots[0]
	page, err := service.BreakLogs(t.Context(), breaklogs.Query{Namespace: "shop", RootUID: "root", Mode: "previous", From: item.From, To: item.To})
	if err != nil || page.Coverage.State != "complete" || page.Coverage.Tip || page.Coverage.Reason != "" || len(page.Coverage.Gaps) != 0 {
		t.Fatalf("successful retry API retained stale budget tip: %+v err=%v", page.Coverage, err)
	}
}

func TestBreakLogTruncatedReadKeepsOnlyObservedEvidence(t *testing.T) {
	now := time.Now().UTC()
	first := now.Add(-2 * time.Minute)
	last := now.Add(-time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, "%s %s\n%s safe\n", first.Format(time.RFC3339Nano), strings.Repeat("x", 4097), last.Format(time.RFC3339Nano))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api"}, "root", kubeindex.ContainerState{Name: "app"}, "current", now.Add(-5*time.Minute), now)
	if gap != nil || !item.Truncated || !item.From.Equal(first) || !item.To.Equal(last) || len(item.Lines) != 2 {
		t.Fatalf("truncated read overclaimed evidence: %+v gap=%+v", item, gap)
	}
}

func TestBreakLogPodPriorityAndCurrentBounds(t *testing.T) {
	now := time.Now().UTC()
	snapshot := breakLogFixture(now, 3, 1)
	snapshot.Records[1].Containers = nil
	snapshot.Records[1].ReadyStatus = "True"
	snapshot.Records[2].Containers = nil
	snapshot.Records[2].ReadyStatus = "False"
	pods := breakLogPods(snapshot.Records, "shop", "root")
	if len(pods) != 3 || pods[0].UID != "pod-2" || pods[1].UID != "pod-1" {
		t.Fatal("priority", pods)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("sinceTime") == "" || request.URL.Query().Has("sinceSeconds") {
			t.Error("missing exact current window")
		}
		fmt.Fprintf(writer, "%s info safe\n", now.Add(-time.Minute).Format(time.RFC3339Nano))
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	item, gap := service.readBreakContainer(t.Context(), pods[0], "root", kubeindex.ContainerState{Name: "app"}, "current", now.Add(-5*time.Minute), now)
	if gap != nil || len(item.Lines) != 1 || item.Slot != "current" || !item.From.Equal(now.Add(-5*time.Minute)) || !item.To.Equal(now) || !item.PodCreatedAt.Equal(pods[0].CreatedAt) {
		t.Fatal("current", gap)
	}
}

func TestBreakLogEmptyReadPreservesObservedWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetScrubber(breakLogScrubber{})
	now := time.Now().UTC()
	from := now.Add(-5 * time.Minute)
	item, gap := service.readBreakContainer(t.Context(), kubeindex.Record{Namespace: "shop", Name: "api"}, "root", kubeindex.ContainerState{Name: "app"}, "current", from, now)
	if gap != nil || item.Truncated || len(item.Lines) != 0 || !item.From.Equal(from) || !item.To.Equal(now) {
		t.Fatalf("empty read lost observed window: %+v gap=%+v", item, gap)
	}
}

func TestBreakLogCaptureDualSlotAndStopDrainsJob(t *testing.T) {
	now := time.Now().UTC()
	var previous, current atomic.Int32
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("previous") == "true" {
			previous.Add(1)
		} else {
			current.Add(1)
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.SetScrubber(breakLogScrubber{})
	snapshot := breakLogFixture(now, 1, 1)
	snapshot.Records[1].Containers[0].WaitingReason = ""
	snapshot.Records[1].Containers[0].Running = true
	snapshot.Records[1].Containers[0].StateKnown = true
	service.enqueueBreakLogs(snapshot, "", t.Context())
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job not started")
	}
	done := make(chan struct{})
	go func() { service.StopCapture(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop failed to drain")
	}
	if previous.Load() != 1 || current.Load() != 0 {
		t.Fatal("cancellation failed")
	}
	fast := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("previous") == "true" {
			previous.Add(1)
		} else {
			current.Add(1)
		}
		fmt.Fprintf(writer, "%s info safe\n", now.Add(-70*time.Second).Format(time.RFC3339Nano))
	}))
	defer fast.Close()
	other := newTestService(t, fast.URL, Scope{ClusterID: "cluster"})
	other.SetChangeStorage(storage.NewMemory())
	other.SetScrubber(breakLogScrubber{})
	other.client.indexLimiter = rate.NewLimiter(rate.Inf, 100)
	other.captureBreakLogs(t.Context(), snapshot, "")
	if previous.Load() != 2 || current.Load() != 1 {
		t.Fatal("running unready container did not capture both slots")
	}
}
