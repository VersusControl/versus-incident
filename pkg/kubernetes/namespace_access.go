package kubernetes

import (
	"context"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// maxReadableNamespaces bounds the namespaces an answer lists and a read
// proves against.
const maxReadableNamespaces = 32

// readableNamespaceScope reports whether the connector identity reads the
// cluster's namespaced kinds cluster-wide and, when it does not, the
// namespaces it has read them in. An identity bound only inside namespaces
// has its Pods read per namespace, which the generation records as
// namespace_scope; a cluster that has not been read yet is not called
// namespace-scoped.
func readableNamespaceScope(generation inventoryGeneration) (bool, []string) {
	// Pods are what every view is built from: an identity that lists them
	// cluster-wide reads the cluster wide, even while another kind it was
	// just granted is still read only inside a namespace.
	if generation.View.Kinds["Pod"].Error != "namespace_scope" {
		return true, nil
	}
	namespaces := []string{}
	for namespace, snapshot := range generation.Namespaces {
		if namespaceSnapshotReadable(snapshot) {
			namespaces = append(namespaces, namespace)
		}
	}
	sort.Strings(namespaces)
	if len(namespaces) > maxReadableNamespaces {
		namespaces = namespaces[:maxReadableNamespaces]
	}
	return false, namespaces
}

// namespaceSnapshotReadable reports whether any kind was read in a namespace.
func namespaceSnapshotReadable(snapshot kubeindex.Snapshot) bool {
	for _, state := range snapshot.Kinds {
		if state.State == "ready" && state.Error == "" && !state.ObservedAt.IsZero() {
			return true
		}
	}
	return false
}

// namespaceOnlyScope returns the namespaces a namespace-bound identity reads,
// or nil when it reads cluster-wide or the inventory cannot say.
func (service *Service) namespaceOnlyScope(ctx context.Context) []string {
	generation, err := service.readInventory(ctx)
	if err != nil {
		return nil
	}
	clusterWide, namespaces := readableNamespaceScope(generation)
	if clusterWide {
		return nil
	}
	return namespaces
}

// HistoryOffNamespaceAccess marks a history answer for an identity that reads
// only inside namespaces. History is not kept for such an identity, so the
// answer is empty by design rather than failed.
const HistoryOffNamespaceAccess = "off_namespace_access"

// historyOffForNamespaceAccess reports whether a refused history read is the
// expected answer for a namespace-bound identity, which keeps no history.
func (service *Service) historyOffForNamespaceAccess(ctx context.Context, err error) bool {
	if !errors.Is(err, errHistoryAuthority) && !errors.Is(err, errHistoryNotEstablished) {
		return false
	}
	generation, readErr := service.readInventory(ctx)
	if readErr != nil {
		return false
	}
	clusterWide, _ := readableNamespaceScope(generation)
	return !clusterWide
}

// productAPIGroups are the API groups whose kinds the product shows. A denied
// discovery of any other group is a capability gap nobody looks at, so it is
// not reported as a partial failure.
var productAPIGroups = map[string]bool{
	"core": true, "": true, "apps": true, "batch": true, "autoscaling": true, "networking.k8s.io": true,
	"gateway.networking.k8s.io": true, "metrics.k8s.io": true, "argoproj.io": true,
	"kustomize.toolkit.fluxcd.io": true, "helm.toolkit.fluxcd.io": true,
}

// presentPartialFailures readies partial failures for a person to read: it
// drops denied discovery of API groups the product does not show, removes
// duplicates, names each entry with a display name and a plain sentence, and
// orders them the same way on every answer: faults first, then capability
// gaps, each by name.
func presentPartialFailures(failures []PartialFailure) []PartialFailure {
	seen := map[PartialFailure]bool{}
	result := make([]PartialFailure, 0, len(failures))
	for _, failure := range failures {
		if failure.Scope == "discovery" && failure.Class == "forbidden" && !productAPIGroups[apiGroupOf(failure.GroupVersion)] {
			continue
		}
		failure.Kind, failure.Message = "", ""
		if seen[failure] {
			continue
		}
		seen[failure] = true
		failure.Kind = partialFailureName(failure)
		failure.Message = partialFailureSentence(failure.Kind, failure.Class)
		result = append(result, failure)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := capabilityGapClass(result[i].Class), capabilityGapClass(result[j].Class)
		if left != right {
			return !left
		}
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].Class != result[j].Class {
			return result[i].Class < result[j].Class
		}
		if result[i].ResourceID != result[j].ResourceID {
			return result[i].ResourceID < result[j].ResourceID
		}
		return result[i].GroupVersion+result[i].Scope < result[j].GroupVersion+result[j].Scope
	})
	return result
}

