//go:build integration

package kubernetes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

// A real local protocol fixture: native fakekube owns discovery and objects;
// test-only log/metrics shapes add burst, idle and blocked-provider behavior.
// Clients use normal HTTP, not an intercepted or replaced RoundTripper.
type nativeResourceBackend struct {
	url     string
	active  atomic.Int32
	opened  atomic.Int32
	metrics atomic.Int32
	mu      sync.Mutex
	queries []url.Values
	lines   int
	padding int
	records []kubeindex.Record
	at      time.Time
	block   bool
	started chan struct{}
	once    sync.Once
}

func nativeResourceService(t *testing.T, backend *nativeResourceBackend) *Service {
	t.Helper()
	api, err := fakekube.NewServer(fakekube.Config{Scenario: "fleet-east"})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range backend.records {
		resource := map[string]string{"StatefulSet": "statefulsets", "Deployment": "deployments", "ReplicaSet": "replicasets", "Pod": "pods"}[record.Kind]
		if resource == "" {
			continue
		}
		owners := []any{}
		for _, owner := range record.Owners {
			owners = append(owners, map[string]any{"kind": owner.Kind, "name": owner.Name, "uid": owner.UID, "controller": true})
		}
		containers, statuses := []any{}, []any{}
		for _, container := range record.LogContainers {
			containers = append(containers, map[string]any{"name": container.Name})
			statuses = append(statuses, map[string]any{"name": container.Name, "restartCount": container.RestartCount,
				"state": map[string]any{"running": map[string]any{"startedAt": container.StartedAt.Format(time.RFC3339Nano)}}})
		}
		body, _ := json.Marshal(map[string]any{"operation": "upsert", "resource": resource, "namespace": record.Namespace, "name": record.Name,
			"object": map[string]any{"kind": record.Kind, "metadata": map[string]any{"uid": record.UID, "ownerReferences": owners},
				"spec": map[string]any{"containers": containers}, "status": map[string]any{"containerStatuses": statuses}}})
		result := httptest.NewRecorder()
		api.ServeHTTP(result, httptest.NewRequest("POST", "/_fake/mutate", strings.NewReader(string(body))))
		if result.Code != 200 {
			t.Fatalf("seed native object: %d %s", result.Code, result.Body)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			backend.active.Add(1)
			backend.opened.Add(1)
			defer backend.active.Add(-1)
			backend.mu.Lock()
			backend.queries = append(backend.queries, r.URL.Query())
			backend.mu.Unlock()
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			since, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("sinceTime"))
			base := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
			for i := 0; i < backend.lines; i++ {
				at := base.Add(time.Duration(i) * time.Millisecond)
				if at.Before(since) {
					continue
				}
				if _, err := fmt.Fprintf(w, "%s record-%06d %s\n", at.Format(time.RFC3339Nano), i, strings.Repeat("x", backend.padding)); err != nil {
					return
				}
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/apis/metrics.k8s.io/v1beta1/pods" {
			backend.metrics.Add(1)
			backend.mu.Lock()
			observedAt := backend.at
			backend.mu.Unlock()
			if backend.started != nil {
				backend.once.Do(func() { close(backend.started) })
			}
			if backend.block {
				<-r.Context().Done()
				return
			}
			items := []any{}
			for _, record := range backend.records {
				if record.Kind == "Pod" {
					items = append(items, map[string]any{
						"kind": "PodMetrics", "metadata": map[string]any{"namespace": record.Namespace, "name": record.Name},
						"timestamp": observedAt.Format(time.RFC3339Nano), "window": "30s",
						"containers": []any{map[string]any{"name": "app-0", "usage": map[string]any{"cpu": "250m", "memory": "64Mi"}}},
					})
				}
			}
			start, _ := strconv.Atoi(r.URL.Query().Get("continue"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if start < 0 || start > len(items) {
				http.Error(w, "invalid continue", 400)
				return
			}
			if limit <= 0 {
				limit = len(items)
			}
			end := min(start+limit, len(items))
			next := ""
			if end < len(items) {
				next = strconv.Itoa(end)
			}
			writeJSON(w, map[string]any{"kind": "PodMetricsList", "metadata": map[string]any{"continue": next}, "items": items[start:end]})
			return
		}
		api.ServeHTTP(w, r)
	}))
	backend.url = server.URL
	t.Cleanup(server.Close)
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "native"})
	service.SetScrubber(replacingScrubber{})
	service.SetChangeStorage(storage.NewMemory())
	registry := NewServiceRegistry(service)
	t.Cleanup(registry.Close)
	return service
}

