package kubernetes

import (
	"context"
	"errors"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/google/uuid"
)

func inventoryCadence(kind string) time.Duration {
	if cadence := captureCadences[kind]; cadence > 0 {
		return cadence
	}
	switch kind {
	case "Namespace", "Service", "Ingress", "HTTPRoute", "Gateway":
		return time.Minute
	default:
		return 5 * time.Minute
	}
}

func inventoryDue(state kubeindex.KindStatus, cadence time.Duration, now time.Time) bool {
	at := state.LastAttemptAt
	if at.IsZero() {
		at = state.ObservedAt
	}
	if state.State == "error" {
		cadence = min(cadence, 15*time.Second)
	}
	return at.IsZero() || now.Sub(at) >= cadence
}

func (service *Service) inventoryWorker(ctx context.Context, cache *inventoryCache) {
	stopWorker := service.recovery.workerStarted()
	defer stopWorker()
	owner := uuid.NewString()
	var ownedLease *inventoryLease
	defer func() {
		if ownedLease != nil {
			if err := ownedLease.release(); err != nil {
				log.Printf("kubernetes: inventory lease release deferred")
			}
		}
	}()
	var index *kubeindex.Index
	var loadedRevision string
	var evidenceCommitted string
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		if err := waitRecovery(ctx, service.recovery); err != nil {
			return
		}
		service.recovery.attempted()
		provider := service.usageProvider()
		cache.mu.RLock()
		generation := cache.generation
		cache.mu.RUnlock()
		lastGood := generation
		var err error
		if provider != nil {
			orgs := service.readOrgs
			if len(orgs) == 0 {
				orgs = []string{service.scope.OrgID}
			}
			for _, org := range orgs {
				if org != service.scope.OrgID && len(generation.View.Kinds) > 0 {
					break
				}
				scope := service.scope
				scope.OrgID = org
				candidate, record, readErr := readInventoryGeneration(provider, scope, generation.Revision)
				if readErr != nil {
					err = readErr
					break
				}
				// The cluster's own metadata decides the first ready moment here,
				// whichever branch below takes the data.
				candidate = withFirstReady(candidate, record)
				if record.Revision != "" && record.Revision == generation.Revision {
					generation.FirstReadyAt = candidate.FirstReadyAt
					break
				}
				if len(candidate.View.Kinds) > 0 {
					generation = candidate
					break
				}
			}
		}
		if err == nil && (index == nil || loadedRevision != generation.Revision) {
			if restoreErr := service.authorizeInventoryRestore(ctx, generation); restoreErr != nil {
				err = restoreErr
			}
			// Validate into a new index before replacing the worker's last-good
			// index. Malformed stored data is retryable, never a permanent exit.
			if err == nil {
				index, err = hydrateInventory(generation.View)
			}
			if err == nil {
				loadedRevision = generation.Revision
				cache.mu.Lock()
				cache.generation, cache.failed = generation, false
				cache.mu.Unlock()
			}
		}
		var payloadFault inventoryPayloadError
		if errors.As(err, &payloadFault) || errors.Is(err, kubeindex.ErrInvalidRecord) {
			lease, owned, claimErr := service.claimInventory(ctx, provider, owner)
			if claimErr != nil {
				err = claimErr
			} else if owned {
				ownedLease = lease
				generation = lastGood
				index, err = hydrateInventory(lastGood.View)
				if err == nil {
					states := make(map[string]kubeindex.KindStatus, len(generation.View.Kinds))
					for kind, state := range generation.View.Kinds {
						state.LastAttemptAt = time.Now().Add(-24 * time.Hour)
						states[kind] = state
					}
					generation.View.Kinds = states
					resourceStates := make(map[string]kubeindex.KindStatus, len(generation.ResourceStatus))
					for id, state := range generation.ResourceStatus {
						state.LastAttemptAt = time.Now().Add(-24 * time.Hour)
						resourceStates[id] = state
					}
					generation.ResourceStatus = resourceStates
					generation.Usage.ObservedAt = time.Time{}
					err = service.inventoryOwnedCycle(ctx, cache, lease, index, &generation)
					if err == nil {
						loadedRevision = generation.Revision
					}
				}
			}
			// An active foreign lease remains authoritative even when its data
			// is unreadable. Only a successful scoped claim may re-collect.
			if err == nil {
				continue
			}
		}

		if err == nil {
			var lease *inventoryLease
			var owned bool
			lease, owned, err = service.claimInventory(ctx, provider, owner)
			if err == nil && owned {
				ownedLease = lease
				if generation.EvidenceBatch != nil && evidenceCommitted != generation.Revision {
					lease.mu.Lock()
					guard := append([]byte(nil), lease.expected...)
					lease.mu.Unlock()
					pending := *generation.EvidenceBatch
					pending.GuardKey = inventoryKey(service.scope)
					pending.GuardExpected = guard
					pendingGeneration := generation
					pendingGeneration.EvidenceBatch = &pending
					err = service.commitEvidence(ctx, pendingGeneration)
					if err == nil {
						evidenceCommitted = generation.Revision
					}
				}
				if err == nil {
					err = service.inventoryOwnedCycle(ctx, cache, lease, index, &generation)
				}
				if err == nil {
					loadedRevision = generation.Revision
				}
			} else if err == nil {
				cache.mu.Lock()
				cache.generation, cache.failed = generation, false
				cache.mu.Unlock()
			}
		}
		if ctx.Err() != nil {
			return
		}
		wait := time.Second
		if err != nil {
			class := errorClass(err)
			// Stable credential bytes do not imply unchanged Kubernetes permissions.
			var unverified *inventoryAuthorityUnavailable
			if service.inventoryRestore != nil && (class == "forbidden" || class == "unauthorized" || errors.As(err, &unverified)) {
				service.inventoryRestore.validated.Store(false)
			}
			service.identity.mu.Lock()
			renewal := service.identity.renewal
			service.identity.mu.Unlock()
			if !renewal {
				service.recovery.failed(err, service.client)
			}
			index = nil
			service.logInventoryCycle(err)
			cache.mu.Lock()
			cache.failed, cache.failedClass = true, class
			if class == "forbidden" || class == "unauthorized" {
				cache.authorityFailure = nil
			}
			cache.mu.Unlock()
			wait, backoff = backoff, min(15*time.Second, backoff*2)
		} else {
			service.clearInventoryCycleLog()
			backoff = 100 * time.Millisecond
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-cache.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (service *Service) inventoryOwnedCycle(parent context.Context, cache *inventoryCache, lease *inventoryLease, index *kubeindex.Index, generation *inventoryGeneration) error {
	now := time.Now().UTC()
	cache.mu.RLock()
	due := cache.scopePending
	cache.mu.RUnlock()
	due = due || len(generation.View.Kinds) < len(indexedResourceIDs) || generation.Usage.ObservedAt.IsZero() || now.Sub(generation.Usage.ObservedAt) >= 30*time.Second
	for kind, state := range generation.View.Kinds {
		due = due || inventoryDue(state, inventoryCadence(kind), now)
	}
	for _, state := range generation.ResourceStatus {
		due = due || inventoryDue(state, 5*time.Minute, now)
	}
	promoted := false
	if !due {
		probe := *generation
		probeCtx := context.WithValue(parent, credentialEpochContextKey{}, service)
		var err error
		promoted, err = service.discoverNamespaceCandidates(probeCtx, cache, &probe, now)
		if err != nil {
			return err
		}
		if !promoted {
			return nil
		}
		// The proof map is freshly allocated by discovery, not shared with readers.
		generation.NamespaceProof = probe.NamespaceProof
	}
	// The candidate owns all mutable maps; committed readers never see collection.
	destination := generation
	candidate, cloneErr := cloneInventoryGeneration(*generation)
	if cloneErr != nil {
		return cloneErr
	}
	generation = &candidate
	ctx, cancel := context.WithTimeout(captureReadContext(parent, lease.record.Owner), 15*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, credentialEpochContextKey{}, service)
	ctx = context.WithValue(ctx, inventoryNamespacePromotedContextKey{}, promoted)
	renewDone := make(chan struct{})
	renewErrors := make(chan error, 1)
	ownedError := func(err error) error {
		select {
		case renewalErr := <-renewErrors:
			return renewalErr
		default:
			return err
		}
	}

	go func() {
		defer close(renewDone)
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				renewStarted := time.Now()
				err := lease.renew(ctx, service)
				service.recovery.stage("lease", renewStarted, err)
				if err != nil {
					renewErrors <- err
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-renewDone }()
	publishCore := func(current *inventoryGeneration) error {
		if err := lease.publish(ctx, service, current); err != nil {
			return err
		}
		committed, err := cloneInventoryGeneration(*current)
		if err != nil {
			return err
		}
		cache.mu.Lock()
		if service.inventoryRestore != nil {
			service.inventoryRestore.validated.Store(true)
		}
		cache.generation, cache.failed = committed, false
		cache.authorityFailure = nil
		*destination = committed
		service.recovery.published(*current, service.scope.OrgID)
		cache.mu.Unlock()
		return lease.commitJournal(ctx, service, committed)
	}
	dirty, err := service.collectInventoryCycle(ctx, cache, index, generation, publishCore)
	if err != nil {
		return ownedError(err)
	}
	if !dirty {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return ownedError(err)
	}
	if err := lease.publish(ctx, service, generation); err != nil {
		return ownedError(err)
	}
	cache.mu.Lock()
	if service.inventoryRestore != nil {
		service.inventoryRestore.validated.Store(true)
	}
	cache.generation, cache.failed = *generation, false
	cache.authorityFailure = nil
	*destination = *generation
	service.recovery.published(*generation, service.scope.OrgID)
	cache.mu.Unlock()
	return lease.commitJournal(ctx, service, *generation)
}

// Discovery already tries the preferred version first. Defensively select one
// group/resource identity and exclude alternate versions of indexed kinds.
func canonicalInventoryResources(resources []ResourceDefinition) []ResourceDefinition {
	seen := map[string]bool{}
	result := []ResourceDefinition{}
	for _, definition := range resources {
		if !definition.Available || definition.Group == "metrics.k8s.io" {
			continue
		}
		key := definition.Group + "\x00" + definition.Resource
		if seen[key] {
			continue
		}
		seen[key] = true
		if id := indexedResourceIDs[definition.Kind]; id != "" {
			parts := strings.Split(id, "~")
			group := parts[0]
			if group == "core" {
				group = ""
			}
			if group == definition.Group && parts[2] == definition.Resource {
				continue
			}
		}
		result = append(result, definition)
	}
	return result
}

func (service *Service) collectInventoryCycle(ctx context.Context, cache *inventoryCache, index *kubeindex.Index, generation *inventoryGeneration, publishCore ...func(*inventoryGeneration) error) (bool, error) {
	now := time.Now().UTC()
	ctx = context.WithValue(ctx, inventoryCollectionAuthorityKey{}, &inventoryCollectionAuthority{})
	discovered, _ := ctx.Value(inventoryNamespacePromotedContextKey{}).(bool)
	var err error
	kinds := []string{}
	cache.mu.Lock()
	scopePending := cache.scopePending || discovered
	cache.scopePending = false
	cache.mu.Unlock()
	// A cluster's first generation carries only the core kinds, so views can
	// open within seconds; every other kind and API group fills in on the
	// following cycles.
	cold := len(generation.View.Kinds) == 0
	for kind := range indexedResourceIDs {
		if cold && !inventoryCoreKinds[kind] {
			continue
		}
		state := generation.View.Kinds[kind]
		if inventoryDue(state, inventoryCadence(kind), now) || scopePending && (state.Error == "forbidden" || state.Error == "namespace_scope") {
			kinds = append(kinds, kind)
		}
	}
	sortCoreKindsFirst(kinds)
	metricsDue := generation.Usage.ObservedAt.IsZero() || now.Sub(generation.Usage.ObservedAt) >= 30*time.Second
	additionalDue := false
	for _, state := range generation.ResourceStatus {
		additionalDue = additionalDue || inventoryDue(state, 5*time.Minute, now)
	}
	if len(kinds) == 0 && !metricsDue && !additionalDue && !discovered {
		discovered, err = service.discoverNamespaceCandidates(ctx, cache, generation, now)
		if err != nil {
			return false, err
		}
		if !discovered {
			return false, nil
		}
	}
	// Keep reporting through the core-only publication until cluster-wide ready.
	var progress *inventoryProgress
	if generation.FirstReadyAt.IsZero() {
		progress = &cache.progress
		progress.due(len(indexedResourceIDs), now)
		for kind, state := range generation.View.Kinds {
			progress.kindObserved(kind, state, now)
			if state.State == "ready" && !state.Absent {
				progress.kindListed(kind, state.Records, now)
			}
		}
		if !cold {
			progress.complete("connected", now)
			progress.complete("building", now)
		}
		if cold {
			progress.advance("connected", time.Now().UTC())
		} else {
			progress.advance("listing", time.Now().UTC())
		}
		defer func() {
			progress.pause(time.Now().UTC())
			status := progress.snapshot()
			log.Printf("kubernetes: first sync phase=%s elapsed_s=%.3f connected_s=%.3f listing_s=%.3f building_s=%.3f metrics_s=%.3f remaining=%s", status.Step, time.Since(status.StartedAt).Seconds(), status.PhaseSeconds["connected"], status.PhaseSeconds["listing"], status.PhaseSeconds["building"], status.PhaseSeconds["metrics"], strings.Join(status.RemainingKinds, ","))
		}()
	}
	resources := make(map[string][]ProjectedResource, len(generation.Resources))
	states := make(map[string]kubeindex.KindStatus, len(generation.ResourceStatus))
	for id, items := range generation.Resources {
		resources[id] = items
	}
	for id, state := range generation.ResourceStatus {
		states[id] = state
	}
	var discovery Discovery
	discoveryStarted := time.Now()
	degradedDiscovery := false
	discoveryCtx, cancelDiscovery := context.WithTimeout(ctx, 10*time.Second)
	if cold {
		discovery, err = service.coreDiscovery(discoveryCtx)
	} else {
		discovery, err = service.Discover(discoveryCtx)
	}
	cancelDiscovery()
	service.recovery.stage("discovery", discoveryStarted, err)
	if err != nil {
		discoveryFailureClass := inventoryCycleClass(errorClass(err))
		if cold || ctx.Err() != nil || service.identity.ctx.Err() != nil || errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrCredentialUnavailable) {
			return false, err
		}
		if generation.Discovery != nil {
			discovery = cloneDiscovery(*generation.Discovery)
		} else {
			fallbackCtx, cancelFallback := context.WithTimeout(ctx, 10*time.Second)
			discovery, err = service.coreDiscovery(fallbackCtx)
			cancelFallback()
			if err != nil {
				return false, err
			}
		}
		degradedDiscovery = true
		discovery.Partial = append(discovery.Partial, PartialFailure{ResourceID: "kubernetes.discovery", Class: discoveryFailureClass})
	}
	storedDiscovery := cloneDiscovery(discovery)
	generation.Discovery = &storedDiscovery

	// Use the discovered preferred version for known resources as well.
	preferred := make(map[string]ResourceDefinition)
	for _, definition := range discovery.Resources {
		id := indexedResourceIDs[definition.Kind]
		if id == "" || !definition.Available {
			continue
		}
		parts := strings.Split(id, "~")
		group := parts[0]
		if group == "core" {
			group = ""
		}
		if definition.Group == group && definition.Resource == parts[2] {
			if _, exists := preferred[definition.Kind]; !exists {
				preferred[definition.Kind] = definition
			}
		}
	}
	if progress != nil {
		progress.complete("connected", time.Now().UTC())
		progress.advance("listing", time.Now().UTC())
	}
	if cold || degradedDiscovery {
		// Resolve core kinds against core discovery only: a core kind whose
		// group the cluster does not serve is absent, not a reason to walk
		// every API group before the first publication.
		ctx = context.WithValue(ctx, discoveryContextKey{}, discovery)
	}
	ctx = context.WithValue(ctx, indexDefinitionContextKey{}, preferred)
	ctx = context.WithValue(ctx, inventoryDiscoveryContextKey{}, discovery)
	generation.DiscoveryPartial = append([]PartialFailure(nil), discovery.Partial...)
	crashes := &crashCaptureBatch{}
	ctx = context.WithValue(ctx, crashCaptureContextKey{}, crashes)
	indexedStarted := time.Now()
	var workers sync.WaitGroup
	var resultMu sync.Mutex
	sem := make(chan struct{}, 4)
	dirty := len(kinds) > 0 || metricsDue || discovered
	inventoryChanged := len(kinds) > 0 || discovered
	for _, kind := range kinds {
		kind := kind
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			refreshErr := index.RefreshGuarded(ctx, kind, 0, now, func(ctx context.Context) ([]kubeindex.Record, bool, error) {
				records, complete, err := service.loadIndexKind(ctx, kind)
				if err == nil && !complete {
					err = ErrIncompleteInventory
				}
				return records, complete, err
			}, func(commit func() error) error { return service.commitCredential(ctx, commit) })
			if refreshErr != nil && ctx.Err() != nil {
				return
			}
			if progress != nil {
				if state, known := index.KindStatus(kind); known {
					progress.kindObserved(kind, state, time.Now().UTC())
					if refreshErr == nil && !state.Absent {
						progress.kindListed(kind, state.Records, time.Now().UTC())
					}
				}
			}
		}()
	}
	workers.Wait()
	service.recovery.stage("indexed", indexedStarted, ctx.Err())
	// Publish usable indexed observations before the optional API tail. The
	// candidate is cloned by the publisher, so later stages cannot mutate it.
	indexedPublished := false
	generation.View = index.Snapshot()
	if currentCollectionIdentityVerified(*generation, now) {
		generation.liveAuthorityOnly = false
	}
	materialDenied := currentMaterialDenied(*generation, now)
	if inventoryCollectionValidationError(ctx) != nil {
		generation.liveAuthorityOnly = true
	}
	if err := collectionEnvelopeAuthorityError(ctx, cache, materialDenied); err != nil {
		return false, err
	}
	if materialDenied && generation.EvidenceScope.ClusterUID != "" {
		if err := service.verifyCurrentInventoryAuthority(ctx, cache, *generation, now); err != nil {
			return false, err
		}
		generation.liveAuthorityOnly = inventoryCollectionValidationError(ctx) != nil || !currentCollectionIdentityVerified(*generation, now)
	}
	cache.mu.RLock()
	authorityUnverified := cache.authorityFailure != nil
	cache.mu.RUnlock()
	if !materialDenied && !authorityUnverified && !cold && !generation.FirstReadyAt.IsZero() && len(kinds) > 0 && len(publishCore) > 0 {
		generation.Pending, generation.PendingDiscovery = pendingInventoryKinds(generation.View), degradedDiscovery
		if err := publishCore[0](generation); err != nil {
			return false, err
		}
		indexedPublished = true
	}
	additionalStarted := time.Now()
	optionalCtx, cancelOptional := context.WithTimeout(ctx, 10*time.Second)
	defer cancelOptional()
	additional := canonicalInventoryResources(discovery.Resources)
	if cold {
		additional = nil
	}
	for _, definition := range additional {
		if !inventoryDue(states[definition.ID], 5*time.Minute, now) {
			continue
		}
		dirty = true
		inventoryChanged = true
		definition := definition
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case sem <- struct{}{}:
			case <-optionalCtx.Done():
				resultMu.Lock()
				state := states[definition.ID]
				state.State, state.Partial, state.Error = "error", true, "timeout"
				state.LastAttemptAt = now
				states[definition.ID] = state
				resultMu.Unlock()
				return
			}
			defer func() { <-sem }()
			items, err := service.loadAdditionalInventory(optionalCtx, definition)
			resultMu.Lock()
			defer resultMu.Unlock()
			state := states[definition.ID]
			state.LastAttemptAt = now
			if err != nil {
				state.State, state.Partial, state.Error = "error", true, errorClass(err)
			} else {
				resources[definition.ID] = items
				state = kubeindex.KindStatus{State: "ready", ObservedAt: now, CompletedAt: time.Now().UTC(), LastAttemptAt: now, Records: len(items)}
			}
			states[definition.ID] = state
		}()
	}
	workers.Wait()
	service.recovery.stage("additional", additionalStarted, optionalCtx.Err())
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if progress != nil {
		status := progress.snapshot()
		if len(status.RemainingKinds) == 0 {
			progress.complete("listing", time.Now().UTC())
		}
		progress.advance("building", time.Now().UTC())
	}
	generation.View = index.Snapshot()
	service.logInventoryKinds(generation.View.Kinds)
	generation.Pending, generation.PendingDiscovery = pendingInventoryKinds(generation.View), cold || degradedDiscovery
	recordNamespaceProofs(generation)
	promoted, err := service.discoverNamespaceCandidates(ctx, cache, generation, now)
	if err != nil {
		return false, err
	}
	discovered = discovered || promoted
	dirty = dirty || discovered
	inventoryChanged = inventoryChanged || discovered
	generation.Resources, generation.ResourceStatus = resources, states
	scopeKinds := append([]string(nil), kinds...)
	if discovered {
		seen := map[string]bool{}
		for _, kind := range scopeKinds {
			seen[kind] = true
		}
		for kind, state := range generation.View.Kinds {
			if (state.Error == "forbidden" || state.Error == "namespace_scope") && !seen[kind] {
				scopeKinds = append(scopeKinds, kind)
			}
		}
	}
	namespaceStarted := time.Now()
	namespaceErr := service.collectScopedInventory(ctx, cache, generation, scopeKinds, now)
	service.recovery.stage("namespace", namespaceStarted, namespaceErr)
	if namespaceErr != nil {
		return false, namespaceErr
	}
	recordNamespaceProofs(generation)
	if inventoryCollectionValidationError(ctx) != nil {
		generation.liveAuthorityOnly = true
	}
	if err := collectionEnvelopeAuthorityError(ctx, cache, materialDenied || currentMaterialDenied(*generation, now)); err != nil {
		return false, err
	}
	if materialDenied || currentMaterialDenied(*generation, now) {
		if err := service.verifyCurrentInventoryAuthority(ctx, cache, *generation, now); err != nil {
			return false, err
		}
		generation.liveAuthorityOnly = inventoryCollectionValidationError(ctx) != nil || !currentCollectionIdentityVerified(*generation, now)
	}
	if crashes.attempted && crashes.read {
		events, _, _ := crashes.resolve(generation.View, time.Now().UTC())
		generation.Crashes, generation.PodSampledAt = events, now
	}
	if progress != nil {
		progress.complete("building", time.Now().UTC())
	}
	if metricsDue || discovered {
		// A first ready publication must not wait for optional usage reads, even
		// when the cold core-only publication left other inventory kinds pending.
		state, _, _, _, _ := summarizeSyncHealth(syncHealthKinds(*generation))
		if !indexedPublished && inventoryChanged && (cold || state == "ready" || !generation.FirstReadyAt.IsZero()) && len(publishCore) > 0 {
			if err := publishCore[0](generation); err != nil {
				return false, err
			}
		}
		if progress != nil {
			progress.advance("metrics", time.Now().UTC())
		}
		namespaces := []string{}
		cache.mu.RLock()
		for ns := range generation.Namespaces {
			if cache.namespaces[ns] || namespaceObserved(*generation, ns, now) {
				namespaces = append(namespaces, ns)
			}
		}
		cache.mu.RUnlock()
		usageStarted := time.Now()
		usageCtx, cancelUsage := context.WithTimeout(ctx, 10*time.Second)
		generation.Usage = service.collectInventoryUsage(context.WithValue(usageCtx, inventoryNamespacesContextKey{}, namespaces), generation.Usage)
		service.recovery.stage("usage", usageStarted, usageCtx.Err())
		cancelUsage()
		if progress != nil {
			if generation.Usage.PodMetrics.Complete && generation.Usage.NodeMetrics.Complete {
				progress.complete("metrics", time.Now().UTC())
			} else {
				progress.phaseResult("metrics", "unavailable (optional)")
			}
		}
	}
	return dirty, ctx.Err()
}

