package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

var errHistoryUnavailable = errors.New("kubernetes: persisted history store unavailable")
var errHistoryTimeout = errors.New("kubernetes: persisted history store timed out")
var errHistoryUpgrading = errors.New("kubernetes: persisted history read models are converting")
var errHistoryAuthority = errors.Join(errors.New("kubernetes: persisted history authority unavailable"), ErrForbidden)
var errHistoryMismatch = errors.New("kubernetes: persisted occurrence identity mismatch")
var errHistoryNotEstablished = errors.New("kubernetes: persisted history authority not established")

// SetEvidenceHeartbeat configures all storage backends before capture starts.
func (registry *ServiceRegistry) SetEvidenceHeartbeat(value string) error {
	duration := 5 * time.Minute
	if value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed < time.Second || parsed > 24*time.Hour {
			return ErrInvalidArguments
		}
		duration = parsed
	}
	if registry == nil {
		return ErrInvalidArguments
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.captureContext != nil {
		return ErrInvalidArguments
	}
	for _, entry := range registry.entries {
		if entry.Service != nil {
			entry.Service.evidenceHeartbeat = duration
		}
	}
	for _, service := range registry.services {
		service.evidenceHeartbeat = duration
	}
	if registry.base != nil {
		registry.base.evidenceHeartbeat = duration
	}
	return nil
}

func (service *Service) historyPrincipal() string {
	if service == nil || service.identity == nil {
		return ""
	}
	epoch := service.identity
	if epoch.ctx.Err() != nil {
		return ""
	}
	if service.client != nil && service.client.credentials == nil {
		endpoint := ""
		if service.client.base != nil {
			endpoint = service.client.base.String()
		}
		return logIdentity("anonymous\x00" + endpoint)
	}
	value, _ := epoch.history.Load().(string)
	return value
}

func (service *Service) historyScopeKey() string {
	endpoint := ""
	if service.client != nil && service.client.base != nil {
		endpoint = service.client.base.String()
	}
	return "kubernetes/history-scope/" + logIdentity(service.scope.OrgID+"\x00"+service.scope.ClusterID+"\x00"+endpoint+"\x00"+service.configuredCredentialID)
}

// The pointer contains no protected objects. Physical identity and authority
// remain in the committed scope checkpoint, never inferred from a cluster name.
func (service *Service) rememberHistoryScope(scope storage.EvidenceScope) error {
	if scope.OrgID != service.scope.OrgID || scope.ClusterID != service.scope.ClusterID || scope.ClusterUID == "" {
		return errHistoryAuthority
	}
	provider := service.usageProvider()
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return storage.ErrUnsupported
	}
	scope.ReadOrgs = nil
	raw, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	key := service.historyScopeKey()
	for attempt := 0; attempt < 4; attempt++ {
		previous, err := provider.ReadBlob(key)
		if err != nil {
			return err
		}
		if string(previous) == string(raw) {
			return nil
		}
		won, err := cas.CompareAndSwapBlob(key, previous, raw)
		if err != nil || won {
			return err
		}
	}
	return storage.ErrEvidenceConflict
}

func (service *Service) persistedHistoryScope(ctx context.Context) (storage.EvidenceScope, storage.EvidenceCheckpoint, error) {
	ctx = storage.EvidenceReadContext(ctx)
	if service.identity != nil && service.identity.ctx.Err() != nil {
		return storage.EvidenceScope{}, storage.EvidenceCheckpoint{}, errHistoryAuthority
	}
	store := service.evidenceStore()
	if store == nil {
		return storage.EvidenceScope{}, storage.EvidenceCheckpoint{}, storage.ErrUnsupported
	}
	scope, err := service.storedHistoryScope(ctx, store)
	if errors.Is(err, errHistoryNotEstablished) {
		err = service.unestablishedHistoryError()
	}
	if err != nil {
		return scope, storage.EvidenceCheckpoint{}, err
	}
	scope.ReadOrgs = append([]string(nil), service.readOrgs...)
	checkpoint, err := store.EvidenceCheckpoint(ctx, scope)
	if err != nil {
		return scope, checkpoint, historyStoreError(err)
	}
	principal := service.historyPrincipal()
	if checkpoint.ID == "" || checkpoint.Principal == "" {
		return scope, checkpoint, service.unestablishedHistoryError()
	}
	if principal == "" {
		return scope, checkpoint, service.unestablishedHistoryError()
	}
	if checkpoint.Principal != principal {
		return scope, checkpoint, errHistoryAuthority
	}
	service.overlayHistoryDenials(scope, principal, &checkpoint)
	return scope, checkpoint, nil
}

