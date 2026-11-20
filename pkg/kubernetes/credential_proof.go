package kubernetes

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/logging"
)

// proofOutcome is what one identity proof of one token established.
//
// A definite rejection (the cluster answered the identity review with 401, or
// the identity provider refused the signed request with 401 or 403) is not an
// outcome: the proof returns an error that wraps ErrUnauthorized, the slot
// records it, and the epoch that asks retires at once with no grace. The
// rejection is cached until its backoff ends, then handed to one more caller
// before the token is checked afresh.
type proofOutcome uint8

const (
	// proofUnavailable means the proof could not be completed now: a
	// timeout, a 429, a 5xx or an unreachable host. It never retires a proven
	// epoch and never upgrades an unknown one.
	proofUnavailable proofOutcome = iota
	// proofNotApplicable means the proof cannot vouch for this token. From
	// the provider it covers a token type the provider cannot read and a
	// token its checks refuse (bad signature, wrong audience, tenant or
	// object ID, expired, or a token-info answer of invalid_token): an epoch
	// started on it stays unknown and byte-bound, and a renewal answered
	// this way retires a provider-proven epoch at once. From identity review
	// it means the cluster refused the review or does not offer it; that is
	// retried after a backoff and never falls back to the provider for a
	// review-based epoch.
	proofNotApplicable
	// proofProven means the provider or the cluster confirmed the identity.
	proofProven
)

const (
	// providerProofTimeout bounds one provider proof call.
	providerProofTimeout = 3 * time.Second
	// reviewProofTimeout bounds one identity review; the review itself keeps
	// the client's shorter request timeout.
	reviewProofTimeout = 5 * time.Second
	// proofBackoffCap is the longest wait between two proofs of one token,
	// whatever the provider's Retry-After says.
	proofBackoffCap = 5 * time.Minute
	// proofGraceMargin ends the grace on the previous token this long before
	// its recorded expiry.
	proofGraceMargin = 30 * time.Second
	// maxProofEntries is how many tokens the proof cache remembers: the
	// current one and the previous one.
	maxProofEntries = 2
)

// provenCredentialSource is a rotating credential source whose renewed tokens
// must be proved before any data request carries them. EKS, AKS and GKE
// sources implement it; every other source keeps the byte-bound rule.
type provenCredentialSource interface {
	CredentialSource
	// verifiedCredentialPrincipal proves which identity a token carries.
	verifiedCredentialPrincipal(context.Context, string) (string, proofOutcome, error)
	// credentialExpiry is the recorded expiry of header, or zero when header
	// is not the source's current token or has no known expiry.
	credentialExpiry(header string) time.Time
	// proofHost names the host the provider proof calls, for the warning that
	// asks to allow it. It is empty when the source has no provider proof.
	proofHost() string
	// currentCredential reports whether header is the token the source
	// issues now; only that token may become the serving one.
	currentCredential(header string) bool
	// provesRenewals marks the source; it has no behaviour.
	provesRenewals()
}

// proofError is a proof that could not be completed. class is one of
// timeout, unreachable, 429, 3xx, 4xx, 5xx, invalid_answer, review_denied,
// clock_skew, unknown_key, jwks_host_not_allowed or pending;
// it never carries a token, digest, claim or principal value.
type proofError struct {
	class      string
	retryAfter time.Duration
}

func (err *proofError) Error() string {
	return "kubernetes: credential identity proof " + err.Code()
}

func (err *proofError) Unwrap() error { return ErrCredentialUnavailable }

// Code is the status and error class. It is the same word the sync report
// uses, so one proof failure has one code on every surface.
func (err *proofError) Code() string {
	return err.syncClass()
}

// syncClass is the class a sync report and a kind carry. A review the
// cluster refused, or a token whose times disagree with this server's clock,
// is proof_unavailable, with a clock or permission remedy; proof keys
// published on a host outside the cloud's allowed hosts are
// proof_host_not_allowed, a setting to check rather than outbound access; a
// proof host that gave no answer, or answered with a failure of its own, is
// proof_unreachable.
func (err *proofError) syncClass() string {
	switch err.class {
	case "review_denied", "clock_skew":
		return "proof_unavailable"
	case "jwks_host_not_allowed":
		return "proof_host_not_allowed"
	}
	return "proof_unreachable"
}

