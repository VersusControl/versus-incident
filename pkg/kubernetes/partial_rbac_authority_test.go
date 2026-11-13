package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"golang.org/x/time/rate"
)

// holdsOnEveryCycle runs cycles one at a time and checks invariant while each
// cycle runs and again once it has settled. It fails on the first cycle where
// the invariant does not hold, rather than polling until it happens to hold.
func holdsOnEveryCycle(t *testing.T, cycles int, run func(cycle int), invariant func() error) {
	t.Helper()
	for cycle := 0; cycle < cycles; cycle++ {
		stop := make(chan struct{})
		done := make(chan struct{})
		var first error
		samples := 0
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				samples++
				if err := invariant(); err != nil && first == nil {
					first = err
				}
				time.Sleep(time.Millisecond)
			}
		}()
		run(cycle)
		close(stop)
		<-done
		if first != nil {
			t.Fatalf("cycle %d of %d: invariant broke while the cycle ran (%d samples): %v", cycle+1, cycles, samples, first)
		}
		if err := invariant(); err != nil {
			t.Fatalf("cycle %d of %d: invariant broke after the cycle settled: %v", cycle+1, cycles, err)
		}
	}
}

const (
	secretOrderFree = iota
	secretOrderFirst
	secretOrderLast
)

// partialRoleCluster is a fakekube cluster behind the permissions of a reader
// role. It answers 403 for the kinds the role does not grant, can serve the
// Gateway API group, and can make the cluster-wide Secret LIST answer before
// or after every other LIST of a cycle.
type partialRoleCluster struct {
	t        *testing.T
	service  *Service
	cache    *inventoryCache
	provider storage.Provider
	registry *ServiceRegistry
	endpoint string

	denySecrets atomic.Bool
	denyGateway atomic.Bool
	denyAll     atomic.Bool
	// denyClusterPods denies the cluster-wide Pod LIST while namespaced Pod
	// LISTs stay allowed, as a role bound only inside namespaces does.
	denyClusterPods    atomic.Bool
	denyNamespacedPods atomic.Bool
	// denyProbes answers 403 to every single-item LIST, which the authority
	// proof and the workload read check use.
	denyProbes atomic.Bool
	// holdScoped, when set, runs before a namespaced LIST is answered, so a
	// test can hold collection at its namespace stage.
	holdScoped atomic.Pointer[func()]
	order      atomic.Int32
	// namespaceOnly answers 403 to every read outside the namespaces a
	// namespace-bound Role grants, as a Role and RoleBinding per namespace
	// without any cluster-wide grant does. Discovery stays readable.
	namespaceOnly atomic.Bool
	// denyGroupDiscovery answers 403 to the API group list and to every
	// group's discovery document; the core API stays readable.
	denyGroupDiscovery atomic.Bool
	// outage answers 503 to every request, as an API server that is down.
	outage atomic.Bool
	// gatewayUnavailable answers 503 to the Gateway API LISTs, as a broken
	// conversion webhook does; gatewayHang holds them until the client gives up.
	gatewayUnavailable atomic.Bool
	gatewayHang        atomic.Bool
	// gatewayRecords serves one object for each Gateway API kind.
	gatewayRecords atomic.Bool
	// extraGroups serves that many additional API groups next to the Gateway
	// API group, the way a cluster with many installed CRDs does.
	extraGroups atomic.Int32

	mu             sync.Mutex
	secretAnswered chan struct{}
	inflight       atomic.Int32
	lastOther      atomic.Int64
}

