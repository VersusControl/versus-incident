package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

const namespaceCandidateLimit = 32
const namespaceCandidateTTL = time.Minute
const namespaceObservationTTL = 10 * time.Minute

type namespaceCandidate struct {
	State                        string
	Requested, Expires, Verified time.Time
}
type namespaceCandidateManifest struct {
	Entries                      map[string]namespaceCandidate
	AdmissionWindow, ProbeWindow time.Time
	Admitted, Probed             int
}

func (service *Service) namespaceCache() *inventoryCache {
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	cache := service.changes.inventories[service.indexScope()]
	if cache == nil {
		cache = &inventoryCache{namespaces: map[string]bool{}, wake: make(chan struct{}, 1)}
		service.changes.inventories[service.indexScope()] = cache
	}
	return cache
}

func (service *Service) editNamespaceCandidates(ctx context.Context, cache *inventoryCache, edit func(*namespaceCandidateManifest) string) (string, error) {
	provider := service.usageProvider()
	if provider == nil {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.candidates.Entries == nil {
			cache.candidates.Entries = map[string]namespaceCandidate{}
		}
		return edit(&cache.candidates), nil
	}
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return "namespace_hint_unavailable", ErrInvalidArguments
	}
	key := inventoryKey(service.scope) + "/namespace-candidates"
	for attempt := 0; attempt < 8; attempt++ {
		raw, err := provider.ReadBlob(key)
		if err != nil {
			return "namespace_hint_unavailable", err
		}
		var manifest namespaceCandidateManifest
		if len(raw) > 16<<10 || len(raw) > 0 && json.Unmarshal(raw, &manifest) != nil || len(manifest.Entries) > namespaceCandidateLimit {
			return "namespace_hint_unavailable", ErrInvalidArguments
		}
		if manifest.Entries == nil {
			manifest.Entries = map[string]namespaceCandidate{}
		}
		status := edit(&manifest)
		replacement, err := json.Marshal(manifest)
		if err != nil {
			return status, err
		}
		if len(raw) == 0 && len(manifest.Entries) == 0 && manifest.AdmissionWindow.IsZero() && manifest.ProbeWindow.IsZero() {
			return status, nil
		}
		if bytes.Equal(raw, replacement) {
			return status, nil
		}
		won := false
		err = service.commitCredential(ctx, func() error { var err error; won, err = cas.CompareAndSwapBlob(key, raw, replacement); return err })
		if err != nil {
			return "namespace_hint_unavailable", err
		}
		if won {
			return status, nil
		}
	}
	return "namespace_hint_rate_limited", ErrIndexStreamBusy
}

func pruneNamespaceCandidates(manifest *namespaceCandidateManifest, now time.Time) {
	for ns, candidate := range manifest.Entries {
		if !candidate.Expires.After(now) {
			delete(manifest.Entries, ns)
		}
	}
}

func namespaceObserved(generation inventoryGeneration, namespace string, now time.Time) bool {
	if at := generation.NamespaceProof[namespace]; !at.IsZero() && now.Sub(at) <= namespaceObservationTTL {
		return true
	}
	snapshot := generation.Namespaces[namespace]
	for _, record := range snapshot.Records {
		state := snapshot.Kinds[record.Kind]
		if record.Namespace == namespace && state.State == "ready" && !state.ObservedAt.IsZero() && now.Sub(state.ObservedAt) <= namespaceObservationTTL {
			return true
		}
	}
	return false
}

func (service *Service) queueInventoryNamespace(namespace string) string {
	return service.queueInventoryNamespaceContext(context.Background(), namespace)
}

func (service *Service) queueInventoryNamespaceContext(ctx context.Context, namespace string) string {
	if !safeSegment(namespace) || len(namespace) > 63 {
		return "namespace_hint_rejected"
	}
	cache := service.namespaceCache()
	now := time.Now().UTC()
	cache.mu.RLock()
	known := cache.namespaces[namespace] || namespaceObserved(cache.generation, namespace, now)
	cache.mu.RUnlock()
	if known {
		return ""
	}
	status, err := service.editNamespaceCandidates(ctx, cache, func(manifest *namespaceCandidateManifest) string {
		pruneNamespaceCandidates(manifest, now)
		if candidate, exists := manifest.Entries[namespace]; exists {
			switch candidate.State {
			case "rejected":
				return "namespace_hint_rejected"
			default:
				return "namespace_hint_pending"
			}
		}
		if now.Sub(manifest.AdmissionWindow) >= 10*time.Second {
			manifest.AdmissionWindow, manifest.Admitted = now, 0
		}
		if manifest.Admitted >= 2 || len(manifest.Entries) >= namespaceCandidateLimit {
			return "namespace_hint_rate_limited"
		}
		manifest.Admitted++
		manifest.Entries[strings.Clone(namespace)] = namespaceCandidate{State: "pending", Requested: now, Expires: now.Add(namespaceCandidateTTL)}
		return "namespace_hint_pending"
	})
	if err != nil {
		return status
	}
	if status == "namespace_hint_pending" {
		select {
		case cache.wake <- struct{}{}:
		default:
		}
	}
	return status
}

