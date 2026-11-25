package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// recoveryScope is one read authority's view of the shared provider. The provider
// is shared per cluster, but a denial belongs to the identity that answered it, so
// one scope's denial or cooldown never stands in for another scope's answer.
type recoveryScope struct {
	next   time.Time
	denied bool
	// cause explains why requests wait: the identity proof that could not be
	// completed, or a token host that gave no answer. It is nil when the last
	// failure said nothing more than that the credential is unavailable. A held
	// request answers with it, so a passing outage is never reported as a
	// credential to fix.
	cause error
}

// tokenEndpointCause is the held answer while the cloud token host gives no
// answer: still a credential fault, so every surface fails closed, but one
// that says Versus keeps retrying. It is a fixed value, so nothing the token
// host sent is kept.
var tokenEndpointCause = credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))

// explainingCause is the part of a failure that tells a held request why it
// waits, or nil when the failure explains nothing beyond unavailability.
func explainingCause(err error) error {
	var proof *proofError
	if errors.As(err, &proof) {
		return proof
	}
	if errors.Is(err, ErrTokenEndpointUnavailable) {
		return tokenEndpointCause
	}
	return nil
}

// heldRequest marks the answer to a request the recovery gate held back. It
// is the gate's own decision, not a new attempt, so it is never recorded as a
// failure.
type heldRequest struct{ cause error }

func (held *heldRequest) Error() string { return held.cause.Error() }
func (held *heldRequest) Unwrap() error { return held.cause }

// errConnectorReconnecting is the held answer while the connector rebuilds its
// connection for a reason that says nothing against the credential: a routine
// token renewal, or a rebuild attempt that ran out of time. Versus retries on
// its own, so it is never answered as a credential to restore or rotate.
var errConnectorReconnecting = errors.New("kubernetes: connector is reconnecting")

// rebuildStalled reports a connector rebuild that could not finish because the
// collection it replaces did not stop in time. Its cause names why: the history
// database when that collection was waiting on it, otherwise a reconnect that
// Versus retries.
type rebuildStalled struct{ cause error }

func (err *rebuildStalled) Error() string {
	return "kubernetes: connector rebuild waited for the previous collection to stop"
}
func (err *rebuildStalled) Unwrap() error { return err.cause }

// ConnectorRecovering reports an answer the connector recovery gave instead of
// a cluster read: a request held back while the connector reconnects, or a
// rebuild that could not finish. Such an answer is about Versus's own
// connection, so a report about the cluster can still be served from local
// state.
func ConnectorRecovering(err error) bool {
	var held *heldRequest
	var stalled *rebuildStalled
	return errors.As(err, &held) || errors.As(err, &stalled)
}

// stallCause is the held answer for a recovery failure that is not a
// credential fault. A storage fault keeps its storage class; anything else is
// a reconnect Versus retries. It is a fixed value, so no failure text is kept.
func stallCause(err error) error {
	var stalled *rebuildStalled
	if errors.As(err, &stalled) && stalled.cause != nil {
		return stalled.cause
	}
	var saveFailure *captureStorageError
	switch {
	case errors.Is(err, errHistoryTimeout):
		return errHistoryTimeout
	case errors.Is(err, errHistoryUnavailable), errors.As(err, &saveFailure):
		return errHistoryUnavailable
	}
	return errConnectorReconnecting
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
	// logf writes the stage lines; nil writes them at debug level.
	logf func(format string, args ...any)
	// cluster names the cluster in warnings; connected is set once its first
	// readable cycle has been reported.
	cluster   string
	connected bool
	// warned holds the credential failure reasons warned about since the last
	// publication, so a recovery clears each of them.
	warned map[string]struct{}
	// inFlight counts the credential rebuilds running now; a request that
	// finds one is held instead of queueing behind it.
	inFlight int
	// closed is set when the registry closes.
	closed bool
	// stall is the held answer after a failure that says nothing against the
	// credential, and renewing marks a rebuild for a routine token renewal.
	// Neither closes the request gate; both only name why a request waits on a
	// rebuild. A publication clears both.
	stall    error
	renewing bool
}