func (err *proofError) unreachable() bool {
	return err.class == "unreachable" || err.class == "timeout"
}

// CredentialProofFailure marks the error as a renewal whose identity proof
// could not be completed.
func (err *proofError) CredentialProofFailure() bool { return true }

// IsCredentialProofFailure reports whether err is a renewal whose identity
// proof could not be completed. The credential itself may be valid, so a
// surface must not name it rejected or ask for it to be rotated.
func IsCredentialProofFailure(err error) bool {
	var failure interface{ CredentialProofFailure() bool }
	return errors.As(err, &failure) && failure.CredentialProofFailure()
}

// IsCredentialHostFailure reports whether err is a failure of a host the
// credential depends on, not of the credential: an identity proof that could
// not be completed, or a cloud token host that gave no definite answer. A
// surface keeps the failure's own code and remedy and never asks for the
// credential to be rotated.
func IsCredentialHostFailure(err error) bool {
	return IsCredentialProofFailure(err) || errors.Is(err, ErrTokenEndpointUnavailable)
}

// unexpectedStatusClass names a proof answer that decides nothing: a
// redirect (never followed) or a 4xx other than a definite refusal.
func unexpectedStatusClass(status int) string {
	if status >= 300 && status < 400 {
		return "3xx"
	}
	return "4xx"
}

// proofTransportError classifies a proof call that got no HTTP answer.
func proofTransportError(err error) *proofError {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return &proofError{class: "timeout"}
	}
	return &proofError{class: "unreachable"}
}

// proofStatusError classifies a 429 or 5xx proof answer and keeps its
// Retry-After.
func proofStatusError(response *http.Response, now time.Time) *proofError {
	failure := &proofError{class: "5xx"}
	if response.StatusCode == http.StatusTooManyRequests {
		failure.class = "429"
	}
	if hint := response.Header.Get("Retry-After"); hint != "" {
		failure.retryAfter = parseRetryAfter(hint, now)
	}
	return failure
}

// proofKind names the principal source a proof asks.
type proofKind uint8

const (
	proofByReview proofKind = iota
	proofByProvider
)

// principalSource is where an epoch's principal came from. An epoch keeps
// one source for its whole life.
type principalSource uint8

const (
	principalUnknown principalSource = iota
	principalReview
	principalProvider
)

// proofSlot is the state of one kind of proof for one token.
type proofSlot struct {
	running   bool
	attempted bool
	outcome   proofOutcome
	principal string
	rejected  error
	class     string
	retryAt   time.Time
	failures  int
	done      chan struct{}
}

type proofEntry struct {
	digest [32]byte
	slots  [2]proofSlot
}

// credentialProofs caches proofs per token, off the epoch lock. Each token
// and kind has at most one proof in flight; everyone asking for it waits on
// the same result. At most two tokens are remembered, failures included.
// Failures back off exponentially up to proofBackoffCap and honour
// Retry-After. A provider host that does not answer at all is remembered for
// the whole cache, so a new token does not wait on it again before its
// backoff ends.
type credentialProofs struct {
	mu      sync.Mutex
	now     func() time.Time
	entries []*proofEntry
	// hostRetryAt and hostClass hold the provider host's last transport
	// failure, and providerProblem names a last failure a setting must fix:
	// unreachable, timeout or jwks_host_not_allowed.
	hostRetryAt     time.Time
	hostClass       string
	providerProblem string
}

func newCredentialProofs(now func() time.Time) *credentialProofs {
	if now == nil {
		now = time.Now
	}
	return &credentialProofs{now: now}
}

// proofCache returns the client's proof cache. It lives on the client, so a
// successor epoch reuses what the previous one already proved.
func (client *Client) proofCache() *credentialProofs {
	client.proofsMu.Lock()
	defer client.proofsMu.Unlock()
	if client.proofs == nil {
		client.proofs = newCredentialProofs(nil)
	}
	return client.proofs
}