func newPartialRoleCluster(t *testing.T, gatewayAPI bool) *partialRoleCluster {
	t.Helper()
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "bad-rollout"})
	if err != nil {
		t.Fatal(err)
	}
	seed := httptest.NewRecorder()
	fixture.ServeHTTP(seed, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"partial-role-physical"}}}`)))
	if seed.Code != http.StatusOK {
		t.Fatal("physical identity fixture failed")
	}
	secret := httptest.NewRecorder()
	fixture.ServeHTTP(secret, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(`{"operation":"upsert","resource":"secrets","namespace":"default","name":"api-token","object":{"kind":"Secret","metadata":{"uid":"secret-api-token"}}}`)))
	if secret.Code != http.StatusOK {
		t.Fatal("secret fixture failed")
	}
	cluster := &partialRoleCluster{t: t, secretAnswered: make(chan struct{})}
	const gatewayGroup = "/apis/gateway.networking.k8s.io/v1"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		query := r.URL.Query()
		list := r.Method == http.MethodGet && query.Has("limit")
		switch {
		case cluster.outage.Load():
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case cluster.denyGroupDiscovery.Load() && strings.HasPrefix(path, "/apis") && discoveryPath(path):
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case gatewayAPI && r.Method == http.MethodGet && path == "/apis":
			recorder := httptest.NewRecorder()
			fixture.ServeHTTP(recorder, r)
			var groups struct {
				Groups []map[string]any `json:"groups"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &groups); err != nil {
				http.Error(w, "fixture discovery", http.StatusInternalServerError)
				return
			}
			for extra := 0; extra < int(cluster.extraGroups.Load()); extra++ {
				name := fmt.Sprintf("extra-%03d.example.com", extra)
				version := map[string]any{"groupVersion": name + "/v1", "version": "v1"}
				groups.Groups = append(groups.Groups, map[string]any{"name": name, "preferredVersion": version, "versions": []any{version}})
			}
			version := map[string]any{"groupVersion": "gateway.networking.k8s.io/v1", "version": "v1"}
			groups.Groups = append(groups.Groups, map[string]any{"name": "gateway.networking.k8s.io", "preferredVersion": version, "versions": []any{version}})
			writeJSON(w, map[string]any{"groups": groups.Groups})
			return
		case gatewayAPI && path == gatewayGroup:
			writeJSON(w, map[string]any{"groupVersion": "gateway.networking.k8s.io/v1", "resources": []any{
				map[string]any{"name": "gateways", "kind": "Gateway", "namespaced": true, "verbs": []string{"get", "list"}},
				map[string]any{"name": "httproutes", "kind": "HTTPRoute", "namespaced": true, "verbs": []string{"get", "list"}},
			}})
			return
		case gatewayAPI && strings.HasPrefix(path, gatewayGroup+"/"):
			if cluster.denyGateway.Load() || cluster.denyAll.Load() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if cluster.gatewayUnavailable.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			if cluster.gatewayHang.Load() {
				<-r.Context().Done()
				return
			}
			items := []any{}
			if cluster.gatewayRecords.Load() {
				resource := path[strings.LastIndex(path, "/")+1:]
				items = append(items, map[string]any{"metadata": map[string]any{"name": "edge", "namespace": "shop", "uid": "uid-" + resource + "-edge"}})
			}
			writeJSON(w, map[string]any{"metadata": map[string]any{"resourceVersion": "1"}, "items": items})
			return
		case strings.HasPrefix(path, "/apis/extra-"):
			parts := strings.Split(strings.Trim(path, "/"), "/")
			if len(parts) == 3 {
				// A few extra groups serve a listable kind for the optional
				// stage; the rest serve kinds that cannot be listed.
				verbs := []string{"get"}
				if strings.HasPrefix(parts[1], "extra-00") {
					verbs = []string{"get", "list"}
				}
				writeJSON(w, map[string]any{"groupVersion": parts[1] + "/v1", "resources": []any{
					map[string]any{"name": "widgets", "kind": "Widget", "namespaced": true, "verbs": verbs},
				}})
				return
			}
			writeJSON(w, map[string]any{"metadata": map[string]any{"resourceVersion": "1"}, "items": []any{}})
			return
		case cluster.namespaceOnly.Load() && !discoveryPath(path) && !strings.Contains(path, "/namespaces/shop/") && !strings.Contains(path, "/namespaces/payments/"):
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case list && cluster.denyAll.Load():
			if hold := cluster.holdScoped.Load(); hold != nil && strings.Contains(path, "/namespaces/") && query.Get("limit") != "1" {
				(*hold)()
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case list && cluster.denyProbes.Load() && query.Get("limit") == "1":
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case list && path == "/api/v1/pods" && cluster.denyClusterPods.Load():
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case list && strings.HasPrefix(path, "/api/v1/namespaces/") && strings.HasSuffix(path, "/pods") && cluster.denyNamespacedPods.Load():
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		case list && strings.HasSuffix(path, "/secrets") && cluster.denySecrets.Load():
			if path == "/api/v1/secrets" && cluster.order.Load() == secretOrderLast {
				cluster.waitOthersIdle()
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			if path == "/api/v1/secrets" {
				cluster.mu.Lock()
				select {
				case <-cluster.secretAnswered:
				default:
					close(cluster.secretAnswered)
				}
				cluster.mu.Unlock()
			}
			return
		}
		if list && query.Get("limit") != "1" && cluster.order.Load() == secretOrderFirst {
			cluster.mu.Lock()
			answered := cluster.secretAnswered
			cluster.mu.Unlock()
			select {
			case <-answered:
			case <-time.After(100 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		cluster.inflight.Add(1)
		fixture.ServeHTTP(w, r)
		cluster.lastOther.Store(time.Now().UnixNano())
		cluster.inflight.Add(-1)
	}))
	t.Cleanup(upstream.Close)
	cluster.endpoint = upstream.URL
	cluster.provider = storage.NewMemory()
	cluster.connect()
	return cluster
}

// connect builds the connector service over the cluster's storage, as a
// process start does.
func (cluster *partialRoleCluster) connect() {
	t := cluster.t
	t.Helper()
	client, err := NewClient(Config{Endpoint: cluster.endpoint, AllowLoopbackHTTP: true, Credentials: staticCredential(t, "partial-role-synthetic")})
	if err != nil {
		t.Fatal(err)
	}
	client.indexLimiter = nil
	cluster.service = NewService(client, Scope{OrgID: "default", ClusterID: "partial-role", CredentialID: "configured"}, time.Hour)
	cluster.service.SetChangeStorage(cluster.provider)
	cluster.registry = NewServiceRegistry(cluster.service)
	t.Cleanup(cluster.registry.Close)
}

// restart stops the running process and starts a new one on the same
// storage, the way an operator applies a changed reader role.
func (cluster *partialRoleCluster) restart() {
	cluster.registry.Close()
	cluster.connect()
	cluster.cache = nil
}

// waitOthersIdle holds the Secret answer until no other LIST has been in
// flight for a short quiet period, so the denial is the last LIST result.
func (cluster *partialRoleCluster) waitOthersIdle() {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		quiet := time.Since(time.Unix(0, cluster.lastOther.Load())) > 40*time.Millisecond
		if cluster.inflight.Load() == 0 && quiet {
			time.Sleep(10 * time.Millisecond)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// discoveryPath reports an API discovery read, which every role may make.
func discoveryPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/api" || path == "/apis" || path == "/version" || path == "/api/v1":
		return true
	case len(parts) == 3 && parts[0] == "apis":
		return true
	}
	return false
}

