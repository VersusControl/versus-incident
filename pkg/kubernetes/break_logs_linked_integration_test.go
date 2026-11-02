package kubernetes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/kubernetes/kref"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
)

// linkedSourceStub is a local HTTP log platform. A line is visible only after
// its ingestion lag has elapsed; requests can be held, failed or slowed, and
// cross-process concurrency is measured at the source itself.
type linkedSourceStub struct {
	mu          sync.Mutex
	lag         time.Duration
	lines       map[string][]breaklogs.LinkedLine
	failures    map[string]int
	requests    map[string]int
	hold        chan struct{}
	arrived     chan string
	pause       time.Duration
	inFlight    int
	maxInFlight int
	events      []string
	server      *httptest.Server
}

func newLinkedSourceStub(t *testing.T) *linkedSourceStub {
	t.Helper()
	stub := &linkedSourceStub{lines: map[string][]breaklogs.LinkedLine{}, failures: map[string]int{}, requests: map[string]int{}}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(func() {
		stub.release()
		stub.server.Close()
	})
	return stub
}

func (stub *linkedSourceStub) release() {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.hold != nil {
		select {
		case <-stub.hold:
		default:
			close(stub.hold)
		}
	}
}

func (stub *linkedSourceStub) add(namespace string, lines ...breaklogs.LinkedLine) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	stub.lines[namespace] = append(stub.lines[namespace], lines...)
}

func (stub *linkedSourceStub) expire(namespace string) {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	delete(stub.lines, namespace)
}

func (stub *linkedSourceStub) count(namespace string) int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.requests[namespace]
}