func (proofs *credentialProofs) clock() time.Time { return proofs.now() }

// size returns how many tokens the cache remembers.
func (proofs *credentialProofs) size() int {
	proofs.mu.Lock()
	defer proofs.mu.Unlock()
	return len(proofs.entries)
}

// problem returns the provider proof's last failure a setting must fix, or
// "" when its last answer was not one.
func (proofs *credentialProofs) problem() string {
	proofs.mu.Lock()
	defer proofs.mu.Unlock()
	return proofs.providerProblem
}

// entry finds or inserts the entry for digest, dropping the oldest when the
// cache is full. Callers hold the lock.
func (proofs *credentialProofs) entry(digest [32]byte) *proofEntry {
	for i, entry := range proofs.entries {
		if subtle.ConstantTimeCompare(entry.digest[:], digest[:]) == 1 {
			proofs.entries = append(append(proofs.entries[:i:i], proofs.entries[i+1:]...), entry)
			return entry
		}
	}
	entry := &proofEntry{digest: digest}
	if len(proofs.entries) >= maxProofEntries {
		proofs.entries = append(proofs.entries[:0:0], proofs.entries[len(proofs.entries)-maxProofEntries+1:]...)
	}
	proofs.entries = append(proofs.entries, entry)
	return entry
}

// lookup returns a copy of the proof state for header and kind. It starts a
// proof when none has run, or when the last one's backoff has passed and it
// may change: an unavailable proof, a definite rejection (a provider can
// reissue the same token after a transient refusal), or a review the cluster
// refused (review can be enabled again). wait is closed when a proof in
// flight ends; it is nil when none is running.
func (proofs *credentialProofs) lookup(kind proofKind, header string, run func(context.Context) (string, proofOutcome, error)) (proofSlot, <-chan struct{}) {
	digest := sha256.Sum256([]byte(header))
	proofs.mu.Lock()
	defer proofs.mu.Unlock()
	entry := proofs.entry(digest)
	slot := &entry.slots[kind]
	now := proofs.now()
	retryable := slot.rejected != nil || slot.outcome == proofUnavailable || kind == proofByReview && slot.outcome == proofNotApplicable
	due := !slot.attempted || retryable && !now.Before(slot.retryAt)
	if !slot.running && due {
		if slot.rejected != nil {
			// A rejection is not served from the cache past its backoff, but
			// the epoch that asks first still gets it: an epoch that saw no
			// request inside the backoff must not read the reset as a proof
			// in flight and keep serving the previous token. The next lookup
			// checks the token afresh.
			snapshot := *slot
			snapshot.done = nil
			slot.attempted, slot.rejected = false, nil
			return snapshot, nil
		}
		if kind == proofByProvider && now.Before(proofs.hostRetryAt) {
			// The host gave no answer recently: record the cached failure
			// without calling it again.
			slot.attempted, slot.outcome, slot.class = true, proofUnavailable, proofs.hostClass
			slot.retryAt = proofs.hostRetryAt
		} else {
			slot.running = true
			slot.done = make(chan struct{})
			timeout := reviewProofTimeout
			if kind == proofByProvider {
				timeout = providerProofTimeout
			}
			go proofs.run(slot, kind, timeout, run)
		}
	}
	snapshot := *slot
	snapshot.done = nil
	var wait <-chan struct{}
	if slot.running {
		wait = slot.done
	}
	return snapshot, wait
}

