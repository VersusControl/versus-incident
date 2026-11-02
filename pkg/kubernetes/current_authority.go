package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const inventoryAuthorityProbeInterval = time.Second
const inventoryAuthorityProbeRequests = 64

// Unverified authority is neither an authorization grant nor proof of universal
// denial. Keep protected reads retryable and unavailable until publication.
type inventoryAuthorityUnavailable struct{ cause error }

func (err *inventoryAuthorityUnavailable) Error() string {
	return "kubernetes: current inventory authority could not be verified"
}
func (err *inventoryAuthorityUnavailable) Unwrap() error { return ErrInventoryNotReady }

func unverifiedInventoryAuthority(cause error) error {
	return &inventoryAuthorityUnavailable{cause: cause}
}

type inventoryCollectionAuthorityKey struct{}
type inventoryCollectionAuthority struct {
	mu                sync.Mutex
	denied, witnessed time.Time
	invalid           error
}

func recordInventoryMaterialResult(ctx context.Context, err error) {
	current, _ := ctx.Value(inventoryCollectionAuthorityKey{}).(*inventoryCollectionAuthority)
	if current == nil {
		return
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	var invalid *inventoryListValidationError
	if errors.As(err, &invalid) || errors.Is(err, ErrIncompleteInventory) || errors.Is(err, ErrGraphIncomplete) || errors.Is(err, ErrInvalidArguments) {
		current.invalid = err
	}
	if err == nil {
		current.witnessed = time.Now()
	} else if errors.Is(err, ErrForbidden) || errorClass(err) == "forbidden" {
		current.denied = time.Now()
	}
}

func hasCurrentInventoryMaterialWitness(ctx context.Context) bool {
	current, _ := ctx.Value(inventoryCollectionAuthorityKey{}).(*inventoryCollectionAuthority)
	if current == nil {
		return false
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.invalid == nil && !current.witnessed.IsZero() && !current.witnessed.Before(current.denied)
}

func inventoryCollectionValidationError(ctx context.Context) error {
	current, _ := ctx.Value(inventoryCollectionAuthorityKey{}).(*inventoryCollectionAuthority)
	if current == nil {
		return nil
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.invalid
}

func collectionEnvelopeAuthorityError(ctx context.Context, cache *inventoryCache, materialDenied bool) error {
	invalid := inventoryCollectionValidationError(ctx)
	var envelope *inventoryListValidationError
	if !materialDenied || !errors.As(invalid, &envelope) {
		return nil
	}
	unavailable := unverifiedInventoryAuthority(invalid)
	cache.mu.Lock()
	cache.authorityFailure = unavailable
	cache.mu.Unlock()
	return unavailable
}

type inventoryAuthorityCall struct {
	binding, episode string
	abandoned        bool
	done             chan struct{}
	err              error
}

type inventoryAuthorityProofState struct {
	mu               sync.Mutex
	running          *inventoryAuthorityCall
	binding, episode string
	completed        bool
	result           error
	next             time.Time
	clock            func() time.Time
}

func (state *inventoryAuthorityProofState) now() time.Time {
	if state.clock != nil {
		return state.clock()
	}
	return time.Now()
}

// Positive results are reusable only for the same denial episode. In particular,
// a cooldown never turns an earlier positive into authority for a newer denial.
func (state *inventoryAuthorityProofState) verify(ctx context.Context, binding, episode string, probe func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state.mu.Lock()
		if call := state.running; call != nil {
			state.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-call.done:
			}
			if call.binding == binding && call.episode == episode {
				return call.err
			}
			continue
		}
		if state.completed && state.binding == binding && state.episode == episode {
			result := state.result
			state.mu.Unlock()
			return result
		}
		if state.now().Before(state.next) {
			result := unverifiedInventoryAuthority(ErrInventoryNotReady)
			if state.binding == binding && (errors.Is(state.result, ErrForbidden) || errors.Is(state.result, ErrUnauthorized)) {
				result = state.result
			}
			state.mu.Unlock()
			return result
		}
		call := &inventoryAuthorityCall{binding: binding, episode: episode, done: make(chan struct{})}
		state.running = call
		state.mu.Unlock()
		result := probe()
		state.mu.Lock()
		call.err = result
		state.running = nil
		state.binding, state.episode = binding, episode
		state.result = result
		state.completed = true
		state.next = state.now().Add(inventoryAuthorityProbeInterval)
		close(call.done)
		state.mu.Unlock()
		return result
	}
}

// A short scoped control-read cache coalesces concurrent UI polling without
// treating publication freshness as authorization for the next denial episode.
func (state *inventoryAuthorityProofState) verifyCurrentRead(ctx context.Context, binding string, probe func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state.mu.Lock()
		if call := state.running; call != nil {
			state.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-call.done:
			}
			if call.binding == binding {
				if call.abandoned {
					continue
				}
				return call.err
			}
			continue
		}
		if state.completed && state.binding == binding && state.now().Before(state.next) {
			result := state.result
			state.mu.Unlock()
			return result
		}
		call := &inventoryAuthorityCall{binding: binding, done: make(chan struct{})}
		state.running = call
		state.mu.Unlock()
		result := probe()
		state.mu.Lock()
		call.err = result
		call.abandoned = ctx.Err() != nil
		state.running = nil
		state.binding = binding
		state.result = result
		state.completed = ctx.Err() == nil
		state.next = state.now().Add(inventoryAuthorityProbeInterval)
		close(call.done)
		state.mu.Unlock()
		return result
	}
}