func (service *Service) collectScopedInventory(ctx context.Context, cache *inventoryCache, generation *inventoryGeneration, kinds []string, now time.Time) error {
	cache.mu.RLock()
	namespaces := []string{}
	for ns := range cache.namespaces {
		namespaces = append(namespaces, ns)
	}
	cache.mu.RUnlock()
	for namespace, at := range generation.NamespaceProof {
		if now.Sub(at) <= namespaceObservationTTL {
			namespaces = append(namespaces, namespace)
		}
	}
	for namespace := range generation.Namespaces {
		if namespaceObserved(*generation, namespace, now) {
			namespaces = append(namespaces, namespace)
		}
	}
	sort.Strings(namespaces)
	// A still-requested namespace keeps its last scoped result, including a
	// failed one, until a cycle that lists its kinds replaces it; a cycle that
	// lists other kinds must not erase it.
	requested := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		requested[ns] = true
	}
	scoped := make(map[string]kubeindex.Snapshot, len(generation.Namespaces))
	for ns, snapshot := range generation.Namespaces {
		verified := false
		for _, state := range snapshot.Kinds {
			verified = verified || !state.CompletedAt.IsZero()
		}
		if verified || requested[ns] || namespaceObserved(*generation, ns, now) {
			scoped[ns] = snapshot
		}
	}
	for i, ns := range namespaces {
		if i > 0 && namespaces[i-1] == ns {
			continue
		}
		scopeIndex, err := hydrateInventory(scoped[ns])
		if err != nil {
			return err
		}
		attempted := false
		for _, kind := range kinds {
			if kind == "Node" || kind == "Namespace" {
				continue
			}
			state := generation.View.Kinds[kind]
			if state.Error != "forbidden" {
				continue
			}
			attempted = true
			refreshErr := scopeIndex.RefreshGuarded(ctx, kind, inventoryCadence(kind), now, func(ctx context.Context) ([]kubeindex.Record, bool, error) {
				snapshot, _ := service.namespaceIssueSnapshot(ctx, ns, []string{kind})
				state := snapshot.Kinds[kind]
				if state.Error != "" {
					return nil, false, indexReadError{error: inventorySyncError(), class: state.Error}
				}
				return snapshot.Records, state.State == "ready" && !state.Partial, ctx.Err()
			}, func(commit func() error) error { return service.commitCredential(ctx, commit) })
			if refreshErr != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if attempted {
			scoped[ns] = scopeIndex.Snapshot()
		}
	}
	generation.Namespaces = scoped
	records := make(map[string]kubeindex.Record, len(generation.View.Records))
	for _, record := range generation.View.Records {
		state := generation.View.Kinds[record.Kind]
		if state.Error != "forbidden" && state.Error != "namespace_scope" {
			records[record.UID] = record
		}
	}
	for _, snapshot := range scoped {
		for kind, state := range snapshot.Kinds {
			global := generation.View.Kinds[kind]
			if (global.Error == "forbidden" || global.Error == "namespace_scope") && state.State == "ready" {
				global.State, global.Partial, global.Error = "partial", true, "namespace_scope"
				global.ObservedAt = state.ObservedAt
				generation.View.Kinds[kind] = global
				for _, record := range snapshot.Records {
					if record.Kind == kind {
						records[record.UID] = record
					}
				}
			}
		}
	}
	generation.View.Records = make([]kubeindex.Record, 0, len(records))
	for _, record := range records {
		generation.View.Records = append(generation.View.Records, record)
	}
	return nil
}