func (service *Service) discoverNamespaceCandidates(ctx context.Context, cache *inventoryCache, generation *inventoryGeneration, now time.Time) (bool, error) {
	selected := ""
	verified := map[string]time.Time{}
	_, err := service.editNamespaceCandidates(ctx, cache, func(manifest *namespaceCandidateManifest) string {
		selected = ""
		verified = map[string]time.Time{}
		pruneNamespaceCandidates(manifest, now)
		for ns, candidate := range manifest.Entries {
			if candidate.State == "pending" && namespaceObserved(*generation, ns, now) {
				candidate.State, candidate.Verified, candidate.Expires = "verified", now, now.Add(namespaceObservationTTL)
				manifest.Entries[ns] = candidate
			}
			if candidate.State == "verified" {
				verified[ns] = candidate.Verified
			}
		}
		pending := []string{}
		for ns, candidate := range manifest.Entries {
			if candidate.State == "pending" {
				pending = append(pending, ns)
			}
		}
		sort.Slice(pending, func(i, j int) bool {
			return manifest.Entries[pending[i]].Requested.Before(manifest.Entries[pending[j]].Requested)
		})
		if len(pending) == 0 {
			return ""
		}
		if now.Sub(manifest.ProbeWindow) >= 5*time.Second {
			manifest.ProbeWindow, manifest.Probed = now, 0
		}
		if manifest.Probed >= 1 {
			return ""
		}
		selected = pending[0]
		candidate := manifest.Entries[selected]
		candidate.State = "probing"
		manifest.Entries[selected] = candidate
		manifest.Probed++
		return ""
	})
	if err != nil {
		return false, err
	}
	if selected != "" {
		valid := service.probeNamespaceCandidate(ctx, selected)
		_, err = service.editNamespaceCandidates(ctx, cache, func(manifest *namespaceCandidateManifest) string {
			candidate, exists := manifest.Entries[selected]
			if !exists {
				return ""
			}
			if valid {
				candidate.State, candidate.Verified, candidate.Expires = "verified", now, now.Add(namespaceObservationTTL)
				verified[selected] = now
			} else {
				candidate.State, candidate.Expires = "rejected", now.Add(namespaceCandidateTTL)
			}
			manifest.Entries[selected] = candidate
			return ""
		})
		if err != nil {
			return false, err
		}
	}
	proofs := make(map[string]time.Time, len(generation.NamespaceProof)+len(verified))
	for ns, at := range generation.NamespaceProof {
		if now.Sub(at) <= namespaceObservationTTL {
			proofs[ns] = at
		}
	}
	changed := false
	for ns, at := range verified {
		if proofs[ns].IsZero() {
			changed = true
		}
		proofs[ns] = at
	}
	generation.NamespaceProof = proofs
	return changed, nil
}

func (service *Service) probeNamespaceCandidate(parent context.Context, namespace string) bool {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	var object struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if service.inventoryRead(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace), &object, false) == nil && object.Metadata.Name == namespace {
		return true
	}
	for _, source := range []struct{ prefix, resource string }{{"/api/v1/namespaces/", "pods"}, {"/apis/apps/v1/namespaces/", "deployments"}, {"/api/v1/namespaces/", "services"}} {
		var page struct {
			Items []struct {
				Metadata struct{ Name, Namespace string } `json:"metadata"`
			} `json:"items"`
		}
		if service.inventoryListRead(ctx, source.prefix+url.PathEscape(namespace)+"/"+source.resource+"?limit=1", &page, false) == nil {
			for _, item := range page.Items {
				if item.Metadata.Namespace == namespace && safeSegment(item.Metadata.Name) {
					return true
				}
			}
		}
	}

	return false
}

func recordNamespaceProofs(generation *inventoryGeneration) {
	proofs := make(map[string]time.Time, len(generation.NamespaceProof))
	for namespace, at := range generation.NamespaceProof {
		proofs[namespace] = at
	}
	for _, record := range generation.View.Records {
		namespace := record.Namespace
		if record.Kind == "Namespace" {
			namespace = record.Name
		}
		if namespace == "" {
			continue
		}
		state := generation.View.Kinds[record.Kind]
		if state.Error == "namespace_scope" {
			state = generation.Namespaces[namespace].Kinds[record.Kind]
		}
		if state.Error == "forbidden" || state.Error == "unauthorized" || state.ObservedAt.IsZero() {
			continue
		}
		if proofs[namespace].Before(state.ObservedAt) {
			proofs[namespace] = state.ObservedAt
		}
	}
	generation.NamespaceProof = proofs
}