// Scope resolution alone grants no read authority. Denial persistence must be
// able to resolve the committed scope even after a credential has been revoked.
func (service *Service) storedHistoryScope(ctx context.Context, store storage.EvidenceStore) (storage.EvidenceScope, error) {
	scope := storage.EvidenceScope{}
	// Published generations are immutable. This path does not validate a
	// bearer, restore inventory, bind an epoch, or start a collection.
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	if cache != nil {
		cache.mu.RLock()
		scope = cache.generation.EvidenceScope
		cache.mu.RUnlock()
	}
	if scope.ClusterUID == "" {
		if lookup, ok := store.(storage.EvidenceScopeLookup); ok {
			committed, err := lookup.FindEvidenceScope(ctx, service.scope.OrgID, service.scope.ClusterID, service.historyScopeKey())
			if err == nil {
				scope = committed
			} else if !errors.Is(err, storage.ErrUnsupported) {
				if errors.Is(err, storage.ErrNotFound) {
					return scope, errHistoryNotEstablished
				}
				return scope, historyStoreError(err)
			}
		}
	}
	if scope.ClusterUID == "" {
		raw, err := service.usageProvider().ReadBlob(service.historyScopeKey())
		if err != nil {
			return scope, historyStoreError(err)
		}
		if len(raw) == 0 {
			return scope, errHistoryNotEstablished
		}
		if json.Unmarshal(raw, &scope) != nil || scope.OrgID != service.scope.OrgID || scope.ClusterID != service.scope.ClusterID || scope.ClusterUID == "" {
			return storage.EvidenceScope{}, errHistoryAuthority
		}
	}
	if scope.OrgID != service.scope.OrgID || scope.ClusterID != service.scope.ClusterID {
		return storage.EvidenceScope{}, errHistoryAuthority
	}
	return scope, nil
}

// Before a first commit, positive denials still outrank a retryable absence of
// proof. This consults only local authority, never the cluster.
func (service *Service) unestablishedHistoryError() error {
	if service.identity != nil && service.identity.ctx.Err() != nil {
		return errHistoryAuthority
	}
	cp := storage.EvidenceCheckpoint{}
	scope := storage.EvidenceScope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID}
	service.overlayHistoryDenials(scope, service.historyPrincipal(), &cp)
	for _, proof := range cp.Authority {
		if proof.State == "denied" {
			return errHistoryAuthority
		}
	}
	return errHistoryNotEstablished
}

func historyStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrEvidenceNotReady) {
		return errors.Join(errHistoryUpgrading, err)
	}
	var state interface{ SQLState() string }
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &state) && state.SQLState() == "57014" {
		return errors.Join(errHistoryTimeout, err)
	}
	return errors.Join(errHistoryUnavailable, err)
}