// run performs one proof outside every epoch lock and records its result.
func (proofs *credentialProofs) run(slot *proofSlot, kind proofKind, timeout time.Duration, run func(context.Context) (string, proofOutcome, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	principal, outcome, err := run(ctx)
	cancel()
	proofs.mu.Lock()
	defer proofs.mu.Unlock()
	defer close(slot.done)
	now := proofs.now()
	slot.running, slot.attempted = false, true
	if err != nil && errors.Is(err, ErrUnauthorized) {
		slot.outcome, slot.rejected, slot.class = proofUnavailable, err, ""
		slot.failures++
		slot.retryAt = now.Add(proofBackoff(slot.failures, 0))
		if kind == proofByProvider {
			proofs.hostRetryAt, proofs.hostClass = time.Time{}, ""
		}
		return
	}
	if kind == proofByReview && outcome == proofNotApplicable {
		// The cluster refused the review. That decides nothing for good:
		// it is asked again after a backoff.
		slot.outcome, slot.principal, slot.class = outcome, "", "review_denied"
		slot.failures++
		slot.retryAt = now.Add(proofBackoff(slot.failures, 0))
		return
	}
	if outcome != proofUnavailable {
		slot.outcome, slot.principal, slot.class, slot.failures = outcome, principal, "", 0
		if kind == proofByProvider {
			proofs.hostRetryAt, proofs.hostClass = time.Time{}, ""
		}
		return
	}
	var failure *proofError
	if !errors.As(err, &failure) {
		switch {
		case err != nil && (errors.Is(err, context.DeadlineExceeded) || isTransportError(err)):
			failure = proofTransportError(err)
		default:
			failure = &proofError{class: "invalid_answer"}
		}
	}
	slot.outcome, slot.class = proofUnavailable, failure.class
	slot.failures++
	delay := proofBackoff(slot.failures, failure.retryAfter)
	slot.retryAt = now.Add(delay)
	if kind == proofByProvider {
		proofs.providerProblem = ""
		if failure.unreachable() || failure.class == "jwks_host_not_allowed" {
			proofs.providerProblem = failure.class
		}
		if failure.unreachable() {
			proofs.hostRetryAt, proofs.hostClass = slot.retryAt, failure.class
		}
	}
	logging.Debugf("kubernetes: credential proof unavailable class=%s retry_in_s=%d", failure.class, int(delay.Seconds()))
}

// proofBackoff is the wait before the next proof of one token: doubling from
// one second, at least the provider's Retry-After, never more than
// proofBackoffCap.
func proofBackoff(failures int, retryAfter time.Duration) time.Duration {
	delay := min(proofBackoffCap, time.Second*time.Duration(1<<min(failures-1, 9)))
	if retryAfter > delay {
		delay = min(retryAfter, proofBackoffCap)
	}
	return delay
}

func isTransportError(err error) bool {
	var netErr net.Error
	var opErr *net.OpError
	return errors.As(err, &netErr) || errors.As(err, &opErr)
}

// credentialAdmission is what a request may use: the header it sends, the
// request generation it belongs to and the pinned client certificate.
type credentialAdmission struct {
	header      string
	generation  uint64
	certificate *tls.Certificate
}

func credentialFingerprint(header string, certificate *tls.Certificate) [32]byte {
	digest := sha256.New()
	digest.Write([]byte(header))
	if certificate != nil {
		for _, encoded := range certificate.Certificate {
			digest.Write(encoded)
		}
	}
	var fingerprint [32]byte
	copy(fingerprint[:], digest.Sum(nil))
	return fingerprint
}

// admitProven admits header for a source whose renewals are proved, waiting
// off the epoch lock for a proof in flight when nothing else may serve.
func (service *Service) admitProven(ctx context.Context, source provenCredentialSource, header string) (credentialAdmission, error) {
	epoch := service.identity
	for {
		epoch.mu.Lock()
		admission, wait, err := service.admitProvenLocked(ctx, source, header, false)
		epoch.mu.Unlock()
		if errors.Is(err, ErrUnauthorized) {
			// A definite rejection retired the epoch. The source drops the
			// token, off the epoch lock, so the next epoch mints and proves
			// a fresh one.
			if rejector, ok := source.(interface{ rejectBearer(string) bool }); ok {
				rejector.rejectBearer(header)
			}
		}
		if err != nil || wait == nil {
			return admission, err
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return credentialAdmission{}, ctx.Err()
		}
	}
}

// admitProvenLocked applies the renewal rule for a proved source. It never
// calls the network: proofs run in the background, and a returned channel
// says the caller has to wait for one because nothing else may serve.
//
// A renewed token is a candidate. It is sent only in its own proof call
// until it is proven for this epoch's principal. Until then requests use the
// previous token, which was itself proven for this epoch and not answered
// 401, until proofGraceMargin before its recorded expiry; after that they
// fail fast with a proof error, and nothing retires. A candidate proven for a
// different principal, rejected outright or impossible to prove retires the
// epoch at once. An epoch that was never proven keeps the byte-bound rule.
//
// committing is set for a commit: only a commit ends an unknown epoch whose
// token was proven after start, so no read a user waits on fails for it.
func (service *Service) admitProvenLocked(ctx context.Context, source provenCredentialSource, header string, committing bool) (credentialAdmission, <-chan struct{}, error) {
	epoch := service.identity
	if err := ctx.Err(); err != nil {
		return credentialAdmission{}, nil, err
	}
	if epoch.retired || epoch.ctx.Err() != nil {
		return credentialAdmission{}, nil, ErrCredentialUnavailable
	}
	certificate, err := source.ClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		service.retireLocked(false)
		return credentialAdmission{}, nil, ErrCredentialUnavailable
	}
	fingerprint := credentialFingerprint(header, certificate)
	proofs := service.client.proofCache()
	if !epoch.initialized {
		return service.initializeProvenLocked(source, header, fingerprint, certificate, proofs)
	}
	if subtle.ConstantTimeCompare(fingerprint[:], epoch.fingerprint[:]) == 1 {
		if !epoch.known {
			if err := service.upgradeUnknownLocked(source, header, proofs, committing); err != nil {
				return credentialAdmission{}, nil, err
			}
		}
		return credentialAdmission{header: header, generation: epoch.requestGeneration.Load(), certificate: epoch.certificate}, nil, nil
	}
	if !epoch.known {
		// Nothing proves the new bytes belong to the same principal: a new
		// token starts a new byte-bound epoch.
		service.retireLocked(true)
		return credentialAdmission{}, nil, ErrCredentialUnavailable
	}
	kind := proofByReview
	if epoch.source == principalProvider {
		kind = proofByProvider
	}
	slot, wait := proofs.lookup(kind, header, service.proofRunner(source, kind, header, certificate))
	switch {
	case slot.rejected != nil:
		service.retireLocked(true)
		return credentialAdmission{}, nil, credentialFault(slot.rejected)
	case slot.attempted && slot.outcome == proofProven:
		principalHash := sha256.Sum256([]byte(slot.principal))
		if subtle.ConstantTimeCompare(principalHash[:], epoch.principal[:]) != 1 {
			service.client.noteIdentityChange(service.scope.ClusterID, changedIdentityFields(epoch.reviewPrincipal, slot.principal))
			service.retireLocked(true)
			return credentialAdmission{}, nil, ErrCredentialUnavailable
		}
		if !source.currentCredential(header) {
			// A slow request still holding an earlier proven token may send
			// it, but never moves the serving token back.
			return credentialAdmission{header: header, generation: epoch.requestGeneration.Load(), certificate: certificate}, nil, nil
		}
		service.promoteLocked(source, header, fingerprint, certificate)
		return credentialAdmission{header: header, generation: epoch.requestGeneration.Load(), certificate: certificate}, nil, nil
	case slot.attempted && slot.outcome == proofNotApplicable && kind == proofByProvider:
		// The provider cannot prove this token type: it cannot ride an
		// epoch proven for another.
		service.retireLocked(true)
		return credentialAdmission{}, nil, ErrCredentialUnavailable
	}
	class := slot.class
	if slot.attempted && slot.outcome == proofNotApplicable {
		// A review-based epoch keeps review as its only principal source: a
		// review the cluster now refuses is unavailable, never a fallback.
		class = "review_denied"
	}
	if service.graceLocked(proofs.clock()) {
		return credentialAdmission{header: epoch.servingHeader, generation: epoch.requestGeneration.Load(), certificate: epoch.certificate}, nil, nil
	}
	if !slot.attempted {
		return credentialAdmission{}, wait, nil
	}
	return credentialAdmission{}, nil, service.proofFailedLocked(source, kind, class, proofs.clock())
}

