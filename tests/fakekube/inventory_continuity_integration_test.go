//go:build integration

package fakekube

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// These tests prove committed Kubernetes inventory continuity across a process
// restart against real durable storage: the file provider on disk and, when
// TEST_POSTGRES_DSN names an isolated database, real PostgreSQL. The Kubernetes
// API is the fakekube server behind a small fault gate, because a real cluster
// cannot hold one LIST open, revoke a credential mid-run or present a second
// physical identity on demand. Every read is traced with safe, typed metadata
// only (scope, error code, sync state, kind status, UIDs of synthetic objects).

var continuityWorkloadResources = []string{"deployments", "statefulsets", "daemonsets", "jobs", "cronjobs", "pods", "replicasets"}

// continuityGate wraps the fake API. Authority probes (limit=1) pass a hold so a
// restore can still prove current access while the replacement LIST is pending.
type continuityGate struct {
	next         http.Handler
	mu           sync.Mutex
	held         map[string]bool
	release      chan struct{}
	heldCount    int
	denyAll      int
	denyKinds    map[string]int
	denyClusterL bool
	// denyClusterKinds forbids only the cluster-wide LIST of a resource, like a
	// credential that may list that kind inside namespaces but not across them.
	denyClusterKinds map[string]bool
	denyNS           map[string]bool
}

func (gate *continuityGate) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if strings.HasPrefix(request.URL.Path, "/_fake/") {
		gate.next.ServeHTTP(writer, request)
		return
	}
	resource := resourceForPath(request.URL.Path)
	isList := request.Method == http.MethodGet && resource != "" && !isItemPath(request.URL.Path) && request.URL.Query().Get("watch") == ""
	namespace := namespaceForPath(request.URL.Path)
	gate.mu.Lock()
	denyAll, denyKind := gate.denyAll, gate.denyKinds[resource]
	denyCluster := (gate.denyClusterL || gate.denyClusterKinds[resource]) && isList && namespace == "" && resource != "namespaces"
	denyNamespace := isList && namespace != "" && gate.denyNS[namespace]
	hold := isList && gate.held[resource] && request.URL.Query().Get("limit") != "1"
	release := gate.release
	if hold {
		gate.heldCount++
	}
	gate.mu.Unlock()
	switch {
	case denyAll != 0:
		http.Error(writer, http.StatusText(denyAll), denyAll)
		return
	case denyKind != 0 && isList:
		http.Error(writer, http.StatusText(denyKind), denyKind)
		return
	case denyCluster || denyNamespace:
		http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if hold {
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
	}
	gate.next.ServeHTTP(writer, request)
}

func (gate *continuityGate) hold(resources ...string) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.held = map[string]bool{}
	for _, resource := range resources {
		gate.held[resource] = true
	}
	gate.release = make(chan struct{})
}

func (gate *continuityGate) releaseHolds() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.release != nil {
		close(gate.release)
	}
	gate.held, gate.release = map[string]bool{}, nil
}

func (gate *continuityGate) reset() {
	gate.releaseHolds()
	gate.mu.Lock()
	gate.denyAll, gate.denyKinds, gate.denyClusterL, gate.denyClusterKinds, gate.denyNS = 0, map[string]int{}, false, map[string]bool{}, map[string]bool{}
	gate.mu.Unlock()
}

type continuityCluster struct {
	server *Server
	gate   *continuityGate
	url    string
}

func newContinuityCluster(t *testing.T, physicalUID, marker string) *continuityCluster {
	t.Helper()
	server, err := NewServer(Config{})
	if err != nil {
		t.Fatal(err)
	}
	gate := &continuityGate{next: server, held: map[string]bool{}, denyKinds: map[string]int{}, denyClusterKinds: map[string]bool{}, denyNS: map[string]bool{}}
	upstream := httptest.NewServer(gate)
	t.Cleanup(func() { gate.reset(); upstream.Close() })
	put := func(resource, namespace, name string, object map[string]any) {
		t.Helper()
		if err := putObject(server.store, resource, namespace, name, object); err != nil {
			t.Fatal(err)
		}
	}
	put("namespaces", "", "kube-system", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"uid": physicalUID}})
	for _, namespace := range []string{"shop", "payments"} {
		put("namespaces", "", namespace, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"uid": physicalUID + "-" + namespace}})
	}
	put("nodes", "", "node-a", map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"uid": physicalUID + "-node-a"}, "status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}})
	cluster := &continuityCluster{server: server, gate: gate, url: upstream.URL}
	cluster.deployment("shop", "web", "web-v1")
	cluster.deployment("payments", "ledger", "ledger-v1")
	cluster.deployment("shop", marker, marker)
	put("statefulsets", "shop", "db", map[string]any{"apiVersion": "apps/v1", "kind": "StatefulSet", "metadata": map[string]any{"uid": "db-v1"}, "spec": map[string]any{"replicas": 1}, "status": map[string]any{"replicas": 1, "readyReplicas": 1}})
	put("cronjobs", "payments", "nightly", map[string]any{"apiVersion": "batch/v1", "kind": "CronJob", "metadata": map[string]any{"uid": "nightly-v1"}, "spec": map[string]any{"schedule": "0 1 * * *"}})
	return cluster
}