// beginAttempt marks a credential rebuild in flight for orgID. The failure
// that made the rebuild necessary is kept as the held answer's cause when it
// explains the wait, so requests held meanwhile are told why. It changes
// neither the retry schedule nor the failure count.
func (r *clusterRecovery) beginAttempt(orgID string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight++
	if cause := explainingCause(err); cause != nil {
		if r.scopes == nil {
			r.scopes = map[string]*recoveryScope{}
		}
		scope := r.scopes[orgID]
		if scope == nil {
			scope = &recoveryScope{}
			r.scopes[orgID] = scope
		}
		scope.cause = cause
	}
}

func (r *clusterRecovery) endAttempt() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight--
}

// close marks the recovery of a closed registry: nothing that finishes
// afterwards is recorded or warned about.
func (r *clusterRecovery) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

// attemptInFlight reports whether a credential rebuild is running now.
func (r *clusterRecovery) attemptInFlight() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inFlight > 0
}

// reportConnected writes the one line per process that says a cluster is
// connected, after its first cycle that read at least one kind. A cluster that
// answers nothing is not reported as connected; its failure is warned about
// instead.
func (r *clusterRecovery) reportConnected(cluster string, kinds map[string]kubeindex.KindStatus) {
	if r == nil {
		return
	}
	ready := 0
	for _, status := range kinds {
		if status.State == "ready" {
			ready++
		}
	}
	if ready == 0 {
		return
	}
	r.mu.Lock()
	logged := r.connected
	r.connected = true
	r.mu.Unlock()
	if !logged {
		logging.Infof("kubernetes: cluster %q connected, %d of %d kinds readable", cluster, ready, len(kinds))
	}
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
	return r.requestGateError(orgID) != nil
}

// requestGateError is the answer a gated request gets, or nil when the scope
// may serve. It is the credential fault the gate stands for, carrying the
// stored cause that explains the wait when there is one.
func (r *clusterRecovery) requestGateError(orgID string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	credential, scope := r.scopes[""], r.scopes[orgID]
	if credential != nil && credential.denied || scope != nil && scope.denied {
		return nil
	}
	now := time.Now()
	for _, gated := range []*recoveryScope{credential, scope} {
		if gated != nil && now.Before(gated.next) {
			return gatedCause(credential, scope)
		}
	}
	return nil
}