func waitNativeCount(t *testing.T, value *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for value.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("native counter=%d want=%d", value.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNativeReadAdmissionHoldsTwoHundredHTTPBodies(t *testing.T) {
	backend := &nativeResourceBackend{}
	service := nativeResourceService(t, backend)
	ctx, cancel := context.WithCancel(logStreamTestContext())
	defer cancel()
	var workers sync.WaitGroup
	errs := make(chan error, MaxProcessLogReads)
	for i := 0; i < MaxProcessLogReads; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errs <- service.streamPodLogs(ctx, PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "app"}, func(PodLogStreamEvent) error { return nil }, time.Minute, time.Minute, time.Minute, MaxLogStreamBytes)
		}()
	}
	waitNativeCount(t, &backend.active, 200)
	before := backend.opened.Load()
	err := service.streamPodLogs(ctx, PodLogStreamOptions{Namespace: "shop", Pod: "api"}, func(PodLogStreamEvent) error { return nil }, time.Minute, time.Minute, time.Minute, MaxLogStreamBytes)
	if !errors.Is(err, ErrLogStreamBusy) || backend.opened.Load() != before {
		t.Fatalf("201st read reached upstream: err=%v opened=%d", err, backend.opened.Load())
	}
	cancel()
	workers.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled reader: %v", err)
		}
	}
	waitNativeCount(t, &backend.active, 0)
	logReadAdmissions.Lock()
	total := logReadAdmissions.total
	logReadAdmissions.Unlock()
	if total != 0 {
		t.Fatalf("reservations leaked=%d", total)
	}
	if err := service.streamPodLogs(logStreamTestContext(), PodLogStreamOptions{Namespace: "shop", Pod: "api"}, func(PodLogStreamEvent) error { return nil }, 30*time.Millisecond, time.Minute, time.Minute, MaxLogStreamBytes); err != nil {
		t.Fatalf("readmission: %v", err)
	}
	waitNativeCount(t, &backend.active, 0)
	t.Log("200 simultaneous real HTTP bodies admitted; 201st rejected before network; all reservations/readers released; readmission succeeds")
}

func TestNativeByteAndDurationRotationPreservesWindow(t *testing.T) {
	for _, mode := range []string{"bytes", "duration"} {
		t.Run(mode, func(t *testing.T) {
			backend := &nativeResourceBackend{}
			duration, budget := 60*time.Millisecond, int64(MaxLogStreamBytes)
			if mode == "bytes" {
				backend.lines, backend.padding = 5000, 12000
				duration = 5 * time.Second
			}
			service := nativeResourceService(t, backend)
			options := PodLogStreamOptions{Namespace: "shop", Pod: "api", Container: "app", SinceSeconds: 60, TailLines: 123}
			var terminal []PodLogStreamEvent
			for read := 0; read < 2; read++ {
				terminal = nil
				if err := service.streamPodLogs(logStreamTestContext(), options, func(event PodLogStreamEvent) error {
					if event.Event == "limit" || event.Event == "end" {
						terminal = append(terminal, event)
					}
					return nil
				}, duration, time.Minute, time.Minute, budget); err != nil {
					t.Fatal(err)
				}
				if len(terminal) != 2 || terminal[0].Reason != mode || terminal[0].Cursor == "" || terminal[0].Cursor != terminal[1].Cursor {
					t.Fatalf("rotation read=%d events=%+v", read, terminal)
				}
				if mode == "duration" && (terminal[0].ReplayUncertain || terminal[1].ReplayUncertain) {
					t.Fatalf("empty retained rotation fabricated a replay gap: %+v", terminal)
				}
				options.Cursor, options.SinceSeconds, options.TailLines = terminal[0].Cursor, 0, 0
			}
			waitNativeCount(t, &backend.active, 0)
			backend.mu.Lock()
			query := backend.queries[1]
			backend.mu.Unlock()
			if query.Get("sinceTime") == "" || mode == "duration" && query.Get("tailLines") != "123" {
				t.Fatalf("resume window lost: %v", query)
			}
			t.Logf("%s: two real HTTP reads rotate with reusable cursor; production byte budget=%d; duration seam=%s", mode, budget, duration)
		})
	}
}

