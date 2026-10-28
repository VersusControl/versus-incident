package kubernetes

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Boot proofs for a fleet of clusters. Every configured cluster publishes its
// own inventory generation at boot without any reader asking first, and one
// cluster's worst case — unreachable, forbidden, stalled, or answering with a
// cyclic page token — never delays, corrupts or blocks another cluster.
//
// Fixtures are TLS API servers built in code, so the proofs need no external
// dependency and no credential material on disk.

type fixtureResource struct {
	Group      string
	Version    string
	Resource   string
	Kind       string
	Namespaced bool
}

// fixtureResources is the catalogue a fake cluster advertises. Only indexed
// kinds are advertised, so the request counts an assertion sees are exactly the
// work the connector chose to do.
var fixtureResources = []fixtureResource{
	{Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true},
	{Version: "v1", Resource: "nodes", Kind: "Node"},
	{Version: "v1", Resource: "namespaces", Kind: "Namespace"},
	{Version: "v1", Resource: "events", Kind: "Event", Namespaced: true},
	{Version: "v1", Resource: "services", Kind: "Service", Namespaced: true},
	{Version: "v1", Resource: "configmaps", Kind: "ConfigMap", Namespaced: true},
	{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Namespaced: true},
	{Group: "apps", Version: "v1", Resource: "replicasets", Kind: "ReplicaSet", Namespaced: true},
	{Group: "apps", Version: "v1", Resource: "statefulsets", Kind: "StatefulSet", Namespaced: true},
	{Group: "apps", Version: "v1", Resource: "daemonsets", Kind: "DaemonSet", Namespaced: true},
	{Group: "apps", Version: "v1", Resource: "controllerrevisions", Kind: "ControllerRevision", Namespaced: true},
	{Group: "batch", Version: "v1", Resource: "jobs", Kind: "Job", Namespaced: true},
	{Group: "batch", Version: "v1", Resource: "cronjobs", Kind: "CronJob", Namespaced: true},
	{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true},
}

// fakeCluster is one in-code Kubernetes API server over TLS.
type fakeCluster struct {
	token string

	mu       sync.Mutex
	extra    map[string]bool
	pods     []string
	objects  map[string][]map[string]any
	served   map[string]bool
	forbid   bool
	stall    chan struct{}
	cyclic   bool
	requests map[string]int
	callers  map[string]int

	server  *httptest.Server
	accepts atomic.Int32
}

func newFakeCluster(t *testing.T, token string, pods ...string) *fakeCluster {
	t.Helper()
	cluster := &fakeCluster{token: token, extra: map[string]bool{}, pods: pods, served: map[string]bool{}, requests: map[string]int{}, callers: map[string]int{}, objects: map[string][]map[string]any{}}
	for _, resource := range fixtureResources {
		cluster.served[resource.Resource] = true
	}
	server := httptest.NewUnstartedServer(cluster)
	server.Listener = &countingListener{Listener: server.Listener, accepts: &cluster.accepts}
	server.StartTLS()
	cluster.server = server
	t.Cleanup(server.Close)
	return cluster
}

// countingListener counts accepted connections, so a proof can tell an idle
// failing cluster from one retrying without bound.
type countingListener struct {
	net.Listener
	accepts *atomic.Int32
}

func (listener *countingListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err == nil {
		listener.accepts.Add(1)
	}
	return connection, err
}

// endpoint is the TLS URL the connector dials.
func (cluster *fakeCluster) endpoint() string { return cluster.server.URL }

// trustedConfig pins the client to this cluster's certificate.
func (cluster *fakeCluster) trustedConfig(credentials CredentialSource) Config {
	config := cluster.config(credentials)
	config.CAData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cluster.server.Certificate().Raw})
	return config
}

// config leaves the certificate unpinned, so every dial fails the handshake.
func (cluster *fakeCluster) config(credentials CredentialSource) Config {
	return Config{Endpoint: cluster.endpoint(), Credentials: credentials, AllowLoopback: true, Timeout: 2 * time.Second}
}

func (cluster *fakeCluster) serve(resource string, items ...map[string]any) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.served[resource] = true
	cluster.objects[resource] = items
}

func (cluster *fakeCluster) unserve(resource string) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.served[resource] = false
}

// rotateToken swaps the one bearer token this cluster accepts.
func (cluster *fakeCluster) rotateToken(token string) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.token = token
}

// acceptToken admits one more bearer token for the same principal, which is how
// two replicas present a rotated token for one identity.
func (cluster *fakeCluster) acceptToken(token string) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.extra["Bearer "+token] = true
}

func (cluster *fakeCluster) denyLists() {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.forbid = true
}

// stallLists holds every LIST open until the returned release function runs.
func (cluster *fakeCluster) stallLists() func() {
	release := make(chan struct{})
	cluster.mu.Lock()
	cluster.stall = release
	cluster.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			cluster.mu.Lock()
			cluster.stall = nil
			cluster.mu.Unlock()
			close(release)
		})
	}
}

func (cluster *fakeCluster) repeatContinueToken() {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	cluster.cyclic = true
}

func (cluster *fakeCluster) requestCount() int {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	total := 0
	for _, count := range cluster.requests {
		total += count
	}
	return total
}

func (cluster *fakeCluster) listCount(resource string) int {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	return cluster.requests[resource]
}

func (cluster *fakeCluster) callerCount(authorization string) int {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	return cluster.callers[authorization]
}

func (cluster *fakeCluster) handshakes() int { return int(cluster.accepts.Load()) }