func (service *Service) persistedHistoryQuery(ctx context.Context, query storage.EvidenceQuery) (storage.EvidencePage, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	scope, checkpoint, err := service.persistedHistoryScope(ctx)
	if err != nil {
		return storage.EvidencePage{}, err
	}
	authority := "current"
	query.EnforceAuthority = true
	namespace := query.Namespace
	if query.AuthorityNamespace != "" {
		namespace = query.AuthorityNamespace
	}
	kinds := map[string]bool{}
	query.Authorities = checkpoint.Authority
	for key := range checkpoint.Authority {
		kind, _, _ := strings.Cut(key, "@")
		kinds[kind] = true
	}
	for kind := range kinds {
		readable := false
		for key, candidate := range checkpoint.Authority {
			source, _, _ := strings.Cut(key, "@")
			if source != kind {
				continue
			}
			proofNamespace := namespace
			if namespace == "" {
				proofNamespace = candidate.Namespace
			}
			proof := effectiveHistoryProof(checkpoint, kind, proofNamespace)
			if proof.State != "allowed" {
				continue
			}
			readable = true
			if time.Since(proof.ProvedAt) > 2*inventoryCadence(kind) {
				authority = "stale"
			}
		}
		if readable {
			query.AllowedKinds = append(query.AllowedKinds, kind)
		}
	}
	sort.Strings(query.AllowedKinds)
	if len(query.AllowedKinds) == 0 {
		for _, proof := range checkpoint.Authority {
			if proof.State == "denied" {
				return storage.EvidencePage{}, errHistoryAuthority
			}
		}
		return storage.EvidencePage{}, errHistoryNotEstablished
	}
	page, err := service.evidenceStore().QueryEvidence(ctx, scope, query)
	if errors.Is(err, storage.ErrInvalidEvidence) || errors.Is(err, storage.ErrEvidenceConflict) {
		return page, errors.Join(ErrInvalidArguments, err)
	}
	if err != nil {
		return page, historyStoreError(err)
	}
	// A principal may retire during a database read. No old-principal payload
	// escapes that race; transient refresh failures do not retire an epoch.
	if service.historyPrincipal() != checkpoint.Principal {
		return storage.EvidencePage{}, errHistoryAuthority
	}
	latest, err := service.evidenceStore().EvidenceCheckpoint(storage.EvidenceReadContext(ctx), scope)
	if err != nil {
		return storage.EvidencePage{}, historyStoreError(err)
	}
	if latest.Principal != checkpoint.Principal {
		return storage.EvidencePage{}, errHistoryAuthority
	}
	service.overlayHistoryDenials(scope, checkpoint.Principal, &latest)
	if historyAuthorityNarrowed(checkpoint.Authority, latest.Authority, namespace) {
		return storage.EvidencePage{}, errHistoryAuthority
	}
	for i, record := range page.Items {
		page.Items[i] = service.projectEvidence(record)
	}
	page.Authority = authority
	return page, nil
}

// SubscribeCommitNotifications is a bounded, process-local wakeup lane.
// Checkpoints remain the durable authority, including across replicas/restarts.
func (service *Service) SubscribeCommitNotifications() (<-chan struct{}, func(), error) {
	state := service.changes
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.commitSubscribers == nil {
		state.commitSubscribers = map[string]map[chan struct{}]struct{}{}
	}
	total := 0
	for _, subscribers := range state.commitSubscribers {
		total += len(subscribers)
	}
	if total >= 32 {
		return nil, nil, ErrIndexStreamBusy
	}
	key := service.scope.OrgID + "\x00" + service.scope.ClusterID
	if state.commitSubscribers[key] == nil {
		state.commitSubscribers[key] = map[chan struct{}]struct{}{}
	}
	wakeup := make(chan struct{}, 1)
	state.commitSubscribers[key][wakeup] = struct{}{}
	return wakeup, func() {
		state.mu.Lock()
		delete(state.commitSubscribers[key], wakeup)
		if len(state.commitSubscribers[key]) == 0 {
			delete(state.commitSubscribers, key)
		}
		state.mu.Unlock()
	}, nil
}

func (service *Service) notifyEvidenceCommit() {
	state := service.changes
	state.mu.Lock()
	defer state.mu.Unlock()
	for wakeup := range state.commitSubscribers[service.scope.OrgID+"\x00"+service.scope.ClusterID] {
		select {
		case wakeup <- struct{}{}:
		default:
		}
	}
}

// CommitStatus reads only the committed checkpoint. It is also used to recover
// dropped notifications and commits made by another process.
func (service *Service) CommitStatus(ctx context.Context) (CommitEvent, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, cp, err := service.persistedHistoryScope(ctx)
	if err != nil {
		return CommitEvent{}, err
	}
	readable, denied := false, false
	for key, proof := range cp.Authority {
		kind, _, _ := strings.Cut(key, "@")
		readable = readable || storage.EvidenceAuthorityAllows(cp.Authority, kind, proof.Namespace)
		denied = denied || proof.State == "denied"
	}
	if !readable {
		if denied {
			return CommitEvent{}, errHistoryAuthority
		}
		return CommitEvent{}, errHistoryNotEstablished
	}
	return CommitEvent{Revision: max(cp.CommitRevision, cp.OccurrenceRevision), EvidenceSequence: cp.HighSequence, Kinds: append([]string{}, cp.Kinds...), ClusterID: service.scope.ClusterID, At: cp.At}, nil
}