// inventoryCoreKinds are listed and published first on a cold cluster: they
// back the overview, issues, workloads, timeline and crash views.
var inventoryCoreKinds = map[string]bool{
	"Node": true, "Namespace": true, "Pod": true, "Deployment": true, "StatefulSet": true, "DaemonSet": true,
	"ReplicaSet": true, "Job": true, "CronJob": true, "Service": true, "Event": true, "HorizontalPodAutoscaler": true,
}

// coreDiscoveryGroups are the API groups that serve the core kinds, plus the
// metrics API the first generation reads usage from.
var coreDiscoveryGroups = map[string]bool{"apps": true, "batch": true, "autoscaling": true, "metrics.k8s.io": true}

// coreDiscovery discovers the core API and only the groups the core kinds need,
// choosing versions the same way full discovery does, instead of walking every
// group the cluster serves.
func (service *Service) coreDiscovery(ctx context.Context) (result Discovery, resultErr error) {
	if err := service.validateCredential(ctx); err != nil {
		return Discovery{}, err
	}
	ctx, cancelIdentity := service.credentialContext(ctx)
	defer cancelIdentity()
	credentialCtx := ctx
	defer func() {
		if err := service.validateCredential(credentialCtx); err != nil {
			result, resultErr = Discovery{}, err
		}
	}()
	ctx, cancel := withOperationBudget(ctx, defaultOperationTimeout, discoveryRequests, defaultOperationBytes, defaultOperationItems)
	defer cancel()
	// Reuse a fresh or in-flight full discovery rather than reading the same
	// API versions twice.
	key := service.cacheKey()
	service.mu.Lock()
	cached, ok := service.cache[key]
	running := service.discoveryCalls[key]
	service.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		return cloneDiscovery(cached.discovery), nil
	}
	if running != nil {
		select {
		case <-ctx.Done():
			return Discovery{}, ctx.Err()
		case <-running.done:
			return cloneDiscovery(running.result), running.err
		}
	}
	if err := ctx.Err(); err != nil {
		return Discovery{}, err
	}
	discovery := Discovery{ObservedAt: time.Now().UTC()}
	type groupVersion struct {
		GroupVersion string `json:"groupVersion"`
		Version      string `json:"version"`
	}
	var groups struct {
		Groups []struct {
			Name             string         `json:"name"`
			PreferredVersion groupVersion   `json:"preferredVersion"`
			Versions         []groupVersion `json:"versions"`
		} `json:"groups"`
	}
	service.discoverVersion(ctx, "", "v1", "/api/v1", &discovery)
	if len(discovery.Partial) > 0 {
		if err := ctx.Err(); err != nil {
			return Discovery{}, err
		}
		return Discovery{}, indexReadError{error: inventorySyncError(), class: discovery.Partial[0].Class}
	}
	if err := service.client.GetJSON(ctx, "/apis", &groups); err != nil {
		if ctx.Err() != nil {
			return Discovery{}, ctx.Err()
		}
		discovery.Partial = append(discovery.Partial, PartialFailure{Class: errorClass(err)})
		return discovery, nil
	}
	for _, group := range groups.Groups {
		if !coreDiscoveryGroups[group.Name] {
			continue
		}
		seen := map[string]bool{}
		groupPartialBefore := len(discovery.Partial)
		for _, version := range append([]groupVersion{group.PreferredVersion}, group.Versions...) {
			if version.Version == "" || seen[version.Version] {
				continue
			}
			seen[version.Version] = true
			before := len(discovery.Resources)
			service.discoverVersion(ctx, group.Name, version.Version, "/apis/"+url.PathEscape(group.Name)+"/"+url.PathEscape(version.Version), &discovery)
			if len(discovery.Resources) > before {
				retainUnreadableVersions(&discovery, groupPartialBefore)
				break
			}
		}
	}
	return discovery, ctx.Err()
}

