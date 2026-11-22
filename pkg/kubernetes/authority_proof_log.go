package kubernetes

import (
	"log/slog"
	"sort"
	"strings"
	"time"
)

// authorityProofWarnAfter is how many reads in a row of one kind must put its
// access in question before the operator is warned.
const authorityProofWarnAfter = 3

// authorityProofStreaks counts, per kind, the reads in a row whose denial put
// the cluster's access in question and so ran the authority proof. A correct
// role settles after one proof: the denial is committed and later reads see
// a steady gap. A kind that keeps triggering the proof means the permission
// keeps changing, or the proof cannot settle it, and each proof withholds the
// kind from readers while it runs. Only the inventory worker of one scope
// updates it, under the cache lock.
type authorityProofStreaks map[string]int

// noteAuthorityProofs records which kinds triggered the authority proof in
// one inventory cycle. A kind that triggered it extends its streak; a kind
// this cycle read without triggering it ends its streak; a kind the cycle did
// not read keeps its streak, since kinds are read on their own cadence. From
// the third read in a row the kind is warned about, throttled per cluster and
// kind; the end of a warned streak clears the warning at debug.
func (service *Service) noteAuthorityProofs(cache *inventoryCache, triggered, read map[string]bool) {
	cache.mu.Lock()
	if cache.proofStreaks == nil {
		cache.proofStreaks = authorityProofStreaks{}
	}
	warn, cleared := []string{}, []string{}
	for kind := range triggered {
		cache.proofStreaks[kind]++
		if cache.proofStreaks[kind] >= authorityProofWarnAfter {
			warn = append(warn, kind)
		}
	}
	for kind, streak := range cache.proofStreaks {
		if triggered[kind] || !read[kind] {
			continue
		}
		delete(cache.proofStreaks, kind)
		if streak >= authorityProofWarnAfter {
			cleared = append(cleared, kind)
		}
	}
	cache.mu.Unlock()
	sort.Strings(warn)
	for _, kind := range warn {
		name := kindLogName(kind)
		problems().Fail(slog.LevelWarn, service.authorityProofKey(kind), "access_rechecked",
			"kubernetes: cluster %q kind=%s puts the connector's access in question on every read (permission keeps changing); check that list on %s is granted steadily to the reader role and that nothing rewrites its role binding", service.scope.ClusterID, name, name)
	}
	for _, kind := range cleared {
		problems().Clear(service.authorityProofKey(kind), "kubernetes: cluster %q kind=%s access settled", service.scope.ClusterID, kindLogName(kind))
	}
}

// kindLogName renders a kind for a log line. A discovered API resource is
// named from the cluster's discovery answer, so each slash-separated segment
// is bounded to one safe word and no name can shape the line.
func kindLogName(kind string) string {
	segments := strings.Split(kind, "/")
	for i, segment := range segments {
		segments[i] = boundedLogToken(segment)
	}
	return strings.Join(segments, "/")
}

func (service *Service) authorityProofKey(kind string) string {
	return problemKey(service.scope.OrgID, service.scope.ClusterID, "authority_proof|"+kind)
}

// questionedKindNames names the kinds in a questioned set: cluster-wide and
// namespace-scoped kinds by kind, discovered API resources by their resource
// ID written with slashes.
func questionedKindNames(questioned inventoryQuestionedKinds, into map[string]bool) {
	for kind := range questioned.kinds {
		into[kind] = true
	}
	for id := range questioned.resources {
		into[resourceLogName(id)] = true
	}
	for _, kinds := range questioned.scoped {
		for kind := range kinds {
			into[kind] = true
		}
	}
}

// readInventoryKinds names the kinds this cycle attempted to read, in the
// same names questionedKindNames uses.
func readInventoryKinds(generation inventoryGeneration, attempt time.Time) map[string]bool {
	read := map[string]bool{}
	for kind, state := range generation.View.Kinds {
		if !state.LastAttemptAt.Before(attempt) {
			read[kind] = true
		}
	}
	for id, state := range generation.ResourceStatus {
		if !state.LastAttemptAt.Before(attempt) {
			read[resourceLogName(id)] = true
		}
	}
	for _, scope := range generation.Namespaces {
		for kind, state := range scope.Kinds {
			if !state.LastAttemptAt.Before(attempt) {
				read[kind] = true
			}
		}
	}
	return read
}

func resourceLogName(id string) string { return strings.ReplaceAll(id, "~", "/") }