func (stub *linkedSourceStub) serve(writer http.ResponseWriter, request *http.Request) {
	values := request.URL.Query()
	namespace := values.Get("namespace")
	from, _ := time.Parse(time.RFC3339Nano, values.Get("from"))
	to, _ := time.Parse(time.RFC3339Nano, values.Get("to"))
	limit, _ := strconv.Atoi(values.Get("limit"))
	pods := map[string]bool{}
	for _, pod := range strings.Split(values.Get("pods"), ",") {
		if pod != "" {
			pods[pod] = true
		}
	}
	stub.mu.Lock()
	stub.requests[namespace]++
	fail := stub.requests[namespace] <= stub.failures[namespace]
	stub.inFlight++
	if stub.inFlight > stub.maxInFlight {
		stub.maxInFlight = stub.inFlight
	}
	stub.events = append(stub.events, fmt.Sprintf("%s +%s %s in-flight=%d", time.Now().Format("15:04:05.000"), namespace, request.Header.Get("User-Agent"), stub.inFlight))
	hold, arrived, pause := stub.hold, stub.arrived, stub.pause
	stub.mu.Unlock()
	defer func() {
		stub.mu.Lock()
		stub.inFlight--
		stub.events = append(stub.events, fmt.Sprintf("%s -%s", time.Now().Format("15:04:05.000"), namespace))
		stub.mu.Unlock()
	}()
	if arrived != nil {
		select {
		case arrived <- namespace:
		default:
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-request.Context().Done():
			return
		}
	}
	if pause > 0 {
		time.Sleep(pause)
	}
	if fail {
		http.Error(writer, "backend unavailable", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	stub.mu.Lock()
	visible := []breaklogs.LinkedLine{}
	for _, line := range stub.lines[namespace] {
		if line.At.Before(from) || line.At.After(to) || len(pods) > 0 && !pods[line.Pod] || line.At.Add(stub.lag).After(now) {
			continue
		}
		visible = append(visible, line)
	}
	stub.mu.Unlock()
	sort.Slice(visible, func(i, j int) bool { return visible[i].At.After(visible[j].At) })
	truncated := false
	if limit > 0 && len(visible) > limit {
		visible, truncated = visible[:limit], true
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"lines": visible, "truncated": truncated})
}

// linkedStubReader is a real HTTP client against the local source; it spends
// the shared per-query call budget exactly as provider adapters do.
type linkedStubReader struct{ endpoint string }

func (reader linkedStubReader) Preview(breaklogs.LinkedQuery) string { return "" }
func (reader linkedStubReader) Read(ctx context.Context, query breaklogs.LinkedQuery) (breaklogs.Result, error) {
	if !breaklogs.AdmitLinkedCall(ctx) {
		return breaklogs.Result{Truncated: true}, nil
	}
	values := url.Values{"namespace": {query.Namespace}, "from": {query.From.Format(time.RFC3339Nano)}, "to": {query.To.Format(time.RFC3339Nano)}, "limit": {strconv.Itoa(query.Limit)}, "pods": {strings.Join(query.Pods, ",")}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reader.endpoint+"/read?"+values.Encode(), nil)
	if err != nil {
		return breaklogs.Result{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return breaklogs.Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return breaklogs.Result{}, fmt.Errorf("linked source status %d", response.StatusCode)
	}
	var body struct {
		Lines     []breaklogs.LinkedLine `json:"lines"`
		Truncated bool                   `json:"truncated"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return breaklogs.Result{}, err
	}
	return breaklogs.Result{Lines: body.Lines, Truncated: body.Truncated}, nil
}

type linkedBackend struct {
	name string
	open func(*testing.T) storage.Provider
}

func TestRetainedBreakExcerptReopensAfterAuthorizationRecovery(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			kube := linkedKube(t)
			cluster := linkedCluster("recovered-excerpt-" + backend.name)
			scope := Scope{OrgID: "default", ClusterID: cluster, CredentialID: "configured"}
			now := time.Now().UTC()
			open := func(generation uint64, principal string) *Service {
				source := &expiringBearerSource{principal: principal, now: time.Now, refresh: func(context.Context) (string, time.Time, error) {
					return "reopen-token-canary", now.Add(time.Hour), nil
				}}
				client, err := NewClient(Config{Endpoint: kube, Credentials: source, AllowLoopbackHTTP: true})
				if err != nil {
					t.Fatal(err)
				}
				client.authGeneration.Store(generation)
				service := NewService(client, scope, 0)
				service.SetScrubber(breakLogScrubber{})
				service.SetChangeStorage(backend.open(t))
				service.changes.evidenceEnabled = true
				t.Cleanup(service.StopCapture)
				return service
			}
			// The live process recovered three times; a new process starts at zero.
			first := open(3, "verified-reader")
			at := now.Add(-4 * time.Minute).Truncate(time.Second)
			manifest := breaklogs.Manifest{ID: breaklogs.ManifestIdentity("0e1031b7-e2cc-4642-a0c9-84c5235d506e", at), Namespace: "retained-reopen", RootUID: "0e1031b7-e2cc-4642-a0c9-84c5235d506e", BreakAt: at, From: at.Add(-5 * time.Minute), To: at.Add(2 * time.Minute), CapturedAt: now, Attempts: 1}
			manifest.OccurrenceID = manifest.ID
			head := breaklogs.Snapshot{Source: "linked", OccurrenceID: manifest.ID, Cluster: cluster, Namespace: manifest.Namespace, RootUID: manifest.RootUID, Pod: "crash-burst-564d48c6bc-mx7sz", PodUID: "5224a26c-a6a1-498c-9d57-1de5c04964bc", Container: "app", Slot: "previous", BreakAt: at, TerminationAt: at.Add(39 * time.Second), From: at.Add(38 * time.Second), To: at.Add(40*time.Second - time.Nanosecond), CapturedAt: now, Truncated: true}
			for i := range 50 {
				head.Lines = append(head.Lines, breaklogs.Line{At: head.From.Add(time.Duration(i+1) * time.Millisecond), Text: fmt.Sprintf("retained head %02d", i)})
			}
			head.ID = breaklogs.Identity(head)
			items := []breaklogs.Snapshot{head}
			for slot := range 2 {
				tail := head
				tail.Source, tail.OccurrenceID, tail.Lines = "", "", nil
				tail.TerminationAt = head.TerminationAt.Add(time.Duration(slot) * time.Minute)
				for i := range 500 {
					tail.Lines = append(tail.Lines, breaklogs.Line{At: head.From.Add(time.Duration(i+100) * time.Millisecond), Text: fmt.Sprintf("native tail %d %03d", slot, i)})
				}
				tail.ID = breaklogs.Identity(tail)
				items = append(items, tail)
			}
			if err := first.changeStore().CommitBreakLogs(items, nil, "", "", now, manifest); err != nil {
				t.Fatal(err)
			}
			query := breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, OccurrenceID: manifest.OccurrenceID, Center: at, From: manifest.From, To: manifest.To, Container: breaklogs.AllContainers, Limit: 500}
			unlinked, err := first.BreakLogs(t.Context(), query)
			if err != nil || len(unlinked.Lines) != 500 || unlinked.Counts.All != 1050 {
				t.Fatalf("unlinked page: lines=%d total=%d err=%v", len(unlinked.Lines), unlinked.Counts.All, err)
			}
			first.client.rejectCredential()
			if err := first.commitCredential(t.Context(), func() error { t.Fatal("retired process published"); return nil }); err == nil {
				t.Fatal("retired process was not fenced")
			}
			reopened := open(0, "verified-reader")
			retained, err := reopened.changeStore().BreakLogs(now)
			if err != nil || len(retained.Snapshots) != 3 {
				t.Fatalf("restart lost retained query coverage: snapshots=%d err=%v", len(retained.Snapshots), err)
			}
			linked := func(page BreakLogPage) []BreakLogLine {
				lines := []BreakLogLine{}
				for _, line := range page.Lines {
					if line.Source == "linked" {
						lines = append(lines, line)
					}
				}
				return lines
			}
			page, err := reopened.BreakLogs(t.Context(), query)
			if err != nil || len(linked(page)) != 50 || !reflect.DeepEqual(linked(page), linked(unlinked)) {
				t.Fatalf("first reopen changed linked excerpt: linked=%d err=%v", len(linked(page)), err)
			}
			again, err := reopened.BreakLogs(t.Context(), query)
			if err != nil || !reflect.DeepEqual(page.Lines, again.Lines) {
				t.Fatalf("repeat changed page: err=%v", err)
			}
			other := open(0, "different-reader")
			isolated, err := other.changeStore().BreakLogs(now)
			if err != nil || len(isolated.Snapshots) != 0 {
				t.Fatalf("different principal read retained excerpt: snapshots=%d err=%v", len(isolated.Snapshots), err)
			}
		})
	}
}

// linkedBackends returns reopenable real stores. Opening again is a process
// restart: no in-memory state is shared except what the store persisted.
func linkedBackends(t *testing.T) []linkedBackend {
	dir := t.TempDir()
	return []linkedBackend{
		{"file", func(t *testing.T) storage.Provider {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			return provider
		}},
		{"postgres", func(t *testing.T) storage.Provider {
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("TEST_POSTGRES_DSN not set: Postgres proof did not run")
			}
			provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			return provider
		}},
	}
}

func linkedKube(t *testing.T) string {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	return server.URL
}

func linkedReplica(t *testing.T, kube, cluster string, provider storage.Provider, endpoint string, delay *int) *Service {
	t.Helper()
	service := newTestService(t, kube, Scope{OrgID: "default", ClusterID: cluster})
	service.SetScrubber(breakLogScrubber{})
	service.SetChangeStorage(provider)
	t.Cleanup(service.StopCapture)
	service.SetBreakLogSources(func(string) []breaklogs.Source {
		return []breaklogs.Source{{Name: "logs", Type: "loki", CacheKey: "stub:" + endpoint, Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
			return linkedStubReader{endpoint}, nil
		}}}
	})
	if err := service.SaveAgentSettings(context.Background(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: delay}}); err != nil {
		t.Fatal(err)
	}
	return service
}

func linkedCluster(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func statefulSetSnapshot(namespace, root string, pods ...kubeindex.Record) kubeindex.Snapshot {
	records := []kubeindex.Record{{Kind: "StatefulSet", APIVersion: "apps/v1", Namespace: namespace, Name: "db", UID: root}}
	for _, pod := range pods {
		pod.Kind, pod.APIVersion, pod.Namespace = "Pod", "v1", namespace
		pod.Owners = []kubeindex.OwnerRef{{Kind: "StatefulSet", UID: root, Name: "db", Namespace: namespace, Controller: true, ControllerKnown: true}}
		records = append(records, pod)
	}
	return kubeindex.Snapshot{Records: records, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}, "StatefulSet": {State: "ready"}}}
}

func manifestFor(t *testing.T, store *kubechanges.Store, id, namespace, root string, breakAt, now time.Time, settleAt time.Time, pods ...breaklogs.ManifestPod) breaklogs.Manifest {
	t.Helper()
	manifest := breaklogs.Manifest{ID: id, Namespace: namespace, RootUID: root, BreakAt: breakAt, From: breakAt.Add(-5 * time.Minute), To: breakAt.Add(2 * time.Minute), CapturedAt: now, SettleAt: settleAt, Pods: pods}
	if manifest.Pods == nil {
		manifest.Pods = []breaklogs.ManifestPod{}
	}
	return manifest
}

// manifestByID reads persisted progress through a fresh store client bound to
// the replica's capture scope (org, cluster and credential identity).
func manifestByID(t *testing.T, provider storage.Provider, replica *Service, id string) (breaklogs.Manifest, []breaklogs.Snapshot) {
	t.Helper()
	state, err := kubechanges.NewStore(provider, replica.indexScope()).BreakLogs(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	snapshots := []breaklogs.Snapshot{}
	for _, item := range state.Snapshots {
		if item.Source == "linked" && item.OccurrenceID == id {
			snapshots = append(snapshots, item)
		}
	}
	for _, manifest := range state.Manifests {
		if manifest.ID == id {
			return manifest, snapshots
		}
	}
	t.Fatalf("manifest %s not persisted", id)
	return breaklogs.Manifest{}, nil
}

func sleepUntil(at time.Time) {
	if wait := time.Until(at); wait > 0 {
		time.Sleep(wait)
	}
}

func gapReasons(page BreakLogPage) []string {
	reasons := []string{}
	for _, gap := range page.Coverage.Gaps {
		reasons = append(reasons, gap.Reason)
	}
	return reasons
}

// A Pod that logged its failure and was deleted before the break was detected
// keeps its prior verified UID and lifetime. With a source that lags 90s, the
// first read is pending with the exact ready time, the line appears once it
// is ingested, a settled excerpt survives restart, and a later empty answer
// from the source does not erase it.
func TestLinkedBreakDeletedPodBeforeDetectionAcrossRestartAndLag(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: wait for delayed source ingestion and settle across a durable restart")
	}
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			kube := linkedKube(t)
			stub := newLinkedSourceStub(t)
			stub.lag = 90 * time.Second
			cluster := linkedCluster("deleted-" + backend.name)
			namespace := "orders"
			delay := 90
			t0 := time.Now().UTC()
			replicaA := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &delay)
			storeA := replicaA.changeStore()
			// Replica A dies after detection; its short lease simply expires.
			if err := storeA.ClaimLease("replica-a", t0, 3*time.Second); err != nil {
				t.Fatal(err)
			}
			pod := kubeindex.Record{Name: "db-0", UID: "pod-old", CreatedAt: t0.Add(-time.Hour), Containers: []kubeindex.ContainerState{{Name: "app", RestartCount: 1, StartedAt: t0.Add(-4*time.Minute + 5*time.Second), TerminatedAt: t0.Add(-4 * time.Minute)}}}
			prior := statefulSetSnapshot(namespace, "root-db", pod)
			if err := storeA.ObserveBreakPods(replicaA.scrubManifestPods(verifiedBreakPods(prior)), "replica-a", t0.Add(-80*time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := storeA.ObserveBreakPods(nil, "replica-a", t0.Add(-70*time.Second)); err != nil {
				t.Fatal(err)
			}
			state, err := storeA.BreakLogs(t0)
			if err != nil {
				t.Fatal(err)
			}
			breakAt := t0.Add(-3*time.Minute - 15*time.Second + 123456789*time.Nanosecond)
			root := kubebreaks.Break{Root: kref.Ref{Kind: "StatefulSet", Namespace: namespace, Name: "db", UID: "root-db"}, Rule: "crashloop", BrokeAt: breakAt}
			manifest := replicaA.newBreakManifest(context.Background(), root, state, t0)
			if len(manifest.Pods) != 1 || manifest.Pods[0].UID != "pod-old" || !manifest.Pods[0].DeletedAt.Equal(t0.Add(-80*time.Second)) {
				t.Fatalf("deleted Pod lost its prior verified lifetime: %+v", manifest.Pods)
			}
			if !manifest.SettleAt.Equal(manifest.To.Add(90 * time.Second)) {
				t.Fatalf("settle not scheduled at ingestion readiness: %v vs %v", manifest.SettleAt, manifest.To)
			}
			if err := storeA.CommitBreakLogs(nil, nil, "", "replica-a", t0, manifest); err != nil {
				t.Fatal(err)
			}
			previous := breaklogs.LinkedLine{At: t0.Add(-6 * time.Minute), Pod: "db-0", Container: "app", Text: "ERROR previous instance crashed MESSAGE_CANARY"}
			current := breaklogs.LinkedLine{At: t0.Add(-85 * time.Second), Pod: "db-0", Container: "app", Text: "ERROR last words before deletion"}
			stub.add(namespace, previous, current)

			// Restart: a different replica process with its own store client.
			replicaB := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &delay)
			query := breaklogs.Query{Namespace: namespace, RootUID: "root-db", From: manifest.From, To: manifest.To, Center: manifest.BreakAt, Container: breaklogs.AllContainers}
			ready := manifest.To.Add(90 * time.Second)
			page, err := replicaB.BreakLogs(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Lines) != 1 || page.Lines[0].PodUID != "pod-old" || page.Lines[0].Slot != "previous" || page.Lines[0].Text != "ERROR previous instance crashed [redacted]" {
				t.Fatalf("deleted Pod line not attributed by prior lifetime: lines=%+v gaps=%v", page.Lines, gapReasons(page))
			}
			if !page.Window.From.Equal(manifest.From) || !page.Window.To.Equal(manifest.To) {
				t.Fatalf("pinned window drifted: %+v", page.Window)
			}
			exactReady := false
			for _, gap := range page.Coverage.Gaps {
				exactReady = exactReady || gap.Reason == "linked_pending" && gap.ReadyAt.Equal(ready)
			}
			if page.Coverage.Reason != "linked_pending" || !exactReady {
				t.Errorf("source still ingesting (ready %s) but a partial answer was final: state=%s reason=%q gaps=%+v", ready.Format(time.RFC3339Nano), page.Coverage.State, page.Coverage.Reason, page.Coverage.Gaps)
			}
			if page.OccurrenceID != manifest.ID {
				t.Fatalf("occurrence not echoed: %q", page.OccurrenceID)
			}
			// The line is ingested at t0+5s; a pending answer may be cached, but never past ready_at.
			sleepUntil(t0.Add(6 * time.Second))
			before := stub.count(namespace)
			if _, err := replicaB.BreakLogs(context.Background(), query); err != nil {
				t.Fatal(err)
			}
			t.Logf("reads before ready: source requests %d -> %d (pending cache)", before, stub.count(namespace))
			sleepUntil(ready.Add(time.Second))
			page, err = replicaB.BreakLogs(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			slots := map[string]string{}
			for _, line := range page.Lines {
				if line.PodUID != "pod-old" || line.Source != "linked" {
					t.Fatalf("line attributed to wrong identity: %+v", line)
				}
				slots[line.Text] = line.Slot
			}
			if len(page.Lines) != 2 || slots["ERROR last words before deletion"] != "current" || slots["ERROR previous instance crashed [redacted]"] != "previous" {
				t.Fatalf("ingested line missing after ready or slot wrong: %+v gaps=%v", page.Lines, gapReasons(page))
			}
			for _, gap := range page.Coverage.Gaps {
				if gap.Reason == "linked_pending" {
					t.Fatalf("still pending after ready: %+v", page.Coverage)
				}
			}

			storeB := replicaB.changeStore()
			if err := storeB.ClaimLease("replica-b", time.Now().UTC(), time.Minute); err != nil {
				t.Fatalf("restarted replica could not take the expired-owner-free lease: %v", err)
			}
			var running sync.WaitGroup
			replicaB.scheduleBreakSettles(context.Background(), "replica-b", &running)
			running.Wait()
			settled, excerpts := manifestByID(t, backend.open(t), replicaA, manifest.ID)
			if !settled.Done || settled.Attempts != 1 || len(excerpts) == 0 {
				t.Fatalf("settle not committed once: attempts=%d done=%v excerpts=%d", settled.Attempts, settled.Done, len(excerpts))
			}
			stub.expire(namespace)
			replicaC := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &delay)
			page, err = replicaC.BreakLogs(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Lines) != 2 {
				t.Fatalf("expired source erased settled excerpt after restart: %+v reasons=%v", page.Lines, gapReasons(page))
			}
			for _, line := range page.Lines {
				if strings.Contains(line.Text, "MESSAGE_CANARY") {
					t.Fatal("canary reached stored excerpt")
				}
			}
			t.Logf("after source expiry: reason=%s gaps=%v", page.Coverage.Reason, gapReasons(page))
		})
	}
}

// A reused StatefulSet name is split by verified UID lifetime. A line from the
// unobserved interval between two lifetimes, or from overlapping lifetimes, is
// never assigned to either UID and never turns the window into no_logs.
func TestLinkedBreakSameNameReplacementAndUnknownLifetime(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			kube := linkedKube(t)
			stub := newLinkedSourceStub(t)
			cluster := linkedCluster("reuse-" + backend.name)
			zero := 0
			t0 := time.Now().UTC().Truncate(time.Millisecond)
			replica := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &zero)
			store := replica.changeStore()
			if err := store.ClaimLease("owner", t0, time.Minute); err != nil {
				t.Fatal(err)
			}
			namespace := "ledger"
			old := kubeindex.Record{Name: "db-0", UID: "uid-old", CreatedAt: t0.Add(-time.Hour), Containers: []kubeindex.ContainerState{{Name: "app", StartedAt: t0.Add(-time.Hour)}}}
			replacement := kubeindex.Record{Name: "db-0", UID: "uid-new", CreatedAt: t0.Add(-5 * time.Minute), Containers: []kubeindex.ContainerState{{Name: "app", StartedAt: t0.Add(-5 * time.Minute)}}}
			for _, observation := range []struct {
				at   time.Duration
				pods []kubeindex.Record
			}{{-6 * time.Minute, []kubeindex.Record{old}}, {-4 * time.Minute, []kubeindex.Record{replacement}}, {-60 * time.Second, []kubeindex.Record{replacement}}, {-50 * time.Second, nil}} {
				if err := store.ObserveBreakPods(replica.scrubManifestPods(verifiedBreakPods(statefulSetSnapshot(namespace, "root-ledger", observation.pods...))), "owner", t0.Add(observation.at)); err != nil {
					t.Fatal(err)
				}
			}
			state, err := store.BreakLogs(t0)
			if err != nil {
				t.Fatal(err)
			}
			root := kubebreaks.Break{Root: kref.Ref{Kind: "StatefulSet", Namespace: namespace, Name: "db", UID: "root-ledger"}, Rule: "crashloop", BrokeAt: t0.Add(-4 * time.Minute)}
			manifest := replica.newBreakManifest(context.Background(), root, state, t0)
			if len(manifest.Pods) != 2 {
				t.Fatalf("both same-name lifetimes not retained: %+v", manifest.Pods)
			}
			if err := store.CommitBreakLogs(nil, nil, "", "owner", t0, manifest); err != nil {
				t.Fatal(err)
			}
			oldLine := breaklogs.LinkedLine{At: t0.Add(-7 * time.Minute), Pod: "db-0", Container: "app", Text: "old instance error"}
			gapLine := breaklogs.LinkedLine{At: t0.Add(-5*time.Minute - 30*time.Second), Pod: "db-0", Container: "app", Text: "unobserved interval line"}
			newLine := breaklogs.LinkedLine{At: t0.Add(-3 * time.Minute), Pod: "db-0", Container: "app", Text: "replacement error"}
			stub.add(namespace, oldLine, gapLine, newLine)
			query := breaklogs.Query{Namespace: namespace, RootUID: "root-ledger", From: manifest.From, To: manifest.To, Center: manifest.BreakAt, Container: breaklogs.AllContainers}
			page, err := replica.BreakLogs(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			owners := map[string]string{}
			for _, line := range page.Lines {
				owners[line.Text] = line.PodUID
			}
			if owners["old instance error"] != "uid-old" || owners["replacement error"] != "uid-new" || owners["unobserved interval line"] != "" || len(page.Lines) != 2 {
				t.Fatalf("same-name lifetimes mixed: %+v", page.Lines)
			}
			for _, gap := range page.Coverage.Gaps {
				if gap.Reason == "no_logs" {
					t.Fatalf("rejected unknown-lifetime line produced no_logs: %v", gapReasons(page))
				}
			}

			// Only an unattributable line exists: still not a truthful empty answer.
			ambiguousNamespace := "ledger-ambiguous"
			overlap := []breaklogs.ManifestPod{
				{Namespace: ambiguousNamespace, RootUID: "root-amb", Name: "db-0", UID: "uid-a", CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-3 * time.Minute), DeletedAt: t0.Add(-3 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}},
				{Namespace: ambiguousNamespace, RootUID: "root-amb", Name: "db-0", UID: "uid-b", CreatedAt: t0.Add(-5 * time.Minute), LastSeen: t0.Add(-90 * time.Second), DeletedAt: t0.Add(-90 * time.Second), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-5 * time.Minute)}}},
			}
			ambiguous := manifestFor(t, store, breaklogs.ManifestIdentity("root-amb", t0.Add(-4*time.Minute)), ambiguousNamespace, "root-amb", t0.Add(-4*time.Minute), t0, time.Time{}, overlap...)
			if err := store.CommitBreakLogs(nil, nil, "", "owner", t0, ambiguous); err != nil {
				t.Fatal(err)
			}
			stub.add(ambiguousNamespace, breaklogs.LinkedLine{At: t0.Add(-4 * time.Minute), Pod: "db-0", Container: "app", Text: "overlap line"})
			page, err = replica.BreakLogs(context.Background(), breaklogs.Query{Namespace: ambiguousNamespace, RootUID: "root-amb", From: ambiguous.From, To: ambiguous.To, Center: ambiguous.BreakAt, Container: breaklogs.AllContainers})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Lines) != 0 {
				t.Fatalf("overlapping lifetimes attributed a line: %+v", page.Lines)
			}
			for _, gap := range page.Coverage.Gaps {
				if gap.Reason == "no_logs" {
					t.Fatalf("overlap ambiguity reported as no_logs: %v", gapReasons(page))
				}
			}
			if page.Coverage.State == "complete" {
				t.Fatalf("ambiguous answer claimed complete coverage")
			}
		})
	}
}

// Two occurrences of the same root keep separate pinned windows and excerpts.
// Selecting one never shows the other's settled lines, even with a window
// that spans both, and ambiguous or mismatched selectors are refused.
func TestLinkedBreakRepeatedOccurrencesStayPinned(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			kube := linkedKube(t)
			stub := newLinkedSourceStub(t)
			cluster := linkedCluster("repeat-" + backend.name)
			zero := 0
			t0 := time.Now().UTC()
			replica := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &zero)
			store := replica.changeStore()
			if err := store.ClaimLease("owner", t0, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
			namespace := "repeat"
			pod := breaklogs.ManifestPod{Namespace: namespace, RootUID: "root-r", Name: "api-0", UID: "uid-r", CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-time.Minute), DeletedAt: t0.Add(-time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}}
			firstAt := t0.Add(-40*time.Minute + 7*time.Nanosecond)
			secondAt := t0.Add(-15*time.Minute + 11*time.Nanosecond)
			first := manifestFor(t, store, breaklogs.ManifestIdentity("root-r", firstAt), namespace, "root-r", firstAt, t0, t0.Add(-time.Second), pod)
			second := manifestFor(t, store, breaklogs.ManifestIdentity("root-r", secondAt), namespace, "root-r", secondAt, t0, t0.Add(-time.Second), pod)
			if err := store.CommitBreakLogs(nil, nil, "", "owner", t0, first, second); err != nil {
				t.Fatal(err)
			}
			stub.add(namespace,
				breaklogs.LinkedLine{At: firstAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "first occurrence"},
				breaklogs.LinkedLine{At: secondAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "second occurrence"})
			var running sync.WaitGroup
			replica.scheduleBreakSettles(context.Background(), "owner", &running)
			running.Wait()
			stub.expire(namespace)
			reopened := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &zero)
			for _, selected := range []struct {
				manifest breaklogs.Manifest
				want     string
			}{{first, "first occurrence"}, {second, "second occurrence"}} {
				page, err := reopened.BreakLogs(context.Background(), breaklogs.Query{Namespace: namespace, RootUID: "root-r", OccurrenceID: selected.manifest.ID, Center: selected.manifest.BreakAt, From: first.From, To: second.To, Container: breaklogs.AllContainers})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Lines) != 1 || page.Lines[0].Text != selected.want || page.OccurrenceID != selected.manifest.ID || !page.Window.From.Equal(first.From) || !page.Window.To.Equal(second.To) {
					t.Fatalf("occurrence %s mixed excerpts or moved window: %+v window=%+v", selected.want, page.Lines, page.Window)
				}
			}
			sameAt := t0.Add(-25 * time.Minute)
			dupA := manifestFor(t, store, "occurrence-a", namespace, "root-r", sameAt, t0, time.Time{}, pod)
			dupA.OccurrenceID = "occurrence-a"
			dupB := manifestFor(t, store, "occurrence-b", namespace, "root-r", sameAt, t0, time.Time{}, pod)
			dupB.OccurrenceID = "occurrence-b"
			if err := store.CommitBreakLogs(nil, nil, "", "owner", time.Now().UTC(), dupA, dupB); err != nil {
				t.Fatal(err)
			}
			if _, err := reopened.BreakLogs(context.Background(), breaklogs.Query{Namespace: namespace, RootUID: "root-r", Center: sameAt, From: dupA.From, To: dupA.To}); !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("ambiguous center accepted: %v", err)
			}
			if _, err := reopened.BreakLogs(context.Background(), breaklogs.Query{Namespace: namespace, RootUID: "root-r", OccurrenceID: "occurrence-a", Center: secondAt, From: second.From, To: second.To}); !errors.Is(err, ErrInvalidArguments) {
				t.Fatalf("mismatched occurrence and center accepted: %v", err)
			}
		})
	}
}

// A replica that loses the capture lease while its settle query is still in
// flight cannot commit the late answer. The new owner resumes from persisted
// progress, runs only the remaining attempt, and nothing runs a third time.
func TestLinkedSettleTakeoverFencesStaleOwnerAwaitingResponse(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: wait for real capture and settle leases to expire across replicas")
	}
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			kube := linkedKube(t)
			stub := newLinkedSourceStub(t)
			stub.hold, stub.arrived = make(chan struct{}), make(chan string, 4)
			cluster := linkedCluster("takeover-" + backend.name)
			zero := 0
			t0 := time.Now().UTC()
			replicaA := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &zero)
			storeA := replicaA.changeStore()
			if err := storeA.ClaimLease("replica-a", t0, 3*time.Second); err != nil {
				t.Fatal(err)
			}
			namespace := "takeover"
			breakAt := t0.Add(-11 * time.Minute)
			pod := breaklogs.ManifestPod{Namespace: namespace, RootUID: "root-t", Name: "api-0", UID: "uid-t", CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-8 * time.Minute), DeletedAt: t0.Add(-8 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}}
			manifest := manifestFor(t, storeA, breaklogs.ManifestIdentity("root-t", breakAt), namespace, "root-t", breakAt, t0, t0.Add(-time.Second), pod)
			if err := storeA.CommitBreakLogs(nil, nil, "", "replica-a", t0, manifest); err != nil {
				t.Fatal(err)
			}
			stub.add(namespace, breaklogs.LinkedLine{At: breakAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "late answer"})
			var runningA sync.WaitGroup
			replicaA.scheduleBreakSettles(context.Background(), "replica-a", &runningA)
			select {
			case <-stub.arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("first owner never queried the source")
			}
			sleepUntil(t0.Add(3*time.Second + 200*time.Millisecond))
			replicaB := linkedReplica(t, kube, cluster, backend.open(t), stub.server.URL, &zero)
			storeB := replicaB.changeStore()
			if err := storeB.ClaimLease("replica-b", time.Now().UTC(), 2*time.Minute); err != nil {
				t.Fatalf("takeover after lease expiry failed: %v", err)
			}
			stub.release()
			runningA.Wait()
			progress, excerpts := manifestByID(t, backend.open(t), replicaA, manifest.ID)
			if progress.Attempts != 1 || progress.Done || len(excerpts) != 0 {
				t.Fatalf("stale owner committed its late answer: attempts=%d done=%v excerpts=%d", progress.Attempts, progress.Done, len(excerpts))
			}
			deadline := time.Now().Add(45 * time.Second)
			for time.Now().Before(deadline) {
				var running sync.WaitGroup
				replicaB.scheduleBreakSettles(context.Background(), "replica-b", &running)
				running.Wait()
				progress, excerpts = manifestByID(t, backend.open(t), replicaA, manifest.ID)
				if progress.Done {
					break
				}
				time.Sleep(time.Second)
			}
			if !progress.Done || progress.Attempts != 2 || len(excerpts) != 1 || excerpts[0].Lines[0].Text != "late answer" {
				t.Fatalf("new owner did not finish the remaining attempt: attempts=%d done=%v excerpts=%+v", progress.Attempts, progress.Done, excerpts)
			}
			var again sync.WaitGroup
			replicaB.scheduleBreakSettles(context.Background(), "replica-b", &again)
			again.Wait()
			if requests := stub.count(namespace); requests != 2 {
				t.Fatalf("settle queried the source %d times, want 2", requests)
			}
		})
	}
}

// A database outage between claim and commit consumes the claimed attempt but
// loses nothing already stored; reads fail instead of answering empty, and
// after recovery exactly one more attempt runs.
func TestLinkedSettlePostgresOutageKeepsAttemptsBounded(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: Postgres outage proof did not run")
	}
	// ALTER DATABASE cannot target the connected database; administer from the
	// maintenance database of the same run-owned server.
	adminURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminURL.Path = "/postgres"
	admin, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	conn, err := admin.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	target, _ := url.Parse(dsn)
	database := strings.TrimPrefix(target.Path, "/")
	quoted := `"` + strings.ReplaceAll(database, `"`, `""`) + `"`
	restore := func() {
		if _, err := conn.ExecContext(context.Background(), "alter database "+quoted+" allow_connections true"); err != nil {
			t.Errorf("restore connections: %v", err)
		}
	}
	defer restore()
	open := func() storage.Provider {
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	kube := linkedKube(t)
	stub := newLinkedSourceStub(t)
	stub.hold, stub.arrived = make(chan struct{}), make(chan string, 4)
	cluster := linkedCluster("outage")
	zero := 0
	t0 := time.Now().UTC()
	replica := linkedReplica(t, kube, cluster, open(), stub.server.URL, &zero)
	store := replica.changeStore()
	if err := store.ClaimLease("owner", t0, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	namespace := "outage"
	breakAt := t0.Add(-11 * time.Minute)
	pod := breaklogs.ManifestPod{Namespace: namespace, RootUID: "root-o", Name: "api-0", UID: "uid-o", CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-8 * time.Minute), DeletedAt: t0.Add(-8 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}}
	keep := breaklogs.Snapshot{Cluster: cluster, Namespace: namespace, RootUID: "root-o", Pod: "api-0", PodUID: "uid-o", PodCreatedAt: pod.CreatedAt, Container: "app", Slot: "current", CapturedAt: t0, From: breakAt.Add(-5 * time.Minute), To: breakAt.Add(-4 * time.Minute), Lines: []breaklogs.Line{{At: breakAt.Add(-270 * time.Second), Text: "captured before outage"}}}
	keep.ID = breaklogs.Identity(keep)
	manifest := manifestFor(t, store, breaklogs.ManifestIdentity("root-o", breakAt), namespace, "root-o", breakAt, t0, t0.Add(-time.Second), pod)
	if err := store.CommitBreakLogs([]breaklogs.Snapshot{keep}, nil, "", "owner", t0, manifest); err != nil {
		t.Fatal(err)
	}
	stub.add(namespace, breaklogs.LinkedLine{At: breakAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "settled after recovery"})
	var running sync.WaitGroup
	replica.scheduleBreakSettles(context.Background(), "owner", &running)
	select {
	case <-stub.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("settle never queried the source")
	}
	if _, err := conn.ExecContext(context.Background(), "alter database "+quoted+" allow_connections false"); err != nil {
		t.Skipf("cannot inject a real outage (needs superuser): %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), "select pg_terminate_backend(pid) from pg_stat_activity where datname = $1 and pid <> pg_backend_pid()", database); err != nil {
		t.Fatal(err)
	}
	stub.release()
	running.Wait()
	_, readErr := replica.BreakLogs(context.Background(), breaklogs.Query{Namespace: namespace, RootUID: "root-o", From: manifest.From, To: manifest.To, Center: manifest.BreakAt, Container: breaklogs.AllContainers})
	if readErr == nil {
		t.Fatal("read during database outage answered instead of failing")
	}
	restore()
	progress, excerpts := manifestByID(t, open(), replica, manifest.ID)
	if progress.Attempts != 1 || progress.Done || len(excerpts) != 0 {
		t.Fatalf("outage commit was not cleanly lost: attempts=%d done=%v excerpts=%d", progress.Attempts, progress.Done, len(excerpts))
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) && !progress.Done {
		var again sync.WaitGroup
		replica.scheduleBreakSettles(context.Background(), "owner", &again)
		again.Wait()
		progress, excerpts = manifestByID(t, open(), replica, manifest.ID)
		time.Sleep(time.Second)
	}
	if !progress.Done || progress.Attempts != 2 || len(excerpts) != 1 {
		t.Fatalf("recovery did not finish with the remaining attempt: attempts=%d done=%v excerpts=%d", progress.Attempts, progress.Done, len(excerpts))
	}
	state, err := kubechanges.NewStore(open(), replica.indexScope()).BreakLogs(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range state.Snapshots {
		found = found || item.ID == keep.ID && len(item.Lines) == 1
	}
	if !found {
		t.Fatal("captured excerpt lost across outage")
	}
	if requests := stub.count(namespace); requests != 2 {
		t.Fatalf("source queried %d times, want 2", requests)
	}
}

// Replica processes sharing one Postgres never run more than two settle
// queries at once across the deployment. Every first attempt fails at the
// source; each manifest ends with exactly two persisted attempts.
func TestLinkedSettleDeploymentAdmissionAcrossReplicaProcesses(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: multi-process admission proof did not run")
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	stub := newLinkedSourceStub(t)
	stub.pause = 1500 * time.Millisecond
	kube := linkedKube(t)
	zero := 0
	t0 := time.Now().UTC()
	clusters := []string{linkedCluster("replica-one"), linkedCluster("replica-two")}
	ids := map[string][]string{}
	seeders := map[string]*Service{}
	for _, cluster := range clusters {
		seeders[cluster] = linkedReplica(t, kube, cluster, provider, stub.server.URL, &zero)
		store := kubechanges.NewStore(provider, seeders[cluster].indexScope())
		for position := 0; position < 3; position++ {
			namespace := fmt.Sprintf("adm-%d-%d", len(ids), position)
			breakAt := t0.Add(-11*time.Minute - time.Duration(position)*time.Second)
			pod := breaklogs.ManifestPod{Namespace: namespace, RootUID: "root", Name: "api-0", UID: "uid-" + namespace, CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-8 * time.Minute), DeletedAt: t0.Add(-8 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}}
			manifest := manifestFor(t, store, breaklogs.ManifestIdentity("root", breakAt), namespace, "root", breakAt, t0, t0.Add(-time.Second), pod)
			if err := store.CommitBreakLogs(nil, nil, "", "", t0, manifest); err != nil {
				t.Fatal(err)
			}
			stub.failures[namespace] = 1
			stub.add(namespace, breaklogs.LinkedLine{At: breakAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "second attempt line"})
			ids[cluster] = append(ids[cluster], manifest.ID)
		}
	}
	// A replica that died or lost its database mid-settle holds its deployment
	// slot until expiry (30s); start from a free deployment so overlap is measured.
	deadline := time.Now().Add(40 * time.Second)
	for {
		raw, err := provider.ReadBlob("kubernetes/break-settle-admission.json")
		live := 0
		if err == nil {
			var slots []struct {
				Until time.Time `json:"until"`
			}
			_ = json.Unmarshal(raw, &slots)
			for _, slot := range slots {
				if slot.Until.After(time.Now()) {
					live++
				}
			}
		}
		if live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d deployment settle slots never expired", live)
		}
		t.Logf("waiting for %d expiring deployment settle slot(s)", live)
		time.Sleep(time.Second)
	}
	commands := []*exec.Cmd{}
	logs := make([]strings.Builder, len(clusters))
	for position, cluster := range clusters {
		command := exec.Command(os.Args[0], "-test.run=^TestLinkedSettleReplicaProcess$", "-test.count=1", "-test.v")
		command.Env = append(os.Environ(), "LINKED_SETTLE_REPLICA_CLUSTER="+cluster, "LINKED_SETTLE_REPLICA_SOURCE="+stub.server.URL, "LINKED_SETTLE_REPLICA_KUBE="+kube, "LINKED_SETTLE_REPLICA_SECONDS=50")
		command.Stdout, command.Stderr = &logs[position], &logs[position]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for position, command := range commands {
		if err := command.Wait(); err != nil {
			t.Errorf("replica process %d failed: %v\n%s", position, err, logs[position].String())
		}
	}
	stub.mu.Lock()
	maxInFlight := stub.maxInFlight
	t.Logf("source timeline:\n%s", strings.Join(stub.events, "\n"))
	stub.mu.Unlock()
	for position := range logs {
		t.Logf("replica process %d output:\n%s", position, logs[position].String())
	}
	t.Logf("deployment-wide max concurrent settle queries: %d", maxInFlight)
	if maxInFlight > 2 {
		t.Errorf("deployment ran %d settle queries at once, limit is 2", maxInFlight)
	}
	if maxInFlight < 2 {
		t.Errorf("replicas never overlapped (max %d); admission not exercised", maxInFlight)
	}
	for _, cluster := range clusters {
		for _, id := range ids[cluster] {
			progress, excerpts := manifestByID(t, provider, seeders[cluster], id)
			if progress.Attempts != 2 || !progress.Done || len(excerpts) != 1 {
				t.Errorf("%s/%s: attempts=%d done=%v excerpts=%d", cluster, id[:8], progress.Attempts, progress.Done, len(excerpts))
			}
		}
	}
	for namespace := range stub.failures {
		if requests := stub.count(namespace); requests != 2 {
			t.Errorf("%s queried %d times, want 2", namespace, requests)
		}
	}
}

// Helper process for the multi-process admission proof; it is not a test on its own.
func TestLinkedSettleReplicaProcess(t *testing.T) {
	cluster := os.Getenv("LINKED_SETTLE_REPLICA_CLUSTER")
	if cluster == "" {
		t.Skip("replica helper process only")
	}
	seconds, _ := strconv.Atoi(os.Getenv("LINKED_SETTLE_REPLICA_SECONDS"))
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: os.Getenv("TEST_POSTGRES_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	zero := 0
	replica := linkedReplica(t, os.Getenv("LINKED_SETTLE_REPLICA_KUBE"), cluster, provider, os.Getenv("LINKED_SETTLE_REPLICA_SOURCE"), &zero)
	owner := "process-" + strconv.Itoa(os.Getpid())
	if err := replica.changeStore().ClaimLease(owner, time.Now().UTC(), 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	done := make(chan struct{})
	replica.runBreakSettles(ctx, owner, done)
	<-done
}

// Under a realistic flood of identity manifests, a newly settled excerpt must
// not be the first thing evicted to keep metadata within the state budget.
func TestLinkedBreakCapPressureKeepsSettledExcerpt(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			provider := backend.open(t)
			cluster := linkedCluster("pressure-" + backend.name)
			store := kubechanges.NewStore(provider, kubeindex.Scope{OrgID: "default", ClusterID: cluster})
			t0 := time.Now().UTC()
			if err := store.ClaimLease("owner", t0, 3*time.Minute); err != nil {
				t.Fatal(err)
			}
			manifests := []breaklogs.Manifest{}
			for position := 0; position < 600; position++ {
				namespace := fmt.Sprintf("team-%03d", position)
				root := fmt.Sprintf("root-%03d", position)
				breakAt := t0.Add(-20*time.Hour + time.Duration(position)*time.Minute)
				pods := []breaklogs.ManifestPod{}
				for replica := 0; replica < breaklogs.MaxManifestPods; replica++ {
					pods = append(pods, breaklogs.ManifestPod{Namespace: namespace, RootUID: root, Name: fmt.Sprintf("checkout-7d9f8c6b5-%05d", replica), UID: fmt.Sprintf("1b4e28ba-2fa1-11d2-883f-%03d%09d", position, replica), Node: "ip-10-0-12-34.ec2.internal", CreatedAt: breakAt.Add(-time.Hour), LastSeen: breakAt, Containers: []breaklogs.ManifestContainer{{Name: "app", Restarts: 3, StartedAt: breakAt.Add(-time.Minute), TerminationAt: breakAt.Add(-2 * time.Minute)}, {Name: "istio-proxy", StartedAt: breakAt.Add(-time.Hour)}, {Name: "istio-init", Init: true, StartedAt: breakAt.Add(-time.Hour), TerminationAt: breakAt.Add(-59 * time.Minute)}}})
				}
				manifests = append(manifests, breaklogs.Manifest{ID: breaklogs.ManifestIdentity(root, breakAt), Namespace: namespace, RootUID: root, BreakAt: breakAt, From: breakAt.Add(-5 * time.Minute), To: breakAt.Add(2 * time.Minute), CapturedAt: t0, Pods: pods, SettleAt: t0.Add(-time.Second)})
			}
			if err := store.CommitBreakLogs(nil, nil, "", "owner", t0, manifests...); err != nil {
				t.Fatal(err)
			}
			state, err := store.BreakLogs(t0)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(state)
			newest := state.Manifests[len(state.Manifests)-1]
			t.Logf("retained manifests %d of 600, break-log state %d bytes", len(state.Manifests), len(encoded))
			claimed, err := store.ClaimBreakSettle(newest.ID, "owner", time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			pod := newest.Pods[0]
			excerpt := breaklogs.Snapshot{Source: "linked", OccurrenceID: newest.ID, BreakAt: newest.BreakAt, Cluster: cluster, Namespace: newest.Namespace, RootUID: newest.RootUID, Pod: pod.Name, PodUID: pod.UID, PodCreatedAt: pod.CreatedAt, Container: "app", Slot: "previous", Restarts: 3, TerminationAt: pod.Containers[0].TerminationAt, From: newest.From, To: pod.Containers[0].TerminationAt, CapturedAt: time.Now().UTC(), Lines: []breaklogs.Line{{At: pod.Containers[0].TerminationAt.Add(-time.Second), Text: "panic: settlement ledger unavailable"}}}
			excerpt.ID = breaklogs.Identity(excerpt)
			if err := store.CommitBreakSettle(claimed, []breaklogs.Snapshot{excerpt}, false, "owner", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			// New breaks keep arriving after the excerpt was settled.
			later := []breaklogs.Manifest{}
			for position := 0; position < 10; position++ {
				next := manifests[position]
				next.BreakAt = t0.Add(-time.Duration(10-position) * time.Second)
				next.From, next.To = next.BreakAt.Add(-5*time.Minute), next.BreakAt.Add(2*time.Minute)
				next.RootUID = fmt.Sprintf("later-%d", position)
				next.ID = breaklogs.ManifestIdentity(next.RootUID, next.BreakAt)
				next.CapturedAt = time.Now().UTC()
				next.Pods = append([]breaklogs.ManifestPod(nil), next.Pods...)
				for i := range next.Pods {
					next.Pods[i].RootUID = next.RootUID
				}
				later = append(later, next)
			}
			if err := store.CommitBreakLogs(nil, nil, "", "owner", time.Now().UTC(), later...); err != nil {
				t.Fatal(err)
			}
			reopened := kubechanges.NewStore(backend.open(t), kubeindex.Scope{OrgID: "default", ClusterID: cluster})
			after, err := reopened.BreakLogs(time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			kept, rotated := false, 0
			for _, item := range after.Snapshots {
				kept = kept || item.ID == excerpt.ID
			}
			for _, gap := range after.Gaps {
				if gap.Reason == "rotated" && gap.RootUID == newest.RootUID {
					rotated++
				}
			}
			encoded, _ = json.Marshal(after)
			t.Logf("after settle: manifests %d snapshots %d state %d bytes rotated-gaps-for-root %d", len(after.Manifests), len(after.Snapshots), len(encoded), rotated)
			if !kept {
				t.Fatalf("settled excerpt evicted by the next identity manifests while %d manifests kept (%d bytes)", len(after.Manifests), len(encoded))
			}
		})
	}
}

// The detail cache never outlives a source switch or another replica's
// settle commit.
func TestLinkedBreakCacheSourceSwitchAndCrossReplicaSettle(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			clearLinkedCache()
			kube := linkedKube(t)
			primary, secondary := newLinkedSourceStub(t), newLinkedSourceStub(t)
			cluster := linkedCluster("cache-" + backend.name)
			zero := 0
			t0 := time.Now().UTC().Truncate(time.Second)
			sources := func(service *Service) {
				service.SetBreakLogSources(func(string) []breaklogs.Source {
					return []breaklogs.Source{
						{Name: "logs", Type: "loki", CacheKey: "stub:" + primary.server.URL, Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
							return linkedStubReader{primary.server.URL}, nil
						}},
						{Name: "logs-b", Type: "elasticsearch", CacheKey: "stub:" + secondary.server.URL, Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
							return linkedStubReader{secondary.server.URL}, nil
						}},
					}
				})
			}
			reader := linkedReplica(t, kube, cluster, backend.open(t), primary.server.URL, &zero)
			sources(reader)
			writer := linkedReplica(t, kube, cluster, backend.open(t), primary.server.URL, &zero)
			sources(writer)
			store := writer.changeStore()
			if err := store.ClaimLease("writer", t0, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
			namespace := "cache"
			breakAt := t0.Add(-30 * time.Minute)
			pod := breaklogs.ManifestPod{Namespace: namespace, RootUID: "root-c", Name: "api-0", UID: "uid-c", CreatedAt: t0.Add(-2 * time.Hour), LastSeen: t0.Add(-time.Minute), DeletedAt: t0.Add(-time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-2 * time.Hour)}}}
			manifest := manifestFor(t, store, breaklogs.ManifestIdentity("root-c", breakAt), namespace, "root-c", breakAt, t0, t0.Add(-time.Second), pod)
			if err := store.CommitBreakLogs(nil, nil, "", "writer", t0, manifest); err != nil {
				t.Fatal(err)
			}
			primary.add(namespace, breaklogs.LinkedLine{At: breakAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "from primary"})
			secondary.add(namespace, breaklogs.LinkedLine{At: breakAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "from secondary"})
			query := breaklogs.Query{Namespace: namespace, RootUID: "root-c", From: manifest.From, To: manifest.To, Center: manifest.BreakAt, Container: breaklogs.AllContainers}
			read := func() BreakLogPage {
				t.Helper()
				page, err := reader.BreakLogs(context.Background(), query)
				if err != nil {
					t.Fatal(err)
				}
				return page
			}
			read()
			read()
			if primary.count(namespace) != 1 {
				t.Fatalf("mature answer not cached: %d requests", primary.count(namespace))
			}
			if err := writer.SaveAgentSettings(context.Background(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs-b", IngestDelay: &zero}}); err != nil {
				t.Fatal(err)
			}
			page := read()
			if secondary.count(namespace) != 1 || len(page.Lines) != 1 || page.Lines[0].Text != "from secondary" {
				t.Fatalf("source switch served the old source's cached answer: %+v", page.Lines)
			}
			if err := writer.SaveAgentSettings(context.Background(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &zero}}); err != nil {
				t.Fatal(err)
			}
			if page := read(); len(page.Lines) != 1 || page.Lines[0].Text != "from primary" {
				t.Fatalf("switch back lost primary answer: %+v", page.Lines)
			}
			// A line ingested after this replica cached its answer becomes visible as
			// soon as another replica settles, not after the 5 minute cache TTL.
			primary.add(namespace, breaklogs.LinkedLine{At: breakAt.Add(-30 * time.Second), Pod: "api-0", Container: "app", Text: "ingested later"})
			var running sync.WaitGroup
			writer.scheduleBreakSettles(context.Background(), "writer", &running)
			running.Wait()
			if page := read(); len(page.Lines) != 2 {
				t.Fatalf("another replica's settle stayed hidden behind this replica's cache: %+v", page.Lines)
			}
		})
	}
}

// Replicas never have perfectly equal clocks. Evidence written by a replica
// whose clock runs two seconds ahead must not make another replica's detail
// reads or capture commits fail on the shared store.
func TestLinkedBreakReplicaClockSkewKeepsSharedStoreUsable(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			cluster := linkedCluster("skew-" + backend.name)
			scope := kubeindex.Scope{OrgID: "default", ClusterID: cluster}
			ahead := kubechanges.NewStore(backend.open(t), scope)
			behind := kubechanges.NewStore(backend.open(t), scope)
			aheadNow := time.Now().UTC().Add(2 * time.Second)
			item := breaklogs.Snapshot{Source: "linked", OccurrenceID: "occurrence", Cluster: cluster, Namespace: "skew", RootUID: "root-s", Pod: "api-0", PodUID: "uid-s", Container: "app", Slot: "current", CapturedAt: aheadNow, From: aheadNow.Add(-time.Minute), To: aheadNow, Lines: []breaklogs.Line{{At: aheadNow.Add(-time.Second), Text: "written by the faster clock"}}}
			item.ID = breaklogs.Identity(item)
			if err := ahead.CommitBreakLogs([]breaklogs.Snapshot{item}, nil, "", "", aheadNow); err != nil {
				t.Fatal(err)
			}
			if _, err := behind.BreakLogs(time.Now().UTC()); err != nil {
				t.Errorf("detail read on the slower replica failed: %v", err)
			}
			pod := breaklogs.ManifestPod{Namespace: "skew", RootUID: "root-s", Name: "api-0", UID: "uid-s", CreatedAt: time.Now().UTC().Add(-time.Hour), LastSeen: time.Now().UTC().Add(-time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app"}}}
			now := time.Now().UTC()
			manifest := breaklogs.Manifest{ID: "skew-manifest", Namespace: "skew", RootUID: "root-s", BreakAt: now.Add(-time.Minute), From: now.Add(-6 * time.Minute), To: now.Add(time.Minute), CapturedAt: now, Pods: []breaklogs.ManifestPod{pod}}
			if err := behind.CommitBreakLogs(nil, nil, "", "", now, manifest); err != nil {
				t.Errorf("capture commit on the slower replica failed: %v", err)
			}
		})
	}
}

