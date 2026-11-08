package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// recoveryScope is one read authority's view of the shared provider. The provider
// is shared per cluster, but a denial belongs to the identity that answered it, so
// one scope's denial or cooldown never stands in for another scope's answer.
type recoveryScope struct {
	next   time.Time
	denied bool
}

// clusterRecovery survives credential epochs. Neither request traffic nor a
// successor's construction is evidence of recovery; only publication resets it.
type clusterRecovery struct {
	mu                sync.Mutex
	failures          int
	replacementStreak int
	scopes            map[string]*recoveryScope
	next              time.Time
	lastCommitted     time.Time
	revision          string
	cancel            context.CancelFunc
	done              chan struct{}
	stages            map[string]recoveryStage
	workers           int
	firstReady        map[string]time.Time
	attempts          uint64
}

func (r *clusterRecovery) deadline() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

// requestGate reports whether this org read scope may not serve a request yet. A
// recorded provider failure in the scope gates request traffic because accepted
// material deliberately outlives it. A permission denial in the scope does not:
// the API server answered, so the read path reports that denial instead of hiding
// it behind unavailability. The empty key is the credential itself, whose answer
// applies to every scope.
func (r *clusterRecovery) requestGate(orgID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	credential, scope := r.scopes[""], r.scopes[orgID]
	if credential != nil && credential.denied || scope != nil && scope.denied {
		return false
	}
	now := time.Now()
	if credential != nil && now.Before(credential.next) {
		return true
	}
	return scope != nil && now.Before(scope.next)
}

// gate applies the scope's decision to the shared retry schedule.
func (r *clusterRecovery) gate(orgID string, now time.Time, denied bool) {
	if r.scopes == nil {
		r.scopes = map[string]*recoveryScope{}
	}
	scope := r.scopes[orgID]
	if scope == nil {
		scope = &recoveryScope{}
		r.scopes[orgID] = scope
	}
	if denied {
		scope.denied = true
	}
	scope.next = maxTime(scope.next, now)
}

// recovered clears the per-scope gates: a committed publication proves the
// identity could read the cluster again.
func (r *clusterRecovery) recovered() {
	for _, scope := range r.scopes {
		scope.next, scope.denied = time.Time{}, false
	}
}

// answerDenied reports whether the API server answered that this identity may not
// read the cluster. Collection reports an answered denial through wrappers whose
// own error is a not-ready state, so the classification looks through them: a
// denial buried behind a wrapper must still never be reported as unavailability.
func answerDenied(err error) bool {
	if err == nil {
		return false
	}
	// Collection reports an unverified authority through a wrapper whose own error
	// is a not-ready state, so the cause decides what the API server answered.
	var unverified *inventoryAuthorityUnavailable
	if errors.As(err, &unverified) {
		return answerDenied(unverified.cause)
	}
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthorized) {
		return true
	}
	switch errorClass(err) {
	case "forbidden", "unauthorized":
		return true
	}
	return false
}

func (r *clusterRecovery) failed(err error, client *Client, orgID string) {
	if r == nil {
		return
	}
	// An answered denial is recorded before anything else, because concurrent
	// failures from a single attempt must not drop the one fact that has to reach
	// the reader: the API server refused this identity.
	denied := answerDenied(err)
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	// Concurrent failures from a single attempt do not multiply the streak, but the
	// scope gate still follows every recorded failure.
	if now.Before(r.next) && r.failures > 0 {
		r.gate(orgID, r.next, denied)
		return
	}
	r.failures++
	delay := time.Second * time.Duration(1<<min(r.failures-1, 6))
	delay += time.Duration(rand.Int63n(int64(delay/4 + 1)))
	r.next = maxTime(r.next, now.Add(delay))
	if client != nil {
		client.backoffMu.Lock()
		r.next = maxTime(r.next, client.backoffUntil)
		client.backoffMu.Unlock()
	}
	r.gate(orgID, r.next, denied)
	log.Printf("kubernetes: recovery stage=credential_recovery reason=%s attempt=%d retry_ms=%d", recoveryReason(err, "credential_recovery"), r.failures, time.Until(r.next).Milliseconds())
}

func (r *clusterRecovery) committed(revision string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures, r.next, r.lastCommitted, r.revision = 0, time.Time{}, time.Now().UTC(), revision
	r.replacementStreak = 0
	r.recovered()
}