type inventoryAuthorityTarget struct {
	id, namespace string
	denied        bool
	metadataOnly  bool
	scopedWitness bool
}

func currentMaterialDenied(generation inventoryGeneration, attempt time.Time) bool {
	for _, state := range generation.View.Kinds {
		if state.LastAttemptAt.Equal(attempt) && state.Error == "forbidden" {
			return true
		}
	}
	for _, state := range generation.ResourceStatus {
		if state.LastAttemptAt.Equal(attempt) && state.Error == "forbidden" {
			return true
		}
	}
	for _, scope := range generation.Namespaces {
		for _, state := range scope.Kinds {
			if state.LastAttemptAt.Equal(attempt) && state.Error == "forbidden" {
				return true
			}
		}
	}
	return false
}

func currentCollectionIdentityVerified(generation inventoryGeneration, attempt time.Time) bool {
	state := generation.View.Kinds["Namespace"]
	uid := evidenceIdentity(generation)
	return uid != "" && state.LastAttemptAt.Equal(attempt) && (generation.EvidenceScope.ClusterUID == "" || generation.EvidenceScope.ClusterUID == uid)
}

// Live collection proves only its exact authorized LIST scope. Persisted
// restore and history continue to use their independent physical-identity gate.
func (service *Service) authorizeLiveInventoryTarget(ctx context.Context, target inventoryAuthorityTarget) error {
	parts := strings.Split(target.id, "~")
	if len(parts) != 3 {
		return ErrInvalidArguments
	}
	path := "/apis/" + parts[0] + "/" + parts[1]
	if parts[0] == "core" {
		path = "/api/" + parts[1]
	}
	if target.namespace != "" {
		path += "/namespaces/" + url.PathEscape(target.namespace)
	}
	path += "/" + parts[2] + "?limit=1"
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	var err error
	if target.metadataOnly {
		err = service.client.GetMetadataJSON(ctx, path, &validatedInventoryList{target: &page, metadataOnly: true})
	} else {
		err = service.client.GetJSON(ctx, path, &validatedInventoryList{target: &page})
	}
	if err != nil {
		return err
	}
	for _, raw := range page.Items {
		var item struct {
			Metadata struct {
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return ErrInvalidArguments
		}
		if target.namespace != "" && item.Metadata.Namespace != target.namespace {
			return ErrInvalidArguments
		}
	}
	return nil
}

// A witness only distinguishes partial capability loss from an unusable current
// read scope. It never changes any denied kind/namespace state to ready.
func (service *Service) inventoryAuthorityTargets(generation inventoryGeneration, requestedNamespaces []string) ([]inventoryAuthorityTarget, error) {
	definitions := map[string]ResourceDefinition{}
	preferred := map[string]ResourceDefinition{}
	if generation.Discovery != nil {
		for _, definition := range generation.Discovery.Resources {
			if !definition.Available {
				continue
			}
			definitions[definition.ID] = definition
			id := indexedResourceIDs[definition.Kind]
			parts := strings.Split(id, "~")
			if len(parts) != 3 {
				continue
			}
			group := parts[0]
			if group == "core" {
				group = ""
			}
			if definition.Group == group && definition.Resource == parts[2] {
				if _, ok := preferred[definition.Kind]; !ok {
					preferred[definition.Kind] = definition
				}
			}
		}
	}
	targets := map[string]inventoryAuthorityTarget{}
	add := func(id, namespace string, denied, metadataOnly bool) error {
		if namespace != "" && !safeSegment(namespace) {
			return ErrInvalidArguments
		}
		key := id + "\x00" + namespace
		target, exists := targets[key]
		if !exists {
			target = inventoryAuthorityTarget{id: id, namespace: namespace, denied: denied, metadataOnly: metadataOnly}
		} else {
			target.denied = target.denied && denied
			target.metadataOnly = target.metadataOnly || metadataOnly
		}
		targets[key] = target
		return nil
	}
	definitionFor := func(kind string) (ResourceDefinition, bool) {
		id := indexedResourceIDs[kind]
		parts := strings.Split(id, "~")
		if len(parts) != 3 {
			return ResourceDefinition{}, false
		}
		if definition, ok := preferred[kind]; ok {
			return definition, true
		}
		if generation.Discovery != nil {
			return ResourceDefinition{}, false
		}
		return ResourceDefinition{ID: id, Kind: kind, Namespaced: kind != "Node" && kind != "Namespace"}, true
	}
	for kind, state := range generation.View.Kinds {
		if state.Absent || state.ObservedAt.IsZero() && state.State != "error" {
			continue
		}
		definition, ok := definitionFor(kind)
		if !ok {
			continue
		}
		namespace := ""
		if definition.Namespaced {
			namespace = service.client.namespace
		}
		if err := add(definition.ID, namespace, state.Error == "forbidden", metadataOnlyIndexKinds[kind]); err != nil {
			return nil, err
		}
	}
	for id, state := range generation.ResourceStatus {
		if state.Absent || state.ObservedAt.IsZero() && state.State != "error" {
			continue
		}
		definition, ok := definitions[id]
		if !ok {
			continue
		}
		namespace := ""
		if definition.Namespaced {
			namespace = service.client.namespace
		}
		if err := add(id, namespace, state.Error == "forbidden", metadataOnlyIndexKinds[definition.Kind]); err != nil {
			return nil, err
		}
	}
	for _, namespace := range requestedNamespaces {
		if service.client.namespace != "" && namespace != service.client.namespace {
			continue
		}
		for kind, state := range generation.View.Kinds {
			if state.Absent {
				continue
			}
			definition, ok := definitionFor(kind)
			if !ok || !definition.Namespaced {
				continue
			}
			if err := add(definition.ID, namespace, state.Error == "forbidden", metadataOnlyIndexKinds[kind]); err != nil {
				return nil, err
			}
		}
	}
	for namespace, scope := range generation.Namespaces {
		if service.client.namespace != "" && namespace != service.client.namespace {
			continue
		}
		for kind, state := range scope.Kinds {
			if state.Absent || state.ObservedAt.IsZero() && state.State != "error" {
				continue
			}
			definition, ok := definitionFor(kind)
			if !ok || !definition.Namespaced {
				continue
			}
			if err := add(definition.ID, namespace, state.Error == "forbidden", metadataOnlyIndexKinds[kind]); err != nil {
				return nil, err
			}
			if state.State == "ready" && !state.Partial {
				key := definition.ID + "\x00" + namespace
				target := targets[key]
				target.scopedWitness = true
				targets[key] = target
			}
		}
	}
	result := make([]inventoryAuthorityTarget, 0, len(targets))
	for _, target := range targets {
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.scopedWitness != b.scopedWitness {
			return a.scopedWitness
		}
		if a.denied != b.denied {
			return !a.denied
		}
		return a.id+"\x00"+a.namespace < b.id+"\x00"+b.namespace
	})
	return result, nil
}