// capabilityGapClass reports a class that describes what the identity may
// read rather than a failure to read it.
func capabilityGapClass(class string) bool {
	return class == "forbidden" || class == "namespace_scope"
}

func apiGroupOf(groupVersion string) string {
	group, _, _ := strings.Cut(groupVersion, "~")
	return group
}

// partialFailureName is the display name of what a partial failure is about.
func partialFailureName(failure PartialFailure) string {
	for kind, id := range indexedResourceIDs {
		if id == failure.ResourceID {
			return kind
		}
	}
	switch failure.ResourceID {
	case "metrics.k8s.io", "metrics.k8s.io~v1beta1~pods", "metrics.k8s.io~v1beta1~nodes":
		if strings.HasSuffix(failure.ResourceID, "~nodes") {
			return "Node usage metrics"
		}
		if strings.HasSuffix(failure.ResourceID, "~pods") {
			return "Pod usage metrics"
		}
		return "Usage metrics"
	case "kubernetes.discovery":
		return "API discovery"
	case "kubernetes.inventory":
		return "Inventory"
	}
	if failure.Scope == "discovery" && failure.GroupVersion != "" {
		return "API group " + apiGroupOf(failure.GroupVersion)
	}
	if parts := strings.Split(failure.ResourceID, "~"); len(parts) == 3 && parts[2] != "" {
		return parts[2]
	}
	if failure.ResourceID != "" {
		return failure.ResourceID
	}
	return "Cluster read"
}

// partialFailureSentence says in plain words what happened to one read.
func partialFailureSentence(name, class string) string {
	switch class {
	case "forbidden":
		return name + " is not readable with this connector's permissions."
	case "namespace_scope":
		return name + " is read only in the namespaces this connector can access."
	case "unauthorized":
		return name + " could not be read because the cluster rejected the connector's credential."
	case "timeout":
		return name + " took too long to read; the next sync tries again."
	case "throttled":
		return "The cluster throttled the " + name + " read; the next sync tries again."
	case "unsupported":
		return name + " is not served by this cluster."
	case "syncing":
		return name + " is still loading."
	case "budget_exhausted", "record_limit", "incomplete_pages", "item_limit", "encoded_result_size", "cursor_limit":
		return name + " was read only in part because it is larger than one read allows."
	}
	return name + " could not be read; the next sync tries again."
}

// seedReadableNamespaces finds the namespaces an identity that cannot list
// namespaces may still read, so its first cycle after a start reads them
// instead of reporting the whole cluster as denied until a view asks for a
// namespace. It runs once per process. The candidates are names this cluster
// was seen with before; each is probed with a single-item read, and only the
// ones that answer are read.
func (service *Service) seedReadableNamespaces(ctx context.Context, cache *inventoryCache) {
	cache.mu.Lock()
	if cache.namespacesSeeded {
		cache.mu.Unlock()
		return
	}
	cache.namespacesSeeded = true
	known := make(map[string]bool, len(cache.namespaces))
	for namespace := range cache.namespaces {
		known[namespace] = true
	}
	cache.mu.Unlock()
	readable := []string{}
	for _, namespace := range service.rememberedNamespaces() {
		if ctx.Err() != nil {
			break
		}
		if !known[namespace] && service.probeNamespaceCandidate(ctx, namespace) {
			readable = append(readable, namespace)
		}
	}
	if len(readable) == 0 {
		return
	}
	cache.mu.Lock()
	for _, namespace := range readable {
		cache.namespaces[namespace] = true
	}
	cache.mu.Unlock()
}