func (cluster *partialRoleCluster) start() {
	t := cluster.t
	t.Helper()
	cluster.registry.StartCapture(t.Context())
	deadline := time.Now().Add(15 * time.Second)
	for {
		report, err := cluster.service.SyncHealth(t.Context())
		// A role that reads Pods only inside namespaces never reports the
		// cluster fully ready, so its first complete publication is partial.
		// A role without any cluster-wide grant first publishes a cluster it
		// cannot read at all, before it learns the namespaces it may read.
		published := !report.FirstReadyAt.IsZero() || cluster.denyClusterPods.Load() && report.State == "partial" || cluster.namespaceOnly.Load() && report.TotalKinds > 0 && report.PendingKinds == 0
		if (err == nil || cluster.namespaceOnly.Load()) && report.PendingKinds == 0 && published {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for complete initial publication: state=%s pending=%d first=%s err=%v", report.State, report.PendingKinds, report.FirstReadyAt, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	cluster.service.changes.mu.Lock()
	cluster.cache = cluster.service.changes.inventories[cluster.service.indexScope()]
	cluster.service.changes.mu.Unlock()
	if cluster.cache == nil {
		t.Fatal("inventory worker did not start")
	}
	cluster.settle()
}

func (cluster *partialRoleCluster) attempts() uint64 {
	recovery := cluster.service.recovery
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	return recovery.attempts
}

func (cluster *partialRoleCluster) wake() {
	select {
	case cluster.cache.wake <- struct{}{}:
	default:
	}
}

// settle returns once the worker has begun a later iteration, so the cycle
// that ran before it has finished publishing.
func (cluster *partialRoleCluster) settle() {
	before := cluster.attempts()
	cluster.wake()
	waitUntil(cluster.t, 15*time.Second, "worker iteration to finish", func() bool { return cluster.attempts() > before })
}

func (cluster *partialRoleCluster) committed() inventoryGeneration {
	cluster.cache.mu.RLock()
	defer cluster.cache.mu.RUnlock()
	return cluster.cache.generation
}

// force makes the named kinds due (every kind when due is nil) and wakes the
// worker. It returns the time the cycle was admitted.
func (cluster *partialRoleCluster) force(due map[string]bool) time.Time {
	t := cluster.t
	cluster.service.InvalidateDiscovery()
	cluster.mu.Lock()
	cluster.secretAnswered = make(chan struct{})
	cluster.mu.Unlock()
	cluster.cache.mu.Lock()
	candidate, err := cloneInventoryGeneration(cluster.cache.generation)
	if err != nil {
		cluster.cache.mu.Unlock()
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for kind, state := range candidate.View.Kinds {
		state.LastAttemptAt = now
		if due == nil || due[kind] {
			state.LastAttemptAt = now.Add(-24 * time.Hour)
		}
		candidate.View.Kinds[kind] = state
	}
	for namespace, scope := range candidate.Namespaces {
		for kind, state := range scope.Kinds {
			if due == nil || due[kind] {
				state.LastAttemptAt = now.Add(-24 * time.Hour)
				scope.Kinds[kind] = state
			}
		}
		candidate.Namespaces[namespace] = scope
	}
	candidate.Usage.ObservedAt = now
	cluster.cache.generation = candidate
	cluster.cache.mu.Unlock()
	cluster.wake()
	return now
}

// cycle forces a cycle, waits for its publication, then for the worker to
// finish it. The cycle is retried if a cycle already running absorbed it.
func (cluster *partialRoleCluster) cycle(due map[string]bool, witness string) {
	t := cluster.t
	t.Helper()
	for attempt := 0; ; attempt++ {
		forced := cluster.force(due)
		deadline := time.Now().Add(5 * time.Second)
		published := false
		for time.Now().Before(deadline) {
			if cluster.committed().View.Kinds[witness].LastAttemptAt.After(forced) {
				published = true
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if published {
			break
		}
		if attempt == 3 {
			t.Fatalf("forced cycle was never published (witness kind %s)", witness)
		}
	}
	cluster.settle()
}

func (cluster *partialRoleCluster) refusal() string {
	cluster.cache.refusal.mu.Lock()
	defer cluster.cache.refusal.mu.Unlock()
	return cluster.cache.refusal.logged
}

// readable is the view a least-privilege install must keep on every cycle.
func (cluster *partialRoleCluster) readable() error {
	ctx := cluster.t.Context()
	report, err := cluster.service.SyncHealth(ctx)
	if err != nil {
		return fmt.Errorf("sync health failed: %w", err)
	}
	// A kind read only inside namespaces is a partial view the role chose,
	// not a sync fault; anything else failing is.
	failing, faults := []string{}, 0
	for _, problem := range report.Failing {
		failing = append(failing, problem.Kind+"="+problem.Class)
		if problem.Class != "namespace_scope" {
			faults++
		}
	}
	if report.State == "error" || faults != 0 || report.FailingKinds > len(report.Failing) {
		return fmt.Errorf("sync health state=%s class=%s failing=%d %v at=%s stages=%s", report.State, report.Class, report.FailingKinds, failing, time.Now().Format(time.RFC3339Nano), cluster.stages())
	}
	if _, err := cluster.service.Overview(ctx); err != nil {
		return fmt.Errorf("overview failed: %w", err)
	}
	workloads, err := cluster.service.Workloads(ctx, WorkloadListOptions{})
	if err != nil || len(workloads.Items) == 0 {
		return fmt.Errorf("workloads unreadable: items=%d err=%v", len(workloads.Items), err)
	}
	return nil
}

func TestSteadyPermissionGapNeverFlapsTheClusterView(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"without-secrets", "without-secrets-or-gateway-api", "namespace-only-pods"} {
		t.Run(profile, func(t *testing.T) {
			t.Parallel()
			gatewayAPI := profile == "without-secrets-or-gateway-api"
			cluster := newPartialRoleCluster(t, gatewayAPI)
			cluster.denySecrets.Store(true)
			cluster.denyGateway.Store(gatewayAPI)
			cluster.denyClusterPods.Store(profile == "namespace-only-pods")
			cluster.start()
			if profile == "namespace-only-pods" {
				// Commit the namespace-scoped Pod reads before counting.
				cluster.cycle(nil, "Deployment")
				if state := cluster.committed().View.Kinds["Pod"]; state.Error != "namespace_scope" {
					t.Fatalf("Pod was not read inside namespaces: %+v", state)
				}
			}
			report, err := cluster.service.SyncHealth(t.Context())
			if err != nil || report.Kinds["Secret"].Error != "forbidden" {
				t.Fatalf("the Secret capability gap was not reported: %+v %v", report.Kinds["Secret"], err)
			}
			if gatewayAPI && report.Kinds["Gateway"].Error != "forbidden" {
				t.Fatalf("the Gateway API capability gap was not reported: %+v", report.Kinds["Gateway"])
			}
			checks := cluster.cache.authorityChecks.Load()
			const cycles = 50
			holdsOnEveryCycle(t, cycles, func(cycle int) {
				if cycle%2 == 0 {
					cluster.order.Store(secretOrderFirst)
				} else {
					cluster.order.Store(secretOrderLast)
				}
				cluster.cycle(nil, "Deployment")
				if failure := cluster.committed().EvidenceFailure; failure != "" {
					t.Fatalf("cycle %d: history capture refused class=%s", cycle+1, failure)
				}
				if refusal := cluster.refusal(); strings.Contains(refusal, "authority_unverified") {
					t.Fatalf("cycle %d: history capture refused %s", cycle+1, refusal)
				}
				if ran := cluster.cache.authorityChecks.Load() - checks; ran != 0 {
					t.Fatalf("cycle %d: a steady permission gap ran %d authority checks", cycle+1, ran)
				}
			}, cluster.readable)
		})
	}
}

func TestReadersKeepLastGenerationWhileANewDenialIsProven(t *testing.T) {
	t.Parallel()
	for _, denied := range []string{"secrets", "gateway-api"} {
		t.Run(denied, func(t *testing.T) {
			t.Parallel()
			cluster := newPartialRoleCluster(t, true)
			cluster.start()
			// Run the remaining kinds once so the generation holds both
			// target kinds as read successfully under this binding.
			cluster.cycle(nil, "Pod")
			before := cluster.committed()
			kinds := []string{"Secret"}
			if denied == "gateway-api" {
				kinds = []string{"Gateway", "HTTPRoute"}
			}
			secrets := 0
			for _, record := range before.View.Records {
				if record.Kind == "Secret" {
					secrets++
				}
			}
			for _, kind := range kinds {
				if state := before.View.Kinds[kind]; state.State != "ready" || state.Error != "" {
					t.Fatalf("%s was not readable before the denial: %+v", kind, state)
				}
			}
			if denied == "secrets" && secrets == 0 {
				t.Fatal("fixture holds no Secret records to protect")
			}
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			hold := func() {
				select {
				case started <- struct{}{}:
				default:
				}
				<-release
			}
			cluster.cache.authorityProof.holdProbe.Store(&hold)
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			checks := cluster.cache.authorityChecks.Load()
			if denied == "secrets" {
				cluster.denySecrets.Store(true)
			} else {
				cluster.denyGateway.Store(true)
			}
			due := map[string]bool{}
			for _, kind := range kinds {
				due[kind] = true
			}
			cluster.force(due)
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the new denial did not start an authority proof")
			}
			ctx := t.Context()
			for i := 0; i < 20; i++ {
				if err := cluster.readable(); err != nil {
					t.Fatalf("a pending proof hid the last generation: %v", err)
				}
				report, err := cluster.service.SyncHealth(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, kind := range kinds {
					if report.Kinds[kind].Error != "forbidden" {
						t.Fatalf("%s whose new denial is in question was still served as readable: %+v", kind, report.Kinds[kind])
					}
				}
				view, _, err := cluster.service.CachedIndexSnapshot(ctx, kinds...)
				if err != nil {
					t.Fatalf("pending proof failed a per-kind read: %v", err)
				}
				for _, record := range view.Records {
					for _, kind := range kinds {
						if record.Kind == kind {
							t.Fatalf("a record of newly denied %s was served while its proof was pending", kind)
						}
					}
				}
			}
			if cluster.cache.authorityChecks.Load() == checks {
				t.Fatal("the new denial ran no authority check")
			}
			revision := cluster.committed().Revision
			released = true
			close(release)
			waitUntil(t, 10*time.Second, "publication after the proof passed", func() bool {
				return cluster.committed().Revision != revision
			})
			cluster.settle()
			after := cluster.committed()
			for _, kind := range kinds {
				if after.View.Kinds[kind].Error != "forbidden" {
					t.Fatalf("published %s did not keep its denial: %+v", kind, after.View.Kinds[kind])
				}
			}
			if after.EvidenceFailure != "" || strings.Contains(cluster.refusal(), "authority_unverified") {
				t.Fatalf("a passed proof refused the capture: class=%s refusal=%s", after.EvidenceFailure, cluster.refusal())
			}
			cluster.cache.mu.RLock()
			failure := cluster.cache.authorityFailure
			cluster.cache.mu.RUnlock()
			if failure != nil {
				t.Fatalf("a passed proof left an authority failure: %v", failure)
			}
			if err := cluster.readable(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLostReadAuthorityStillHidesDataAndRefusesCapture(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	cluster.cycle(nil, "Pod")
	committed := cluster.committed()
	if committed.View.Kinds["Pod"].State != "ready" {
		t.Fatal("Pod was not readable before the permission loss")
	}
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "partial-role", ClusterUID: "partial-role-physical"}
	store := cluster.provider.(storage.EvidenceStore)
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	hold := func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	cluster.cache.authorityProof.holdProbe.Store(&hold)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	checks := cluster.cache.authorityChecks.Load()
	cluster.denyAll.Store(true)
	cluster.force(nil)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the lost read authority did not start an authority proof")
	}
	ctx := t.Context()
	pods, _, err := cluster.service.CachedIndexSnapshot(ctx, "Pod")
	if err == nil {
		for _, record := range pods.Records {
			if record.Kind == "Pod" {
				t.Fatal("a Pod record was served while its denial waited for proof")
			}
		}
	}
	workloads, _ := cluster.service.Workloads(ctx, WorkloadListOptions{})
	if len(workloads.Items) != 0 {
		t.Fatal("workloads were served while the denial waited for proof")
	}
	released = true
	close(release)
	waitUntil(t, 10*time.Second, "failed proof to fail the cycle", func() bool {
		cluster.cache.mu.RLock()
		defer cluster.cache.mu.RUnlock()
		return cluster.cache.failed
	})
	if cluster.cache.authorityChecks.Load() == checks {
		t.Fatal("the lost read authority ran no authority check")
	}
	denied, err := cluster.service.Workloads(ctx, WorkloadListOptions{})
	if !errors.Is(err, ErrForbidden) || len(denied.Items) != 0 {
		t.Fatalf("data stayed readable after the proof failed: items=%d err=%v", len(denied.Items), err)
	}
	if cluster.committed().Revision != committed.Revision {
		t.Fatal("a failed proof published a new generation")
	}
	current, err := store.EvidenceCheckpoint(ctx, scope)
	if err != nil || current.ID != checkpoint.ID {
		t.Fatalf("a failed proof saved a capture: %+v err=%v", current, err)
	}
}

func TestOnlyADenialOfAKindReadBeforeUnderTheSameBindingIsMaterial(t *testing.T) {
	at := time.Now().UTC()
	earlier := at.Add(-time.Minute)
	ready := kubeindex.KindStatus{State: "ready", ObservedAt: earlier, LastAttemptAt: earlier}
	gap := kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden", LastAttemptAt: earlier}
	deniedNow := kubeindex.KindStatus{State: "error", Partial: true, Error: "forbidden", LastAttemptAt: at}
	committed := inventoryGeneration{
		View:           kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": ready, "Secret": gap, "Gateway": gap}},
		ResourceStatus: map[string]kubeindex.KindStatus{"example.com~v1~widgets": ready, "example.com~v1~gadgets": gap},
		Namespaces:     map[string]kubeindex.Snapshot{"shop": {Kinds: map[string]kubeindex.KindStatus{"Secret": ready}}},
	}
	current := inventoryGeneration{
		View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{
			"Secret": deniedNow, "Gateway": deniedNow, "Pod": {State: "ready", ObservedAt: at, LastAttemptAt: at}, "Ingress": deniedNow,
		}},
		ResourceStatus: map[string]kubeindex.KindStatus{"example.com~v1~gadgets": deniedNow},
		Namespaces:     map[string]kubeindex.Snapshot{"shop": {Kinds: map[string]kubeindex.KindStatus{"Secret": gap}}},
	}
	baseline := newInventoryAuthorityBaseline("binding", "binding", committed)
	if currentMaterialDenied(baseline, current, at) {
		t.Fatalf("steady and first-seen denials were material: %+v", newlyDeniedInventory(baseline, current, at))
	}
	if currentMaterialDenied(inventoryAuthorityBaseline{}, current, at) {
		t.Fatal("a first sync treated its denials as material")
	}

	lost := current
	lost.View.Kinds = maps.Clone(current.View.Kinds)
	lost.View.Kinds["Pod"] = deniedNow
	questioned := newlyDeniedInventory(baseline, lost, at)
	if !questioned.kinds["Pod"] || len(questioned.kinds) != 1 {
		t.Fatalf("a kind read before and denied now was not material: %+v", questioned)
	}
	if currentMaterialDenied(newInventoryAuthorityBaseline("binding", "rebound", committed), lost, at) {
		t.Fatal("a baseline from another binding judged the new binding")
	}
	stale := lost
	stale.View.Kinds = maps.Clone(lost.View.Kinds)
	stale.View.Kinds["Pod"] = kubeindex.KindStatus{State: "error", Error: "forbidden", LastAttemptAt: earlier}
	if currentMaterialDenied(baseline, stale, at) {
		t.Fatal("a denial from an earlier attempt was judged again")
	}

	resource := current
	resource.ResourceStatus = map[string]kubeindex.KindStatus{"example.com~v1~widgets": deniedNow}
	if questioned := newlyDeniedInventory(baseline, resource, at); !questioned.resources["example.com~v1~widgets"] {
		t.Fatal("a readable API resource denied now was not material")
	}
	scoped := current
	scoped.Namespaces = map[string]kubeindex.Snapshot{"shop": {Kinds: map[string]kubeindex.KindStatus{"Secret": deniedNow}}}
	if questioned := newlyDeniedInventory(baseline, scoped, at); !questioned.scoped["shop"]["Secret"] {
		t.Fatal("a namespace read denied now was not material")
	}
}

func TestQuestionedKindsAreWithheldWithoutChangingTheCommittedGeneration(t *testing.T) {
	at := time.Now().UTC()
	ready := kubeindex.KindStatus{State: "ready", ObservedAt: at, LastAttemptAt: at}
	committed := inventoryGeneration{
		View:           kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": ready, "Secret": ready}},
		ResourceStatus: map[string]kubeindex.KindStatus{"example.com~v1~widgets": ready},
		Namespaces:     map[string]kubeindex.Snapshot{"shop": {Kinds: map[string]kubeindex.KindStatus{"Secret": ready}}},
	}
	questioned := inventoryQuestionedKinds{kinds: map[string]bool{"Secret": true}}.merge(inventoryQuestionedKinds{
		resources: map[string]bool{"example.com~v1~widgets": true},
		scoped:    map[string]map[string]bool{"shop": {"Secret": true}},
	})
	view := questionInventoryKinds(committed, questioned)
	if view.View.Kinds["Secret"].Error != "forbidden" || view.ResourceStatus["example.com~v1~widgets"].Error != "forbidden" || view.Namespaces["shop"].Kinds["Secret"].Error != "forbidden" {
		t.Fatalf("questioned kinds stayed readable: %+v", view)
	}
	if view.View.Kinds["Pod"] != ready || view.View.Kinds["Secret"].CycleFailure {
		t.Fatal("a questioned kind changed an unrelated kind or became a whole-cycle failure")
	}
	if committed.View.Kinds["Secret"] != ready || committed.ResourceStatus["example.com~v1~widgets"] != ready || committed.Namespaces["shop"].Kinds["Secret"] != ready {
		t.Fatal("withholding a kind mutated the committed generation")
	}
}

func TestLosingANamespaceGrantStillRunsTheProof(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.denyClusterPods.Store(true)
	cluster.start()
	cluster.cycle(nil, "Deployment")
	committed := cluster.committed()
	namespace := ""
	for name, scope := range committed.Namespaces {
		if state := scope.Kinds["Pod"]; state.State == "ready" && !state.Partial {
			for _, record := range scope.Records {
				if record.Kind == "Pod" {
					namespace = name
				}
			}
		}
	}
	if namespace == "" {
		t.Fatalf("no namespace held Pod records read under its own grant: %+v", committed.Namespaces)
	}
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "partial-role", ClusterUID: "partial-role-physical"}
	store := cluster.provider.(storage.EvidenceStore)
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	hold := func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	cluster.cache.authorityProof.holdProbe.Store(&hold)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	checks := cluster.cache.authorityChecks.Load()
	// The namespace grant goes away and nothing the proof could read remains.
	cluster.denyNamespacedPods.Store(true)
	cluster.denyProbes.Store(true)
	cluster.force(map[string]bool{"Pod": true})
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("losing a namespace grant did not start an authority proof")
	}
	ctx := t.Context()
	view, _, err := cluster.service.CachedNamespaceSnapshot(ctx, namespace, "Pod")
	if err == nil {
		for _, record := range view.Records {
			if record.Kind == "Pod" {
				t.Fatal("a Pod record of the lost namespace grant was served while its proof was pending")
			}
		}
	}
	released = true
	close(release)
	waitUntil(t, 10*time.Second, "failed proof to fail the cycle", func() bool {
		cluster.cache.mu.RLock()
		defer cluster.cache.mu.RUnlock()
		return cluster.cache.failed || cluster.cache.authorityFailure != nil
	})
	if cluster.cache.authorityChecks.Load() == checks {
		t.Fatal("losing a namespace grant ran no authority check")
	}
	if pods, _, err := cluster.service.CachedNamespaceSnapshot(ctx, namespace, "Pod"); err == nil {
		for _, record := range pods.Records {
			if record.Kind == "Pod" {
				t.Fatal("a Pod record stayed readable after the proof failed")
			}
		}
	}
	if cluster.committed().Revision != committed.Revision {
		t.Fatal("a failed proof published a new generation")
	}
	current, err := store.EvidenceCheckpoint(ctx, scope)
	if err != nil || current.ID != checkpoint.ID {
		t.Fatalf("a failed proof saved a capture: %+v err=%v", current, err)
	}
}

func TestQuestionedClusterKindIsNotServedFromRetainedNamespaceReads(t *testing.T) {
	at := time.Now().UTC()
	ready := kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at, LastAttemptAt: at, Records: 1}
	pod := kubeindex.Record{Kind: "Pod", Namespace: "shop", Name: "api", UID: "pod-api"}
	committed := inventoryGeneration{
		View:       kubeindex.Snapshot{Records: []kubeindex.Record{pod}, Kinds: map[string]kubeindex.KindStatus{"Pod": ready}},
		Namespaces: map[string]kubeindex.Snapshot{"shop": {Records: []kubeindex.Record{pod}, Kinds: map[string]kubeindex.KindStatus{"Pod": ready}}},
	}
	withheld := questionInventoryKinds(committed, inventoryQuestionedKinds{kinds: map[string]bool{"Pod": true}})
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "org", ClusterID: "retained-scope"})
	view, _, err := service.CachedNamespaceSnapshot(pinInventory(t.Context(), withheld), "shop", "Pod")
	if err != nil && !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	for _, record := range view.Records {
		if record.Kind == "Pod" {
			t.Fatal("a questioned kind was served from its retained namespace read")
		}
	}
}

// stages names the last reason the worker recorded for each capture stage, so
// a broken invariant says which stage of the cycle failed.
func (cluster *partialRoleCluster) stages() string {
	recovery := cluster.service.recovery
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	names := make([]string, 0, len(recovery.stages))
	for stage, value := range recovery.stages {
		names = append(names, stage+"="+value.reason+"@"+value.at.Format("15:04:05.000"))
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// A cluster that serves many API groups, the Gateway API among them, keeps the
// served Gateway kinds listed and the cluster report out of error on every
// cycle while background reads are paced. Every forced cycle walks discovery
// again, as an expired cache does.
func TestServedGatewayKindsStayListedWhileDiscoveryWalksManyGroups(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.extraGroups.Store(150)
	// The LIST pace admits about 100 reads within the ten-second discovery
	// bound, fewer than the groups the cluster serves. Discovery keeps the
	// pace a running install gives it.
	cluster.service.client.indexLimiter = rate.NewLimiter(10, 4)
	cluster.start()
	started := time.Now().UTC()
	holdsOnEveryCycle(t, 4, func(int) { cluster.cycle(map[string]bool{"Gateway": true, "HTTPRoute": true}, "Gateway") }, func() error {
		if err := cluster.readable(); err != nil {
			return err
		}
		report, err := cluster.service.SyncHealth(cluster.t.Context())
		if err != nil {
			return err
		}
		if report.State != "ready" {
			return fmt.Errorf("sync health state=%s, want ready: failing=%+v", report.State, report.Failing)
		}
		for _, kind := range []string{"Gateway", "HTTPRoute"} {
			if state := report.Kinds[kind]; state.State != "ready" || state.Error != "" || state.Absent {
				return fmt.Errorf("served %s was not listed: %+v stages=%s", kind, state, cluster.stages())
			}
		}
		return nil
	})
	// Every cycle above re-read the Gateway kinds through a fresh discovery
	// walk; a kept earlier read would not carry this cycle's attempt.
	if state := cluster.committed().View.Kinds["Gateway"]; !state.LastAttemptAt.After(started) {
		t.Fatalf("the Gateway kinds were not read by the checked cycles: %+v", state)
	}
}

// A new credential whose API group discovery is denied cannot read the kinds
// of those groups. The kinds the old credential read there are not served to
// it on any cycle: the denial is recorded as the new credential's answer, not
// kept aside as a read that did not happen.
func TestSwappedCredentialWithDeniedGroupDiscoveryServesNoOldRecords(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.gatewayRecords.Store(true)
	cluster.start()
	cluster.cycle(nil, "Pod")
	for _, kind := range []string{"Deployment", "Gateway"} {
		if cluster.recordsOf(kind) == 0 {
			t.Fatalf("fixture holds no %s records read by the first credential", kind)
		}
	}
	cluster.swapCredential()
	cluster.denyGroupDiscovery.Store(true)
	cluster.cycle(nil, "Pod")
	holdsOnEveryCycle(t, 4, func(int) { cluster.cycle(nil, "Pod") }, func() error {
		for _, kind := range []string{"Deployment", "Gateway"} {
			if served := cluster.recordsOf(kind); served != 0 {
				return fmt.Errorf("%d %s records read by the replaced credential were served: %+v", served, kind, cluster.committed().View.Kinds[kind])
			}
		}
		return nil
	})
}

// An API server that answers nothing fails the cycle as a whole, even though
// the kinds that were not due still hold an earlier read.
func TestAPIServerOutageIsAClusterError(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	cluster.cycle(nil, "Pod")
	cluster.outage.Store(true)
	before := cluster.attempts()
	cluster.force(map[string]bool{"Pod": true, "Event": true})
	waitUntil(t, 15*time.Second, "the outage cycle to finish", func() bool { return cluster.attempts() >= before+2 })
	report, err := cluster.service.SyncHealth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.State != "error" {
		t.Fatalf("an API server outage reported state=%s: failing=%+v", report.State, report.Failing)
	}
}

// syncState is the aggregate the cluster reports now.
func (cluster *partialRoleCluster) syncState() string {
	report, err := cluster.service.SyncHealth(cluster.t.Context())
	if err != nil {
		return "unreadable: " + err.Error()
	}
	return report.State
}

// One kind whose LIST fails, alone or beside core kinds, fails on its own.
// The cluster report never turns into an error, also on the cycles where that
// kind is the only one due, as it is between the core kinds' cadences.
func TestOneFailingKindNeverShowsAClusterError(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"unavailable", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			cluster := newPartialRoleCluster(t, true)
			cluster.start()
			if failure == "timeout" {
				cluster.service.client.timeout = 300 * time.Millisecond
				cluster.gatewayHang.Store(true)
			} else {
				cluster.gatewayUnavailable.Store(true)
			}
			holdsOnEveryCycle(t, 10, func(cycle int) {
				due := map[string]bool{"Gateway": true}
				if cycle%3 == 2 {
					due["Pod"] = true
				}
				cluster.cycle(due, "Gateway")
				if state := cluster.committed().View.Kinds["Gateway"]; state.Error != failure {
					t.Fatalf("cycle %d: Gateway failed as %+v, want %s", cycle+1, state, failure)
				}
			}, func() error {
				if state := cluster.syncState(); state == "error" || strings.HasPrefix(state, "unreadable") {
					return fmt.Errorf("one failing kind showed state=%s stages=%s", state, cluster.stages())
				}
				return nil
			})
		})
	}
}