type CommitEvent struct {
	Revision         uint64    `json:"revision"`
	EvidenceSequence uint64    `json:"evidence_sequence"`
	Kinds            []string  `json:"kinds"`
	ClusterID        string    `json:"cluster_id"`
	At               time.Time `json:"-"`
}

// persistedPodAuthority protects saved log excerpts without making a live LIST.
// A later native/linked source read still uses that source's own authentication.
func (service *Service) persistedPodAuthority(ctx context.Context, namespace string) error {
	_, cp, err := service.persistedHistoryScope(ctx)
	if err != nil {
		return err
	}
	proof := effectiveHistoryProof(cp, "Pod", namespace)
	if proof.State != "allowed" {
		return errHistoryAuthority
	}
	return nil
}

func (service *Service) authorizeBreakLogHistory(ctx context.Context, namespace string) error {
	if service.evidenceStore() != nil {
		err := service.persistedPodAuthority(ctx, namespace)
		if errors.Is(err, errHistoryAuthority) || errors.Is(err, errHistoryNotEstablished) {
			clearLinkedCache()
		}
		return err
	}
	if err := service.validateCredential(ctx); err != nil {
		clearLinkedCache()
		return err
	}
	err := service.authorizeEvidenceKind(ctx, namespace, "core~v1~pods")
	if err != nil {
		clearLinkedCache()
	}
	return err
}

func historyProofs(generation inventoryGeneration) map[string]storage.EvidenceAuthority {
	proofs := map[string]storage.EvidenceAuthority{}
	for kind, state := range generation.View.Kinds {
		switch {
		case state.Error == "forbidden" || state.Error == "unauthorized" || state.Error == "namespace_scope":
			proofs[kind] = storage.EvidenceAuthority{State: "denied", ProvedAt: state.LastAttemptAt}
		case state.State == "ready" && !state.Absent && !state.ObservedAt.IsZero():
			proofs[kind] = storage.EvidenceAuthority{State: "allowed", ProvedAt: state.ObservedAt}
		}
	}
	// Namespace-fallback snapshots prove only their actual namespace. Never
	// promote a union of namespaced LISTs to all-namespaces authority.
	for namespace, snapshot := range generation.Namespaces {
		for kind, state := range snapshot.Kinds {
			if state.State == "ready" && !state.Absent && !state.ObservedAt.IsZero() && proofs[kind].State != "allowed" {
				proofs[kind+"@"+namespace] = storage.EvidenceAuthority{State: "allowed", Namespace: namespace, ProvedAt: state.ObservedAt}
			} else if state.Error == "forbidden" || state.Error == "unauthorized" {
				proofs[kind+"@"+namespace] = storage.EvidenceAuthority{State: "denied", Namespace: namespace, ProvedAt: state.LastAttemptAt}
			}
		}
	}
	return proofs
}