// One replica alone may run two settle queries at once; this isolates the
// per-process limiter from the deployment-wide one.
func TestLinkedSettleSingleReplicaConcurrency(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) { linkedSingleReplicaConcurrency(t, backend.open(t)) })
	}
}

func linkedSingleReplicaConcurrency(t *testing.T, provider storage.Provider) {
	kube := linkedKube(t)
	stub := newLinkedSourceStub(t)
	stub.pause = time.Second
	zero := 0
	cluster := linkedCluster("single")
	replica := linkedReplica(t, kube, cluster, provider, stub.server.URL, &zero)
	store := replica.changeStore()
	t0 := time.Now().UTC()
	if err := store.ClaimLease("owner", t0, time.Minute); err != nil {
		t.Fatal(err)
	}
	for position := 0; position < 3; position++ {
		namespace := fmt.Sprintf("single-%d", position)
		breakAt := t0.Add(-11*time.Minute - time.Duration(position)*time.Second)
		pod := breaklogs.ManifestPod{Namespace: namespace, RootUID: "root", Name: "api-0", UID: "uid-" + namespace, CreatedAt: t0.Add(-time.Hour), LastSeen: t0.Add(-8 * time.Minute), DeletedAt: t0.Add(-8 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}}
		if err := store.CommitBreakLogs(nil, nil, "", "owner", t0, manifestFor(t, store, breaklogs.ManifestIdentity("root", breakAt), namespace, "root", breakAt, t0, t0.Add(-time.Second), pod)); err != nil {
			t.Fatal(err)
		}
	}
	var running sync.WaitGroup
	started := time.Now()
	replica.scheduleBreakSettles(context.Background(), "owner", &running)
	running.Wait()
	stub.mu.Lock()
	maxInFlight, total := stub.maxInFlight, 0
	for _, count := range stub.requests {
		total += count
	}
	stub.mu.Unlock()
	t.Logf("one schedule pass: %d source requests, max %d concurrent, %s", total, maxInFlight, time.Since(started))
	if maxInFlight != 2 {
		t.Errorf("one replica ran %d settle queries at once, want 2", maxInFlight)
	}
}