// pendingInventoryKinds lists the indexed kinds the view has not listed yet.
func pendingInventoryKinds(view kubeindex.Snapshot) []string {
	pending := []string{}
	for kind, id := range indexedResourceIDs {
		if _, listed := view.Kinds[kind]; !listed {
			pending = append(pending, id)
		}
	}
	sort.Strings(pending)
	if len(pending) == 0 {
		return nil
	}
	return pending
}

func sortCoreKindsFirst(kinds []string) {
	sort.Slice(kinds, func(left, right int) bool {
		if inventoryCoreKinds[kinds[left]] != inventoryCoreKinds[kinds[right]] {
			return inventoryCoreKinds[kinds[left]]
		}
		return kinds[left] < kinds[right]
	})
}

type indexDefinitionContextKey struct{}
type inventoryDiscoveryContextKey struct{}
type inventoryNamespacesContextKey struct{}

func inventorySyncError() error { return errors.New("kubernetes: inventory is syncing") }

// inventoryKindLog remembers the last state reported for each kind so a problem
// is written to the server log once per state change, not once per cycle. It
// carries only kind names, state, class and counts — never a record name, label,
// value, payload, or credential.
//
// State is kept per (organization, cluster, credential) scope: services for
// different orgs on the same cluster share one logger, and one org's failure must
// never suppress or overwrite another org's line. The scope map is bounded, and
// dropping the oldest bookkeeping costs at most a repeated line.
type inventoryKindLog struct {
	mu     sync.Mutex
	scopes map[kubeindex.Scope]map[string]string
}