// An outage that begins on a cycle where only an optional kind is due shows
// as an error no later than the next cycle that lists a core kind, and the
// report is ready again once that kind is read after the outage.
func TestOutageShowsByTheNextCoreCycleAndRecovers(t *testing.T) {
	t.Parallel()
	cluster := newPartialRoleCluster(t, true)
	cluster.start()
	cluster.outage.Store(true)
	cluster.cycle(map[string]bool{"Gateway": true}, "Gateway")
	cluster.cycle(map[string]bool{"Pod": true}, "Pod")
	if state := cluster.syncState(); state != "error" {
		t.Fatalf("the outage showed state=%s after a core cycle", state)
	}
	holdsOnEveryCycle(t, 3, func(int) { cluster.cycle(map[string]bool{"Pod": true}, "Pod") }, func() error {
		if state := cluster.syncState(); state != "error" {
			return fmt.Errorf("a running outage showed state=%s", state)
		}
		return nil
	})
	cluster.outage.Store(false)
	cluster.cycle(map[string]bool{"Pod": true, "Gateway": true}, "Pod")
	holdsOnEveryCycle(t, 3, func(int) { cluster.cycle(map[string]bool{"Pod": true}, "Pod") }, func() error {
		if state := cluster.syncState(); state != "ready" {
			return fmt.Errorf("after the outage state=%s", state)
		}
		return nil
	})
}