// rememberedNamespaces returns the namespace names the stored generation holds:
// first the namespaces read one by one, then those its records live in. Only
// names are taken; nothing stored is served. The list is bounded.
func (service *Service) rememberedNamespaces() []string {
	provider := service.usageProvider()
	if provider == nil {
		return nil
	}
	generation, _, err := readInventoryGeneration(provider, service.scope, "")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	first, rest := []string{}, []string{}
	add := func(list *[]string, namespace string) {
		if namespace == "" || seen[namespace] || !safeSegment(namespace) || len(namespace) > 63 {
			return
		}
		seen[namespace] = true
		*list = append(*list, namespace)
	}
	for namespace := range generation.Namespaces {
		add(&first, namespace)
	}
	for namespace := range generation.NamespaceProof {
		add(&first, namespace)
	}
	for _, record := range generation.View.Records {
		if record.Kind == "Namespace" {
			add(&rest, record.Name)
		} else {
			add(&rest, record.Namespace)
		}
	}
	sort.Strings(first)
	sort.Strings(rest)
	result := append(first, rest...)
	if len(result) > maxReadableNamespaces {
		result = result[:maxReadableNamespaces]
	}
	return result
}

// namespaceReadProofs keeps one workload-read proof per namespace, each with
// the same reuse interval and credential binding as the single-scope proof,
// so an answer across a few namespaces does not re-prove each one on every
// request. It is bounded: past the bound the oldest entries are dropped.
type namespaceReadProofs struct {
	mu     sync.Mutex
	states map[string]*inventoryAuthorityProofState
}

func (proofs *namespaceReadProofs) verify(ctx context.Context, binding string, probe func() error) error {
	if proofs == nil {
		// Without a proof cache every answer proves afresh.
		return probe()
	}
	proofs.mu.Lock()
	if proofs.states == nil {
		proofs.states = map[string]*inventoryAuthorityProofState{}
	}
	state := proofs.states[binding]
	if state == nil {
		if len(proofs.states) >= 2*maxReadableNamespaces {
			clear(proofs.states)
		}
		state = &inventoryAuthorityProofState{}
		proofs.states[binding] = state
	}
	proofs.mu.Unlock()
	return state.verifyCurrentRead(ctx, binding, probe)
}

// verifyNamespaceRead proves a namespace for the current credential. The credential
// generation is part of the binding, so a replaced credential proves again.
func (service *Service) verifyNamespaceRead(ctx context.Context, namespace string) error {
	binding := logIdentity(service.cacheKey() + "\x00" + strconv.FormatUint(service.credentialGeneration(), 10) + "\x00" + namespace)
	return service.namespaceReads.verify(ctx, binding, func() error { return service.proveNamespaceWorkloadRead(ctx, namespace) })
}

// requestNamespaceReread asks the inventory worker to read the namespaced
// kinds again on its next cycle, after a namespace it publishes was denied.
func (service *Service) requestNamespaceReread() {
	if service == nil || service.changes == nil {
		return
	}
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	if cache == nil {
		return
	}
	cache.mu.Lock()
	cache.scopePending = true
	cache.mu.Unlock()
	select {
	case cache.wake <- struct{}{}:
	default:
	}
}

// keepProvedNamespaces drops records of namespaces outside proved. A nil
// proved keeps every record; cluster-scoped records are always kept.
func keepProvedNamespaces(records []kubeindex.Record, proved map[string]bool) []kubeindex.Record {
	if proved == nil {
		return records
	}
	kept := make([]kubeindex.Record, 0, len(records))
	for _, record := range records {
		if record.Namespace == "" || proved[record.Namespace] {
			kept = append(kept, record)
		}
	}
	return kept
}

// errResourceExcluded reports a read of a resource the connector is configured
// never to list. It reads as not found to every caller that only tests for one.
var errResourceExcluded = errors.Join(errors.New("kubernetes: resource excluded by configuration"), ErrNotFound)

// SetCollectionScope configures resources, by plural API name, the connector
// never lists, and the namespaces it reads when its identity may read only
// inside some. Call it before capture starts.
func (service *Service) SetCollectionScope(excluded, namespaces []string) {
	if service == nil {
		return
	}
	service.excluded = nil
	if len(excluded) > 0 {
		service.excluded = make(map[string]bool, len(excluded))
		for _, resource := range excluded {
			service.excluded[strings.ToLower(strings.TrimSpace(resource))] = true
		}
	}
	service.namespaces = nil
	for _, namespace := range namespaces {
		if safeSegment(namespace) && len(namespace) <= 63 && len(service.namespaces) < maxReadableNamespaces {
			service.namespaces = append(service.namespaces, namespace)
		}
	}
	sort.Strings(service.namespaces)
}

