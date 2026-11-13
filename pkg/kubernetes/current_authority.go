package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
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

// inventoryCollectionAuthority records what one collection cycle read
// successfully. Each kind or resource keeps the credential generation of its
// successful read, so the record is a set: the order in which concurrent LISTs
// finish never changes what it proves.
type inventoryCollectionAuthority struct {
	mu      sync.Mutex
	read    map[string]uint64
	invalid error
}

func recordInventoryMaterialResult(ctx context.Context, kind string, credential uint64, err error) {
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
		if current.read == nil {
			current.read = map[string]uint64{}
		}
		current.read[kind] = credential
	}
}

// hasCurrentInventoryMaterialWitness reports whether this cycle completed at
// least one valid LIST and no LIST answered with an invalid envelope.
func hasCurrentInventoryMaterialWitness(ctx context.Context) bool {
	current, _ := ctx.Value(inventoryCollectionAuthorityKey{}).(*inventoryCollectionAuthority)
	if current == nil {
		return false
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.invalid == nil && len(current.read) > 0
}

// hasCurrentInventoryRead reports whether this cycle read any kind
// successfully, whatever else it found.
func hasCurrentInventoryRead(ctx context.Context) bool {
	current, _ := ctx.Value(inventoryCollectionAuthorityKey{}).(*inventoryCollectionAuthority)
	if current == nil {
		return false
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	return len(current.read) > 0
}

// currentInventoryIdentityWitness reports whether this cycle proved its read
// authority by itself: the Namespace LIST matched the bound cluster identity
// and at least one other kind was read, both with the credential generation
// that is current now. It holds whatever order the LISTs finished in.
func currentInventoryIdentityWitness(ctx context.Context, generation inventoryGeneration, attempt time.Time, credential uint64) bool {
	current, _ := ctx.Value(inventoryCollectionAuthorityKey{}).(*inventoryCollectionAuthority)
	if current == nil || !currentCollectionIdentityVerified(generation, attempt) || generation.EvidenceScope.ClusterUID == "" {
		return false
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	if current.invalid != nil {
		return false
	}
	if at, ok := current.read["Namespace"]; !ok || at != credential {
		return false
	}
	for kind, at := range current.read {
		if kind != "Namespace" && at == credential {
			return true
		}
	}
	return false
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
	// holdProbe, when set, runs as a live proof begins. Tests use it to hold a
	// proof open and observe what readers see meanwhile.
	holdProbe atomic.Pointer[func()]
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

// inventoryAuthorityBaseline is what the last committed generation read under
// one binding: the connector identity, credential generation, cluster UID and
// namespace scope. A denial is new only against a baseline of the same binding.
type inventoryAuthorityBaseline struct {
	binding   string
	kinds     map[string]kubeindex.KindStatus
	resources map[string]kubeindex.KindStatus
	scoped    map[string]map[string]kubeindex.KindStatus
}

// inventoryAuthorityBinding names the identity a generation is collected
// under. A new credential generation (a rejected or replaced credential), a
// different cluster UID or namespace scope is a different binding.
func (service *Service) inventoryAuthorityBinding(clusterUID string) string {
	if service == nil || service.client == nil {
		return ""
	}
	return logIdentity(service.cacheKey() + "\x00" + strconv.FormatUint(service.credentialGeneration(), 10) + "\x00" + clusterUID + "\x00" + service.client.namespace)
}

// newInventoryAuthorityBaseline copies the kind states of a committed
// generation, so collection may rebuild its candidate maps freely. The
// baseline is empty unless the generation was committed under binding.
func newInventoryAuthorityBaseline(committedBinding, binding string, generation inventoryGeneration) inventoryAuthorityBaseline {
	if binding == "" || committedBinding != binding {
		return inventoryAuthorityBaseline{}
	}
	baseline := inventoryAuthorityBaseline{binding: binding, kinds: maps.Clone(generation.View.Kinds), resources: maps.Clone(generation.ResourceStatus)}
	if len(generation.Namespaces) > 0 {
		baseline.scoped = make(map[string]map[string]kubeindex.KindStatus, len(generation.Namespaces))
		for namespace, scope := range generation.Namespaces {
			baseline.scoped[namespace] = maps.Clone(scope.Kinds)
		}
	}
	return baseline
}

// readUnderBaseline reports whether the baseline holds a successful read of
// the kind in that scope: it was observed and was not denied when committed. A
// kind denied since its first sync, or still denied, is a capability gap. A
// cluster-wide kind read only inside namespaces carries namespace_scope: its
// cluster-wide LIST was denied, so that denial is a gap too.
func readUnderBaseline(states map[string]kubeindex.KindStatus, kind string) bool {
	state, ok := states[kind]
	return ok && !state.Absent && !state.ObservedAt.IsZero() && state.Error != "forbidden" && state.Error != "namespace_scope"
}

// inventoryQuestionedKinds are the kinds this cycle found newly denied. Until
// the authority proof settles, readers see them as unreadable rather than the
// whole cluster as unavailable.
type inventoryQuestionedKinds struct {
	kinds     map[string]bool
	resources map[string]bool
	scoped    map[string]map[string]bool
}

func (questioned inventoryQuestionedKinds) empty() bool {
	return len(questioned.kinds) == 0 && len(questioned.resources) == 0 && len(questioned.scoped) == 0
}

func (questioned inventoryQuestionedKinds) merge(other inventoryQuestionedKinds) inventoryQuestionedKinds {
	result := inventoryQuestionedKinds{kinds: maps.Clone(questioned.kinds), resources: maps.Clone(questioned.resources)}
	if result.kinds == nil {
		result.kinds = map[string]bool{}
	}
	if result.resources == nil {
		result.resources = map[string]bool{}
	}
	maps.Copy(result.kinds, other.kinds)
	maps.Copy(result.resources, other.resources)
	result.scoped = map[string]map[string]bool{}
	for _, source := range []map[string]map[string]bool{questioned.scoped, other.scoped} {
		for namespace, kinds := range source {
			if result.scoped[namespace] == nil {
				result.scoped[namespace] = map[string]bool{}
			}
			maps.Copy(result.scoped[namespace], kinds)
		}
	}
	return result
}

// newlyDeniedInventory lists the kinds that are forbidden in this attempt but
// were read successfully by the baseline. Without a baseline (first sync, or a
// binding that changed since the last commit) nothing is new: a new identity
// proves itself through the Namespace identity read instead.
func newlyDeniedInventory(baseline inventoryAuthorityBaseline, generation inventoryGeneration, attempt time.Time) inventoryQuestionedKinds {
	if baseline.binding == "" {
		return inventoryQuestionedKinds{}
	}
	return deniedInventory(generation, attempt, func(namespace string, resource bool, kind string) bool {
		switch {
		case resource:
			return readUnderBaseline(baseline.resources, kind)
		case namespace != "":
			return readUnderBaseline(baseline.scoped[namespace], kind)
		default:
			return readUnderBaseline(baseline.kinds, kind)
		}
	})
}

// unreadInventory lists every kind denied in this attempt. It applies when a
// cycle without a baseline read nothing at all: that is not a capability gap
// but possibly a connector without any permission, so its denials need proof.
func unreadInventory(generation inventoryGeneration, attempt time.Time) inventoryQuestionedKinds {
	return deniedInventory(generation, attempt, func(string, bool, string) bool { return true })
}

// deniedInventory lists the kinds forbidden in this attempt that include
// selects, by namespace scope ("" for cluster-wide), API resource or kind.
func deniedInventory(generation inventoryGeneration, attempt time.Time, include func(namespace string, resource bool, kind string) bool) inventoryQuestionedKinds {
	questioned := inventoryQuestionedKinds{}
	// Namespace LISTs stamp their own start time, which follows the cycle
	// start, so any attempt from the cycle start on belongs to this cycle.
	deniedNow := func(state kubeindex.KindStatus) bool {
		return !state.LastAttemptAt.Before(attempt) && state.Error == "forbidden"
	}
	for kind, state := range generation.View.Kinds {
		if deniedNow(state) && include("", false, kind) {
			if questioned.kinds == nil {
				questioned.kinds = map[string]bool{}
			}
			questioned.kinds[kind] = true
		}
	}
	for id, state := range generation.ResourceStatus {
		if deniedNow(state) && include("", true, id) {
			if questioned.resources == nil {
				questioned.resources = map[string]bool{}
			}
			questioned.resources[id] = true
		}
	}
	for namespace, scope := range generation.Namespaces {
		for kind, state := range scope.Kinds {
			if deniedNow(state) && include(namespace, false, kind) {
				if questioned.scoped == nil {
					questioned.scoped = map[string]map[string]bool{}
				}
				if questioned.scoped[namespace] == nil {
					questioned.scoped[namespace] = map[string]bool{}
				}
				questioned.scoped[namespace][kind] = true
			}
		}
	}
	return questioned
}

// currentMaterialDenied reports a denial that may mean lost read authority: a
// kind the same binding read before is forbidden now.
func currentMaterialDenied(baseline inventoryAuthorityBaseline, generation inventoryGeneration, attempt time.Time) bool {
	return !newlyDeniedInventory(baseline, generation, attempt).empty()
}

// questionInventoryKinds marks newly denied kinds unreadable in a reader's
// copy of the committed generation. Their records are withheld by the usual
// per-kind denial handling; every other kind stays as committed.
func questionInventoryKinds(generation inventoryGeneration, questioned inventoryQuestionedKinds) inventoryGeneration {
	if questioned.empty() {
		return generation
	}
	deny := func(state kubeindex.KindStatus) kubeindex.KindStatus {
		state.State, state.Partial, state.Error = "error", true, "forbidden"
		return state
	}
	if len(questioned.kinds) > 0 {
		kinds := maps.Clone(generation.View.Kinds)
		for kind := range questioned.kinds {
			if state, ok := kinds[kind]; ok {
				kinds[kind] = deny(state)
			}
		}
		generation.View.Kinds = kinds
	}
	if len(questioned.resources) > 0 {
		resources := maps.Clone(generation.ResourceStatus)
		for id := range questioned.resources {
			if state, ok := resources[id]; ok {
				resources[id] = deny(state)
			}
		}
		generation.ResourceStatus = resources
	}
	// A questioned cluster-wide kind is withheld in every namespace too:
	// readers fall back to a retained namespace read when the cluster-wide
	// kind is denied, and that read predates the denial in question.
	if len(questioned.scoped) > 0 || len(questioned.kinds) > 0 && len(generation.Namespaces) > 0 {
		scopes := make(map[string]kubeindex.Snapshot, len(generation.Namespaces))
		for namespace, scope := range generation.Namespaces {
			var states map[string]kubeindex.KindStatus
			for kind, state := range scope.Kinds {
				if questioned.kinds[kind] || questioned.scoped[namespace][kind] {
					if states == nil {
						states = maps.Clone(scope.Kinds)
					}
					states[kind] = deny(state)
				}
			}
			if states != nil {
				scope.Kinds = states
			}
			scopes[namespace] = scope
		}
		generation.Namespaces = scopes
	}
	return generation
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
	cache.authorityChecks.Add(1)
	if currentInventoryIdentityWitness(ctx, generation, attempt, service.credentialGeneration()) {
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
		// Readers keep the last committed generation while the proof runs;
		// only the kinds whose new denial is in question are withheld.
		if hold := cache.authorityProof.holdProbe.Load(); hold != nil {
			(*hold)()
		}
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

// credentialGeneration is the client's credential generation: it changes when a
// credential is rejected or replaced, which fences reads made before that.
func (service *Service) credentialGeneration() uint64 {
	if service == nil || service.client == nil {
		return 0
	}
	return service.client.authGeneration.Load()
}
