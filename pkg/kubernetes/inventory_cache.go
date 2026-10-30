package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Projected data is published by the revision in a separate small scoped lease.
// Readers retain this immutable generation until a newer revision is available.
type inventoryGeneration struct {
	liveAuthorityOnly bool
	Discovery         *Discovery
	EvidenceScope     storage.EvidenceScope
	EvidenceBatch     *storage.EvidenceBatch
	EvidenceFailure   string
	View              kubeindex.Snapshot
	Usage             Usage
	Resources         map[string][]ProjectedResource
	ResourceStatus    map[string]kubeindex.KindStatus
	Revision          string
	Namespaces        map[string]kubeindex.Snapshot
	Crashes           []kubechanges.CrashEvent
	PodSampledAt      time.Time
	DiscoveryPartial  []PartialFailure
	NamespaceProof    map[string]time.Time
	// Pending lists indexed resource IDs this generation has not listed yet,
	// and PendingDiscovery marks that the cluster's other API groups have not
	// been discovered yet. Both clear once the full set is published.
	Pending          []string `json:",omitempty"`
	PendingDiscovery bool     `json:",omitempty"`
	// FirstReadyAt is when this cluster first published a generation its health
	// report called ready. The cluster's inventory metadata is the authority; this
	// is the copy the report reads, carried forward on every publish and seeded
	// back after a restart, so a later syncing report is never mistaken for a
	// first boot.
	FirstReadyAt time.Time `json:",omitempty"`
}

func cloneInventoryGeneration(generation inventoryGeneration) (inventoryGeneration, error) {
	raw, err := json.Marshal(generation)
	if err != nil {
		return inventoryGeneration{}, err
	}
	var copy inventoryGeneration
	err = json.Unmarshal(raw, &copy)
	copy.liveAuthorityOnly = generation.liveAuthorityOnly
	if generation.Discovery != nil {
		discovery := cloneDiscovery(*generation.Discovery)
		copy.Discovery = &discovery
	}
	return copy, err
}

type inventoryCache struct {
	mu               sync.RWMutex
	generation       inventoryGeneration
	cancel           context.CancelFunc
	done             chan struct{}
	failed           bool
	failedClass      string
	authorityFailure error
	authorityProof   inventoryAuthorityProofState
	started          bool
	namespaces       map[string]bool
	wake             chan struct{}
	scopePending     bool
	candidates       namespaceCandidateManifest
	progress         inventoryProgress
}

func inventoryKey(scope Scope) string {
	return "kubernetes/inventory/" + logIdentity(scope.OrgID+"\x00"+scope.ClusterID+"\x00"+scope.CredentialID)
}