// deployment writes a Deployment with one controller-owned ReplicaSet and Pod.
func (cluster *continuityCluster) deployment(namespace, name, uid string) {
	labels := map[string]string{"app": name}
	store := cluster.server.store
	_ = putObject(store, "deployments", namespace, name, map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"uid": uid, "labels": labels, "generation": 1}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels}}, "status": map[string]any{"replicas": 1, "readyReplicas": 1, "availableReplicas": 1, "observedGeneration": 1}})
	_ = putObject(store, "replicasets", namespace, name+"-rs", map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "metadata": map[string]any{"uid": uid + "-rs", "labels": labels, "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "name": name, "uid": uid, "controller": true}}}, "spec": map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels}}, "status": map[string]any{"replicas": 1, "readyReplicas": 1}})
	_ = putObject(store, "pods", namespace, name+"-0", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"uid": uid + "-pod", "labels": labels, "ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": name + "-rs", "uid": uid + "-rs", "controller": true}}}, "spec": map[string]any{"nodeName": "node-a", "containers": []any{map[string]any{"name": "app", "image": "example.invalid/" + name + ":v1"}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "ready": true, "restartCount": 0, "state": map[string]any{"running": map[string]any{}}}}}})
}

func (cluster *continuityCluster) deleteDeployment(namespace, name string) {
	store := cluster.server.store
	store.Delete("deployments", namespace, name)
	store.Delete("replicasets", namespace, name+"-rs")
	store.Delete("pods", namespace, name+"-0")
}

// continuityStore opens one real durable provider and reopens it like a restart.
type continuityStore struct {
	name    string
	open    func(t *testing.T) storage.Provider
	tryOpen func(t *testing.T) (storage.Provider, error)
	breakS  func(t *testing.T)
	repair  func(t *testing.T)
	meta    func(t *testing.T, org string) map[string]any
}

func continuityStores(t *testing.T) []continuityStore {
	dir := filepath.Join(t.TempDir(), "file-provider")
	stores := []continuityStore{{
		name: "file",
		open: func(t *testing.T) storage.Provider {
			provider, err := storage.NewFile(storage.FileOptions{DataDir: dir})
			if err != nil {
				t.Fatalf("open file provider: %v", err)
			}
			return provider
		},
		tryOpen: func(t *testing.T) (storage.Provider, error) {
			return storage.NewFile(storage.FileOptions{DataDir: dir})
		},
		breakS: func(t *testing.T) {
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		},
		repair: func(t *testing.T) { _ = os.Chmod(dir, 0o700) },
		meta: func(t *testing.T, org string) map[string]any {
			names := []string{}
			_ = filepath.WalkDir(filepath.Join(dir), func(path string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && strings.Contains(path, "inventory") {
					rel, _ := filepath.Rel(dir, path)
					names = append(names, rel)
				}
				return nil
			})
			return map[string]any{"inventory_blobs": len(names)}
		},
	}}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Log("postgres provider unavailable: TEST_POSTGRES_DSN not set (blocked, not passed)")
		return stores
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		admin = nil
	}
	stores = append(stores, continuityStore{
		name: "postgres",
		open: func(t *testing.T) storage.Provider {
			provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			if err != nil {
				t.Fatalf("open postgres provider: %v", err)
			}
			t.Cleanup(func() { provider.Close() })
			return provider
		},
		tryOpen: func(t *testing.T) (storage.Provider, error) {
			provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			if err == nil {
				t.Cleanup(func() { provider.Close() })
			}
			return provider, err
		},
		breakS: func(t *testing.T) {
			db := continuityDB(t, dsn, admin)
			if _, err := db.Exec("ALTER TABLE vs_blobs RENAME TO vs_blobs_outage"); err != nil {
				t.Fatalf("simulate storage outage: %v", err)
			}
			t.Cleanup(func() { _, _ = db.Exec("ALTER TABLE IF EXISTS vs_blobs_outage RENAME TO vs_blobs") })
		},
		repair: func(t *testing.T) {
			db := continuityDB(t, dsn, admin)
			if _, err := db.Exec("ALTER TABLE IF EXISTS vs_blobs_outage RENAME TO vs_blobs"); err != nil {
				t.Fatalf("repair storage outage: %v", err)
			}
		},
		meta: func(t *testing.T, org string) map[string]any {
			db := continuityDB(t, dsn, admin)
			var count int
			_ = db.QueryRow("SELECT count(*) FROM vs_blobs WHERE name LIKE 'kubernetes/inventory/%'").Scan(&count)
			return map[string]any{"inventory_blobs": count}
		},
	})
	return stores
}

func continuityDB(t *testing.T, dsn string, admin *sql.DB) *sql.DB {
	t.Helper()
	if admin != nil {
		return admin
	}
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	return provider.(storage.SQLAccessor).DB()
}

type continuityProcess struct {
	service  *kubernetes.Service
	registry *kubernetes.ServiceRegistry
	cancel   context.CancelFunc
}