// gatedError is the answer for a request held back while recovery waits for
// its next attempt: the stored cause when one is recorded, the bare credential
// fault while a credential failure holds the gate, and otherwise why the
// connector is reconnecting. A rebuild that no credential failure explains is
// never answered as a credential to restore or rotate.
func (r *clusterRecovery) gatedError(orgID string) error {
	if r == nil {
		return ErrCredentialUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	credential, scope := r.scopes[""], r.scopes[orgID]
	if r.credentialRecordedLocked(orgID, time.Now()) {
		return gatedCause(credential, scope)
	}
	if r.stall != nil {
		return &heldRequest{cause: r.stall}
	}
	if r.renewing {
		return &heldRequest{cause: errConnectorReconnecting}
	}
	return gatedCause(credential, scope)
}

// credentialRecordedLocked reports whether a credential failure still holds
// this scope: a stored cause, or a gate that has not reopened yet.
func (r *clusterRecovery) credentialRecordedLocked(orgID string, now time.Time) bool {
	for _, gated := range []*recoveryScope{r.scopes[""], r.scopes[orgID]} {
		if gated != nil && (gated.cause != nil || gated.denied || now.Before(gated.next)) {
			return true
		}
	}
	return false
}

// noteRebuild records why the credential epoch is being rebuilt: a routine
// token renewal, or a retired credential.
func (r *clusterRecovery) noteRebuild(renewal bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renewing = renewal
}

// gatedCause picks the stored cause, the credential's own before the org
// scope's. A stored cause stands until a publication clears it or a failure
// that says something else about the credential replaces it.
func gatedCause(credential, scope *recoveryScope) error {
	for _, gated := range []*recoveryScope{credential, scope} {
		if gated != nil && gated.cause != nil {
			return &heldRequest{cause: gated.cause}
		}
	}
	return &heldRequest{cause: ErrCredentialUnavailable}
}

// gate applies the scope's decision to the shared retry schedule. cause
// explains the decision, or is nil. A failure that says nothing about the
// credential (keepCause) leaves the stored cause in place.
func (r *clusterRecovery) gate(orgID string, now time.Time, denied bool, cause error, keepCause bool) {
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
	if !keepCause {
		scope.cause = cause
	}
	scope.next = maxTime(scope.next, now)
}

// recovered clears the per-scope gates: a committed publication proves the
// identity could read the cluster again.
func (r *clusterRecovery) recovered() {
	for _, scope := range r.scopes {
		scope.next, scope.denied, scope.cause = time.Time{}, false, nil
	}
	r.stall, r.renewing = nil, false
	for reason := range r.warned {
		problems().Clear(credentialProblemKey(r.cluster, reason), "kubernetes: cluster %q credential check recovered", r.cluster)
	}
	r.warned = nil
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
	// A held request's answer is the gate's own decision, not an attempt. An
	// answered denial joined to it is still recorded.
	denied := answerDenied(err)
	var held *heldRequest
	if errors.As(err, &held) && !denied {
		return
	}
	cause := explainingCause(err)
	// A context that ran out or was cancelled only says the attempt stopped
	// waiting, not why the credential failed. It still counts toward the retry
	// schedule, so the supervisor keeps backing off, but it never replaces the
	// cause the gate answers with: a request must keep hearing about the proof
	// outage, not be told to rotate a credential.
	credentialAnswer := denied || cause != nil || countsAgainstCredential(err)
	// Nor does any other failure that says nothing against the credential:
	// only a credential answer may replace the stored cause.
	keepCause := cause == nil && (!credentialAnswer || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
	// A rebuild that could not stop the previous collection is never a
	// credential attempt, whatever the gate holds already.
	var stalled *rebuildStalled
	stall := errors.As(err, &stalled) && !denied
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	now := time.Now()
	if stall || !credentialAnswer && !r.credentialRecordedLocked(orgID, now) {
		// Nothing here says the credential failed: the attempt ran out of
		// time, the cluster or the history store did not answer, or the
		// previous collection would not stop. The retry is still backed off,
		// but requests are not gated, and one that waits on the rebuild is
		// told why instead of being sent to rotate a credential. A cause the
		// gate already holds is left as it is, and still answers first.
		r.stall = stallCause(err)
		if now.Before(r.next) && r.failures > 0 {
			return
		}
		r.failures++
		delay := time.Second * time.Duration(1<<min(r.failures-1, 6))
		delay += time.Duration(rand.Int63n(int64(delay/4 + 1)))
		r.next = maxTime(r.next, now.Add(delay))
		logging.Debugf("kubernetes: recovery stage=credential_recovery reason=%s attempt=%d retry_ms=%d", recoveryReason(err, "credential_recovery"), r.failures, time.Until(r.next).Milliseconds())
		return
	}
	if credentialAnswer {
		r.stall = nil
	}
	// Concurrent failures from a single attempt do not multiply the streak, but the
	// scope gate still follows every recorded failure.
	if now.Before(r.next) && r.failures > 0 {
		r.gate(orgID, r.next, denied, cause, keepCause)
		// The retry is already scheduled, so nothing new is logged at debug, but
		// the warning still follows a change of cause (a token host that stops
		// timing out and starts refusing); the throttle drops a repeat.
		if !keepCause {
			r.warnCredential(recoveryReason(err, "credential_recovery"))
		}
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
	r.gate(orgID, r.next, denied, cause, keepCause)
	reason := recoveryReason(err, "credential_recovery")
	logging.Debugf("kubernetes: recovery stage=credential_recovery reason=%s attempt=%d retry_ms=%d", reason, r.failures, time.Until(r.next).Milliseconds())
	// An attempt that stopped waiting names no cause the operator can act on,
	// and a warning would hide the proof outage behind a timeout.
	if !keepCause {
		r.warnCredential(reason)
	}
}

// warnCredential writes the throttled warning for a failed credential check:
// once for each cause, then at most once per throttle window while that cause
// lasts. Each cause keeps its own throttle entry, so a token host that
// alternates between two answers warns about each once per window, not on
// every attempt.
func (r *clusterRecovery) warnCredential(reason string) {
	if cause, check, ok := problemCause(reason, false); ok {
		if r.warned == nil {
			r.warned = map[string]struct{}{}
		}
		r.warned[reason] = struct{}{}
		problems().Fail(slog.LevelWarn, credentialProblemKey(r.cluster, reason), reason, "kubernetes: cluster %q credential check failed reason=%s (%s); %s", r.cluster, reason, cause, check)
	}
}

// credentialProblemKey is the warning throttle key for one credential failure
// cause of a cluster.
func credentialProblemKey(cluster, reason string) string {
	return problemKey("", cluster, "credential|"+reason)
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
			recovery.recordStage("worker_recovery", time.Now(), nil, true)
		}

		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := registry.resolveCluster(attempt, current.scope.OrgID, clusterID, nil)
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
	var proofFailure *proofError
	switch {
	case err == nil:
		return "ready"
	case errors.As(err, &proofFailure):
		return proofFailure.Code()
	case errors.Is(err, errInventoryLeaseLost), errors.Is(err, storage.ErrEvidenceConflict):
		return "lease_lost"
	case errors.As(err, &saveFailure) && saveFailure.rejected:
		return "storage_rejected"
	case errors.As(err, &saveFailure):
		return "storage_unavailable"
	case errors.Is(err, ErrTokenEndpointUnavailable):
		return "token_endpoint_unavailable"
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

// stage records how one stage of a capture cycle ended. A healthy cluster
// stays quiet: a stage that keeps succeeding writes no log line, and a
// success is logged only when it ends a failure. A failure is logged when it
// starts or changes reason, and repeats at most once a minute while it lasts.
func (r *clusterRecovery) stage(stage string, started time.Time, err error) {
	r.recordStage(stage, started, err, false)
}

// recordStage records a stage result; always logs it even when it is a
// success that follows no failure, for one-off events such as a restarted
// worker.
func (r *clusterRecovery) recordStage(stage string, started time.Time, err error, always bool) {
	if r == nil {
		return
	}
	reason := recoveryReason(err, stage)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stages == nil {
		r.stages = map[string]recoveryStage{}
	}
	previous, seen := r.stages[stage]
	if reason == "ready" && !always && (!seen || previous.reason == "ready") {
		r.stages[stage] = recoveryStage{reason: reason, at: time.Now()}
		return
	}
	if previous.reason == reason && time.Since(previous.at) < time.Minute {
		return
	}
	r.stages[stage] = recoveryStage{reason: reason, at: time.Now()}
	age := int64(-1)
	if !r.lastCommitted.IsZero() {
		age = int64(time.Since(r.lastCommitted).Seconds())
	}
	logf := r.logf
	if logf == nil {
		logf = logging.Debugf
	}
	logf("kubernetes: capture stage=%s reason=%s duration_ms=%d committed_age_s=%d failures=%d workers=%d attempts=%d", stage, reason, time.Since(started).Milliseconds(), age, r.failures, r.workers, r.attempts)
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
	logging.Debugf("kubernetes: recovery stage=epoch_rebuild reason=%s attempt=%d retry_ms=%d", reason, r.replacementStreak, time.Until(r.next).Milliseconds())
}

// identityFieldNames is the fixed set of field names an identity-change line
// may carry. A key name read from the cluster is never logged.
var identityFieldNames = []string{"username", "uid", "groups", "extra:oid"}

// unstableIdentityRebuilds is how many principal-change rebuilds in a row
// raise the warning that a cluster identity is not stable.
const unstableIdentityRebuilds = 3

// changedIdentityFields names the reviewed identity fields that differ
// between two review principals, from identityFieldNames only. It returns
// nil when either principal is not a review principal.
func changedIdentityFields(previous, next string) []string {
	decode := func(principal string) (reviewedUserInfo, bool) {
		var user reviewedUserInfo
		encoded, found := strings.CutPrefix(principal, "self-subject-review:")
		return user, found && json.Unmarshal([]byte(encoded), &user) == nil
	}
	before, okBefore := decode(previous)
	after, okAfter := decode(next)
	if !okBefore || !okAfter {
		return nil
	}
	changed := map[string]bool{
		"username":  before.Username != after.Username,
		"uid":       before.UID != after.UID,
		"groups":    !slices.Equal(before.Groups, after.Groups),
		"extra:oid": !slices.Equal(before.Extra["oid"], after.Extra["oid"]),
	}
	var fields []string
	for _, name := range identityFieldNames {
		if changed[name] {
			fields = append(fields, name)
		}
	}
	return fields
}

func identityFieldList(fields []string) string {
	if len(fields) == 0 {
		return "principal"
	}
	return strings.Join(fields, ",")
}

// noteIdentityChange records a principal-change rebuild for cluster. Each one
// logs its changed field names at debug; the third in a row warns once per
// throttle window that the identity is not stable.
func (client *Client) noteIdentityChange(cluster string, fields []string) {
	if client == nil {
		return
	}
	client.identityMu.Lock()
	client.identityChanges++
	if client.identityFields == nil {
		client.identityFields = map[string]bool{}
	}
	for _, field := range fields {
		client.identityFields[field] = true
	}
	count := client.identityChanges
	var seen []string
	for _, name := range identityFieldNames {
		if client.identityFields[name] {
			seen = append(seen, name)
		}
	}
	client.identityMu.Unlock()
	logging.Debugf("kubernetes: cluster %q identity changed at a credential renewal fields=%s; rebuilding collection", cluster, identityFieldList(fields))
	if count >= unstableIdentityRebuilds {
		problems().Fail(slog.LevelWarn, problemKey("", cluster, "identity_unstable"), "unstable",
			"kubernetes: cluster %q identity is not stable: %s changed on %d credential renewals in a row (identity changed), and each change rebuilds collection; check the cluster's authentication setup, or report it with this line", cluster, identityFieldList(seen), count)
	}
}

// noteIdentityKept ends a run of principal-change rebuilds: a renewal kept
// the same identity.
func (client *Client) noteIdentityKept(cluster string) {
	if client == nil {
		return
	}
	client.identityMu.Lock()
	client.identityChanges, client.identityFields = 0, nil
	client.identityMu.Unlock()
	problems().Clear(problemKey("", cluster, "identity_unstable"), "kubernetes: cluster %q identity stable again", cluster)
}

// rebuildStalled is the failure of a rebuild whose previous collection did not
// stop in time. When that collection was failing on the history store, which
// is what keeps a worker from stopping, the store is named; otherwise it is a
// reconnect Versus retries.
func (service *Service) rebuildStalled(previous []*Service) error {
	for _, old := range append([]*Service{service}, previous...) {
		if old.historyStoreFailing() {
			return &rebuildStalled{cause: errHistoryUnavailable}
		}
	}
	return &rebuildStalled{cause: errConnectorReconnecting}
}

// historyStoreFailing reports whether this scope's collection is failing on
// the history store: its last inventory cycle failed with a storage class, or
// its last save stage did.
func (service *Service) historyStoreFailing() bool {
	if service == nil || service.changes == nil {
		return false
	}
	service.changes.mu.Lock()
	cache := service.changes.inventories[service.indexScope()]
	service.changes.mu.Unlock()
	if cache != nil {
		cache.mu.RLock()
		failed, class := cache.failed, cache.failedClass
		cache.mu.RUnlock()
		if failed && storageClass(class) {
			return true
		}
	}
	return service.recovery.storageStageFailing()
}

// storageStageFailing reports whether a save stage last ended on a storage
// failure.
func (r *clusterRecovery) storageStageFailing() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, stage := range r.stages {
		if storageClass(stage.reason) {
			return true
		}
	}
	return false
}

func storageClass(class string) bool {
	return class == "storage_unavailable" || class == "storage_rejected"
}