func (service *Service) verifyCurrentInventoryAuthority(ctx context.Context, cache *inventoryCache, generation inventoryGeneration, attempt time.Time) error {
	if hasCurrentInventoryMaterialWitness(ctx) {
		return service.validateCredential(ctx)
	}
	expected := generation.EvidenceScope.ClusterUID
	cache.mu.RLock()
	requestedNamespaces := make([]string, 0, len(cache.namespaces))
	for namespace := range cache.namespaces {
		requestedNamespaces = append(requestedNamespaces, namespace)
	}
	cache.mu.RUnlock()
	targets, err := service.inventoryAuthorityTargets(generation, requestedNamespaces)
	if err != nil || len(targets) == 0 {
		if err == nil {
			err = ErrIncompleteInventory
		}
		err = unverifiedInventoryAuthority(err)
		cache.mu.Lock()
		cache.authorityFailure = err
		cache.mu.Unlock()
		return err
	}
	// The result classifies usable authority in this read scope; it grants no
	// permissions to individual targets, whose denial states remain intact.
	binding := logIdentity(service.cacheKey() + "\x00" + expected + "\x00" + service.client.namespace)
	episode := attempt.UTC().Format(time.RFC3339Nano)
	err = cache.authorityProof.verify(ctx, binding, episode, func() error {
		pending := unverifiedInventoryAuthority(ErrInventoryNotReady)
		cache.mu.Lock()
		cache.authorityFailure = pending
		cache.mu.Unlock()
		if err := service.validateCredential(ctx); err != nil {
			return err
		}
		bound, cancelIdentity := service.credentialContext(ctx)
		defer cancelIdentity()
		proof, cancel := withOperationBudget(bound, 6*time.Second, inventoryAuthorityProbeRequests, 2<<20, 10000)
		defer cancel()
		// Permission proofs use the bounded control-read lane, as cold restores
		// do, rather than waiting behind the slow bulk-collection limiter.
		proof = context.WithValue(proof, captureReadContextKey{}, nil)
		exhausted := len(targets) > inventoryAuthorityProbeRequests
		if exhausted {
			targets = targets[:inventoryAuthorityProbeRequests]
		}
		for _, target := range targets {
			readErr := service.authorizeLiveInventoryTarget(proof, target)
			if readErr == nil {
				if err := service.validateCredential(proof); err != nil {
					return err
				}
				return nil
			}
			if errors.Is(readErr, ErrUnauthorized) {
				return ErrUnauthorized
			}
			if !errors.Is(readErr, ErrForbidden) {
				return unverifiedInventoryAuthority(readErr)
			}
		}
		if exhausted {
			return unverifiedInventoryAuthority(ErrOperationBudget)
		}
		return ErrForbidden
	})
	var unavailable *inventoryAuthorityUnavailable
	if errors.As(err, &unavailable) {
		cache.mu.Lock()
		cache.authorityFailure = unavailable
		cache.mu.Unlock()
	}
	return err
}