func (service *Service) startInventory(parent context.Context) {
	if service == nil || service.client == nil || service.changes == nil {
		return
	}
	service.changes.mu.Lock()
	scope := service.indexScope()
	if service.changes.stopped[scope] {
		service.changes.mu.Unlock()
		return
	}
	cache := service.changes.inventories[scope]
	if cache != nil && cache.started {
		service.changes.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(service.identity.ctx, cancel)
	if cache == nil {
		cache = &inventoryCache{}
	}
	cache.done, cache.cancel, cache.started = make(chan struct{}), func() { stop(); cancel() }, true
	if cache.namespaces == nil {
		cache.namespaces = make(map[string]bool)
	}
	if cache.wake == nil {
		cache.wake = make(chan struct{}, 1)
	}
	namespace := service.client.namespace
	if namespace == "" {
		namespace = "default"
	}
	cache.namespaces[namespace] = true
	service.changes.inventories[scope] = cache
	service.changes.mu.Unlock()
	go service.runInventory(ctx, cache)
}

func (service *Service) readInventory(ctx context.Context) (inventoryGeneration, error) {
	if pinned, ok := ctx.Value(inventoryReadContextKey{}).(inventoryGeneration); ok {
		if err := service.validateCredential(ctx); err != nil {
			return inventoryGeneration{}, err
		}
		return pinned, nil
	}
	if err := service.validateCredential(ctx); err != nil {
		return inventoryGeneration{}, err
	}
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	provider := service.changes.provider
	service.changes.mu.Unlock()
	if cache != nil {
		cache.mu.RLock()
		generation, failed, failedClass, authorityFailure := cache.generation, cache.failed, cache.failedClass, cache.authorityFailure
		cache.mu.RUnlock()
		if authorityFailure != nil {
			return inventoryGeneration{}, authorityFailure
		}
		if len(generation.View.Kinds) > 0 {
			if failed {
				generation.View.Kinds = failedInventoryKinds(generation.View.Kinds, failedClass)
				generation.ResourceStatus = failedInventoryKinds(generation.ResourceStatus, failedClass)
				scopes := make(map[string]kubeindex.Snapshot, len(generation.Namespaces))
				for namespace, snapshot := range generation.Namespaces {
					snapshot.Kinds = failedInventoryKinds(snapshot.Kinds, failedClass)
					scopes[namespace] = snapshot
				}
				generation.Namespaces = scopes
				generation.Usage.Fresh, generation.Usage.Availability = false, "stale"
				generation.Usage.Partial = append(append([]PartialFailure(nil), generation.Usage.Partial...), PartialFailure{ResourceID: "kubernetes.inventory", Class: inventoryCycleClass(failedClass)})
			}
			return generation, nil
		}
		if failed {
			generation.View.Kinds = cycleFailureKinds(inventoryCycleClass(failedClass))
			return generation, nil
		}
	}
	if provider != nil {
		// Upgrade precedence is explicit; never combine generations from different
		// orgs or credential epochs.
		orgs := service.readOrgs
		if len(orgs) == 0 {
			orgs = []string{service.scope.OrgID}
		}
		for _, org := range orgs {
			scope := service.scope
			scope.OrgID = org
			generation, record, err := readInventoryGeneration(provider, scope, "")
			if err != nil {
				return inventoryGeneration{}, err
			}
			generation = withFirstReady(generation, record)
			if len(generation.View.Kinds) > 0 {
				if err := service.validateCredential(ctx); err != nil {
					return inventoryGeneration{}, err
				}
				if err := service.authorizeInventoryRestore(ctx, generation); err != nil {
					return inventoryGeneration{}, err
				}
				return generation, nil
			}
		}
	}
	if publishedOnly, _ := ctx.Value(cachedCaptureContextKey{}).(bool); publishedOnly {
		if cache == nil {
			return inventoryGeneration{}, nil
		}
		cache.mu.RLock()
		generation := cache.generation
		cache.mu.RUnlock()
		return generation, nil
	}
	if cache == nil {
		if service.indexes != nil {
			index, err := service.indexes.ForScope(service.indexScope())
			if err == nil {
				return inventoryGeneration{View: index.Snapshot()}, nil
			}
		}
		return inventoryGeneration{}, nil
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	generation := cache.generation
	if len(generation.View.Kinds) == 0 && service.indexes != nil {
		index, err := service.indexes.ForScope(service.indexScope())
		if err == nil {
			generation.View = index.Snapshot()
		}
	}
	return generation, nil
}

// failedInventoryKinds reports that a cycle failed before these kinds could be
// relisted. The class comes from the cycle's own failure, so the answer is as
// specific as the reason we have. The cycle mark keeps a whole-cluster failure a
// fault: a connector that could not read anything has no per-kind capability gap
// to report, and calling it one would leave a dead credential looking healthy.
func failedInventoryKinds(kinds map[string]kubeindex.KindStatus, class string) map[string]kubeindex.KindStatus {
	result := make(map[string]kubeindex.KindStatus, len(kinds))
	for kind, state := range kinds {
		state.State, state.Partial, state.CycleFailure = "error", true, true
		if state.Error == "" || class == "forbidden" || class == "unauthorized" {
			state.Error = inventoryCycleClass(class)
		}
		result[kind] = state
	}
	return result
}

// cycleFailureKinds marks every known kind as belonging to a cycle that failed as
// a whole. A connector that read nothing has no per-kind capability gap to
// report, so the cycle mark keeps each kind a fault rather than a permission
// limit, and the class comes from the failure that stopped the cycle.
func cycleFailureKinds(class string) map[string]kubeindex.KindStatus {
	kinds := make(map[string]kubeindex.KindStatus, len(indexedResourceIDs))
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "error", Partial: true, Error: class, CycleFailure: true}
	}
	return kinds
}

// inventoryCycleClass keeps a cycle failure inside the known failure classes.
func inventoryCycleClass(class string) string {
	switch class {
	case "":
		return "unavailable"
	case "record_rejected", "record_limit", "incomplete_pages", "commit_conflict", "forbidden", "unauthorized", "timeout", "throttled", "unavailable", "budget_exhausted", "unsupported", "syncing", "namespace_scope":
		return class
	default:
		return "unavailable"
	}
}