// initializeProvenLocked starts the epoch on its first token. Identity review
// is asked first; the provider proof only when review proves nothing.
func (service *Service) initializeProvenLocked(source provenCredentialSource, header string, fingerprint [32]byte, certificate *tls.Certificate, proofs *credentialProofs) (credentialAdmission, <-chan struct{}, error) {
	epoch := service.identity
	review, wait := proofs.lookup(proofByReview, header, service.proofRunner(source, proofByReview, header, certificate))
	if !review.attempted {
		return credentialAdmission{}, wait, nil
	}
	if review.rejected != nil {
		service.retireLocked(false)
		return credentialAdmission{}, nil, credentialFault(review.rejected)
	}
	principal, from := "", principalUnknown
	switch review.outcome {
	case proofProven:
		principal, from = review.principal, principalReview
	case proofUnavailable:
		// Review exists but did not answer: the epoch starts unknown and
		// byte-bound, and upgrades once review answers. Falling back to the
		// provider here would give replicas different principals for one
		// identity.
	default:
		provider, wait := proofs.lookup(proofByProvider, header, service.proofRunner(source, proofByProvider, header, certificate))
		if !provider.attempted {
			return credentialAdmission{}, wait, nil
		}
		if provider.rejected != nil {
			service.retireLocked(false)
			return credentialAdmission{}, nil, credentialFault(provider.rejected)
		}
		if provider.outcome == proofProven {
			principal, from = provider.principal, principalProvider
		} else {
			service.warnProofHostLocked(source, provider)
		}
	}
	principalHash := sha256.Sum256([]byte(principal))
	epoch.initialized, epoch.fingerprint, epoch.principal, epoch.known, epoch.source = true, fingerprint, principalHash, principal != "", from
	if from == principalReview {
		epoch.reviewPrincipal, epoch.reviewFingerprint = principal, fingerprint
	}
	epoch.requestGeneration.Add(1)
	epoch.certificate = certificate
	service.serveLocked(source, header)
	history := fingerprint
	if epoch.known {
		history = principalHash
	}
	epoch.history.Store(hex.EncodeToString(history[:]))
	return credentialAdmission{header: header, generation: epoch.requestGeneration.Load(), certificate: certificate}, nil, nil
}