func (cluster *fakeCluster) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	authorization := request.Header.Get("Authorization")
	path := request.URL.Path
	resource := fixtureResourceFor(path)
	cluster.mu.Lock()
	cluster.callers[authorization]++
	expected, forbid, cyclic, stall := cluster.token, cluster.forbid, cluster.cyclic, cluster.stall
	extra := map[string]bool{}
	for header := range cluster.extra {
		extra[header] = true
	}
	served := cluster.served[resource]
	items := append([]map[string]any(nil), cluster.objects[resource]...)
	pods := append([]string(nil), cluster.pods...)
	cluster.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	if authorization != "Bearer "+expected && !extra[authorization] {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Unauthorized","code":401}`))
		return
	}
	switch path {
	case "/api":
		writeJSON(writer, map[string]any{"versions": []string{"v1"}})
		return
	case "/apis":
		writeJSON(writer, map[string]any{"groups": fixtureGroups()})
		return
	}
	if discovery := cluster.discoveryFor(path); discovery != nil {
		writeJSON(writer, map[string]any{"resources": discovery})
		return
	}
	if resource == "" {
		http.NotFound(writer, request)
		return
	}
	cluster.mu.Lock()
	cluster.requests[resource]++
	cluster.mu.Unlock()
	if stall != nil {
		select {
		case <-stall:
		case <-request.Context().Done():
			return
		}
	}
	if !served {
		http.NotFound(writer, request)
		return
	}
	if forbid {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Forbidden","code":403}`))
		return
	}
	switch resource {
	case "pods":
		for _, pod := range pods {
			items = append(items, fixtureObject("v1", "Pod", "shop", pod))
		}
	case "nodes":
		items = append(items, map[string]any{"apiVersion": "v1", "kind": "Node", "metadata": map[string]any{"name": "node-1", "uid": "node-1"}, "status": map[string]any{"allocatable": map[string]any{"cpu": "2", "memory": "4Gi"}, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "lastTransitionTime": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)}}}})
	case "namespaces":
		items = append(items, fixtureObject("v1", "Namespace", "", "shop"))
	}
	if strings.Contains(request.Header.Get("Accept"), "PartialObjectMetadataList") {
		// Metadata-only kinds are negotiated as a partial object list; answer
		// in the same shape a real API server does.
		partial := []any{}
		for _, item := range items {
			partial = append(partial, map[string]any{"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1", "metadata": item["metadata"]})
		}
		writeJSON(writer, map[string]any{"kind": "PartialObjectMetadataList", "apiVersion": "meta.k8s.io/v1", "items": partial})
		return
	}
	page := map[string]any{"items": items}
	if cyclic && resource == "pods" {
		page["metadata"] = map[string]any{"continue": "fixture-cyclic-token"}
	}
	writeJSON(writer, page)
}

// fixtureResourceFor maps a list path to the resource it addresses.
func fixtureResourceFor(path string) string {
	for _, resource := range fixtureResources {
		base := "/api/" + resource.Version
		if resource.Group != "" {
			base = "/apis/" + resource.Group + "/" + resource.Version
		}
		if path == base+"/"+resource.Resource {
			return resource.Resource
		}
		if resource.Namespaced && strings.HasPrefix(path, base+"/namespaces/") && strings.HasSuffix(path, "/"+resource.Resource) {
			return resource.Resource
		}
	}
	return ""
}

// discoveryFor answers the discovery document for one API version, advertising
// only the resources this cluster currently serves, so a proof can bring a kind
// into service in a later cycle.
func (cluster *fakeCluster) discoveryFor(path string) []any {
	variants := []struct{ Group, Version string }{{Version: "v1"}}
	seen := map[string]bool{}
	for _, resource := range fixtureResources {
		if resource.Group == "" || seen[resource.Group] {
			continue
		}
		seen[resource.Group] = true
		variants = append(variants, struct{ Group, Version string }{resource.Group, resource.Version})
	}
	for _, variant := range variants {
		base := "/api/" + variant.Version
		if variant.Group != "" {
			base = "/apis/" + variant.Group + "/" + variant.Version
		}
		if path != base {
			continue
		}
		cluster.mu.Lock()
		served := map[string]bool{}
		for resource, available := range cluster.served {
			served[resource] = available
		}
		cluster.mu.Unlock()
		result := []any{}
		for _, resource := range fixtureResources {
			if resource.Group != variant.Group || !served[resource.Resource] {
				continue
			}
			result = append(result, map[string]any{"name": resource.Resource, "kind": resource.Kind, "namespaced": resource.Namespaced, "verbs": []string{"get", "list"}})
		}
		return result
	}
	return nil
}

func fixtureGroups() []any {
	seen := map[string]bool{}
	groups := []any{}
	for _, resource := range fixtureResources {
		if resource.Group == "" || seen[resource.Group] {
			continue
		}
		seen[resource.Group] = true
		groupVersion := resource.Group + "/" + resource.Version
		groups = append(groups, map[string]any{
			"name":             resource.Group,
			"versions":         []any{map[string]any{"groupVersion": groupVersion, "version": resource.Version}},
			"preferredVersion": map[string]any{"groupVersion": groupVersion, "version": resource.Version},
		})
	}
	return groups
}

func fixtureObject(apiVersion, kind, namespace, name string) map[string]any {
	metadata := map[string]any{"name": name, "uid": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	object := map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": metadata}
	if kind == "Pod" {
		object["status"] = map[string]any{"phase": "Running"}
	}
	return object
}

// inventoryBroker records which replica published which scope generation and
// when, so a proof can order publishes and attribute them to one replica.
type inventoryBroker struct {
	mu     sync.Mutex
	events []publishEvent
}

type publishEvent struct {
	Replica string
	Key     string
	At      time.Time
}

func (broker *inventoryBroker) record(event publishEvent) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.events = append(broker.events, event)
}

func (broker *inventoryBroker) first(replica, key string) (time.Time, bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	for _, event := range broker.events {
		if event.Replica == replica && event.Key == key {
			return event.At, true
		}
	}
	return time.Time{}, false
}

func (broker *inventoryBroker) count(key string) int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	total := 0
	for _, event := range broker.events {
		if event.Key == key {
			total++
		}
	}
	return total
}

// replicas lists, in order, the replicas that published one scope.
func (broker *inventoryBroker) replicas(key string) []string {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	seen := map[string]bool{}
	result := []string{}
	for _, event := range broker.events {
		if event.Key != key || seen[event.Replica] {
			continue
		}
		seen[event.Replica] = true
		result = append(result, event.Replica)
	}
	sort.Strings(result)
	return result
}

// lastPublished reports when the newest publication of a scope landed.
func (broker *inventoryBroker) lastPublished(key string) (time.Time, bool) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	var at time.Time
	for _, event := range broker.events {
		if event.Key == key {
			at = event.At
		}
	}
	return at, !at.IsZero()
}

// replicaStorage is one replica's connection to the shared store. It records
// this replica's publication points and can refuse a lease key, which is how a
// proof holds one cluster's ownership still while another cluster moves.
type replicaStorage struct {
	storage.Provider
	cas     storage.BlobCAS
	broker  *inventoryBroker
	replica string

	mu   sync.Mutex
	deny map[string]bool
}