// CachedIndexSnapshot reads a generation only. Collection is never charged to, or
// awaited by, the UI request.
func (service *Service) CachedIndexSnapshot(ctx context.Context, kinds ...string) (kubeindex.Snapshot, kubeindex.Status, error) {
	return service.CachedNamespaceSnapshot(ctx, "", kinds...)
}

func (service *Service) CachedNamespaceSnapshot(ctx context.Context, namespace string, kinds ...string) (kubeindex.Snapshot, kubeindex.Status, error) {
	if namespace != "" {
		service.queueInventoryNamespaceContext(ctx, namespace)
	}
	for _, kind := range kinds {
		if indexedResourceIDs[kind] == "" {
			return kubeindex.Snapshot{}, kubeindex.Status{}, ErrInvalidArguments
		}
	}
	generation, err := service.readInventory(ctx)
	if err != nil {
		return kubeindex.Snapshot{}, kubeindex.Status{}, err
	}
	if scoped, exists := generation.Namespaces[namespace]; namespace != "" && exists {
		states := make(map[string]kubeindex.KindStatus, len(generation.View.Kinds))
		for kind, state := range generation.View.Kinds {
			states[kind] = state
		}
		for kind, state := range scoped.Kinds {
			if states[kind].State != "ready" {
				states[kind] = state
			}
		}
		records := make([]kubeindex.Record, 0)
		for _, record := range generation.View.Records {
			if record.Namespace == "" || record.Namespace == namespace {
				records = append(records, record)
			}
		}
		byUID := make(map[string]bool, len(records))
		for _, record := range records {
			byUID[record.UID] = true
		}
		for _, record := range scoped.Records {
			if !byUID[record.UID] {
				records = append(records, record)
			}
		}
		generation.View = kubeindex.Snapshot{Records: records, Kinds: states}
	}
	observed, forbidden, unauthorized := false, false, false
	cycleForbidden, cycleUnauthorized := false, false
	for _, state := range generation.View.Kinds {
		observed = observed || !state.ObservedAt.IsZero()
	}
	for _, kind := range kinds {
		state := generation.View.Kinds[kind]
		observed = observed || !state.ObservedAt.IsZero()
		forbidden = forbidden || state.Error == "forbidden"
		unauthorized = unauthorized || state.Error == "unauthorized"
		cycleForbidden = cycleForbidden || state.CycleFailure && state.Error == "forbidden"
		cycleUnauthorized = cycleUnauthorized || state.CycleFailure && state.Error == "unauthorized"
	}
	if cycleUnauthorized {
		return kubeindex.Snapshot{}, kubeindex.Status{}, ErrUnauthorized
	}
	if cycleForbidden {
		return kubeindex.Snapshot{}, kubeindex.Status{}, ErrForbidden
	}
	if !observed && !service.recordedFirstReadyAt().IsZero() {
		if unauthorized {
			return kubeindex.Snapshot{}, kubeindex.Status{}, ErrUnauthorized
		}
		if forbidden {
			return kubeindex.Snapshot{}, kubeindex.Status{}, ErrForbidden
		}
		return kubeindex.Snapshot{}, kubeindex.Status{}, ErrInventoryNotReady
	}
	pending := generation.pendingResources(kinds)
	if len(pending) > 0 {
		// A kind the published generation has not listed yet is loading, never
		// empty: report it as pending so views neither show zero nor complete.
		states := make(map[string]kubeindex.KindStatus, len(generation.View.Kinds)+len(pending))
		for kind, state := range generation.View.Kinds {
			states[kind] = state
		}
		for _, kind := range kinds {
			if _, listed := states[kind]; !listed && containsString(pending, indexedResourceIDs[kind]) {
				// A loading kind carries no partial flag: the state already says it
				// is not complete, and a consumer that checks partial first would
				// read "failed" where the truth is "loading".
				states[kind] = kubeindex.KindStatus{State: "pending"}
			}
		}
		generation.View.Kinds = states
	}
	status := scopedIndexStatus(kubeindex.Status{Kinds: generation.View.Kinds}, kinds)
	status.Pending, status.PendingKinds = pending, generation.pendingKinds()
	// Kinds still loading do not hide the age of the listed ones: when the only
	// non-ready kinds are pending, an aged listed kind still makes the view
	// stale, reported together with sync.pending.
	onlyPending := true
	for _, state := range status.Kinds {
		if state.State != "pending" && (state.State != "ready" || state.Partial) {
			onlyPending = false
		}
	}
	if status.State == "ready" || status.State == "partial" && onlyPending {
		for kind, state := range status.Kinds {
			if state.State == "ready" && !state.ObservedAt.IsZero() && time.Since(state.ObservedAt) > max(3*time.Minute, 2*inventoryCadence(kind)) {
				status.State, status.Partial = "stale", true
			}
		}
	}
	if status.State == "warming" {
		status.State, status.Partial = "syncing", false
		status.Progress = service.coldSyncProgress()
	}
	requested := make(map[string]bool, len(kinds))
	capacity := 0
	for _, kind := range kinds {
		requested[kind] = true
		capacity += generation.View.Kinds[kind].Records
	}
	records := make([]kubeindex.Record, 0, min(capacity, len(generation.View.Records)))
	for _, record := range generation.View.Records {
		state := generation.View.Kinds[record.Kind]
		if requested[record.Kind] && state.Error != "forbidden" && state.Error != "unauthorized" {
			records = append(records, record)
		}
	}
	generation.View.Records = records
	return generation.View, status, nil
}