// upgradeUnknownLocked proves the token an unknown epoch already serves. When
// a proof succeeds later, the epoch is marked ready to restart: reads keep
// using the same token, which is now proven, and the next commit or cluster
// lookup ends the epoch quietly. The next one starts proven, under the capture
// mode, lease and storage keys a fresh start computes, so it shares them with
// every replica that booted healthy; the registry hands it the warm inventory,
// so nothing is collected again. Review is asked first; the provider only when
// the cluster does not offer review.
func (service *Service) upgradeUnknownLocked(source provenCredentialSource, header string, proofs *credentialProofs, committing bool) error {
	epoch := service.identity
	if epoch.upgradeReady {
		if committing {
			return service.endUpgradedLocked()
		}
		return nil
	}
	certificate := epoch.certificate
	proven := false
	var principal string
	var rejected error
	for _, kind := range []proofKind{proofByReview, proofByProvider} {
		slot, _ := proofs.lookup(kind, header, service.proofRunner(source, kind, header, certificate))
		if slot.rejected != nil {
			rejected = slot.rejected
			break
		}
		if slot.attempted && slot.outcome == proofProven {
			// The successor reads the principal from the cached proof.
			proven, principal = true, slot.principal
			break
		}
		if kind == proofByProvider {
			// The host can stop answering after start.
			service.warnProofHostLocked(source, slot)
		}
		if !slot.attempted || slot.outcome != proofNotApplicable {
			// The provider principal is used only when the cluster does
			// not offer identity review at all.
			break
		}
	}
	if rejected != nil {
		service.retireLocked(false)
		return credentialFault(rejected)
	}
	if !proven {
		return nil
	}
	// The epoch's keys were computed for an unknown identity, and no restart
	// or peer replica computes them again, so it ends before anything more is
	// written under them.
	epoch.upgradeReady = true
	epoch.upgradePrincipal = sha256.Sum256([]byte(principal))
	logging.Debugf("kubernetes: cluster %q credential identity proved after start; the epoch restarts under the proven identity", service.scope.ClusterID)
	if committing {
		return service.endUpgradedLocked()
	}
	return nil
}