func startContinuityProcess(t *testing.T, cluster *continuityCluster, provider storage.Provider, scope kubernetes.Scope) *continuityProcess {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("synthetic-reader-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewClient(kubernetes.Config{Endpoint: cluster.url, TokenFile: tokenFile, AllowLoopbackHTTP: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	service := kubernetes.NewService(client, scope, time.Minute)
	service.SetChangeStorage(provider)
	service.SetScrubber(continuityScrubber{})
	registry := kubernetes.NewServiceRegistry(service)
	ctx, cancel := context.WithCancel(context.Background())
	registry.StartCapture(ctx)
	process := &continuityProcess{service: service, registry: registry, cancel: cancel}
	t.Cleanup(process.stop)
	return process
}

type continuityScrubber struct{}

func (continuityScrubber) Scrub(value string) string { return value }

// probeWorkloadLogs opens the UID-bound workload log stream and reports which
// pod incarnations it admitted. Only identities and typed codes are traced.
func probeWorkloadLogs(t *testing.T, process *continuityProcess, store continuityStore, phase, namespace, name, uid string) (string, []string) {
	t.Helper()
	ctx := core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true, Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	options := kubernetes.WorkloadLogStreamOptions{Kind: "Deployment", Namespace: namespace, Name: name, UID: uid, Container: "app", TailLines: 5, Timestamps: true}
	validate := ""
	if err := process.service.ValidateWorkloadLogStream(ctx, options); err != nil {
		validate = kubernetes.DiagnoseError(err).Code
		if errors.Is(err, kubernetes.ErrNotFound) {
			validate = "not_found"
		}
	}
	pods := []string{}
	session, err := process.service.OpenWorkloadLogStream(ctx, options)
	if err == nil {
		seen := map[string]bool{}
		var mu sync.Mutex
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = session.Stream(func(event kubernetes.WorkloadLogStreamEvent) error {
				mu.Lock()
				defer mu.Unlock()
				if event.PodUID != "" && !seen[event.PodUID] {
					seen[event.PodUID] = true
					pods = append(pods, event.PodUID)
				}
				for _, source := range event.Sources {
					if source.Admitted && source.PodUID != "" && !seen[source.PodUID] {
						seen[source.PodUID] = true
						pods = append(pods, source.PodUID)
					}
				}
				return nil
			})
		}()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
		}
		session.Close()
		<-done
		mu.Lock()
		pods = append([]string(nil), pods...)
		mu.Unlock()
	} else if validate == "" {
		validate = "open:" + kubernetes.DiagnoseError(err).Code
	}
	continuityTrace(t, continuityObservation{Phase: phase, Storage: store.name, Namespace: namespace, Kind: "Deployment/" + name + "#" + uid, Outcome: "logs", Code: validate, Rows: pods})
	return validate, pods
}

func (process *continuityProcess) stop() {
	closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = process.registry.CloseContext(closeCtx)
	process.cancel()
}

type continuityObservation struct {
	Test      string         `json:"test"`
	Phase     string         `json:"phase"`
	Storage   string         `json:"storage"`
	Namespace string         `json:"namespace"`
	Kind      string         `json:"kind,omitempty"`
	Outcome   string         `json:"outcome"`
	Code      string         `json:"code,omitempty"`
	Sync      map[string]any `json:"sync,omitempty"`
	Rows      []string       `json:"rows,omitempty"`
	Counts    map[string]int `json:"counts,omitempty"`
	Partial   []string       `json:"partial,omitempty"`
	Health    map[string]any `json:"health,omitempty"`
	Storage2  map[string]any `json:"storage_meta,omitempty"`
	At        string         `json:"at"`
	page      kubernetes.WorkloadPage
	err       error
}

var continuityTraceMu sync.Mutex