const (
	// maxInventoryLogScopes bounds the per-scope bookkeeping.
	maxInventoryLogScopes = 32
	// inventoryCycleLogKey is the entry that records the last cycle failure.
	inventoryCycleLogKey = "|cycle"
)

func newInventoryKindLog() *inventoryKindLog {
	return &inventoryKindLog{scopes: make(map[kubeindex.Scope]map[string]string)}
}

// entries returns the bookkeeping for one scope. Callers hold the lock.
func (state *inventoryKindLog) entries(scope kubeindex.Scope) map[string]string {
	if state.scopes == nil {
		state.scopes = make(map[kubeindex.Scope]map[string]string)
	}
	entries := state.scopes[scope]
	if entries == nil {
		if len(state.scopes) >= maxInventoryLogScopes {
			state.scopes = make(map[kubeindex.Scope]map[string]string)
		}
		entries = make(map[string]string)
		state.scopes[scope] = entries
	}
	return entries
}

// report writes one line for every kind whose reported state changed.
func (state *inventoryKindLog) report(scope kubeindex.Scope, kinds map[string]kubeindex.KindStatus) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	last := state.entries(scope)
	names := make([]string, 0, len(kinds))
	for kind := range kinds {
		names = append(names, kind)
	}
	sort.Strings(names)
	for _, kind := range names {
		status := kinds[kind]
		signature := status.State + "|" + status.Error + "|" + strconv.Itoa(status.Skipped) + "|" + strconv.FormatBool(status.Absent)
		if last[kind] == signature {
			continue
		}
		last[kind] = signature
		log.Print(inventoryKindLine(kind, status))
	}
}