func newReplicaStorage(replica string, base storage.Provider, broker *inventoryBroker) *replicaStorage {
	return &replicaStorage{Provider: base, cas: base.(storage.BlobCAS), broker: broker, replica: replica, deny: map[string]bool{}}
}

func (view *replicaStorage) refuse(key string) {
	view.mu.Lock()
	defer view.mu.Unlock()
	view.deny[key] = true
}

func (view *replicaStorage) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	view.mu.Lock()
	denied := view.deny[key]
	view.mu.Unlock()
	if denied {
		return false, nil
	}
	won, err := view.cas.CompareAndSwapBlob(key, expected, replacement)
	if err != nil || !won || !isInventoryPointer(key) {
		return won, err
	}
	var record inventoryLeaseRecord
	if json.Unmarshal(replacement, &record) != nil || record.Revision == "" {
		return won, err
	}
	view.broker.record(publishEvent{Replica: view.replica, Key: key, At: time.Now()})
	return won, err
}

func isInventoryPointer(key string) bool {
	return strings.HasPrefix(key, "kubernetes/inventory/") && !strings.Contains(key, "/data/")
}

// bootedCluster is one configured cluster inside a fleet proof.
type bootedCluster struct {
	id      string
	fixture *fakeCluster
	service *Service
	key     string
}

// bootFleet is a configured set of clusters started together at boot.
type bootFleet struct {
	t        *testing.T
	org      string
	provider storage.Provider
	broker   *inventoryBroker
	view     *replicaStorage
	registry *ServiceRegistry
	clusters []*bootedCluster
}

func newBootFleet(t *testing.T, org string, base storage.Provider, replica string) *bootFleet {
	t.Helper()
	return newBootFleetWithBroker(t, org, base, replica, &inventoryBroker{})
}

// newBootFleetWithBroker joins a fleet to a shared publication log, so two
// replicas over one store can be compared.
func newBootFleetWithBroker(t *testing.T, org string, base storage.Provider, replica string, broker *inventoryBroker) *bootFleet {
	t.Helper()
	return &bootFleet{t: t, org: org, provider: base, broker: broker, view: newReplicaStorage(replica, base, broker)}
}

// add configures one cluster. A short discoveryTTL keeps a proof able to
// observe a later API generation without waiting a production cache window.
func (fleet *bootFleet) add(id string, fixture *fakeCluster, credentials CredentialSource, discoveryTTL time.Duration) *bootedCluster {
	fleet.t.Helper()
	client, err := NewClient(fixture.trustedConfig(credentials))
	if err != nil {
		fleet.t.Fatalf("cluster %s client: %v", id, err)
	}
	return fleet.adopt(id, fixture, client, discoveryTTL)
}

// addUnreachable configures a cluster whose certificate is not pinned, so every
// request stops at the TLS handshake.
func (fleet *bootFleet) addUnreachable(id string, fixture *fakeCluster, credentials CredentialSource) *bootedCluster {
	fleet.t.Helper()
	client, err := NewClient(fixture.config(credentials))
	if err != nil {
		fleet.t.Fatalf("cluster %s client: %v", id, err)
	}
	return fleet.adopt(id, fixture, client, 0)
}

func (fleet *bootFleet) adopt(id string, fixture *fakeCluster, client *Client, discoveryTTL time.Duration) *bootedCluster {
	fleet.t.Helper()
	// The background rate limiter is not what these proofs measure; the
	// existing suite removes it for the same reason.
	client.indexLimiter = nil
	service := NewService(client, Scope{OrgID: fleet.org, ClusterID: id, CredentialID: id + "-configured"}, discoveryTTL)
	service.SetChangeStorage(fleet.view)
	fleet.t.Cleanup(service.StopCapture)
	cluster := &bootedCluster{id: id, fixture: fixture, service: service, key: inventoryKey(service.ActiveScope())}
	fleet.clusters = append(fleet.clusters, cluster)
	return cluster
}

// startWithin builds the registry and starts every cluster, proving the boot
// call returns on its own: a cluster that never answers must not hold the host.
func (fleet *bootFleet) startWithin(budget time.Duration) time.Duration {
	fleet.t.Helper()
	entries := make([]ClusterEntry, 0, len(fleet.clusters))
	for _, cluster := range fleet.clusters {
		entries = append(entries, ClusterEntry{Info: ClusterInfo{ID: cluster.id, Provider: "generic"}, Service: cluster.service})
	}
	fleet.registry = NewClusterRegistryWithScope(true, entries, tenancy.NewOrgScope(fleet.org))
	ctx, cancel := context.WithCancel(context.Background())
	fleet.t.Cleanup(func() {
		cancel()
		fleet.registry.Close()
	})
	done := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		fleet.registry.StartCapture(ctx)
		done <- time.Since(started)
	}()
	select {
	case elapsed := <-done:
		return elapsed
	case <-time.After(budget):
		cancel()
		fleet.t.Fatalf("boot did not return within %s: one cluster held the start-up of the others", budget)
		return 0
	}
}

func (fleet *bootFleet) publishedAt(cluster *bootedCluster) (time.Time, bool) {
	return fleet.broker.first(fleet.view.replica, cluster.key)
}

// waitPublished waits until the cluster's own generation reaches the store and
// reports how long that took from origin.
func (fleet *bootFleet) waitPublished(cluster *bootedCluster, origin time.Time, budget time.Duration) time.Duration {
	fleet.t.Helper()
	var at time.Time
	waitUntil(fleet.t, budget, "cluster "+cluster.id+" to publish its own generation", func() bool {
		published, ok := fleet.publishedAt(cluster)
		if ok {
			at = published
		}
		return ok
	})
	return at.Sub(origin)
}

func (fleet *bootFleet) published(cluster *bootedCluster) bool {
	_, ok := fleet.publishedAt(cluster)
	return ok
}

func (fleet *bootFleet) generation(cluster *bootedCluster) (inventoryGeneration, bool) {
	return publishedGeneration(fleet.provider, cluster.service.ActiveScope())
}

func publishedGeneration(provider storage.Provider, scope Scope) (inventoryGeneration, bool) {
	generation, _, err := readInventoryGeneration(provider, scope, "")
	if err != nil {
		return inventoryGeneration{}, false
	}
	return generation, generation.Revision != "" && len(generation.View.Kinds) > 0
}