func (service *Service) denyHistoryAuthority(ctx context.Context, path string, unauthorized bool) {
	bare := strings.Split(path, "?")[0]
	wholeScope := unauthorized || bare == "/api" || bare == "/apis" || bare == "/api/v1/namespaces/kube-system"
	store, ok := service.usageProvider().(storage.EvidenceAuthorityStore)
	if !ok || service.evidenceStore() == nil {
		return
	}
	principal, _ := service.identity.history.Load().(string)
	if service.client.credentials == nil {
		endpoint := ""
		if service.client.base != nil {
			endpoint = service.client.base.String()
		}
		principal = logIdentity("anonymous\x00" + endpoint)
	}
	if principal == "" {
		return
	}
	proofs := map[string]storage.EvidenceAuthority{}
	now := time.Now().UTC()
	for kind, id := range indexedResourceIDs {
		resource := strings.Split(id, "~")
		if wholeScope {
			proofs[kind] = storage.EvidenceAuthority{State: "denied", ProvedAt: now}
			continue
		}
		if len(resource) != 3 {
			continue
		}
		base := "/apis/" + resource[0] + "/" + resource[1] + "/"
		if resource[0] == "core" {
			base = "/api/" + resource[1] + "/"
		}
		tail, match := strings.CutPrefix(strings.Split(path, "?")[0], base)
		if !match {
			continue
		}
		namespace := ""
		if tail != resource[2] {
			parts := strings.Split(tail, "/")
			if len(parts) != 3 || parts[0] != "namespaces" || parts[2] != resource[2] || !safeSegment(parts[1]) {
				continue
			}
			namespace = parts[1]
		}
		key := kind
		if namespace != "" {
			key += "@" + namespace
		}
		proofs[key] = storage.EvidenceAuthority{State: "denied", Namespace: namespace, ProvedAt: now}
	}
	if len(proofs) == 0 {
		return
	}
	scope := storage.EvidenceScope{OrgID: service.scope.OrgID, ClusterID: service.scope.ClusterID}
	service.rememberHistoryDenials(scope, principal, proofs)
	denialCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	if cache != nil {
		cache.mu.RLock()
		scope = cache.generation.EvidenceScope
		cache.mu.RUnlock()
	}
	if scope.ClusterUID == "" {
		resolved, err := service.storedHistoryScope(storage.EvidenceReadContext(denialCtx), service.evidenceStore())
		if err != nil {
			return
		}
		scope = resolved
	}
	if scope.OrgID != service.scope.OrgID || scope.ClusterID != service.scope.ClusterID || scope.ClusterUID == "" {
		return
	}
	scope.ReadOrgs = nil
	if err := store.SetEvidenceAuthority(denialCtx, scope, principal, proofs); err != nil {
		log.Printf("kubernetes: persisted history denial deferred")
	} else {
		service.notifyEvidenceCommit()
	}
}

func historyDenialKey(scope storage.EvidenceScope, principal string) string {
	return scope.OrgID + "\x00" + scope.ClusterID + "\x00" + scope.ClusterUID + "\x00" + principal
}

func (service *Service) rememberHistoryDenials(scope storage.EvidenceScope, principal string, proofs map[string]storage.EvidenceAuthority) {
	state := service.changes
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.historyDenials == nil {
		state.historyDenials = map[string]map[string]storage.EvidenceAuthority{}
	}
	key := historyDenialKey(scope, principal)
	if state.historyDenials[key] == nil {
		state.historyDenials[key] = map[string]storage.EvidenceAuthority{}
	}
	for kind, proof := range proofs {
		if !proof.ProvedAt.Before(state.historyDenials[key][kind].ProvedAt) {
			state.historyDenials[key][kind] = proof
		}
	}
}

func (service *Service) overlayHistoryDenials(scope storage.EvidenceScope, principal string, cp *storage.EvidenceCheckpoint) {
	state := service.changes
	state.mu.Lock()
	defer state.mu.Unlock()
	if cp.Authority == nil {
		cp.Authority = map[string]storage.EvidenceAuthority{}
	}
	wildcard := scope
	wildcard.ClusterUID = ""
	for _, key := range []string{historyDenialKey(scope, principal), historyDenialKey(wildcard, principal)} {
		denials := state.historyDenials[key]
		for kind, proof := range denials {
			if cp.Authority[kind].ProvedAt.After(proof.ProvedAt) {
				delete(denials, kind)
				continue
			}
			cp.Authority[kind] = proof
		}
	}
}

func historyAuthorityNarrowed(before, after map[string]storage.EvidenceAuthority, namespace string) bool {
	keys := map[string]struct{}{}
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	for key := range keys {
		kind, scoped, _ := strings.Cut(key, "@")
		if namespace != "" {
			scoped = namespace
		}
		if storage.EvidenceAuthorityAllows(before, kind, scoped) && !storage.EvidenceAuthorityAllows(after, kind, scoped) {
			return true
		}
	}
	return false
}

func effectiveHistoryProof(cp storage.EvidenceCheckpoint, kind, namespace string) storage.EvidenceAuthority {
	global := cp.Authority[kind]
	if namespace != "" {
		if scoped, ok := cp.Authority[kind+"@"+namespace]; ok && (scoped.ProvedAt.After(global.ProvedAt) || scoped.ProvedAt.Equal(global.ProvedAt) && scoped.State != "allowed") {
			return scoped
		}
	}
	return global
}