// More than 2,000 unrelated live Pods must not evict a target root's deleted
// Pod identity or poison every manifest as truncated (the former 2,000 count
// cap). Beyond the metadata byte budget, loss stays scoped to the roots whose
// history was trimmed: only their manifests are truncated, others are not.
func TestLinkedBreakUnrelatedPodFloodKeepsScopedIdentity(t *testing.T) {
	for _, flood := range []struct {
		name       string
		pods       int
		wantNoLoss bool
		broken     bool
	}{{"2100-within-budget", 2100, true, false}, {"5000-over-budget", 5000, false, false}, {"5000-broken-priority", 5000, false, true}} {
		for _, backend := range linkedBackends(t) {
			t.Run(flood.name+"/"+backend.name, func(t *testing.T) {
				kube := linkedKube(t)
				cluster := linkedCluster("flood-" + backend.name)
				zero := 0
				replica := linkedReplica(t, kube, cluster, backend.open(t), "http://127.0.0.1:1", &zero)
				store := replica.changeStore()
				t0 := time.Now().UTC()
				if err := store.ClaimLease("owner", t0, 3*time.Minute); err != nil {
					t.Fatal(err)
				}
				target := breaklogs.ManifestPod{Namespace: "orders", RootUID: "root-target", Name: "db-0", UID: "uid-target", Node: "ip-10-0-1-1.ec2.internal", CreatedAt: t0.Add(-time.Hour), Containers: []breaklogs.ManifestContainer{{Name: "app"}}}
				if flood.broken {
					target.Containers[0].Restarts = 2
					target.Containers[0].StartedAt = t0.Add(-4 * time.Minute)
					target.Containers[0].TerminationAt = t0.Add(-3*time.Minute - 30*time.Second)
				}
				unrelated := []breaklogs.ManifestPod{}
				for position := 0; position < flood.pods; position++ {
					rootIndex := position / 2
					unrelated = append(unrelated, breaklogs.ManifestPod{Namespace: fmt.Sprintf("tenant-%03d", rootIndex%250), RootUID: fmt.Sprintf("rs-%04d", rootIndex), Name: fmt.Sprintf("web-7d9f8c6b5-%05d", position), UID: fmt.Sprintf("1b4e28ba-2fa1-11d2-883f-%012d", position), Node: "ip-10-0-12-34.ec2.internal", Broken: position%100 == 99, CreatedAt: t0.Add(-2 * time.Hour), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-2 * time.Hour)}, {Name: "istio-proxy", StartedAt: t0.Add(-2 * time.Hour)}}})
				}
				if err := store.ObserveBreakPods(append(append([]breaklogs.ManifestPod(nil), unrelated...), target), "owner", t0.Add(-80*time.Second)); err != nil {
					t.Fatal(err)
				}
				// The target Pod is deleted; the unrelated Pods are still live.
				if err := store.ObserveBreakPods(unrelated, "owner", t0.Add(-70*time.Second)); err != nil {
					t.Fatal(err)
				}
				afterDeletion, err := store.BreakLogs(t0)
				if err != nil {
					t.Fatal(err)
				}
				deletedManifest := replica.newBreakManifest(context.Background(), kubebreaks.Break{Root: kref.Ref{Kind: "StatefulSet", Namespace: "orders", Name: "db", UID: "root-target"}, Rule: "crashloop", BrokeAt: t0.Add(-3*time.Minute - 15*time.Second)}, afterDeletion, t0)
				if len(deletedManifest.Pods) == 0 && !deletedManifest.Truncated {
					t.Fatal("healthy target deleted before detection silently lost at next inventory")
				}
				if err := store.ObserveBreakPods(unrelated, "owner", t0.Add(-60*time.Second)); err != nil {
					t.Fatal(err)
				}
				state, err := kubechanges.NewStore(backend.open(t), replica.indexScope()).BreakLogs(t0)
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(state)
				lostRoots := map[string]bool{}
				sampledRoots := map[string]bool{}
				for _, gap := range state.PodHistoryGaps {
					if gap.Reason == "sampled" {
						if gap.To.After(t0.Add(-60*time.Second)) || gap.From.After(gap.To) || gap.PodUID != "" {
							t.Fatalf("sampling invented UID lifetime or future coverage loss: %+v", gap)
						}
					}
					sampledRoots[gap.Namespace+"/"+gap.RootUID] = true
					lostRoots[gap.Namespace+"/"+gap.RootUID] = true
				}
				targetKept := false
				keptUIDs := map[string]bool{}
				for _, pod := range state.PodHistory {
					targetKept = targetKept || pod.UID == "uid-target"
					keptUIDs[pod.UID] = true
				}
				t.Logf("history kept %d of %d Pods, %d scoped markers (%d sampled roots), retired target kept=%v, break-log state %d bytes", len(state.PodHistory), flood.pods+1, len(state.PodHistoryGaps), len(sampledRoots), targetKept, len(encoded))
				metadata := state
				metadata.Snapshots = nil
				rawMetadata, _ := json.Marshal(metadata)
				if len(rawMetadata) > breaklogs.MaxMetadataBytes || (flood.wantNoLoss || flood.broken) && !targetKept {
					t.Fatalf("metadata cap or retired priority violated: bytes=%d target=%v", len(rawMetadata), targetKept)
				}
				for _, pod := range unrelated {
					if pod.Broken && !keptUIDs[pod.UID] {
						t.Fatalf("healthy lexical bias displaced broken identity %s", pod.UID)
					}
					if !keptUIDs[pod.UID] && !sampledRoots[pod.Namespace+"/"+pod.RootUID] && !sampledRoots[pod.Namespace+"/"] {
						t.Fatalf("capped live identity %s disappeared without scoped sampling marker", pod.UID)
					}
				}
				if len(encoded) > breaklogs.MaxBytes {
					t.Fatalf("break-log state %d bytes exceeds the %d cap", len(encoded), breaklogs.MaxBytes)
				}
				breakAt := t0.Add(-3*time.Minute - 15*time.Second)
				manifest := replica.newBreakManifest(context.Background(), kubebreaks.Break{Root: kref.Ref{Kind: "StatefulSet", Namespace: "orders", Name: "db", UID: "root-target"}, Rule: "crashloop", BrokeAt: breakAt}, state, t0)
				if flood.wantNoLoss {
					if len(state.PodHistoryGaps) != 0 || len(state.PodHistory) != flood.pods+1 {
						t.Fatalf("%d Pods above the old 2000 cap lost history: kept %d, loss markers %d", flood.pods+1, len(state.PodHistory), len(state.PodHistoryGaps))
					}
					if manifest.Truncated || len(manifest.Pods) != 1 || manifest.Pods[0].UID != "uid-target" || !manifest.Pods[0].DeletedAt.Equal(t0.Add(-80*time.Second)) {
						t.Fatalf("target identity lost or truncated under unrelated flood: truncated=%v pods=%+v", manifest.Truncated, manifest.Pods)
					}
				}
				if manifest.Truncated != (lostRoots["orders/root-target"] || lostRoots["orders/"]) || len(manifest.Pods) == 0 && !manifest.Truncated {
					t.Fatalf("target manifest truncated=%v pods=%d but loss marker=%v: identity loss not reported honestly", manifest.Truncated, len(manifest.Pods), lostRoots["orders/root-target"])
				}
				// Sampling is partial coverage, not a fabricated vanished UID.
				var keptRoot, lostRoot breaklogs.ManifestPod
				for _, pod := range state.PodHistory {
					if pod.RootUID != "root-target" && !sampledRoots[pod.Namespace+"/"+pod.RootUID] {
						keptRoot = pod
						break
					}
				}
				for _, pod := range unrelated {
					if sampledRoots[pod.Namespace+"/"+pod.RootUID] {
						lostRoot = pod
						break
					}
				}
				checks := []struct {
					pod  breaklogs.ManifestPod
					want bool
				}{{keptRoot, false}}
				if !flood.wantNoLoss {
					checks = append(checks, struct {
						pod  breaklogs.ManifestPod
						want bool
					}{lostRoot, true})
				}
				for _, check := range checks {
					if check.pod.UID == "" {
						t.Fatalf("no unrelated root for truncation=%v check", check.want)
					}
					other := replica.newBreakManifest(context.Background(), kubebreaks.Break{Root: kref.Ref{Kind: "ReplicaSet", Namespace: check.pod.Namespace, Name: "web", UID: check.pod.RootUID}, Rule: "crashloop", BrokeAt: breakAt}, state, t0)
					if other.Truncated != check.want {
						t.Fatalf("root %s/%s truncated=%v, want %v", check.pod.Namespace, check.pod.RootUID, other.Truncated, check.want)
					}
				}
				if err := store.CommitBreakLogs(nil, nil, "", "owner", t0, manifest); err != nil {
					t.Fatal(err)
				}
				persisted, _ := manifestByID(t, backend.open(t), replica, manifest.ID)
				if persisted.Truncated != manifest.Truncated || len(persisted.Pods) != len(manifest.Pods) {
					t.Fatalf("restart changed target manifest identity: %+v", persisted)
				}
				page, err := replica.BreakLogs(context.Background(), breaklogs.Query{Namespace: "orders", RootUID: "root-target", From: manifest.From, To: manifest.To, Center: manifest.BreakAt, Container: breaklogs.AllContainers})
				if err != nil {
					t.Fatal(err)
				}
				if !targetKept {
					budget := false
					for _, gap := range page.Coverage.Gaps {
						budget = budget || gap.Reason == "budget"
					}
					if !manifest.Truncated || !budget || page.Coverage.State == "complete" || page.Coverage.Reason == "no_logs" {
						t.Fatalf("healthy deleted identity loss looks complete: manifest=%+v coverage=%+v", manifest, page.Coverage)
					}
				} else if len(manifest.Pods) != 1 || manifest.Pods[0].UID != target.UID || !manifest.Pods[0].DeletedAt.Equal(t0.Add(-80*time.Second)) {
					t.Fatalf("surviving target lost validated UID lifetime: %+v", manifest.Pods)
				}
			})
		}
	}
}