func (fleet *bootFleet) resolve(cluster *bootedCluster) *Service {
	fleet.t.Helper()
	service, err := fleet.registry.ResolveCluster(fleet.org, cluster.id)
	if err != nil {
		fleet.t.Fatalf("resolve %s: %v", cluster.id, err)
	}
	return service
}

// settle waits until a scope has published nothing new for quiet, so a proof
// can compare one cluster before and after another cluster changes.
func (fleet *bootFleet) settle(cluster *bootedCluster, quiet time.Duration, budget time.Duration) {
	fleet.t.Helper()
	deadline := time.Now().Add(budget)
	for {
		last, ok := fleet.broker.lastPublished(cluster.key)
		if ok && time.Since(last) >= quiet {
			return
		}
		if time.Now().After(deadline) {
			fleet.t.Fatalf("cluster %s never stopped republishing", cluster.id)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitUntil(t *testing.T, budget time.Duration, describe string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", describe)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// podNames lists the Pod names one published generation holds.
func podNames(generation inventoryGeneration) []string {
	names := []string{}
	for _, record := range generation.View.Records {
		if record.Kind == "Pod" {
			names = append(names, record.Name)
		}
	}
	sort.Strings(names)
	return names
}

func staticCredential(t *testing.T, token string) CredentialSource {
	t.Helper()
	source, err := staticTokenCredential(token)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

// flexibleCredential is a credential a proof can rotate or retire: it models a
// projected token file that is rewritten, or one whose provider stops
// answering.
type flexibleCredential struct {
	mu     sync.Mutex
	token  string
	retire bool
}

func (source *flexibleCredential) Authorization(context.Context) (string, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.retire {
		return "", errors.New("fixture credential retired")
	}
	header, err := bearerHeader(source.token)
	return header, err
}

func (source *flexibleCredential) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

func (source *flexibleCredential) rotate(token string) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.token, source.retire = token, false
}

func (source *flexibleCredential) retired() {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.retire = true
}

// TestEveryConfiguredClusterPublishesAtBoot is the base case the worst cases
// are measured against: N configured clusters each publish a generation of
// their own, before any explorer request, without sharing a scope.
func TestEveryConfiguredClusterPublishesAtBoot(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	east := newFakeCluster(t, "east-token", "east-api")
	west := newFakeCluster(t, "west-token", "west-api")
	north := newFakeCluster(t, "north-token", "north-api")
	clusters := []*bootedCluster{
		fleet.add("east", east, staticCredential(t, "east-token"), 0),
		fleet.add("west", west, staticCredential(t, "west-token"), 0),
		fleet.add("north", north, staticCredential(t, "north-token"), 0),
	}
	origin := time.Now()
	if boot := fleet.startWithin(2 * time.Second); boot > 2*time.Second {
		t.Fatalf("boot held the host for %s", boot)
	}
	keys := map[string]string{}
	for _, cluster := range clusters {
		fleet.waitPublished(cluster, origin, 10*time.Second)
	}
	for _, cluster := range clusters {
		generation, ok := fleet.generation(cluster)
		if !ok {
			t.Fatalf("cluster %s published no generation", cluster.id)
		}
		want := cluster.id + "-api"
		if names := podNames(generation); len(names) != 1 || names[0] != want {
			t.Fatalf("cluster %s published %v, want only its own %q", cluster.id, names, want)
		}
		if state := generation.View.Kinds["Pod"]; state.State != "ready" || state.Records != 1 {
			t.Fatalf("cluster %s published Pod state %+v", cluster.id, state)
		}
		if other, taken := keys[cluster.key]; taken {
			t.Fatalf("clusters %s and %s share one scope key", other, cluster.id)
		}
		keys[cluster.key] = cluster.id
		if cluster.fixture.listCount("pods") == 0 {
			t.Fatalf("cluster %s published without listing the cluster", cluster.id)
		}
	}
	// The first explorer read of each cluster finds a warm, published
	// generation and does not send the connector back to that cluster.
	for _, cluster := range clusters {
		before := cluster.fixture.requestCount()
		service := fleet.resolve(cluster)
		overview, err := service.Overview(t.Context())
		if err != nil || overview.Pods != 1 || overview.Sync.State == "syncing" {
			t.Fatalf("cluster %s first read was not warm: pods=%d state=%q err=%v", cluster.id, overview.Pods, overview.Sync.State, err)
		}
		if after := cluster.fixture.requestCount(); after != before {
			t.Fatalf("cluster %s first read re-listed the cluster: %d -> %d requests", cluster.id, before, after)
		}
	}
}

// W1: an unreachable cluster (TLS handshake failure) publishes nothing, reports
// the failure, and neither delays nor blocks the healthy cluster's boot.
func TestUnreachableClusterDoesNotDelayOrBlockAnotherClustersPublish(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	broken := newFakeCluster(t, "broken-token")
	healthy := newFakeCluster(t, "healthy-token", "healthy-api")
	failing := fleet.addUnreachable("a-unreachable", broken, staticCredential(t, "broken-token"))
	healthyCluster := fleet.add("b-healthy", healthy, staticCredential(t, "healthy-token"), 0)
	origin := time.Now()
	fleet.startWithin(2 * time.Second)
	if elapsed := fleet.waitPublished(healthyCluster, origin, 5*time.Second); elapsed > 2*time.Second {
		t.Fatalf("the healthy cluster published only after %s because another cluster was unreachable", elapsed)
	}
	if fleet.published(failing) {
		t.Fatal("the unreachable cluster published a generation")
	}
	// The failed cluster reports the failure instead of an empty healthy view.
	failingService := fleet.resolve(failing)
	waitUntil(t, 5*time.Second, "the unreachable cluster to report its failure", func() bool {
		overview, err := failingService.Overview(t.Context())
		return err == nil && overview.Sync.State == "error" && overview.Pods == 0
	})
	// The healthy cluster's generation is complete and untouched.
	generation, ok := fleet.generation(healthyCluster)
	if !ok {
		t.Fatal("the healthy cluster published no generation")
	}
	if names := podNames(generation); len(names) != 1 || names[0] != "healthy-api" {
		t.Fatalf("the healthy generation was corrupted: %v", names)
	}
	// No retry storm: a failing cluster backs off instead of spinning.
	time.Sleep(250 * time.Millisecond)
	settled := broken.handshakes()
	time.Sleep(400 * time.Millisecond)
	if grown := broken.handshakes() - settled; grown > 8 {
		t.Fatalf("the unreachable cluster was retried %d times in 400ms", grown)
	}
}

// W2: a cluster answering 403 to every list publishes a truthful permission
// failure with no records, and the other cluster publishes complete data.
func TestForbiddenClusterReportsPermissionFailureWithoutBlockingAnother(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	forbidden := newFakeCluster(t, "forbidden-token", "hidden-api")
	forbidden.denyLists()
	healthy := newFakeCluster(t, "healthy-token", "visible-api")
	forbiddenCluster := fleet.add("a-forbidden", forbidden, staticCredential(t, "forbidden-token"), 0)
	healthyCluster := fleet.add("b-healthy", healthy, staticCredential(t, "healthy-token"), 0)
	origin := time.Now()
	fleet.startWithin(2 * time.Second)
	permissionFailure := 0
	waitUntil(t, 10*time.Second, "the forbidden cluster to publish a permission failure", func() bool {
		generation, ok := fleet.generation(forbiddenCluster)
		if !ok || generation.View.Kinds["Pod"].State == "" {
			return false
		}
		state := generation.View.Kinds["Pod"]
		permissionFailure = 0
		if state.Error == "forbidden" || state.Error == "unauthorized" {
			permissionFailure++
		}
		return true
	})
	if permissionFailure != 1 {
		t.Fatalf("the forbidden cluster did not record a permission failure")
	}
	generation, ok := fleet.generation(forbiddenCluster)
	if !ok {
		t.Fatal("the forbidden cluster published no generation")
	}
	if len(podNames(generation)) != 0 || generation.View.Kinds["Pod"].Records != 0 {
		t.Fatalf("the forbidden cluster published records: %v", podNames(generation))
	}
	healthyGeneration, ok := fleet.generation(healthyCluster)
	if !ok {
		t.Fatal("the healthy cluster published no generation")
	}
	if elapsed := fleet.waitPublished(healthyCluster, origin, 5*time.Second); elapsed > 2*time.Second {
		t.Fatalf("the healthy cluster published only after %s", elapsed)
	}
	if names := podNames(healthyGeneration); len(names) != 1 || names[0] != "visible-api" {
		t.Fatalf("the healthy generation was corrupted: %v", names)
	}
	if state := healthyGeneration.View.Kinds["Pod"]; state.State != "ready" || state.Records != 1 {
		t.Fatalf("the healthy Pod kind is not complete: %+v", state)
	}
	// A permission failure is not a slow sync: it is visible to a reader.
	overview, err := fleet.resolve(forbiddenCluster).Overview(t.Context())
	if err != nil || overview.Pods != 0 {
		t.Fatalf("the forbidden cluster read as %+v err=%v", overview, err)
	}
}

// W3: one cluster's LIST never answers. The healthy cluster must publish while
// the stalled cluster is still held, and its publish time must stay close to a
// single-cluster baseline rather than absorbing the stalled cluster's work.
func TestStalledClusterDoesNotDelayAnotherClustersPublish(t *testing.T) {
	baselineFixture := newFakeCluster(t, "baseline-token", "baseline-api")
	baselineFleet := newBootFleet(t, "acme", storage.NewMemory(), "boot")
	baselineCluster := baselineFleet.add("only", baselineFixture, staticCredential(t, "baseline-token"), 0)
	baselineOrigin := time.Now()
	baselineFleet.startWithin(2 * time.Second)
	baseline := baselineFleet.waitPublished(baselineCluster, baselineOrigin, 10*time.Second)

	fleet := newBootFleet(t, "acme", storage.NewMemory(), "boot")
	stalled := newFakeCluster(t, "stalled-token", "stalled-api")
	healthy := newFakeCluster(t, "healthy-token", "healthy-api")
	release := stalled.stallLists()
	defer release()
	stalledCluster := fleet.add("a-stalled", stalled, staticCredential(t, "stalled-token"), 0)
	healthyCluster := fleet.add("b-healthy", healthy, staticCredential(t, "healthy-token"), 0)
	origin := time.Now()
	fleet.startWithin(2 * time.Second)
	// The hold has to be in effect before the healthy publish is measured: an
	// observation taken first cannot tell a held LIST from a released one.
	waitUntil(t, 5*time.Second, "the stalled cluster to be asked to list", func() bool {
		return stalled.requestCount() > 0
	})
	if fleet.published(stalledCluster) {
		t.Fatal("the stalled cluster published while its LIST was unanswered")
	}
	elapsed := fleet.waitPublished(healthyCluster, origin, 5*time.Second)
	bound := max(750*time.Millisecond, 5*baseline)
	if elapsed > bound {
		t.Fatalf("the healthy cluster published after %s against a %s single-cluster baseline: start-up was serialized behind the stall", elapsed, baseline)
	}
	if fleet.published(stalledCluster) {
		t.Fatal("the stalled cluster published during the healthy cluster's publish while its LIST was unanswered")
	}
	if generation, ok := fleet.generation(healthyCluster); !ok || len(podNames(generation)) != 1 {
		t.Fatalf("the healthy cluster published nothing usable while the other stalled")
	}
	// Both clusters finish once the stalled LIST is answered.
	release()
	fleet.waitPublished(stalledCluster, origin, 10*time.Second)
}

// W4: a cluster answering a cyclic continue token aborts its own sync with a
// bounded number of requests, and the other cluster is unaffected.
func TestCyclicContinueTokenStopsOnlyThatClustersSync(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	cyclic := newFakeCluster(t, "cyclic-token")
	cyclic.repeatContinueToken()
	healthy := newFakeCluster(t, "healthy-token", "healthy-api")
	cyclicCluster := fleet.add("a-cyclic", cyclic, staticCredential(t, "cyclic-token"), 0)
	healthyCluster := fleet.add("b-healthy", healthy, staticCredential(t, "healthy-token"), 0)
	origin := time.Now()
	fleet.startWithin(2 * time.Second)
	fleet.waitPublished(healthyCluster, origin, 5*time.Second)
	fleet.waitPublished(cyclicCluster, origin, 10*time.Second)
	generation, ok := fleet.generation(cyclicCluster)
	if !ok {
		t.Fatal("the cyclic cluster published no generation")
	}
	state := generation.View.Kinds["Pod"]
	if state.State == "ready" || state.Records != 0 {
		t.Fatalf("the cyclic Pod page chain was accepted: %+v", state)
	}
	if len(podNames(generation)) != 0 {
		t.Fatalf("the cyclic cluster published pod records: %v", podNames(generation))
	}
	// Bounded: a repeated token ends the page chain, it does not loop on it.
	requests := cyclic.listCount("pods")
	time.Sleep(300 * time.Millisecond)
	if grown := cyclic.listCount("pods") - requests; grown > 4 {
		t.Fatalf("the cyclic cluster issued %d more Pod lists in 300ms", grown)
	}
	healthyGeneration, ok := fleet.generation(healthyCluster)
	if !ok {
		t.Fatal("the healthy cluster published no generation")
	}
	if names := podNames(healthyGeneration); len(names) != 1 || names[0] != "healthy-api" {
		t.Fatalf("the healthy generation was corrupted: %v", names)
	}
}

// W5: one cluster's credential is retired and rotated. Only that cluster's
// scope changes; the other keeps serving its cache, and no credential or
// cluster is read through another's scope.
func TestCredentialRotationTouchesOnlyItsOwnCluster(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	rotating := newFakeCluster(t, "alpha-token", "alpha-api")
	steady := newFakeCluster(t, "steady-token", "steady-api")
	alphaCredential := &flexibleCredential{token: "alpha-token"}
	alphaCluster := fleet.add("a-rotating", rotating, alphaCredential, 0)
	steadyCluster := fleet.add("b-steady", steady, staticCredential(t, "steady-token"), 0)
	origin := time.Now()
	fleet.startWithin(2 * time.Second)
	fleet.waitPublished(alphaCluster, origin, 10*time.Second)
	fleet.waitPublished(steadyCluster, origin, 10*time.Second)
	fleet.settle(steadyCluster, 150*time.Millisecond, 5*time.Second)
	steadyBefore, ok := fleet.generation(steadyCluster)
	if !ok {
		t.Fatal("the steady cluster published no generation")
	}
	steadyService := fleet.resolve(steadyCluster)
	steadyPublishes := fleet.broker.count(steadyCluster.key)
	steadyLists := steady.listCount("pods")
	alphaService := fleet.resolve(alphaCluster)
	alphaScope := alphaService.ActiveScope()
	alphaKey := inventoryKey(alphaScope)

	// The credential provider stops answering: this cluster's epoch is retired
	// and cannot serve a read.
	alphaCredential.retired()
	if _, err := fleet.registry.ResolveCluster(fleet.org, alphaCluster.id); err == nil {
		t.Fatal("a retired credential kept serving its cluster")
	}
	// It returns with a rotated token for the same cluster.
	alphaCredential.rotate("beta-token")
	rotating.rotateToken("beta-token")
	rotatedService, err := fleet.registry.ResolveCluster(fleet.org, alphaCluster.id)
	if err != nil {
		t.Fatalf("the rotated credential was not accepted: %v", err)
	}
	if rotatedService == alphaService {
		t.Fatal("rotation reused the retired service instance")
	}
	rotatedScope := rotatedService.ActiveScope()
	if rotatedScope.CredentialID == alphaScope.CredentialID {
		t.Fatal("rotation reused the retired credential scope")
	}
	if rotatedScope.ClusterID != alphaScope.ClusterID || rotatedScope.OrgID != alphaScope.OrgID {
		t.Fatalf("rotation moved the cluster or org scope: %+v", rotatedScope)
	}
	rotatedKey := inventoryKey(rotatedScope)
	if rotatedKey == alphaKey || rotatedKey == steadyCluster.key {
		t.Fatal("rotation reused another scope's storage key")
	}
	waitUntil(t, 10*time.Second, "the rotated cluster to publish under its new scope", func() bool {
		generation, ok := publishedGeneration(base, rotatedScope)
		if !ok {
			return false
		}
		return len(podNames(generation)) == 1 && podNames(generation)[0] == "alpha-api"
	})
	// The retired scope's last generation is retained, not rewritten.
	retiredGeneration, ok := publishedGeneration(base, alphaScope)
	if !ok || retiredGeneration.Revision == "" {
		t.Fatal("rotation erased the retired scope's last good generation")
	}
	// The untouched cluster kept its instance, its scope, its cache and its data.
	if fleet.resolve(steadyCluster) != steadyService {
		t.Fatal("another cluster's credential change replaced the steady service")
	}
	if publishes := fleet.broker.count(steadyCluster.key); publishes != steadyPublishes {
		t.Fatalf("the steady cluster republished %d times because of another cluster's rotation", publishes-steadyPublishes)
	}
	steadyAfter, ok := fleet.generation(steadyCluster)
	if !ok || steadyAfter.Revision != steadyBefore.Revision {
		t.Fatal("another cluster's rotation rewrote the steady generation")
	}
	overview, err := steadyService.Overview(t.Context())
	if err != nil || overview.Pods != 1 || overview.Sync.State == "syncing" {
		t.Fatalf("the steady cluster stopped serving its cache: pods=%d state=%q err=%v", overview.Pods, overview.Sync.State, err)
	}
	if lists := steady.listCount("pods"); lists != steadyLists {
		t.Fatalf("the steady cluster was re-listed %d times because of another cluster's rotation", lists-steadyLists)
	}
	// No credential crossed a cluster boundary.
	if count := rotating.callerCount("Bearer steady-token"); count != 0 {
		t.Fatalf("the rotating cluster was read with the steady cluster's credential %d times", count)
	}
	if count := steady.callerCount("Bearer alpha-token"); count != 0 {
		t.Fatalf("the steady cluster was read with a retired credential %d times", count)
	}
	if count := steady.callerCount("Bearer beta-token"); count != 0 {
		t.Fatalf("the steady cluster was read with the rotated credential %d times", count)
	}
}

// W7: a cluster removed from configuration stops serving and stops its
// workers; the remaining cluster keeps serving its own generation.
func TestRemovedClusterStopsWithoutDisturbingTheRemainder(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	removed := newFakeCluster(t, "removed-token", "removed-api")
	remaining := newFakeCluster(t, "remaining-token", "remaining-api")
	removedCluster := fleet.add("a-removed", removed, staticCredential(t, "removed-token"), 0)
	remainingCluster := fleet.add("b-remaining", remaining, staticCredential(t, "remaining-token"), 0)
	origin := time.Now()
	fleet.startWithin(2 * time.Second)
	fleet.waitPublished(removedCluster, origin, 10*time.Second)
	fleet.waitPublished(remainingCluster, origin, 10*time.Second)
	fleet.settle(remainingCluster, 150*time.Millisecond, 5*time.Second)
	remainingBefore, ok := fleet.generation(remainingCluster)
	if !ok {
		t.Fatal("the remaining cluster published no generation")
	}
	remainingLists := remaining.listCount("pods")

	fleet.registry.DeregisterCluster("a-removed")

	clusters := fleet.registry.Clusters()
	if len(clusters) != 1 || clusters[0].ID != "b-remaining" {
		t.Fatalf("removal left the catalog as %+v", clusters)
	}
	if _, err := fleet.registry.ResolveCluster(fleet.org, "a-removed"); !errors.Is(err, ErrClusterNotFound) {
		t.Fatalf("the removed cluster still resolves: %v", err)
	}
	scope := removedCluster.service.indexScope()
	removedCluster.service.changes.mu.Lock()
	stopped := removedCluster.service.changes.stopped[scope]
	cache := removedCluster.service.changes.inventories[scope]
	removedCluster.service.changes.mu.Unlock()
	if !stopped {
		t.Fatal("the removed cluster's capture was not stopped")
	}
	if cache == nil {
		t.Fatal("the removed cluster had no inventory worker to stop")
	}
	select {
	case <-cache.done:
	default:
		t.Fatal("the removed cluster's inventory worker is still running")
	}
	requests := removed.requestCount()
	time.Sleep(300 * time.Millisecond)
	if grown := removed.requestCount() - requests; grown != 0 {
		t.Fatalf("the removed cluster was still polled %d times", grown)
	}
	// The remaining cluster keeps serving, unchanged.
	if _, err := fleet.registry.ResolveCluster(fleet.org, "b-remaining"); err != nil {
		t.Fatalf("the remaining cluster stopped resolving: %v", err)
	}
	remainingAfter, ok := fleet.generation(remainingCluster)
	if !ok || remainingAfter.Revision == "" {
		t.Fatal("the remaining cluster lost its generation")
	}
	overview, err := fleet.resolve(remainingCluster).Overview(t.Context())
	if err != nil || overview.Pods != 1 {
		t.Fatalf("the remaining cluster stopped serving: pods=%d err=%v", overview.Pods, err)
	}
	if lists := remaining.listCount("pods"); lists != remainingLists {
		t.Fatalf("removing another cluster re-listed the remaining cluster %d times", lists-remainingLists)
	}
	if names := podNames(remainingBefore); len(names) != 1 || names[0] != "remaining-api" {
		t.Fatalf("removal disturbed the remaining generation: %v", names)
	}
	if names := podNames(remainingAfter); len(names) != 1 || names[0] != "remaining-api" {
		t.Fatalf("removal replaced the remaining generation: %v", names)
	}
}

type passthroughScrubber struct{}

func (passthroughScrubber) Scrub(value string) string { return value }

// W9: a kind the cluster first serves in a later cycle is listed for the first
// time there. Arriving as a whole kind is not a creation: no created or deleted
// changes, no false break and no alert on either cluster.
func TestKindFirstListedLaterProducesNoChangesOrBreaks(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	late := newFakeCluster(t, "late-token", "late-api")
	late.unserve("configmaps")
	steady := newFakeCluster(t, "steady-token", "steady-api")
	lateCluster := fleet.add("a-late", late, staticCredential(t, "late-token"), 150*time.Millisecond)
	steadyCluster := fleet.add("b-steady", steady, staticCredential(t, "steady-token"), 150*time.Millisecond)
	alerts := map[string]*atomic.Int32{"a-late": {}, "b-steady": {}}
	for _, cluster := range []*bootedCluster{lateCluster, steadyCluster} {
		counter := alerts[cluster.id]
		cluster.service.SetScrubber(passthroughScrubber{})
		cluster.service.SetIssueAlertSink(func(context.Context, IssueAlert) (Outcome, error) {
			counter.Add(1)
			return Outcome{}, nil
		})
		if err := cluster.service.SaveIssueAlertSettings(t.Context(), true, "warning", "boot-proof"); err != nil {
			t.Fatalf("enable alerts for %s: %v", cluster.id, err)
		}
	}
	origin := time.Now()
	fleet.startWithin(2 * time.Second)
	fleet.waitPublished(lateCluster, origin, 10*time.Second)
	fleet.waitPublished(steadyCluster, origin, 10*time.Second)
	// At boot the kind exists on one cluster only. A cluster that does not
	// serve it records it as known and empty rather than as a failure.
	waitUntil(t, 10*time.Second, "the kind to be recorded on both clusters", func() bool {
		steadyGeneration, steadyOK := fleet.generation(steadyCluster)
		lateGeneration, lateOK := fleet.generation(lateCluster)
		if !steadyOK || !lateOK {
			return false
		}
		_, steadyListed := steadyGeneration.View.Kinds["ConfigMap"]
		_, lateListed := lateGeneration.View.Kinds["ConfigMap"]
		return steadyListed && lateListed
	})
	lateGeneration, _ := fleet.generation(lateCluster)
	if state := lateGeneration.View.Kinds["ConfigMap"]; state.State != "ready" || state.Records != 0 {
		t.Fatalf("a kind the cluster does not serve is not recorded as known and empty: %+v", state)
	}

	// The cluster starts serving the kind: its first real listing is a later
	// cycle, reached through the same refresh an explorer read drives.
	late.serve("configmaps", fixtureObject("v1", "ConfigMap", "shop", "late-config"))
	service := fleet.resolve(lateCluster)
	listed := false
	waitUntil(t, 40*time.Second, "the late kind to be listed with content", func() bool {
		snapshot, _, err := service.IndexSnapshot(t.Context(), "ConfigMap")
		if err != nil {
			return false
		}
		for _, record := range snapshot.Records {
			if record.Kind == "ConfigMap" && record.Name == "late-config" {
				listed = true
				return true
			}
		}
		return false
	})
	if !listed {
		t.Fatal("the late kind was never listed")
	}
	// Give the change pipeline a cycle to misreport the arrival.
	time.Sleep(400 * time.Millisecond)

	for _, cluster := range []*bootedCluster{lateCluster, steadyCluster} {
		service := fleet.resolve(cluster)
		page, err := service.Changes(t.Context(), ChangeQuery{})
		if err != nil {
			t.Fatalf("%s change feed: %v", cluster.id, err)
		}
		for _, change := range page.Items {
			if change.Kind == "ConfigMap" {
				t.Fatalf("%s reported a %s change for the late kind %q", cluster.id, change.Type, change.Name)
			}
			if change.Type == kubechanges.Created || change.Type == kubechanges.Deleted {
				t.Fatalf("%s reported a %s change for %q on first observation", cluster.id, change.Type, change.Name)
			}
		}
		issues, err := service.Issues(t.Context(), IssueOptions{Limit: 10})
		if err != nil {
			t.Fatalf("%s issues: %v", cluster.id, err)
		}
		if issues.Broken.Total != 0 || len(issues.Items) != 0 {
			t.Fatalf("%s reported a false break on first observation: broken=%d items=%d", cluster.id, issues.Broken.Total, len(issues.Items))
		}
		if delivered := alerts[cluster.id].Load(); delivered != 0 {
			t.Fatalf("%s delivered %d issue alerts for a first observation", cluster.id, delivered)
		}
	}
}

// A kind the cluster does not serve is published as known and empty but marked
// as never listed, and that mark is in the generation a new replica reads: the
// replica's first listing of the kind later is still a baseline.
func TestAbsentKindSurvivesThePublishedGenerationForANewReplica(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	fixture := newFakeCluster(t, "boot-token", "boot-api")
	fixture.unserve("configmaps")
	cluster := fleet.add("a-unserved", fixture, staticCredential(t, "boot-token"), 0)
	fleet.startWithin(2 * time.Second)
	fleet.waitPublished(cluster, time.Now(), 10*time.Second)
	waitUntil(t, 10*time.Second, "the unserved kind to reach the published generation", func() bool {
		generation, ok := fleet.generation(cluster)
		if !ok {
			return false
		}
		_, listed := generation.View.Kinds["ConfigMap"]
		return listed
	})
	generation, ok := fleet.generation(cluster)
	if !ok {
		t.Fatal("the cluster published no generation")
	}
	if state := generation.View.Kinds["ConfigMap"]; !state.Absent || state.State != "ready" || state.Records != 0 {
		t.Fatalf("the published generation lost the kind's unlisted state: %+v", state)
	}
	restored, err := kubeindex.Restore(generation.View)
	if err != nil {
		t.Fatalf("a published absent kind was rejected on restore: %v", err)
	}
	if state := restored.Snapshot().Kinds["ConfigMap"]; !state.Absent || state.Records != 0 {
		t.Fatalf("the restored replica invented a listed kind: %+v", state)
	}
	deltas, unsubscribe := restored.Subscribe(8)
	defer unsubscribe()
	if err := restored.Ingest("ConfigMap", []kubeindex.Record{{Kind: "ConfigMap", UID: "late", Namespace: "shop", Name: "late-config"}}, true, time.Now().UTC()); err != nil {
		t.Fatalf("restored listing: %v", err)
	}
	if len(deltas) != 0 {
		t.Fatal("the restored replica reported the first listing of an unserved kind as a change")
	}
}

// The change journal is fed by the published generation the capture loop relays,
// never by a live listing, so the relay is where an unserved kind keeps that
// state. A relay that records the generation as a listing with no objects makes
// the kind's first real listing arrive as created objects.
func TestRelayedGenerationKeepsAnUnservedKindUnlistedForTheJournal(t *testing.T) {
	base := storage.NewMemory()
	fleet := newBootFleet(t, "acme", base, "boot")
	late := newFakeCluster(t, "late-token", "late-api")
	late.unserve("configmaps")
	cluster := fleet.add("a-late", late, staticCredential(t, "late-token"), 150*time.Millisecond)
	fleet.startWithin(2 * time.Second)
	fleet.waitPublished(cluster, time.Now(), 10*time.Second)
	service := fleet.resolve(cluster)
	index, err := service.indexes.ForScope(service.indexScope())
	if err != nil {
		t.Fatalf("cluster index: %v", err)
	}
	// The capture loop relays the published generation on its own, at the kind's
	// cadence. Observing the relayed state keeps the proof on the path
	// production uses: a live read would be what records the kind as unserved.
	waitUntil(t, 10*time.Second, "the capture loop to relay the unserved kind", func() bool {
		status, _ := index.KindStatus("ConfigMap")
		return !status.ObservedAt.IsZero()
	})
	relayed, _ := index.KindStatus("ConfigMap")
	if !relayed.Absent {
		t.Fatalf("the relay recorded a kind the cluster does not serve as a listing: %+v", relayed)
	}

	// Production reaches the next listing of the kind a cadence later. Waiting
	// out the product's own per-kind refresh window and then making the kind due
	// moves that listing here with the product's own timestamps.
	if wait := time.Until(relayed.ObservedAt.Add(kubernetesIndexRefreshInterval + time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	late.serve("configmaps", fixtureObject("v1", "ConfigMap", "shop", "late-config"))
	cache := service.changes.inventories[service.indexScope()]
	cache.mu.Lock()
	due := cache.generation.View.Kinds["ConfigMap"]
	due.LastAttemptAt = time.Now().Add(-time.Hour)
	cache.generation.View.Kinds["ConfigMap"] = due
	cache.mu.Unlock()
	waitUntil(t, 10*time.Second, "the late kind to be published with content", func() bool {
		generation, ok := fleet.generation(cluster)
		if !ok {
			return false
		}
		state, listed := generation.View.Kinds["ConfigMap"]
		return listed && !state.Absent && state.Records == 1
	})

	// Relay it exactly as the next capture cycle does.
	if _, _, err := service.IndexSnapshot(context.WithValue(t.Context(), cachedCaptureContextKey{}, true), "ConfigMap"); err != nil {
		t.Fatalf("relayed cycle: %v", err)
	}
	page, err := service.Changes(t.Context(), ChangeQuery{})
	if err != nil {
		t.Fatalf("change feed: %v", err)
	}
	for _, change := range page.Items {
		if change.Kind == "ConfigMap" {
			t.Fatalf("the relay reported a %s change for the late kind %q on first observation", change.Type, change.Name)
		}
	}
}