// endUpgradedLocked ends an epoch marked ready to restart: no renewal backoff,
// no warning. The next epoch starts from the cached proof.
func (service *Service) endUpgradedLocked() error {
	service.identity.upgraded = true
	service.retireLocked(false)
	return commitConflictError{cause: errEpochUpgraded}
}

// endIfUpgradeReady ends the epoch when its token was proven after start. The
// registry calls it before handing the service to a request, and its
// supervisor every cycle, so the request runs on the successor instead of
// failing.
func (service *Service) endIfUpgradeReady() bool {
	if service == nil || service.identity == nil {
		return false
	}
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	if epoch.upgraded {
		// A commit ended it first: the request still goes to the successor.
		return true
	}
	if !epoch.upgradeReady || epoch.retired {
		return false
	}
	_ = service.endUpgradedLocked()
	return true
}

// errEpochUpgraded ends an unknown epoch whose token was proven after start.
// It is not a credential failure.
var errEpochUpgraded = errors.New("kubernetes: credential identity proven; epoch restarts under it")

// warnProofHostLocked warns, once per throttle window, when the provider proof
// of a never-proven epoch fails for a reason a setting must fix: its host does
// not answer, or the key URL it publishes is not on the cloud's allowed hosts.
func (service *Service) warnProofHostLocked(source provenCredentialSource, slot proofSlot) {
	if !slot.attempted || slot.outcome != proofUnavailable || source.proofHost() == "" {
		return
	}
	key := problemKey(service.scope.OrgID, service.scope.ClusterID, "proof_host")
	switch failure := (&proofError{class: slot.class}); {
	case failure.unreachable():
		problems().Fail(slog.LevelWarn, key, "unreachable",
			"kubernetes: cluster %q cannot reach the identity proof host %s (unreachable); allow outbound HTTPS to it so credential renewals keep collection continuous", service.scope.ClusterID, source.proofHost())
	case slot.class == "jwks_host_not_allowed":
		problems().Fail(slog.LevelWarn, key, "host_not_allowed",
			"kubernetes: cluster %q identity proof keys are published on a host outside the allowed hosts for %s (host not allowed); check that the connector's cloud environment matches the cluster's tenant", service.scope.ClusterID, source.proofHost())
	}
}

// promoteLocked makes a proven candidate the serving token.
func (service *Service) promoteLocked(source provenCredentialSource, header string, fingerprint [32]byte, certificate *tls.Certificate) {
	epoch := service.identity
	epoch.fingerprint = fingerprint
	epoch.requestGeneration.Add(1)
	epoch.certificate = certificate
	service.serveLocked(source, header)
	service.client.noteIdentityKept(service.scope.ClusterID)
	if epoch.proofFailing {
		epoch.proofFailing = false
		if problems().Clear(problemKey(service.scope.OrgID, service.scope.ClusterID, "credential_proof"), "kubernetes: cluster %q credential proof recovered", service.scope.ClusterID) {
			logging.Infof("kubernetes: cluster %q credential renewal proved again; requests resumed with the renewed credential", service.scope.ClusterID)
		}
		return
	}
	logging.Debugf("kubernetes: cluster %q renewed credential proved for the same identity", service.scope.ClusterID)
}

// serveLocked records header as the token requests send, with its expiry.
func (service *Service) serveLocked(source provenCredentialSource, header string) {
	epoch := service.identity
	epoch.servingHeader, epoch.servingExpires, epoch.servingRejected = header, source.credentialExpiry(header), false
}