func TestLinkedBreakDurableIndependentMetadataAndLiveSamplingCaps(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			now := time.Now().UTC()
			provider := backend.open(t)
			scope := kubeindex.Scope{OrgID: "default", ClusterID: linkedCluster("reserved-" + backend.name)}
			store := kubechanges.NewStore(provider, scope)
			if err := store.ClaimLease("owner", now, 3*time.Minute); err != nil {
				t.Fatal(err)
			}
			pod := breaklogs.ManifestPod{Namespace: "shop", RootUID: "root", Name: "old", UID: "deleted", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-time.Minute), DeletedAt: now.Add(-time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app"}}}
			manifest := breaklogs.Manifest{ID: "pending", Namespace: "shop", RootUID: "root", BreakAt: now.Add(-time.Minute), From: now.Add(-5 * time.Minute), To: now.Add(time.Minute), CapturedAt: now, SettleAt: now.Add(90 * time.Second), Pods: []breaklogs.ManifestPod{pod}}
			incoming := []breaklogs.Snapshot{}
			for i := 0; i < 137; i++ {
				at := now.Add(time.Duration(i-137) * time.Second)
				item := breaklogs.Snapshot{Cluster: "cluster", Namespace: "shop", RootUID: "root", Pod: fmt.Sprint(i), PodUID: fmt.Sprint(i), Container: "app", Slot: "previous", CapturedAt: at, From: at.Add(-time.Minute), To: at}
				for j := 0; j < 15; j++ {
					item.Lines = append(item.Lines, breaklogs.Line{At: at, Text: strings.Repeat("x", 4096)})
				}
				item.ID = breaklogs.Identity(item)
				incoming = append(incoming, item)
			}
			if err := store.CommitBreakLogs(incoming, nil, "", "owner", now, manifest); err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 3; pass++ {
				pods := []breaklogs.ManifestPod{}
				for i := 0; i < 60+pass; i++ {
					pods = append(pods, breaklogs.ManifestPod{Namespace: "shop", RootUID: "live-root", Name: fmt.Sprint(i), UID: fmt.Sprintf("uid%02d", i), Broken: i == 59, CreatedAt: now.Add(-time.Hour), Containers: []breaklogs.ManifestContainer{{Name: "app"}}})
				}
				at := now.Add(time.Duration(pass) * time.Second)
				if err := store.ObserveBreakPods(pods, "owner", at); err != nil {
					t.Fatal(err)
				}
				// Reopen through storage, not the in-memory return value.
				state, err := kubechanges.NewStore(backend.open(t), scope).BreakLogs(at)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(state)
				metadata := state
				metadata.Snapshots = nil
				metaRaw, _ := json.Marshal(metadata)
				broken := false
				for _, pod := range state.PodHistory {
					broken = broken || pod.UID == "uid59"
				}
				if len(raw) > breaklogs.MaxBytes || len(metaRaw) > breaklogs.MaxMetadataBytes || len(state.Manifests) != 1 || len(state.Manifests[0].Pods) != 1 || state.Manifests[0].Pods[0].UID != "deleted" || !broken || len(state.Snapshots) >= 137 || len(state.Snapshots) == 0 || state.Snapshots[len(state.Snapshots)-1].ID != incoming[len(incoming)-1].ID {
					t.Fatalf("durable cap/priority regression: total=%d metadata=%d logs=%d manifest=%d broken=%v", len(raw), len(metaRaw), len(state.Snapshots), len(state.Manifests), broken)
				}
				for _, gap := range state.PodHistoryGaps {
					if gap.RootUID != "live-root" || (gap.Reason != "sampled" && gap.Reason != "budget") || !gap.From.Equal(now.Add(-time.Hour)) || !gap.To.Equal(at) || gap.PodUID != "" {
						t.Fatalf("live sampling lost scoped coverage or fabricated UID lifetime: %+v", gap)
					}
				}
				t.Logf("pass=%d total=%d metadata=%d retained logs=%d history=%d, pending deleted identity and newest excerpt preserved", pass, len(raw), len(metaRaw), len(state.Snapshots), len(state.PodHistory))
			}
		})
	}
}