func continuityTrace(t *testing.T, observation continuityObservation) {
	observation.Test, observation.At = t.Name(), time.Now().UTC().Format(time.RFC3339Nano)
	encoded, _ := json.Marshal(observation)
	t.Log(string(encoded))
	dir := os.Getenv("HARNESS_RESULTS_DIR")
	if dir == "" {
		return
	}
	continuityTraceMu.Lock()
	defer continuityTraceMu.Unlock()
	file, err := os.OpenFile(filepath.Join(dir, "inventory-continuity-trace.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

// readWorkloads performs one bounded Workloads read and traces its safe metadata.
func readWorkloads(t *testing.T, process *continuityProcess, store continuityStore, phase, namespace, kind string) continuityObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	page, err := process.service.Workloads(ctx, kubernetes.WorkloadListOptions{Namespace: namespace, Kind: kind, Limit: 200})
	observation := continuityObservation{Phase: phase, Storage: store.name, Namespace: namespace, Kind: kind, page: page, err: err}
	if err != nil {
		observation.Outcome, observation.Code = "error", kubernetes.DiagnoseError(err).Code
		if errors.Is(err, context.DeadlineExceeded) {
			observation.Code = "deadline"
		}
	} else {
		observation.Outcome = "ok"
		observation.Sync = map[string]any{"state": page.Sync.State, "age_s": page.Sync.AgeSeconds, "partial": page.Sync.Partial, "pending": page.Sync.Pending, "pending_kinds": page.Sync.PendingKinds}
		for _, item := range page.Items {
			observation.Rows = append(observation.Rows, item.Kind+"/"+item.Namespace+"/"+item.Name+"#"+item.UID)
		}
		sort.Strings(observation.Rows)
		observation.Counts = page.Counts
		for _, partial := range page.Partial {
			observation.Partial = append(observation.Partial, partial.ResourceID+"="+partial.Class)
		}
	}
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer healthCancel()
	if health, healthErr := process.service.SyncHealth(healthCtx); healthErr == nil {
		failing := []string{}
		for kind, state := range health.Kinds {
			if state.State != "ready" {
				failing = append(failing, kind+"="+state.State+":"+state.Error)
			}
		}
		sort.Strings(failing)
		if len(failing) > 12 {
			failing = append(failing[:12], fmt.Sprintf("+%d more", len(failing)-12))
		}
		observation.Health = map[string]any{"state": health.State, "class": health.Class, "first_ready": !health.FirstReadyAt.IsZero(), "not_ready_kinds": failing}
	} else {
		observation.Health = map[string]any{"error": kubernetes.DiagnoseError(healthErr).Code}
	}
	continuityTrace(t, observation)
	return observation
}

func (observation continuityObservation) has(row string) bool {
	for _, value := range observation.Rows {
		if strings.HasPrefix(value, row) {
			return true
		}
	}
	return false
}

func (observation continuityObservation) hasUID(uid string) bool {
	for _, value := range observation.Rows {
		if strings.HasSuffix(value, "#"+uid) {
			return true
		}
	}
	return false
}

func waitWorkloads(t *testing.T, process *continuityProcess, store continuityStore, phase, namespace, kind string, timeout time.Duration, done func(continuityObservation) bool) (continuityObservation, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last continuityObservation
	for {
		last = readWorkloads(t, process, store, phase, namespace, kind)
		if done(last) {
			return last, true
		}
		if time.Now().After(deadline) {
			return last, false
		}
		time.Sleep(time.Second)
	}
}

func continuityScope(store continuityStore, name string) kubernetes.Scope {
	suffix := fmt.Sprintf("%s-%s-%d", name, store.name, time.Now().UnixNano())
	return kubernetes.Scope{OrgID: "default", ClusterID: "continuity-" + suffix, CredentialID: "reader-epoch-1"}
}

func warmProcess(t *testing.T, cluster *continuityCluster, store continuityStore, provider storage.Provider, scope kubernetes.Scope, marker string) *continuityProcess {
	t.Helper()
	process := startContinuityProcess(t, cluster, provider, scope)
	observation, ok := waitWorkloads(t, process, store, "warm", "", "", 90*time.Second, func(o continuityObservation) bool {
		return o.err == nil && o.page.Sync.State == "ready" && o.hasUID("web-v1") && o.hasUID("ledger-v1") && o.hasUID(marker) && o.hasUID("nightly-v1") && o.hasUID("db-v1")
	})
	if !ok {
		t.Fatalf("warm committed publication never became ready: %+v", observation)
	}
	// Visit the shop namespace so a scoped projection is also committed.
	readWorkloads(t, process, store, "warm-shop", "shop", "")
	return process
}

func TestInventoryContinuityRestoreWhileReplacementPending(t *testing.T) {
	for _, store := range continuityStores(t) {
		t.Run(store.name, func(t *testing.T) {
			cluster := newContinuityCluster(t, "physical-a", "only-a")
			scope := continuityScope(store, "restore")
			first := warmProcess(t, cluster, store, store.open(t), scope, "only-a")
			first.stop()
			continuityTrace(t, continuityObservation{Phase: "after-warm-stop", Storage: store.name, Outcome: "meta", Storage2: store.meta(t, scope.OrgID)})

			// Replacement collection is pending and the live object was replaced
			// by a same-name object with a new UID.
			cluster.gate.hold(continuityWorkloadResources...)
			cluster.deleteDeployment("shop", "web")
			cluster.deployment("shop", "web", "web-v2")
			second := startContinuityProcess(t, cluster, store.open(t), scope)

			shop, ok := waitWorkloads(t, second, store, "restart-pending-shop", "shop", "", 20*time.Second, func(o continuityObservation) bool { return o.err == nil && len(o.Rows) > 0 })
			if !ok {
				t.Errorf("restart with pending replacement: shop committed rows unavailable from %s storage; last outcome=%s code=%s sync=%v", store.name, shop.Outcome, shop.Code, shop.Sync)
			} else {
				if !shop.hasUID("web-v1") || shop.hasUID("web-v2") {
					t.Errorf("restored shop projection is not the committed generation: rows=%v", shop.Rows)
				}
				if shop.has("Deployment/payments/") || shop.has("CronJob/payments/") {
					t.Errorf("shop projection borrowed another namespace: rows=%v", shop.Rows)
				}
				if shop.page.Counts["Deployment"] == 0 {
					t.Errorf("restored shop counts claim zero deployments: %v", shop.page.Counts)
				}
			}
			payments := readWorkloads(t, second, store, "restart-pending-first-visit-payments", "payments", "")
			if payments.err != nil {
				t.Errorf("first visit to payments during pending replacement failed: code=%s", payments.Code)
			} else if !payments.hasUID("ledger-v1") || payments.has("Deployment/shop/") || payments.has("StatefulSet/shop/") {
				t.Errorf("first payments visit is not its committed projection: rows=%v", payments.Rows)
			}
			cron := readWorkloads(t, second, store, "restart-pending-first-filter-cronjob", "", "CronJob")
			if cron.err != nil {
				t.Errorf("first CronJob filter during pending replacement failed: code=%s", cron.Code)
			} else if !cron.hasUID("nightly-v1") || len(cron.page.Items) != 1 {
				t.Errorf("CronJob filter rows=%v", cron.Rows)
			}
			unseen := readWorkloads(t, second, store, "restart-pending-uncaptured-namespace", "staging", "")
			if unseen.err == nil && len(unseen.Rows) > 0 {
				t.Errorf("uncaptured namespace borrowed rows: %v", unseen.Rows)
			}

			// The restored generation still names the old UID while the live
			// same-name object is a new incarnation. The UID-bound log stream
			// must not admit the new incarnation's pod for the old UID.
			_, pendingPods := probeWorkloadLogs(t, second, store, "restart-pending-old-uid-logs", "shop", "web", "web-v1")
			for _, pod := range pendingPods {
				if pod != "web-v1-pod" {
					t.Errorf("old workload UID admitted a different pod incarnation during pending replacement: %v", pendingPods)
				}
			}
			cluster.gate.releaseHolds()
			replaced, ok := waitWorkloads(t, second, store, "replacement-published-shop", "shop", "", 120*time.Second, func(o continuityObservation) bool { return o.err == nil && o.hasUID("web-v2") })
			if !ok {
				t.Errorf("replacement with new UID never published: %+v", replaced.Rows)
			} else if replaced.hasUID("web-v1") || replaced.hasUID("web-v1-pod") {
				t.Errorf("old UID survived beside same-name replacement: rows=%v", replaced.Rows)
			}
			oldCode, oldPods := probeWorkloadLogs(t, second, store, "replacement-published-old-uid-logs", "shop", "web", "web-v1")
			if oldCode == "" || len(oldPods) > 0 {
				t.Errorf("same-name replacement satisfied the old workload UID: code=%q pods=%v", oldCode, oldPods)
			}
			probeWorkloadLogs(t, second, store, "replacement-published-new-uid-logs", "shop", "web", "web-v2")
			global := readWorkloads(t, second, store, "replacement-published-global", "", "Deployment")
			if global.hasUID("web-v1") {
				t.Errorf("global view kept the replaced UID: rows=%v", global.Rows)
			}

			// A complete empty replacement removes every payments workload.
			cluster.deleteDeployment("payments", "ledger")
			cluster.server.store.Delete("cronjobs", "payments", "nightly")
			empty, ok := waitWorkloads(t, second, store, "empty-replacement-payments", "payments", "", 150*time.Second, func(o continuityObservation) bool {
				return o.err == nil && len(o.Rows) == 0 && o.page.Sync.State == "ready"
			})
			if !ok {
				t.Errorf("empty replacement did not remove payments rows: outcome=%s rows=%v sync=%v", empty.Outcome, empty.Rows, empty.Sync)
			}
			second.stop()

			// Restart again with replacement pending: deleted rows must not resurrect.
			cluster.gate.hold(continuityWorkloadResources...)
			third := startContinuityProcess(t, cluster, store.open(t), scope)
			deadline := time.Now().Add(12 * time.Second)
			for time.Now().Before(deadline) {
				view := readWorkloads(t, third, store, "restart-after-empty-payments", "payments", "")
				if view.hasUID("ledger-v1") || view.hasUID("nightly-v1") {
					t.Errorf("deleted payments rows resurrected from storage: %v", view.Rows)
					break
				}
				shopView := readWorkloads(t, third, store, "restart-after-replacement-shop", "shop", "")
				if shopView.hasUID("web-v1") {
					t.Errorf("replaced UID resurrected from storage: %v", shopView.Rows)
					break
				}
				time.Sleep(2 * time.Second)
			}
			cluster.gate.releaseHolds()
		})
	}
}

func TestInventoryRestoreFailsClosed(t *testing.T) {
	for _, store := range continuityStores(t) {
		t.Run(store.name, func(t *testing.T) {
			cluster := newContinuityCluster(t, "physical-a", "only-a")
			scope := continuityScope(store, "fenced")
			warm := warmProcess(t, cluster, store, store.open(t), scope, "only-a")
			warm.stop()
			probe := func(name string, configure func(), cleanup func(), target *continuityCluster, forbidden ...string) {
				t.Run(name, func(t *testing.T) {
					configure()
					defer cleanup()
					process := startContinuityProcess(t, target, store.open(t), scope)
					deadline := time.Now().Add(10 * time.Second)
					seenError := false
					for time.Now().Before(deadline) {
						for _, namespace := range []string{"", "shop", "payments"} {
							view := readWorkloads(t, process, store, name, namespace, "")
							for _, uid := range forbidden {
								if view.hasUID(uid) {
									t.Errorf("%s replayed protected row %s in namespace %q: rows=%v", name, uid, namespace, view.Rows)
									return
								}
							}
							seenError = seenError || view.err != nil
						}
						time.Sleep(time.Second)
					}
					if !seenError {
						t.Logf("%s: no explicit error observed (reads stayed pending/empty)", name)
					}
					process.stop()
				})
			}
			probe("revoked-credential", func() {
				cluster.gate.mu.Lock()
				cluster.gate.denyAll = http.StatusUnauthorized
				cluster.gate.mu.Unlock()
			}, cluster.gate.reset, cluster, "web-v1", "ledger-v1", "only-a", "nightly-v1", "db-v1")
			probe("whole-denial", func() {
				cluster.gate.mu.Lock()
				for _, resource := range append(continuityWorkloadResources, "namespaces", "nodes", "events") {
					cluster.gate.denyKinds[resource] = http.StatusForbidden
				}
				cluster.gate.mu.Unlock()
			}, cluster.gate.reset, cluster, "web-v1", "ledger-v1", "only-a", "nightly-v1", "db-v1")
			probe("namespace-denial", func() {
				cluster.gate.mu.Lock()
				cluster.gate.denyClusterL, cluster.gate.denyNS = true, map[string]bool{"shop": true}
				cluster.gate.mu.Unlock()
				cluster.gate.hold("deployments", "statefulsets", "cronjobs", "pods", "replicasets", "jobs", "daemonsets")
			}, cluster.gate.reset, cluster, "web-v1", "only-a", "db-v1")
			other := newContinuityCluster(t, "physical-b", "only-b")
			probe("physical-cluster-mismatch", func() { other.gate.hold(continuityWorkloadResources...) }, other.gate.reset, other, "only-a")
		})
	}
}

func TestInventoryStorageOutageKeepsMemoryAndReportsError(t *testing.T) {
	for _, store := range continuityStores(t) {
		t.Run(store.name, func(t *testing.T) {
			cluster := newContinuityCluster(t, "physical-a", "only-a")
			scope := continuityScope(store, "outage")
			process := warmProcess(t, cluster, store, store.open(t), scope, "only-a")
			baseline := readWorkloads(t, process, store, "warm-before-outage", "", "")
			baseClass, _ := baseline.Health["class"].(string)
			store.breakS(t)
			cluster.deployment("shop", "added", "added-v1")
			observed := false
			deadline := time.Now().Add(100 * time.Second)
			for time.Now().Before(deadline) {
				view := readWorkloads(t, process, store, "warm-storage-outage", "", "")
				if view.err != nil || !view.hasUID("web-v1") {
					t.Errorf("authorized memory publication lost during storage outage: code=%s rows=%v", view.Code, view.Rows)
					break
				}
				class, _ := view.Health["class"].(string)
				if view.page.Sync.State != "ready" || len(view.page.Partial) > len(baseline.page.Partial) || class != baseClass {
					observed = true
					t.Logf("storage outage observable: sync=%v partial=%v health=%v", view.Sync, view.Partial, view.Health)
					break
				}
				time.Sleep(2 * time.Second)
			}
			if !observed {
				t.Errorf("storage outage stayed invisible for 100s: Workloads stayed ready without a storage partial for %s", store.name)
			}
			cluster.gate.hold(continuityWorkloadResources...)
			coldProvider, openErr := store.tryOpen(t)
			if openErr != nil {
				continuityTrace(t, continuityObservation{Phase: "cold-storage-outage-open", Storage: store.name, Outcome: "error", Code: "storage_open_refused"})
			} else {
				cold := startContinuityProcess(t, cluster, coldProvider, continuityScope(store, "outage-cold"))
				deadline = time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					view := readWorkloads(t, cold, store, "cold-storage-outage", "", "")
					if view.err == nil && view.page.Sync.State == "ready" && len(view.Rows) == 0 {
						t.Errorf("cold read during storage outage manufactured ready empty inventory: sync=%v", view.Sync)
						break
					}
					time.Sleep(time.Second)
				}
				cold.stop()
			}
			cluster.gate.releaseHolds()
			store.repair(t)
			recovered, ok := waitWorkloads(t, process, store, "storage-repaired", "", "", 120*time.Second, func(o continuityObservation) bool {
				return o.err == nil && o.page.Sync.State == "ready" && o.hasUID("added-v1")
			})
			if !ok {
				t.Errorf("did not recover after storage repair: %+v", recovered.Sync)
			}
		})
	}
}

func TestInventoryOptionalKindPendingOrDenied(t *testing.T) {
	for _, store := range continuityStores(t) {
		t.Run(store.name, func(t *testing.T) {
			cluster := newContinuityCluster(t, "physical-a", "only-a")
			cluster.gate.hold("cronjobs")
			cluster.gate.mu.Lock()
			cluster.gate.denyKinds["daemonsets"] = http.StatusForbidden
			cluster.gate.mu.Unlock()
			process := startContinuityProcess(t, cluster, store.open(t), continuityScope(store, "optional"))
			// No successful LIST means no known count, including when the first
			// attempt has finished with a timeout rather than remaining pending.
			knownWhileUnobserved := []string{}
			sawNeverListedTimeout := false
			unobservedCountsUnknown := func(o continuityObservation) {
				if o.err != nil {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				health, err := process.service.SyncHealth(ctx)
				if carried, ok := kubernetes.SyncHealthReport(err); ok {
					health = carried
				}
				// Health is read after the page; ObservedAt retains successful
				// observations, so zero now also means unobserved on that page.
				for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"} {
					state := health.Kinds[kind]
					if count, known := o.page.Counts[kind]; known && state.ObservedAt.IsZero() && len(knownWhileUnobserved) < 12 {
						knownWhileUnobserved = append(knownWhileUnobserved, fmt.Sprintf("%s %s ns=%q sync=%s %s=%d state=%s error=%s", o.Storage, o.Phase, o.Namespace, o.page.Sync.State, kind, count, state.State, state.Error))
					}
					if kind == "CronJob" && state.ObservedAt.IsZero() && state.State == "error" && state.Error == "timeout" {
						for _, partial := range o.page.Partial {
							sawNeverListedTimeout = sawNeverListedTimeout || partial.ResourceID == "batch~v1~cronjobs" && partial.Class == "timeout"
						}
					}
				}
			}
			view, ok := waitWorkloads(t, process, store, "optional-kind-pending-denied", "", "", 60*time.Second, func(o continuityObservation) bool {
				unobservedCountsUnknown(o)
				return o.err == nil && o.hasUID("web-v1") && o.hasUID("db-v1") && sawNeverListedTimeout
			})
			if !ok {
				t.Fatalf("one pending and one denied optional kind blocked eligible workloads: code=%s sync=%v", view.Code, view.Sync)
			}
			denied := false
			for _, partial := range view.page.Partial {
				denied = denied || partial.ResourceID == "apps~v1~daemonsets" && partial.Class == "forbidden"
			}
			if !denied {
				t.Errorf("denied DaemonSet kind not reported as partial forbidden: %v", view.Partial)
			}
			pending := view.page.Sync.State != "ready"
			for _, id := range view.page.Sync.Pending {
				pending = pending || id == "batch~v1~cronjobs"
			}
			if !pending {
				t.Errorf("pending CronJob kind reported as complete ready: sync=%v counts=%v", view.Sync, view.Counts)
			}
			if view.hasUID("nightly-v1") {
				t.Errorf("held CronJob rows appeared before its LIST completed")
			}
			shop := readWorkloads(t, process, store, "optional-kind-first-namespace", "shop", "")
			if shop.err != nil || !shop.hasUID("web-v1") {
				t.Errorf("first shop visit with optional kinds pending/denied: code=%s rows=%v", shop.Code, shop.Rows)
			}
			unobservedCountsUnknown(shop)
			if len(knownWhileUnobserved) > 0 {
				t.Errorf("never-listed workload kinds reported as known counts instead of unknown: %v", knownWhileUnobserved)
			}
			cluster.gate.releaseHolds()
			process.stop()
		})
	}
}

// A Workloads cursor issued under one committed generation must not silently
// page a different generation: a replacement that inserts or removes rows
// before the cursor would otherwise duplicate or skip rows as a success.
func TestWorkloadCursorAcrossReplacementGeneration(t *testing.T) {
	for _, store := range continuityStores(t) {
		t.Run(store.name, func(t *testing.T) {
			cluster := newContinuityCluster(t, "physical-a", "only-a")
			process := warmProcess(t, cluster, store, store.open(t), continuityScope(store, "cursor"), "only-a")
			ctx := context.Background()
			first, err := process.service.Workloads(ctx, kubernetes.WorkloadListOptions{Kind: "Deployment", Limit: 1})
			if err != nil || len(first.Items) != 1 || first.Next == "" {
				t.Fatalf("first page: %v %+v", err, first)
			}
			firstRow := first.Items[0].Namespace + "/" + first.Items[0].Name
			following, err := process.service.Workloads(ctx, kubernetes.WorkloadListOptions{Kind: "Deployment", Limit: 1, Cursor: first.Next})
			if err != nil || len(following.Items) != 1 {
				t.Fatalf("second page before replacement: %v", err)
			}
			expectedNext := following.Items[0].Namespace + "/" + following.Items[0].Name
			continuityTrace(t, continuityObservation{Phase: "cursor-issued", Storage: store.name, Kind: "Deployment", Outcome: "ok", Rows: []string{firstRow, expectedNext}})
			// A routine resync that lists the same Deployments again must not expire
			// the issued cursor: the unchanged generation still pages to its successor.
			baseline := kindObservedAt(t, process, "Deployment")
			resynced := false
			for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
				if resynced = kindObservedAt(t, process, "Deployment").After(baseline); resynced {
					break
				}
			}
			if !resynced {
				t.Errorf("no routine Deployment resync observed within 120s; unchanged-generation continuation not exercised")
			} else {
				again, err := process.service.Workloads(ctx, kubernetes.WorkloadListOptions{Kind: "Deployment", Limit: 1, Cursor: first.Next})
				observation := continuityObservation{Phase: "cursor-reused-after-unchanged-resync", Storage: store.name, Kind: "Deployment", Outcome: "ok"}
				if err != nil {
					observation.Outcome, observation.Code = "error", kubernetes.DiagnoseError(err).Code
				}
				for _, item := range again.Items {
					observation.Rows = append(observation.Rows, item.Namespace+"/"+item.Name)
				}
				continuityTrace(t, observation)
				if err != nil || len(observation.Rows) != 1 || observation.Rows[0] != expectedNext {
					t.Errorf("unchanged resync expired or moved the cursor: err=%v rows=%v want %s", err, observation.Rows, expectedNext)
				}
			}
			// Replacement inserts a row that sorts before the issued cursor.
			cluster.deployment("payments", "aaa-inserted", "aaa-inserted-v1")
			if _, ok := waitWorkloads(t, process, store, "cursor-replacement", "", "Deployment", 90*time.Second, func(o continuityObservation) bool { return o.err == nil && o.hasUID("aaa-inserted-v1") }); !ok {
				t.Fatal("replacement with inserted row never published")
			}
			stale, err := process.service.Workloads(ctx, kubernetes.WorkloadListOptions{Kind: "Deployment", Limit: 1, Cursor: first.Next})
			observation := continuityObservation{Phase: "cursor-reused-after-replacement", Storage: store.name, Kind: "Deployment", Outcome: "ok"}
			if err != nil {
				observation.Outcome, observation.Code = "error", kubernetes.DiagnoseError(err).Code
			} else {
				for _, item := range stale.Items {
					observation.Rows = append(observation.Rows, item.Namespace+"/"+item.Name)
				}
			}
			continuityTrace(t, observation)
			if !errors.Is(err, kubernetes.ErrInvalidArguments) {
				t.Errorf("stale cursor %q must return typed invalidation: err=%v rows=%v, page one was %s and its successor was %s", first.Next, err, observation.Rows, firstRow, expectedNext)
			}
		})
	}
}