// cycle writes one line when the inventory cycle itself fails. A cycle failure
// is not a kind: it names the class only.
func (state *inventoryKindLog) cycle(scope kubeindex.Scope, err error) {
	if state == nil || err == nil {
		return
	}
	class := errorClass(err)
	state.mu.Lock()
	defer state.mu.Unlock()
	last := state.entries(scope)
	if last[inventoryCycleLogKey] == class {
		return
	}
	last[inventoryCycleLogKey] = class
	log.Printf("kubernetes: inventory cycle failed class=%s", class)
}

// cycleSucceeded clears the cycle failure mark, so a failure that returns after a
// healthy cycle is reported again instead of staying silent.
func (state *inventoryKindLog) cycleSucceeded(scope kubeindex.Scope) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.entries(scope), inventoryCycleLogKey)
}

// inventoryKindLine renders a kind state without any of the objects it holds.
func inventoryKindLine(kind string, status kubeindex.KindStatus) string {
	line := "kubernetes: inventory kind=" + boundedLogToken(kind) + " state=" + boundedLogToken(status.State)
	if status.Error != "" {
		line += " class=" + boundedLogToken(status.Error)
	}
	if status.Skipped > 0 {
		line += " skipped=" + strconv.Itoa(status.Skipped)
	}
	if status.Absent {
		line += " absent=true"
	}
	return line + " records=" + strconv.Itoa(status.Records)
}

// boundedLogToken keeps a log field to one safe word so no value from a cluster
// object can shape the line.
func boundedLogToken(value string) string {
	if value == "" {
		return "unknown"
	}
	var token strings.Builder
	for _, r := range value {
		if token.Len() == 64 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			token.WriteRune(r)
			continue
		}
		token.WriteByte('_')
	}
	if token.Len() == 0 {
		return "unknown"
	}
	return token.String()
}

func (service *Service) logInventoryKinds(kinds map[string]kubeindex.KindStatus) {
	if service == nil {
		return
	}
	service.inventoryLog.report(service.indexScope(), kinds)
}

func (service *Service) logInventoryCycle(cause error) {
	if service == nil {
		return
	}
	service.inventoryLog.cycle(service.indexScope(), cause)
}

func (service *Service) clearInventoryCycleLog() {
	if service == nil {
		return
	}
	service.inventoryLog.cycleSucceeded(service.indexScope())
}

type inventoryNamespacePromotedContextKey struct{}
