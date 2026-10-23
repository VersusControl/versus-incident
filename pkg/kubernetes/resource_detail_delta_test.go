package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func streamPodObject(record kubeindex.Record) map[string]any {
	owners := []any{}
	for _, owner := range record.Owners {
		owners = append(owners, map[string]any{"kind": owner.Kind, "name": owner.Name, "uid": owner.UID, "controller": owner.Controller})
	}
	spec, statuses := []any{}, []any{}
	containers := record.LogContainers
	if len(containers) == 0 {
		containers = record.Containers
	}
	for _, container := range containers {
		spec = append(spec, map[string]any{"name": container.Name})
		state := map[string]any{}
		if container.Running {
			state["running"] = map[string]any{"startedAt": container.StartedAt.Format(time.RFC3339Nano)}
		}
		statuses = append(statuses, map[string]any{"name": container.Name, "ready": container.Ready, "restartCount": container.RestartCount, "state": state})
	}
	return map[string]any{"kind": "Pod", "metadata": map[string]any{"namespace": record.Namespace, "name": record.Name, "uid": record.UID, "ownerReferences": owners}, "spec": map[string]any{"containers": spec}, "status": map[string]any{"phase": record.Phase, "containerStatuses": statuses}}
}

func serveStreamPodObject(writer http.ResponseWriter, request *http.Request, records []kubeindex.Record) bool {
	for _, record := range records {
		if record.Kind == "Pod" && request.URL.Path == "/api/v1/namespaces/"+record.Namespace+"/pods/"+record.Name {
			writeJSON(writer, streamPodObject(record))
			return true
		}
	}
	return false
}

func TestForbiddenWorkloadSourcesAreStableStatusesNotReplayGaps(t *testing.T) {
	records := workloadStreamRecords("StatefulSet", 2, 1)
	var logReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if serveStreamPodObject(writer, request, records) {
			return
		}
		logReads.Add(1)
		writer.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, records)
	ctx, cancel := context.WithTimeout(logStreamTestContext(), 6100*time.Millisecond)
	defer cancel()
	statuses, gaps := 0, 0
	err := service.StreamWorkloadLogs(ctx, WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root"}, func(event WorkloadLogStreamEvent) error {
		if event.Event == "source_status" {
			statuses++
			if event.ReplayUncertain {
				gaps++
			}
		}
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if statuses != 2 || logReads.Load() != 2 || gaps != 0 {
		t.Fatalf("persistent denial retried or produced gaps: statuses=%d reads=%d gaps=%d", statuses, logReads.Load(), gaps)
	}
}

func TestInitialWorkloadReadValidatesUIDDespiteFreshIndex(t *testing.T) {
	records := workloadStreamRecords("StatefulSet", 1, 1)
	replacement := records[1]
	replacement.UID = "replacement"
	var logs, gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/log") {
			gets.Add(1)
			writeJSON(writer, streamPodObject(replacement))
			return
		}
		logs.Add(1)
		fmt.Fprintln(writer, "2026-10-01T00:00:00Z replacement must not use old UID")
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	seedWorkloadStreamIndex(t, service, records)
	lines := 0
	err := service.StreamWorkloadLogs(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root", Previous: true}, func(event WorkloadLogStreamEvent) error {
		if event.Event == "line" {
			lines++
		}
		return nil
	})
	if err != nil || logs.Load() != 0 || lines != 0 || gets.Load() != 1 {
		t.Fatalf("fresh session trusted stale index: gets=%d logs=%d lines=%d err=%v", gets.Load(), logs.Load(), lines, err)
	}
}

type delayedUsageProvider struct {
	storage.Provider
	delay     time.Duration
	failSwaps atomic.Int32
	casCalls  atomic.Int32
}

func (provider *delayedUsageProvider) ReadBlob(key string) ([]byte, error) {
	time.Sleep(provider.delay)
	return provider.Provider.ReadBlob(key)
}

func (provider *delayedUsageProvider) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	provider.casCalls.Add(1)
	time.Sleep(provider.delay)
	for {
		left := provider.failSwaps.Load()
		if left <= 0 {
			break
		}

		if provider.failSwaps.CompareAndSwap(left, left-1) {
			return false, nil
		}
	}
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, expected, replacement)
}

func waitUsageInterestCount(t *testing.T, service *Service, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, _, err := readUsageManifest(service.usageProvider(), usageBridgePrefix(service.cacheKey())+"interest")
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("interest worker did not persist expected batch", count)
}