// pendingKinds counts the resource types a published generation still lacks.
func (generation inventoryGeneration) pendingKinds() int {
	if len(generation.View.Kinds) == 0 {
		return 0
	}
	return len(generation.Pending)
}

// pendingResources returns the requested kinds' resource IDs that a published
// generation has not listed yet.
func (generation inventoryGeneration) pendingResources(kinds []string) []string {
	if generation.pendingKinds() == 0 {
		return nil
	}
	var pending []string
	for _, kind := range kinds {
		id := indexedResourceIDs[kind]
		if _, listed := generation.View.Kinds[kind]; !listed && containsString(generation.Pending, id) && !containsString(pending, id) {
			pending = append(pending, id)
		}
	}
	return pending
}

func (service *Service) runInventory(ctx context.Context, cache *inventoryCache) {
	defer close(cache.done)
	defer cache.cancel()
	service.inventoryWorker(ctx, cache)
}

func (service *Service) collectInventoryUsage(ctx context.Context, previous Usage) Usage {
	result := previous
	result.Partial = nil
	result.Omitted = nil
	result.ObservedAt = time.Now().UTC()
	discovery, discovered := ctx.Value(inventoryDiscoveryContextKey{}).(Discovery)
	available := map[string]bool{}
	for _, definition := range discovery.Resources {
		available[definition.ID] = definition.Available
	}
	for _, source := range []struct{ resource, kind string }{{"nodes", "Node"}, {"pods", "Pod"}} {
		id := "metrics.k8s.io~v1beta1~" + source.resource
		if discovered && !available[id] {
			result.Omitted = appendUnique(result.Omitted, id)
			if source.kind == "Pod" {
				result.PodMetrics.Availability, result.PodMetrics.Fresh, result.PodMetrics.Complete = "unavailable", false, false
			} else {
				result.NodeMetrics.Availability, result.NodeMetrics.Fresh, result.NodeMetrics.Complete = "unavailable", false, false
			}
			continue
		}
		var samples []ResourceUsage
		seenSamples := map[string]bool{}
		token := ""
		pageLimit := kubernetesIndexPageSize
		seen := map[string]bool{}
		var loadErr error
		for {
			query := url.Values{"limit": {strconv.Itoa(pageLimit)}}
			if token != "" {
				query.Set("continue", token)
			}
			var page struct {
				Items    []map[string]any `json:"items"`
				Metadata struct {
					Continue string `json:"continue"`
				} `json:"metadata"`
			}
			pageCtx := context.WithValue(ctx, operationBudgetKey{}, (*operationBudget)(nil))
			loadErr = service.inventoryListRead(pageCtx, "/apis/metrics.k8s.io/v1beta1/"+source.resource+"?"+query.Encode(), &page, false)
			if source.kind == "Pod" && errors.Is(loadErr, ErrForbidden) {
				namespaces, _ := ctx.Value(inventoryNamespacesContextKey{}).([]string)
				readable := false
				for _, ns := range namespaces {
					items, err := service.loadAdditionalInventory(ctx, ResourceDefinition{ID: "metrics.k8s.io~v1beta1~pods", Kind: "PodMetrics", Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods", Namespaced: true}, ns)
					if err != nil {
						continue
					}
					readable = true
					for _, item := range items {
						if !item.metricsValid {
							loadErr = ErrInvalidArguments
							readable = false
							break
						}
						sample := ResourceUsage{Kind: "Pod", Namespace: item.Namespace, Name: item.Name, CPU: summaryString(item.Summary, "usage_cpu"), Memory: summaryString(item.Summary, "usage_memory"), Window: summaryString(item.Summary, "window")}
						sample.Timestamp, _ = time.Parse(time.RFC3339Nano, summaryString(item.Summary, "timestamp"))
						samples = append(samples, sample)
					}
				}
				if readable {
					loadErr = nil
					result.Partial = append(result.Partial, PartialFailure{ResourceID: "metrics.k8s.io~v1beta1~pods", Class: "namespace_scope"})
				}
			}
			if loadErr != nil {
				if errors.Is(loadErr, ErrResponseTooLarge) && pageLimit > minimumPageSize {
					pageLimit = max(minimumPageSize, pageLimit/2)
					continue
				}
				break
			}
			for _, raw := range page.Items {
				resource := service.projectResource(ResourceDefinition{ID: "metrics.k8s.io~v1beta1~" + source.resource, Kind: source.kind + "Metrics", Group: "metrics.k8s.io", Version: "v1beta1"}, raw)
				sample := ResourceUsage{Kind: source.kind, Namespace: resource.Namespace, Name: resource.Name, CPU: summaryString(resource.Summary, "usage_cpu"), Memory: summaryString(resource.Summary, "usage_memory"), Window: summaryString(resource.Summary, "window")}
				sample.Timestamp, _ = time.Parse(time.RFC3339Nano, summaryString(resource.Summary, "timestamp"))
				key := sample.Namespace + "\x00" + sample.Name
				if seenSamples[key] {
					loadErr = ErrIncompleteInventory
					break
				}
				seenSamples[key] = true
				samples = append(samples, sample)
				if !resource.metricsValid {
					loadErr = ErrInvalidArguments
					break
				}
			}
			if loadErr != nil || page.Metadata.Continue == "" {
				break
			}
			token = page.Metadata.Continue
			if len(token) > 4096 || seen[token] {
				loadErr = ErrInvalidArguments
				break
			}
			seen[token] = true
		}
		if loadErr != nil {
			result.Partial = append(result.Partial, PartialFailure{ResourceID: "metrics.k8s.io~v1beta1~" + source.resource, Class: errorClass(loadErr)})
			if source.kind == "Pod" {
				if !previous.PodMetrics.Complete && previous.PodMetrics.Total == 0 && len(samples) > 0 {
					result.Pods = samples
					result.PodMetrics = summarizeMetricsSource(samples, result.ObservedAt)
				}
				result.PodMetrics.Fresh, result.PodMetrics.Complete = false, false
				result.PodMetrics.Availability = "partial"
			} else {
				if !previous.NodeMetrics.Complete && previous.NodeMetrics.Total == 0 && len(samples) > 0 {
					result.Nodes = samples
					result.NodeMetrics = summarizeMetricsSource(samples, result.ObservedAt)
				}
				result.NodeMetrics.Fresh, result.NodeMetrics.Complete = false, false
				result.NodeMetrics.Availability = "partial"
			}
			continue
		}
		status := summarizeMetricsSource(samples, result.ObservedAt)
		status.Complete = true
		if source.kind == "Pod" {
			result.Pods, result.PodMetrics = samples, status
		} else {
			result.Nodes, result.NodeMetrics = samples, status
		}
	}
	result.Availability, result.Fresh = "available", true
	if len(result.Partial) > 0 {
		result.Availability, result.Fresh = "partial", false
	} else if result.PodMetrics.Total > 0 && !result.PodMetrics.Fresh || result.NodeMetrics.Total > 0 && !result.NodeMetrics.Fresh {
		result.Availability, result.Fresh = "stale", false
	}
	if len(result.Pods)+len(result.Nodes) == 0 {
		result.Availability, result.Fresh = "unavailable", false
	}
	return result
}

func (service *Service) inventoryRead(ctx context.Context, path string, value any, metadata bool) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		pageContext := context.WithValue(ctx, operationBudgetKey{}, (*operationBudget)(nil))
		if metadata {
			err = service.client.GetMetadataJSON(pageContext, path, value)
		} else {
			err = service.client.GetJSON(pageContext, path, value)
		}
		var invalid *inventoryListValidationError
		if errors.As(err, &invalid) {
			return err
		}
		if err == nil || !DiagnoseError(err).Retryable || errors.Is(err, ErrResponseTooLarge) || attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

type NamespacePage struct {
	Items   []string         `json:"items"`
	Sync    SyncStatus       `json:"sync"`
	Partial []PartialFailure `json:"partial_failures,omitempty"`
}

func (service *Service) Namespaces(ctx context.Context, hints ...string) (NamespacePage, error) {
	hintStates := make(map[string]string)
	for _, hint := range hints {
		if hint == "" {
			continue
		}
		if !safeSegment(hint) || len(hint) > 63 {
			return NamespacePage{}, ErrInvalidArguments
		}
		hintStates[hint] = service.queueInventoryNamespaceContext(ctx, hint)
	}
	view, status, err := service.CachedIndexSnapshot(ctx, "Namespace")
	result := NamespacePage{Items: []string{}, Sync: SyncStatus{State: status.State, Partial: status.Partial, AgeSeconds: status.AgeSeconds, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}}
	if err != nil {
		return result, err
	}
	state := view.Kinds["Namespace"]
	if state.Error != "" {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: indexedResourceIDs["Namespace"], Class: state.Error})
	}
	if state.Error != "forbidden" && state.Error != "unauthorized" {
		for _, record := range view.Records {
			if record.Kind == "Namespace" {
				result.Items = append(result.Items, record.Name)
			}
		}
	}
	generation, readErr := service.readInventory(ctx)
	if readErr != nil {
		return result, readErr
	}
	for _, hint := range hints {
		if hint != "" {
			if class := hintStates[hint]; class != "" && class != "namespace_hint_pending" {
				result.Sync.State, result.Sync.Partial = "partial", true
				result.Partial = append(result.Partial, PartialFailure{ResourceID: indexedResourceIDs["Namespace"], Scope: hint, Class: class})
			} else if _, observed := generation.Namespaces[hint]; !observed && hintStates[hint] == "namespace_hint_pending" {
				result.Sync.State, result.Sync.Partial = "syncing", false
			}
		}
	}
	seen := map[string]bool{}
	for _, ns := range result.Items {
		seen[ns] = true
	}
	for ns, at := range generation.NamespaceProof {
		if time.Since(at) <= namespaceObservationTTL && !seen[ns] {
			result.Items = append(result.Items, ns)
			seen[ns] = true
		}
	}
	for ns, scoped := range generation.Namespaces {
		for _, state := range scoped.Kinds {
			if !state.ObservedAt.IsZero() && state.Error != "forbidden" && state.Error != "unauthorized" && !seen[ns] {
				result.Items, seen[ns] = append(result.Items, ns), true
				break
			}
		}
	}
	sort.Strings(result.Items)
	return result, nil
}