func TestNativeWorkloadQuietReferencesAndFallbackWindow(t *testing.T) {
	backend := &nativeResourceBackend{records: workloadStreamRecords("StatefulSet", 3, 1)}
	service := nativeResourceService(t, backend)
	seedWorkloadStreamIndex(t, service, backend.records)
	options := WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", SinceSeconds: 60, TailLines: 123}
	quiet, err := service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer quiet.Close()
	token := quiet.checkpoint()
	first := token
	for i := 0; i < 300; i++ {
		if quiet.checkpoint() != token {
			t.Fatal("unchanged quiet checkpoint churned references")
		}
	}
	busy, err := service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	for i := 0; i < 300; i++ {
		for id := range busy.cursor.Sources {
			busy.cursor.Sources[id] = fmt.Sprintf("changing-%d", i)
			break
		}
		busy.checkpoint()
	}
	options.Cursor, options.SinceSeconds, options.TailLines = first, 0, 0
	restored, err := service.workloadCursor(options)
	if err != nil || restored.Uncertain || len(restored.Sources) != 3 {
		t.Fatalf("busy viewer evicted quiet viewer: %+v %v", restored, err)
	}
	var decoded workloadLogCursor
	raw, _ := base64.RawURLEncoding.DecodeString(first)
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	workloadCheckpoints.Lock()
	removeWorkloadCheckpoint(decoded.Reference)
	workloadCheckpoints.Unlock()
	quiet.Close()
	busy.Close()
	fallback, err := service.OpenWorkloadLogStream(logStreamTestContext(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fallback.Close()
	if !fallback.uncertain || fallback.options.SinceSeconds != 60 || fallback.options.TailLines != 123 {
		t.Fatalf("reference fallback changed bounds: %+v", fallback.options)
	}
	ctx, cancel := context.WithTimeout(logStreamTestContext(), 100*time.Millisecond)
	defer cancel()
	// Exercise the actual reopened HTTP readers, not just cursor decoding.
	fallback.cancel()
	fallback.Close()
	err = service.StreamWorkloadLogs(ctx, options, func(WorkloadLogStreamEvent) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded quiet stream=%v", err)
	}
	waitNativeCount(t, &backend.active, 0)
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.queries) != 3 {
		t.Fatalf("fallback reads=%d", len(backend.queries))
	}
	for _, query := range backend.queries {
		if query.Get("sinceSeconds") != "60" || query.Get("tailLines") != "123" {
			t.Errorf("source fallback window=%v", query)
		}
	}
	t.Log("quiet refs stable; busy viewer cannot evict active quiet latest; missing ref marks uncertainty and all three real requests retain60s/123")
}

func awaitNativeHistory(t *testing.T, service *Service, samples int) UsageHistory {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		history, err := service.UsageHistory(logStreamTestContext(), "shop", "api-00")
		if err != nil {
			t.Fatal(err)
		}
		if len(history.Samples) == samples {
			return history
		}
		if time.Now().After(deadline) {
			t.Fatalf("eventual history samples=%d want%d", len(history.Samples), samples)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNativeHistoryReplicaInterestAndRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := &nativeResourceBackend{records: workloadStreamRecords("StatefulSet", 1, 1), at: now}
	owner := nativeResourceService(t, backend)
	seedWorkloadStreamIndex(t, owner, backend.records)
	replicaScope := Scope{OrgID: owner.scope.OrgID, ClusterID: owner.scope.ClusterID, CredentialID: owner.configuredCredentialID}
	viewer := newTestService(t, backend.url, replicaScope)
	viewer.SetChangeStorage(owner.usageProvider())
	seedWorkloadStreamIndex(t, viewer, backend.records)
	t.Cleanup(NewServiceRegistry(viewer).Close)
	if owner.usageHistory == viewer.usageHistory {
		t.Fatal("replicas unexpectedly share process memory")
	}
	if owner.cacheKey() != viewer.cacheKey() {
		t.Fatal("replicas do not use the same configured credential identity")
	}
	if _, err := viewer.UsageHistory(logStreamTestContext(), "shop", "api-00"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		manifest, _, err := readUsageManifest(owner.usageProvider(), usageBridgePrefix(owner.cacheKey())+"interest")
		if err != nil {
			t.Fatal(err)
		}
		if len(manifest) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("off-request drawer interest was not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	owner.sampleUsageHistory(logStreamTestContext(), now)
	owner.sampleUsageHistory(logStreamTestContext(), now.Add(10*time.Second))
	history := awaitNativeHistory(t, viewer, 1)
	if history.Samples[0].CPUMilli != 250 || history.Samples[0].MemoryBytes != 64<<20 || backend.metrics.Load() != 1 {
		t.Fatalf("shared interest/snapshot/cadence: %+v requests=%d", history, backend.metrics.Load())
	}
	// Simulate owner failover: independent empty memory, shared real CAS provider.
	next := newTestService(t, backend.url, replicaScope)
	next.SetChangeStorage(owner.usageProvider())
	seedWorkloadStreamIndex(t, next, backend.records)
	t.Cleanup(NewServiceRegistry(next).Close)
	awaitNativeHistory(t, next, 1)
	// Ordered upgrade scope uses the same provider, never a request-supplied org.
	licensed := NewClusterRegistryWithScope(false, []ClusterEntry{{Info: ClusterInfo{ID: "native"}, Service: next}}, tenancy.OrgScope{Write: "licensed", Read: []string{"licensed", "default"}})
	t.Cleanup(licensed.Close)
	upgraded := licensed.ResolveOrg("licensed")
	awaitNativeHistory(t, upgraded, 1)
	provider := owner.usageProvider()
	legacyKey := usageRecordKey(owner.cacheKey(), "shop", "api-00")
	legacyBefore, err := provider.ReadBlob(legacyKey)
	if err != nil || len(legacyBefore) == 0 {
		t.Fatalf("legacy sample missing before upgrade: %v", err)
	}
	seedWorkloadStreamIndex(t, upgraded, backend.records)
	upgraded.sampleUsageHistory(logStreamTestContext(), now)
	licensedSnapshot, err := provider.ReadBlob(usageRecordKey(upgraded.cacheKey(), "shop", "api-00"))
	legacyAfter, legacyErr := provider.ReadBlob(legacyKey)
	if err != nil || legacyErr != nil || len(licensedSnapshot) == 0 || !bytes.Equal(legacyBefore, legacyAfter) {
		t.Fatalf("upgrade sample write scope: licensed bytes%d legacy changed%v errors%v/%v", len(licensedSnapshot), !bytes.Equal(legacyBefore, legacyAfter), err, legacyErr)
	}
	t.Log("non-owner drawer interest reached owner; native metrics sampled once/60s; CAS snapshot read by viewer and fresh failover replica")
	t.Log("licensed ordered read recovered default sample; new native sample written under licensed cache only; default snapshot unchanged")

	// Exercise 361 physical metrics responses with logical minute ticks. Keep
	// this retention ring independent of provider bridge reads at wall-clock now.
	ringBackend := &nativeResourceBackend{records: workloadStreamRecords("StatefulSet", 1, 1)}
	ring := nativeResourceService(t, ringBackend)
	ring.SetChangeStorage(nil)
	seedWorkloadStreamIndex(t, ring, ringBackend.records)
	if _, err := ring.UsageHistory(logStreamTestContext(), "shop", "api-00"); err != nil {
		t.Fatal(err)
	}
	for minute := 0; minute <= 360; minute++ {
		at := now.Add(time.Duration(minute-360) * time.Minute)
		ringBackend.mu.Lock()
		ringBackend.at = at
		ringBackend.mu.Unlock()
		ring.sampleUsageHistory(logStreamTestContext(), at)
	}
	retained, err := ring.UsageHistory(logStreamTestContext(), "shop", "api-00")
	if err != nil || len(retained.Samples) != 360 || ringBackend.metrics.Load() != 361 || retained.Samples[0].Timestamp.Before(now.Add(-6*time.Hour)) {
		t.Fatalf("retention samples=%d calls=%d err=%v", len(retained.Samples), ringBackend.metrics.Load(), err)
	}
	ring.usageHistory.Lock()
	pruneUsage(ring.usageHistory.scope(ring.cacheKey()), now.Add(6*time.Hour+time.Second))
	remaining := ring.usageHistory.podCount(now.Add(6*time.Hour + time.Second))
	ring.usageHistory.Unlock()
	if remaining != 0 {
		t.Fatalf("expired history retained Pods=%d", remaining)
	}
	t.Log("361 real metrics responses ->360 chronological samples in six-hour ring; logical expiry releases all stale history")
}

func TestNativeHistoryCapacityUsesBoundedSharedManifest(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	backend := &nativeResourceBackend{records: workloadStreamRecords("StatefulSet", 2001, 1), at: now}
	service := nativeResourceService(t, backend)
	seedWorkloadStreamIndex(t, service, backend.records)
	updates := make([]usageInterest, 0, 2001)
	for _, record := range backend.records {
		if record.Kind == "Pod" {
			updates = append(updates, usageInterest{Namespace: record.Namespace, Pod: record.Name, UID: record.UID, Opened: now, Touched: now})
		}
	}
	if err := service.updateUsageManifest(logStreamTestContext(), service.usageProvider(), updates[:2000], now); err != nil {
		t.Fatal(err)
	}
	if err := service.updateUsageManifest(logStreamTestContext(), service.usageProvider(), updates[2000:], now); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("overflow must be explicitly rejected, got %v", err)
	}
	service.sampleUsageHistory(logStreamTestContext(), now)
	service.usageHistory.Lock()
	count := service.usageHistory.podCount(now)
	sampled := 0
	for _, entry := range service.usageHistory.scope(service.cacheKey()).pods {
		if len(entry.samples) != 0 {
			sampled++
		}
	}
	service.usageHistory.Unlock()
	manifest, _, err := readUsageManifest(service.usageProvider(), usageBridgePrefix(service.cacheKey())+"interest")
	if err != nil || len(manifest) != 2000 || count != 2000 || sampled != 2000 {
		t.Fatalf("capacity manifest=%d history=%d sampled=%d err=%v", len(manifest), count, sampled, err)
	}
	history, err := service.UsageHistory(logStreamTestContext(), "shop", "api-2000")
	if err != nil || len(history.Samples) != 0 {
		t.Fatalf("unadmitted Pod history=%+v err=%v", history, err)
	}
	t.Log("2001 indexed Pods/metrics; shared CAS manifest and sampled history both capped2000; excluded Pod receives unavailable fallback")
}

func TestNativeBlockedMetricsDoesNotBlockCaptureOwner(t *testing.T) {
	now := time.Now().UTC()
	backend := &nativeResourceBackend{records: workloadStreamRecords("StatefulSet", 1, 1), block: true, started: make(chan struct{})}
	service := nativeResourceService(t, backend)
	if _, _, err := service.IndexSnapshot(logStreamTestContext(), "Pod", "StatefulSet"); err != nil {
		t.Fatalf("warm native index/discovery: %v", err)
	}
	if _, err := service.UsageHistory(logStreamTestContext(), "shop", "api-00"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(logStreamTestContext())
	defer cancel()
	done := make(chan struct{})
	loop := &captureLoop{ctx: ctx, cancel: cancel, done: done, owner: "native-owner", next: map[string]time.Time{}, success: map[string]time.Time{}, completed: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: CaptureStatus{Mode: "continuous"}}
	for kind := range captureCadences {
		loop.next[kind] = now.Add(time.Hour)
	}
	service.changes.captures[service.indexScope()] = loop
	store := service.changeStore()
	if err := store.ClaimLease(loop.owner, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	go func() {
		defer close(done)
		service.captureOwned(loop, store, time.Minute, 20*time.Millisecond, now.Add(time.Minute), ticks)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owner or blocked metrics goroutine leaked")
		}
	}()
	select {
	case <-backend.started:
	case <-time.After(2 * time.Second):
		t.Fatal("metrics request never started")
	}
	index, _ := service.indexes.ForScope(service.indexScope())
	// The actual owner must refresh a stale Pod index while metrics is blocked.
	if err := index.Refresh(ctx, "Pod", 0, now.Add(-time.Minute), func(context.Context) ([]kubeindex.Record, bool, error) { return backend.records[1:], true, nil }); err != nil {
		t.Fatal(err)
	}
	loop.mu.Lock()
	loop.next["Pod"] = now.Add(-time.Second)
	loop.mu.Unlock()
	ticks <- time.Now()
	deadline := time.Now().Add(time.Second)
	for {
		observed := index.Snapshot().Kinds["Pod"].ObservedAt
		if observed.After(now) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("blocked native metrics request stalled owner Pod refresh")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Log("actual captureOwned refreshes native Pod LIST while metrics HTTP body is blocked; cancellation joins sampler and lease-renewal children")
}