func waitUsageInterestIdle(t *testing.T, service *Service) *usageInterestWorker {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		service.usageHistory.Lock()
		worker := service.usageHistory.workers[service.cacheKey()]
		service.usageHistory.Unlock()
		if worker != nil {
			worker.mu.Lock()
			count := len(worker.pending)
			worker.mu.Unlock()
			if count == 0 {
				return worker
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("interest worker did not drain")
	return nil
}

func TestHistoryInterestWorkerRetriesWholeBatchAndSkipsRecentTouches(t *testing.T) {
	provider := &delayedUsageProvider{Provider: storage.NewMemory(), delay: 2 * time.Millisecond}
	provider.failSwaps.Store(12)
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(provider)
	defer service.StopCapture()
	for i := 0; i < 50; i++ {
		if _, err := service.UsageHistory(logStreamTestContext(), "shop", fmt.Sprintf("api-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	waitUsageInterestCount(t, service, 50)
	worker := waitUsageInterestIdle(t, service)
	if worker.failures.Load() == 0 {
		t.Fatal("exhausted CAS batch was not surfaced as a logged failure")
	}
	before := provider.casCalls.Load()
	for i := 0; i < 50; i++ {
		if _, err := service.UsageHistory(logStreamTestContext(), "shop", fmt.Sprintf("api-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	waitUsageInterestIdle(t, service)
	if provider.casCalls.Load() != before {
		t.Fatal("recent drawer reads rewrote manifest", before, provider.casCalls.Load())
	}
	service.StopCapture()
	service.usageHistory.Lock()
	remaining := len(service.usageHistory.workers)
	service.usageHistory.Unlock()
	if remaining != 0 {
		t.Fatal("stopped interest worker leaked")
	}
}

type blockingUsageProvider struct {
	storage.Provider
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (provider *blockingUsageProvider) ReadBlob(key string) ([]byte, error) {
	if strings.HasSuffix(key, "/interest") {
		provider.once.Do(func() { close(provider.started) })
		<-provider.release
	}
	return provider.Provider.ReadBlob(key)
}

func (provider *blockingUsageProvider) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, expected, replacement)
}

func TestHistoryInterestShutdownJoinsUncancellableProviderCall(t *testing.T) {
	provider := &blockingUsageProvider{Provider: storage.NewMemory(), started: make(chan struct{}), release: make(chan struct{})}
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(provider)
	if _, err := service.UsageHistory(logStreamTestContext(), "shop", "api"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		close(provider.release)
		service.StopCapture()
		t.Fatal("worker did not enter provider call")
	}
	stopped := make(chan struct{})
	go func() { service.StopCapture(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("shutdown claimed cancellation of an uncancellable provider call")
	case <-time.After(30 * time.Millisecond):
	}
	close(provider.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("worker did not join after provider returned")
	}
	service.usageHistory.Lock()
	count := len(service.usageHistory.workers)
	service.usageHistory.Unlock()
	if count != 0 {
		t.Fatal("worker survived scope shutdown")
	}
}

type blockingUsageCASProvider struct {
	storage.Provider
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (provider *blockingUsageCASProvider) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	provider.once.Do(func() { close(provider.started) })
	<-provider.release
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, expected, replacement)
}

func TestHistoryCacheWriteDoesNotHoldCredentialReadLock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("history GET reached Kubernetes") }))
	defer server.Close()
	credentials := &credentialSource{authorization: func(ctx context.Context) (string, error) { return "Bearer unit-cache-token", ctx.Err() }}
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	provider := &blockingUsageCASProvider{Provider: storage.NewMemory(), started: make(chan struct{}), release: make(chan struct{})}
	service.SetChangeStorage(provider)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(provider.release) }) }
	defer func() { release(); service.StopCapture() }()
	if _, err := service.UsageHistory(logStreamTestContext(), "shop", "first"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not reach CAS")
	}
	read := make(chan error, 1)
	go func() { _, err := service.UsageHistory(logStreamTestContext(), "shop", "second"); read <- err }()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(150 * time.Millisecond):
		release()
		<-read
		t.Fatal("cache IO held the credential lock and stalled history GET")
	}
}

func TestSuspendedSourceOnlyRetriesAfterMeaningfulObservationChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "cluster"})
	service.SetScrubber(replacingScrubber{})
	records := workloadStreamRecords("StatefulSet", 1, 1)
	seedWorkloadStreamIndex(t, service, records)
	session, err := service.OpenWorkloadLogStream(logStreamTestContext(), WorkloadLogStreamOptions{Kind: "StatefulSet", Name: "api", Namespace: "shop", UID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var id string
	for key, worker := range session.workers {
		id = key
		worker.started, worker.finished, worker.suspended = true, true, true
		worker.cancel = func() {}
		worker.release()
		close(worker.done)
	}
	old := session.workers[id]
	for i := 0; i < 3; i++ {
		records[1].ResourceVersion = fmt.Sprint(i)
		if err := session.reconcile(records[1:]); err != nil {
			t.Fatal(err)
		}
		if session.workers[id] != old {
			t.Fatal("metadata refresh retried a persistent failure")
		}
	}
	records[1].LogContainers[0].Ready = true
	if err := session.reconcile(records[1:]); err != nil {
		t.Fatal(err)
	}
	if session.workers[id] == old {
		t.Fatal("meaningful state change did not permit retry")
	}
}

func TestConcurrentHistoryReadsQueueAllInterestsWithoutStorageLatency(t *testing.T) {
	provider := &delayedUsageProvider{Provider: storage.NewMemory(), delay: 2 * time.Millisecond}
	service := NewService(nil, Scope{OrgID: "default", ClusterID: "cluster"}, 0)
	service.SetChangeStorage(provider)
	defer service.StopCapture()
	start := make(chan struct{})
	var requests sync.WaitGroup
	requests.Add(50)
	clock := time.Now()
	for i := 0; i < 50; i++ {
		go func(i int) {
			defer requests.Done()
			<-start
			if _, err := service.UsageHistory(logStreamTestContext(), "shop", fmt.Sprintf("api-%02d", i)); err != nil {
				t.Errorf("history read %v", err)
			}
		}(i)
	}
	close(start)
	requests.Wait()
	if elapsed := time.Since(clock); elapsed > 200*time.Millisecond {
		t.Errorf("history GET waited on manifest IO: %s", elapsed)
	}
	deadline := time.Now().Add(3 * time.Second)
	count := 0
	for time.Now().Before(deadline) {
		entries, _, err := readUsageManifest(provider, usageBridgePrefix(service.cacheKey())+"interest")
		if err != nil {
			t.Fatal(err)
		}
		count = len(entries)
		if count == 50 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("concurrent history interests were lost: %d/50", count)
}