func indexedKind(resourceID string) string {
	for kind, id := range indexedResourceIDs {
		if id == resourceID {
			return kind
		}
	}
	return ""
}

func (service *Service) cachedList(ctx context.Context, options ListOptions) (ResourcePage, error) {
	kind := indexedKind(options.ResourceID)
	if kind == "" || options.Namespace != "" && !safeSegment(options.Namespace) {
		return ResourcePage{}, ErrInvalidArguments
	}
	view, status, err := service.CachedNamespaceSnapshot(ctx, options.Namespace, kind)
	result := ResourcePage{Items: []ProjectedResource{}, Sync: SyncStatus{State: status.State, Partial: status.Partial, AgeSeconds: status.AgeSeconds, Progress: status.Progress, Pending: status.Pending, PendingKinds: status.PendingKinds}}
	if err != nil {
		return result, err
	}
	state := status.Kinds[kind]
	if state.Error != "" {
		result.Partial = append(result.Partial, PartialFailure{ResourceID: options.ResourceID, Class: state.Error})
	}
	if state.Error == "forbidden" || state.Error == "unauthorized" {
		return result, nil
	}
	for _, record := range view.Records {
		if record.Kind == kind && (options.Namespace == "" || record.Namespace == options.Namespace) {
			result.Items = append(result.Items, projectedWorkloadIndexRecord(record))
		}
	}
	sort.Slice(result.Items, func(i, j int) bool {
		if result.Items[i].Namespace != result.Items[j].Namespace {
			return result.Items[i].Namespace < result.Items[j].Namespace
		}
		return result.Items[i].Name < result.Items[j].Name
	})
	return result, nil
}