// kindObservedAt reports when the last successful LIST of one kind started.
func kindObservedAt(t *testing.T, process *continuityProcess, kind string) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	health, err := process.service.SyncHealth(ctx)
	if carried, ok := kubernetes.SyncHealthReport(err); ok {
		health = carried
	}
	return health.Kinds[kind].ObservedAt
}

// A namespace-scoped snapshot committed while the cluster-wide Pod LIST was
// forbidden must not hide a later cluster-wide Pod timeout. When the global
// LIST is allowed again but times out, a scoped read reports that timeout
// instead of presenting the older scoped rows as a complete, current view.
func TestScopedSnapshotDoesNotHideNewerGlobalTimeout(t *testing.T) {
	for _, store := range continuityStores(t) {
		t.Run(store.name, func(t *testing.T) {
			cluster := newContinuityCluster(t, "physical-a", "only-a")
			process := warmProcess(t, cluster, store, store.open(t), continuityScope(store, "scoped-timeout"), "only-a")
			podState := func() (string, string, time.Time) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				health, err := process.service.SyncHealth(ctx)
				if carried, ok := kubernetes.SyncHealthReport(err); ok {
					health = carried
				}
				state := health.Kinds["Pod"]
				return state.State, state.Error, state.LastAttemptAt
			}
			podPartial := func(o continuityObservation) string {
				for _, partial := range o.page.Partial {
					if partial.ResourceID == "core~v1~pods" {
						return partial.Class
					}
				}
				return ""
			}
			cluster.gate.mu.Lock()
			cluster.gate.denyClusterKinds["pods"] = true
			cluster.gate.mu.Unlock()
			scoped, ok := waitWorkloads(t, process, store, "scoped-pods-under-cluster-denial", "shop", "Pod", 150*time.Second, func(o continuityObservation) bool {
				// The worker proves namespace authority and reports the cluster-wide
				// limitation as forbidden or namespace_scope; either admits scoped rows.
				_, class, _ := podState()
				return (class == "forbidden" || class == "namespace_scope") && o.err == nil && o.hasUID("web-v1-pod") && podPartial(o) == ""
			})
			if !ok {
				t.Fatalf("namespace-scoped Pod snapshot never replaced the cluster-wide denial: code=%s rows=%v partial=%v", scoped.Code, scoped.Rows, scoped.Partial)
			}
			scopedAt := time.Now()
			cluster.gate.mu.Lock()
			cluster.gate.denyClusterKinds = map[string]bool{}
			cluster.gate.mu.Unlock()
			cluster.gate.hold("pods")
			defer cluster.gate.releaseHolds()
			globalClass := ""
			for deadline := time.Now().Add(150 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
				state, class, attempted := podState()
				if state == "error" && class != "" && class != "forbidden" && class != "namespace_scope" && attempted.After(scopedAt) {
					globalClass = class
					break
				}
			}
			continuityTrace(t, continuityObservation{Phase: "global-pods-after-scoped", Storage: store.name, Kind: "Pod", Outcome: "health", Code: globalClass})
			if globalClass == "" {
				t.Fatalf("cluster-wide Pod LIST never recorded a newer non-permission failure; scenario not exercised")
			}
			global := readWorkloads(t, process, store, "global-after-newer-timeout", "", "Pod")
			if global.err == nil && podPartial(global) != globalClass {
				t.Errorf("cluster-wide read hid the newer Pod %s: partial=%v", globalClass, global.Partial)
			}
			view := readWorkloads(t, process, store, "scoped-after-newer-global-timeout", "shop", "Pod")
			if view.err != nil {
				t.Errorf("non-permission Pod %s blocked the scoped read: code=%s", globalClass, view.Code)
			} else if podPartial(view) != globalClass {
				t.Errorf("older scoped snapshot concealed the newer cluster-wide Pod %s: sync=%v partial=%v rows=%v", globalClass, view.Sync, view.Partial, view.Rows)
			}
			cluster.gate.releaseHolds()
			process.stop()
		})
	}
}