// namespaceOnlyCluster runs a cluster that was ready under the full role and
// was then restarted with a Role and RoleBinding in a few namespaces and no
// cluster-wide grant, after a view has asked for those namespaces.
func namespaceOnlyCluster(t *testing.T) *partialRoleCluster {
	t.Helper()
	cluster := newPartialRoleCluster(t, false)
	cluster.start()
	cluster.denySecrets.Store(true)
	cluster.namespaceOnly.Store(true)
	cluster.restart()
	cluster.start()
	for _, namespace := range []string{"shop", "payments"} {
		cluster.service.queueInventoryNamespace(namespace)
	}
	cluster.wake()
	waitUntil(t, 20*time.Second, "namespaced kinds read inside the granted namespaces", func() bool {
		committed := cluster.committed()
		return len(committed.View.Kinds) == len(indexedResourceIDs) && committed.View.Kinds["Deployment"].Error == "namespace_scope" && committed.View.Kinds["Pod"].Error == "namespace_scope"
	})
	cluster.cycle(nil, "Pod")
	return cluster
}

func unreadableKinds(report SyncHealth) []string {
	kinds := []string{}
	for _, row := range report.Unreadable {
		kinds = append(kinds, row.Kind)
	}
	sort.Strings(kinds)
	return kinds
}