// graceLocked reports whether the previous token may still serve: it was
// proven for this epoch's principal, the cluster has not answered it 401,
// and it is more than proofGraceMargin from its recorded expiry. Nothing
// extends the bound.
func (service *Service) graceLocked(now time.Time) bool {
	epoch := service.identity
	return epoch.known && epoch.servingHeader != "" && !epoch.servingRejected && !epoch.servingExpires.IsZero() && now.Before(epoch.servingExpires.Add(-proofGraceMargin))
}

// proofFailedLocked fails a request fast once grace is over, warning once
// per throttle window with the cause and what to check.
func (service *Service) proofFailedLocked(source provenCredentialSource, kind proofKind, class string, now time.Time) error {
	epoch := service.identity
	if !epoch.proofFailing {
		epoch.proofFailing, epoch.proofFailingSince = true, now
	}
	failure := &proofError{class: class}
	epoch.proofFailingCode = failure.Code()
	check := "check that the cluster API server answers identity reviews"
	switch {
	case class == "clock_skew":
		check = "check that this server's clock is synchronized (NTP); the renewed token's validity times disagree with it"
	case kind == proofByProvider && source.proofHost() != "":
		check = "check that Versus can reach " + source.proofHost()
	}
	problems().Fail(slog.LevelWarn, problemKey(service.scope.OrgID, service.scope.ClusterID, "credential_proof"), class,
		"kubernetes: cluster %q cannot prove its renewed credential (%s); requests pause until the proof succeeds, and nothing is rebuilt. %s", service.scope.ClusterID, class, check)
	return credentialFault(failure)
}

// retireLocked ends the epoch. renewal marks a retirement caused by a token
// change, which the rebuild treats as a renewal.
func (service *Service) retireLocked(renewal bool) {
	epoch := service.identity
	if renewal {
		epoch.renewal = true
	}
	epoch.retired = true
	epoch.cancel()
}

// proofRunner returns the proof call for one token and kind.
func (service *Service) proofRunner(source provenCredentialSource, kind proofKind, header string, certificate *tls.Certificate) func(context.Context) (string, proofOutcome, error) {
	client := service.client
	if kind == proofByProvider {
		return func(ctx context.Context) (string, proofOutcome, error) {
			return source.verifiedCredentialPrincipal(ctx, header)
		}
	}
	return func(ctx context.Context) (string, proofOutcome, error) {
		principal, err := client.selfSubjectPrincipal(ctx, header, certificate)
		switch {
		case err == nil && principal != "":
			return principal, proofProven, nil
		case err == nil, errors.Is(err, errSelfSubjectReviewUnsupported):
			return "", proofNotApplicable, nil
		case errors.Is(err, ErrUnauthorized):
			return "", proofUnavailable, err
		}
		return "", proofUnavailable, err
	}
}

// noteRejectedCredential records that the cluster answered header with 401.
// A serving token answered that way can no longer bridge a renewal.
func (service *Service) noteRejectedCredential(header string) {
	if service == nil || service.identity == nil || header == "" {
		return
	}
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(header), []byte(epoch.servingHeader)) == 1 {
		epoch.servingRejected = true
	}
}

// proofStatus returns the capture status reason a failing proof sets, and
// since when, or "" when no proof is failing.
func (service *Service) proofStatus() (string, time.Time) {
	if service == nil || service.identity == nil || service.client == nil {
		return "", time.Time{}
	}
	if _, ok := service.client.credentials.(provenCredentialSource); !ok {
		return "", time.Time{}
	}
	epoch := service.identity
	epoch.mu.Lock()
	failing, code, since, known := epoch.proofFailing, epoch.proofFailingCode, epoch.proofFailingSince, epoch.known
	epoch.mu.Unlock()
	if failing {
		return code, since
	}
	if !known {
		switch problem := service.client.proofCache().problem(); {
		case problem == "jwks_host_not_allowed":
			return "proof_host_not_allowed", time.Time{}
		case problem != "":
			return "proof_unreachable", time.Time{}
		}
	}
	return "", time.Time{}
}