// A healthy Pod is deterministically evicted by the per-root cap, then deleted
// before detection. Loss must span the unseen interval, survive another tick
// and a real-store reopen, and never masquerade as a complete empty answer.
func TestLinkedBreakHealthyEvictedPodDeletedBetweenCommittedInventories(t *testing.T) {
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			kube := linkedKube(t)
			stub := newLinkedSourceStub(t)
			zero := 0
			replica := linkedReplica(t, kube, linkedCluster("healthy-evicted-"+backend.name), backend.open(t), stub.server.URL, &zero)
			now := time.Now().UTC()
			first, second := now.Add(-10*time.Minute), now.Add(-4*time.Minute)
			target := breaklogs.ManifestPod{Namespace: "orders", RootUID: "root-target", Name: "db-0", UID: "uid-target", CreatedAt: now.Add(-time.Hour), Containers: []breaklogs.ManifestContainer{{Name: "app"}}}
			live := []breaklogs.ManifestPod{}
			for i := 0; i < breaklogs.MaxHistoryPodsPerRoot; i++ {
				pod := target
				pod.Name, pod.UID = fmt.Sprintf("db-%d", i+1), fmt.Sprintf("a%03d", i)
				live = append(live, pod)
			}
			store := replica.changeStore()
			if err := store.ClaimLease("owner", now, 3*time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := store.ObserveBreakPods(append(append([]breaklogs.ManifestPod(nil), live...), target), "owner", first); err != nil {
				t.Fatal(err)
			}
			initial, err := store.BreakLogs(now)
			if err != nil {
				t.Fatal(err)
			}
			for _, pod := range initial.PodHistory {
				if pod.UID == target.UID {
					t.Fatal("test did not evict the healthy original target")
				}
			}
			root := kubebreaks.Break{Root: kref.Ref{Kind: "StatefulSet", Namespace: "orders", Name: "db", UID: target.RootUID}, Rule: "crashloop", BrokeAt: second.Add(-30 * time.Second)}
			for _, at := range []time.Time{second, second.Add(time.Minute)} {
				if err := store.ObserveBreakPods(nil, "owner", at); err != nil {
					t.Fatal(err)
				}
				state, err := kubechanges.NewStore(backend.open(t), replica.indexScope()).BreakLogs(now)
				if err != nil {
					t.Fatal(err)
				}
				manifest := replica.newBreakManifest(context.Background(), root, state, now)
				if !manifest.From.After(first) || !manifest.Truncated || len(manifest.Pods) != 0 {
					t.Fatalf("unseen disappearance interval lost: first=%s manifest=%+v gaps=%+v", first, manifest, state.PodHistoryGaps)
				}
				for _, gap := range state.PodHistoryGaps {
					if !gap.To.Equal(second) || gap.PodUID != "" {
						t.Fatalf("closed eviction window changed on later tick: %+v", gap)
					}
				}
				for _, pod := range manifest.Pods {
					if pod.UID == target.UID {
						t.Fatal("fabricated an evicted healthy UID lifetime")
					}
				}
				for _, other := range []kubebreaks.Break{
					{Root: kref.Ref{Namespace: "orders", UID: "unrelated-root"}, BrokeAt: root.BrokeAt},
					{Root: kref.Ref{Namespace: "unrelated", UID: target.RootUID}, BrokeAt: root.BrokeAt},
					{Root: root.Root, BrokeAt: now.Add(6 * time.Minute)},
					{Root: root.Root, BrokeAt: now.Add(-2 * time.Hour)},
				} {
					if replica.newBreakManifest(context.Background(), other, state, now).Truncated {
						t.Fatalf("unrelated scope or nonintersecting window poisoned: %+v", other)
					}
				}
			}
			state, err := store.BreakLogs(now)
			if err != nil {
				t.Fatal(err)
			}
			manifest := replica.newBreakManifest(context.Background(), root, state, now)
			if err := store.CommitBreakLogs(nil, nil, "", "owner", now, manifest); err != nil {
				t.Fatal(err)
			}
			reopened := linkedReplica(t, kube, replica.indexScope().ClusterID, backend.open(t), stub.server.URL, &zero)
			page, err := reopened.BreakLogs(context.Background(), breaklogs.Query{Namespace: target.Namespace, RootUID: target.RootUID, From: manifest.From, To: manifest.To, Center: manifest.BreakAt, Container: breaklogs.AllContainers})
			if err != nil {
				t.Fatal(err)
			}
			budget := false
			for _, gap := range page.Coverage.Gaps {
				budget = budget || gap.Reason == "budget"
			}
			if !budget || page.Coverage.State == "complete" || page.Coverage.Reason == "no_logs" {
				t.Fatalf("evicted healthy identity loss looks complete after reopen: %+v", page.Coverage)
			}
			t.Logf("healthy target evicted; persistent scoped loss %s..%s, query coverage=%s gaps=%v", first, second, page.Coverage.State, gapReasons(page))
		})
	}
}