// resourceExcluded reports whether a resource ID or indexed kind names a
// resource the connector is configured never to list.
func (service *Service) resourceExcluded(idOrKind string) bool {
	if service == nil || len(service.excluded) == 0 {
		return false
	}
	id := idOrKind
	if indexed := indexedResourceIDs[idOrKind]; indexed != "" {
		id = indexed
	}
	parts := strings.Split(id, "~")
	return service.excluded[parts[len(parts)-1]]
}

// notCollectedKinds lists, sorted, the indexed kinds the connector is
// configured never to list.
func (service *Service) notCollectedKinds() []string {
	kinds := []string{}
	for kind := range indexedResourceIDs {
		if service.resourceExcluded(kind) {
			kinds = append(kinds, kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}

// unmatchedExclusions lists, sorted, the configured excluded resource names
// that no resource of the discovery has, as a misspelling such as "secret"
// would. Such a name excludes nothing.
func (service *Service) unmatchedExclusions(discovery Discovery) []string {
	if service == nil || len(service.excluded) == 0 {
		return nil
	}
	served := map[string]bool{}
	for _, definition := range discovery.Resources {
		served[definition.Resource] = true
	}
	unmatched := []string{}
	for name := range service.excluded {
		if !served[name] {
			unmatched = append(unmatched, name)
		}
	}
	sort.Strings(unmatched)
	return unmatched
}

// reportUnmatchedExclusions logs, once per process and cluster, the excluded
// resource names the cluster does not serve, after its first full discovery.
func (service *Service) reportUnmatchedExclusions(cache *inventoryCache, discovery Discovery) {
	cache.mu.Lock()
	checked := cache.exclusionsChecked
	cache.exclusionsChecked = true
	cache.mu.Unlock()
	if checked {
		return
	}
	for _, name := range service.unmatchedExclusions(discovery) {
		log.Printf("kubernetes: exclude_resources entry %q matches no resource this cluster serves; check the plural API name, for example secrets", boundedLogToken(name))
	}
}

// commitStreamKeepAlive is how often a stream for an identity without history
// repeats an unchanged sync state.
const commitStreamKeepAlive = 15 * time.Second

// StreamSyncEvent is the sync event a commit stream sends next to its commits.
type StreamSyncEvent struct {
	State     string  `json:"state"`
	Age       float64 `json:"age"`
	Reason    string  `json:"reason"`
	ClusterID string  `json:"cluster_id"`
	History   string  `json:"history,omitempty"`
}

// StreamSyncPacer remembers what one stream last sent, so an unchanged state
// is repeated only at the keep-alive pace.
type StreamSyncPacer struct {
	state string
	sent  time.Time
}

// NextStreamSync returns the sync event a commit stream sends after commit,
// and whether to send it now. With history kept, the state follows the age of
// the last saved capture and is sent on every poll. Without history, as for an
// identity that reads only inside namespaces, no capture is ever saved, so the
// state is the cluster's own sync report, sent when it changes and otherwise
// only as a keep-alive.
func (service *Service) NextStreamSync(ctx context.Context, commit CommitEvent, pacer *StreamSyncPacer) (StreamSyncEvent, bool) {
	event := StreamSyncEvent{State: "ready", ClusterID: commit.ClusterID, History: commit.History}
	if commit.History == "" {
		event.Age = max(0, time.Since(commit.At).Seconds())
		if event.Age > 120 {
			event.State, event.Reason = "refreshing", "collection_stale"
		}
		return event, true
	}
	report, err := service.SyncHealth(ctx)
	if err != nil {
		return event, false
	}
	event.State = report.State
	if report.AgeSeconds != nil {
		event.Age = *report.AgeSeconds
	}
	if pacer != nil {
		if report.State == pacer.state && time.Since(pacer.sent) < commitStreamKeepAlive {
			return event, false
		}
		pacer.state, pacer.sent = report.State, time.Now()
	}
	return event, true
}