// A Role and RoleBinding in a few namespaces, with no cluster-wide grant,
// holds one answer: the namespaced kinds are read inside the granted
// namespaces, the kinds only a cluster-wide grant can read are not readable,
// and the report does not switch between two answers from cycle to cycle.
func TestNamespaceOnlyRoleHoldsOneSyncReport(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	ctx := t.Context()
	first, err := cluster.service.SyncHealth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(unreadableKinds(first), ",")
	for _, kind := range []string{"Namespace", "Node", "Secret"} {
		if !strings.Contains(","+want+",", ","+kind+",") {
			t.Fatalf("%s is not reported as not readable: %s", kind, want)
		}
	}
	for _, kind := range []string{"Pod", "Deployment", "Service", "Event"} {
		if strings.Contains(","+want+",", ","+kind+",") {
			t.Fatalf("%s read inside the granted namespaces is reported as not readable: %s", kind, want)
		}
	}
	holdsOnEveryCycle(t, 12, func(cycle int) {
		due := map[string]bool(nil)
		if cycle%2 == 1 {
			// Kinds fall due at different times in a running install.
			due = map[string]bool{"Pod": true, "Event": true}
		}
		cluster.cycle(due, "Pod")
	}, func() error {
		report, err := cluster.service.SyncHealth(ctx)
		if err != nil {
			return fmt.Errorf("sync health failed: %w", err)
		}
		if report.State != first.State || report.State == "error" {
			return fmt.Errorf("sync health state=%s, held %s: unreadable=%v failing=%+v stages=%s", report.State, first.State, unreadableKinds(report), report.Failing, cluster.stages())
		}
		if got := strings.Join(unreadableKinds(report), ","); got != want {
			return fmt.Errorf("not readable kinds changed to %s from %s", got, want)
		}
		for _, row := range report.Failing {
			if row.Class != "namespace_scope" {
				return fmt.Errorf("%s failed with %s", row.Kind, row.Class)
			}
		}
		workloads, err := cluster.service.Workloads(ctx, WorkloadListOptions{Namespace: "shop"})
		if err != nil || len(workloads.Items) == 0 {
			return fmt.Errorf("workloads of a granted namespace unreadable: items=%d err=%v", len(workloads.Items), err)
		}
		return nil
	})
}

// When a namespace-only grant is widened to cluster-wide, the first
// cluster-wide read of one such kind brings every other kind read only
// inside namespaces back to a cluster-wide read on the next cycle, rather
// than each waiting for its own cadence.
func TestWidenedGrantReachesReadyWithinTwoCycles(t *testing.T) {
	t.Parallel()
	cluster := namespaceOnlyCluster(t)
	cluster.namespaceOnly.Store(false)
	cluster.denySecrets.Store(false)
	for cycle := 0; cycle < 2; cycle++ {
		cluster.cycle(map[string]bool{"Pod": true}, "Pod")
	}
	if state := cluster.syncState(); state != "ready" {
		report, _ := cluster.service.SyncHealth(t.Context())
		t.Fatalf("a widened grant reported state=%s after two cycles: unreadable=%v failing=%+v", state, unreadableKinds(report), report.Failing)
	}
	holdsOnEveryCycle(t, 3, func(int) { cluster.cycle(map[string]bool{"Pod": true}, "Pod") }, func() error {
		if state := cluster.syncState(); state != "ready" {
			return fmt.Errorf("after the widened grant state=%s", state)
		}
		return nil
	})
}