// breakWriteCounter counts full-state writes made through the real store.
type breakWriteCounter struct {
	storage.Provider
	mu     sync.Mutex
	writes map[string]int
	bytes  map[string]int
}

func (counter *breakWriteCounter) count(key string, size int) {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counter.writes[key]++
	counter.bytes[key] += size
}

func (counter *breakWriteCounter) WriteBlob(key string, data []byte) error {
	counter.count(key, len(data))
	return counter.Provider.WriteBlob(key, data)
}

func (counter *breakWriteCounter) CompareAndSwapBlob(key string, old, next []byte) (bool, error) {
	counter.count(key, len(next))
	return counter.Provider.(storage.BlobCAS).CompareAndSwapBlob(key, old, next)
}

func (counter *breakWriteCounter) total() (int, int) {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	writes, bytes := 0, 0
	for key := range counter.writes {
		writes += counter.writes[key]
		bytes += counter.bytes[key]
	}
	return writes, bytes
}

// Idle ticks are bounded on real storage: an unchanged inventory publication
// writes at most one state blob and never grows it, and the demand settle
// loop writes nothing and claims no lease while the linked source is removed
// or its ingestion delay was raised. Once eligible, demand settles once.
func TestLinkedBreakIdleTicksBoundedWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: observe idle demand loops over multiple real scheduler ticks")
	}
	for _, backend := range linkedBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			kube := linkedKube(t)
			stub := newLinkedSourceStub(t)
			counter := &breakWriteCounter{Provider: backend.open(t), writes: map[string]int{}, bytes: map[string]int{}}
			cluster := linkedCluster("idle-" + backend.name)
			zero := 0
			replica := linkedReplica(t, kube, cluster, counter, stub.server.URL, &zero)
			store := replica.changeStore()
			t0 := time.Now().UTC()
			if err := store.ClaimLease("inventory", t0, time.Minute); err != nil {
				t.Fatal(err)
			}
			pods := []breaklogs.ManifestPod{}
			for position := 0; position < 200; position++ {
				pods = append(pods, breaklogs.ManifestPod{Namespace: "idle", RootUID: fmt.Sprintf("rs-%03d", position/4), Name: fmt.Sprintf("web-%03d", position), UID: fmt.Sprintf("uid-idle-%03d", position), CreatedAt: t0.Add(-time.Hour), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: t0.Add(-time.Hour)}}})
			}
			if err := store.ObserveBreakPods(pods, "inventory", t0.Add(-10*time.Second)); err != nil {
				t.Fatal(err)
			}
			base, _ := counter.total()
			sizes := []int{}
			for tick := 1; tick <= 5; tick++ {
				before, _ := counter.total()
				if err := store.ObserveBreakPods(pods, "inventory", t0.Add(time.Duration(tick-10)*time.Second)); err != nil {
					t.Fatal(err)
				}
				after, _ := counter.total()
				if after-before > 1 {
					t.Fatalf("idle inventory tick %d wrote %d state blobs", tick, after-before)
				}
				state, err := store.BreakLogs(time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				encoded, _ := json.Marshal(state)
				sizes = append(sizes, len(encoded))
			}
			idleWrites, idleBytes := counter.total()
			t.Logf("5 idle inventory ticks: %d writes, %d bytes written in total since setup; break-log state sizes %v", idleWrites-base, idleBytes, sizes)
			for _, size := range sizes[1:] {
				if size != sizes[0] {
					t.Fatalf("idle ticks grew break-log state: %v", sizes)
				}
			}
			if err := store.ReleaseLease("inventory"); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			breakAt := now.Add(-11 * time.Minute)
			pod := breaklogs.ManifestPod{Namespace: "idle-break", RootUID: "root-i", Name: "api-0", UID: "uid-i", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-8 * time.Minute), DeletedAt: now.Add(-8 * time.Minute), Containers: []breaklogs.ManifestContainer{{Name: "app", StartedAt: now.Add(-time.Hour)}}}
			manifest := manifestFor(t, store, breaklogs.ManifestIdentity("root-i", breakAt), "idle-break", "root-i", breakAt, now, now.Add(-time.Second), pod)
			if err := store.CommitBreakLogs(nil, nil, "", "", now, manifest); err != nil {
				t.Fatal(err)
			}
			stub.add("idle-break", breaklogs.LinkedLine{At: breakAt.Add(-time.Minute), Pod: "api-0", Container: "app", Text: "eligible demand line"})
			runDemand := func(name string, seconds time.Duration) (int, int) {
				before, beforeBytes := counter.total()
				ctx, cancel := context.WithTimeout(context.Background(), seconds)
				defer cancel()
				loop := &captureLoop{ctx: ctx, owner: "demand-" + name, done: make(chan struct{})}
				replica.runDemandBreakSettles(loop)
				after, afterBytes := counter.total()
				return after - before, afterBytes - beforeBytes
			}
			replica.SetBreakLogSources(func(string) []breaklogs.Source { return nil })
			if writes, size := runDemand("removed", 3200*time.Millisecond); writes != 0 {
				t.Fatalf("source removed: demand loop wrote %d blobs (%d bytes) over ~3 ticks", writes, size)
			}
			if err := store.CheckCaptureLease(""); err != nil {
				t.Fatalf("source removed: idle demand loop holds the capture lease: %v", err)
			}
			replica.SetBreakLogSources(func(string) []breaklogs.Source {
				return []breaklogs.Source{{Name: "logs", Type: "loki", CacheKey: "stub:" + stub.server.URL, Build: func(breaklogs.Fields) (breaklogs.LinkedReader, error) {
					return linkedStubReader{stub.server.URL}, nil
				}}}
			})
			raised := 3600
			if err := replica.SaveAgentSettings(context.Background(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &raised}}); err != nil {
				t.Fatal(err)
			}
			if writes, size := runDemand("raised", 3200*time.Millisecond); writes != 0 {
				t.Fatalf("delay raised: demand loop wrote %d blobs (%d bytes) over ~3 ticks", writes, size)
			}
			if err := store.CheckCaptureLease(""); err != nil {
				t.Fatalf("delay raised: idle demand loop holds the capture lease: %v", err)
			}
			if stub.count("idle-break") != 0 {
				t.Fatalf("ineligible demand queried the source %d times", stub.count("idle-break"))
			}
			if err := replica.SaveAgentSettings(context.Background(), breaklogs.Settings{BreakLogs: breaklogs.CaptureSettings{LinkedSource: "logs", IngestDelay: &zero}}); err != nil {
				t.Fatal(err)
			}
			writes, size := runDemand("eligible", 3200*time.Millisecond)
			progress, excerpts := manifestByID(t, backend.open(t), replica, manifest.ID)
			t.Logf("eligible demand: %d writes, %d bytes; attempts=%d done=%v excerpts=%d source reads=%d", writes, size, progress.Attempts, progress.Done, len(excerpts), stub.count("idle-break"))
			if !progress.Done || progress.Attempts != 1 || len(excerpts) != 1 || stub.count("idle-break") != 1 {
				t.Fatalf("eligible demand did not settle exactly once: attempts=%d done=%v excerpts=%d reads=%d", progress.Attempts, progress.Done, len(excerpts), stub.count("idle-break"))
			}
			if err := store.CheckCaptureLease(""); err != nil {
				t.Fatalf("demand loop did not release the capture lease after settling: %v", err)
			}
		})
	}
}