func waitRecovery(ctx context.Context, r *clusterRecovery) error {
	for {
		delay := time.Until(r.deadline())
		if delay <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (registry *ServiceRegistry) superviseCluster(ctx context.Context, clusterID string, recovery *clusterRecovery) {
	defer close(recovery.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := waitRecovery(ctx, recovery); err != nil {
			return
		}
		registry.mu.Lock()
		var current *Service
		for _, entry := range registry.entries {
			if entry.Info.ID == clusterID {
				current = entry.Service
				break
			}
		}
		registry.mu.Unlock()
		if current == nil {
			return
		}
		// A producer exit is not itself a credential failure. Once its joined
		// done channel closes, restart in the same still-authorized scope and keep
		// the committed generation. Credential retirement takes the separate path.
		current.changes.mu.Lock()
		cache := current.changes.inventories[current.indexScope()]
		stopped := current.changes.stopped[current.indexScope()]
		restart := false
		if cache != nil && cache.done != nil && !stopped && current.identity.ctx.Err() == nil {
			select {
			case <-cache.done:
				cache.started = false
				restart = true
			default:
			}
		}
		current.changes.mu.Unlock()
		if stopped {
			return
		}
		if restart {
			current.startInventory(ctx)
			recovery.stage("worker_recovery", time.Now(), nil)
		}

		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := registry.resolveCluster(attempt, current.scope.OrgID, clusterID)
		cancel()
		if err != nil && ctx.Err() == nil {
			recovery.failed(err, current.client, current.scope.OrgID)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *clusterRecovery) published(generation inventoryGeneration, orgID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !generation.FirstReadyAt.IsZero() {
		if r.firstReady == nil {
			r.firstReady = map[string]time.Time{}
		}
		if r.firstReady[orgID].IsZero() {
			r.firstReady[orgID] = generation.FirstReadyAt
		}
	}
	fresh := false
	for _, state := range generation.View.Kinds {
		if state.State == "ready" && !state.Partial && state.CompletedAt.After(r.lastCommitted) {
			fresh = true
			break
		}
	}
	if !fresh {
		return
	}
	r.failures, r.next, r.lastCommitted, r.revision = 0, time.Time{}, time.Now().UTC(), generation.Revision
	r.replacementStreak = 0
	r.recovered()
}

type recoveryStage struct {
	reason string
	at     time.Time
}

// captureStorageError marks a failure saving captured state. The cluster was
// read successfully; only storage failed, so it never counts against the
// credential or the cluster. A rejected save is one the store refused as
// unsupported, oversized or invalid: retrying the same capture cannot succeed.
type captureStorageError struct {
	stage    string
	cause    error
	rejected bool
}

func (err *captureStorageError) Error() string {
	return "kubernetes: capture storage " + err.stage + " failed"
}

func (err *captureStorageError) Unwrap() error { return err.cause }

// storageStageFailure wraps a storage-stage error unless it is a lease,
// credential, permission or cancellation outcome, which keep their meaning. A
// journal conflict is the store's lease fence answering that another owner
// moved the inventory, so it is a lease outcome too.
func storageStageFailure(stage string, err error) error {
	var credential *credentialFailure
	if err == nil || errors.As(err, &credential) || errors.Is(err, errInventoryLeaseLost) || errors.Is(err, kubechanges.ErrLeaseLost) ||
		errors.Is(err, storage.ErrEvidenceConflict) ||
		errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) ||
		errors.Is(err, context.Canceled) {
		return err
	}
	return &captureStorageError{stage: stage, cause: err, rejected: historySaveRejected(err)}
}

// countsAgainstCredential reports whether a capture cycle failure may close the
// credential recovery gate, which every reader is told as an unresolved
// connector credential. Only a typed credential failure, a credential answer
// (unavailable or rejected) or an answered permission denial counts; storage,
// lease, deadline, cancellation, contention and unknown failures never do.
func countsAgainstCredential(err error) bool {
	var failure *credentialFailure
	return errors.As(err, &failure) || errors.Is(err, ErrCredentialUnavailable) || errors.Is(err, ErrUnauthorized) || answerDenied(err)
}

// inventoryReadFailure marks a failed read of the published inventory as a
// storage failure. Malformed stored data keeps its meaning so the worker can
// re-collect over it.
func inventoryReadFailure(err error) error {
	var payloadFault inventoryPayloadError
	if errors.As(err, &payloadFault) || errors.Is(err, kubeindex.ErrInvalidRecord) {
		return err
	}
	return storageStageFailure("read", err)
}

// errInvalidCapture is a capture whose own payload is invalid, found before it
// reaches the store.
var errInvalidCapture = fmt.Errorf("kubernetes: capture payload is invalid: %w", storage.ErrInvalidEvidence)

// historySaveRejected reports a refusal that a retry of the same capture cannot
// fix, as opposed to an outage that clears on its own. Invalid evidence counts
// only when the capture itself is invalid: the store also answers invalid
// evidence for inconsistent saved state, which repair or retention can clear.
func historySaveRejected(err error) bool {
	return errors.Is(err, storage.ErrEvidenceLimit) || errors.Is(err, errInvalidCapture) || errors.Is(err, storage.ErrUnsupported) || errors.Is(err, storage.ErrEvidenceUnstorable)
}

// captureRefusalReason names, in a fixed vocabulary, why a capture was not
// saved to history. It never carries record text, error text or cluster names.
func captureRefusalReason(err error) string {
	switch {
	case errors.Is(err, storage.ErrEvidenceLimit):
		return "over_save_limit"
	case errors.Is(err, storage.ErrEvidenceUnstorable):
		return "unstorable_text"
	case errors.Is(err, errInvalidCapture), errors.Is(err, storage.ErrInvalidEvidence):
		return "invalid_capture"
	case errors.Is(err, storage.ErrUnsupported):
		return "store_unsupported"
	case errors.Is(err, ErrInventoryNotReady):
		return "authority_unverified"
	case errors.Is(err, ErrIncompleteInventory):
		return "incomplete_inventory"
	}
	return errorClass(err)
}

func recoveryReason(err error, stage string) string {
	var saveFailure *captureStorageError
	switch {
	case err == nil:
		return "ready"
	case errors.Is(err, errInventoryLeaseLost), errors.Is(err, storage.ErrEvidenceConflict):
		return "lease_lost"
	case errors.As(err, &saveFailure) && saveFailure.rejected:
		return "storage_rejected"
	case errors.As(err, &saveFailure):
		return "storage_unavailable"
	case errors.Is(err, ErrCredentialUnavailable):
		return "credential_unavailable"
	case errors.Is(err, ErrUnauthorized):
		return "credential_rejected"
	case errors.As(err, new(*credentialFailure)):
		return "credential_unavailable"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	}
	if stage == "payload" || stage == "cas" || stage == "journal" || stage == "lease" {
		return "storage_unavailable"
	}
	return "collection_unavailable"
}

func (r *clusterRecovery) stage(stage string, started time.Time, err error) {
	if r == nil {
		return
	}
	reason := recoveryReason(err, stage)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stages == nil {
		r.stages = map[string]recoveryStage{}
	}
	previous := r.stages[stage]
	if previous.reason == reason && time.Since(previous.at) < time.Minute {
		return
	}
	r.stages[stage] = recoveryStage{reason: reason, at: time.Now()}
	age := int64(-1)
	if !r.lastCommitted.IsZero() {
		age = int64(time.Since(r.lastCommitted).Seconds())
	}
	log.Printf("kubernetes: capture stage=%s reason=%s duration_ms=%d committed_age_s=%d failures=%d workers=%d attempts=%d", stage, reason, time.Since(started).Milliseconds(), age, r.failures, r.workers, r.attempts)
}

func (r *clusterRecovery) workerStarted() func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.workers++
	r.mu.Unlock()
	return func() { r.mu.Lock(); r.workers--; r.mu.Unlock() }
}
func (r *clusterRecovery) attempted() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.attempts++
	r.mu.Unlock()
}

func (r *clusterRecovery) firstPublication(orgID string) time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstReady[orgID]
}

func (r *clusterRecovery) reserveReplacement(renewal bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replacementStreak++
	streak := max(r.replacementStreak, r.failures)
	delay := time.Second * time.Duration(1<<min(streak-1, 6))
	delay += time.Duration(rand.Int63n(int64(delay/4 + 1)))
	r.next = maxTime(r.next, time.Now().Add(delay))
	reason := "credential_recovery"
	if renewal {
		reason = "credential_renewal"
	}
	log.Printf("kubernetes: recovery stage=epoch_rebuild reason=%s attempt=%d retry_ms=%d", reason, r.replacementStreak, time.Until(r.next).Milliseconds())
}
