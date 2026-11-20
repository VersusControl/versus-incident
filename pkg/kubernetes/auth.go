package kubernetes

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxCredentialFileBytes = int64(1 << 20)

var errSelfSubjectReviewUnsupported = errors.New("kubernetes: self subject review unsupported")

// ErrTokenEndpointUnavailable marks a cloud token request that got no
// definite answer: the connection failed or timed out, or the host answered
// 429 or 5xx. The configured credential may be fine, so the request is retried
// with backoff until the host answers. Any other failure (a 4xx such as
// invalid_client, a missing token file, an invalid answer) is a definite one.
var ErrTokenEndpointUnavailable = errors.New("kubernetes: cloud token endpoint unavailable")

// tokenEndpointUnavailable classifies a cloud token request. It returns an
// error wrapping ErrTokenEndpointUnavailable when the request got no definite
// answer, and nil when response is a definite answer for the caller to read.
// A failed request drops the client's idle connections, so the next attempt
// dials again and resolves the host again rather than reusing a connection
// opened while the network was still coming up.
// abandonedAttempt reports whether err only says that the caller stopped
// waiting. A caller that cancelled is never evidence about the credential. A
// deadline that passed while the token host had not answered is: the token
// request got no answer in time, which is a token endpoint failure whichever
// deadline (the client's, the attempt's or the request's) ran out first.
func abandonedAttempt(ctx context.Context, err error) bool {
	if ctx.Err() == nil {
		return false
	}
	return errors.Is(ctx.Err(), context.Canceled) || !errors.Is(err, ErrTokenEndpointUnavailable)
}

func tokenEndpointUnavailable(client *http.Client, response *http.Response, err error, failure string) error {
	if err == nil && response != nil && response.StatusCode != http.StatusTooManyRequests && response.StatusCode < 500 {
		return nil
	}
	if client != nil {
		client.CloseIdleConnections()
	}
	return fmt.Errorf("%s: %w", failure, ErrTokenEndpointUnavailable)
}

// CredentialSource supplies request and TLS credentials without exposing them
// through connector configuration or status DTOs.
type CredentialSource interface {
	Authorization(context.Context) (string, error)
	ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error)
}

type credentialSource struct {
	authorization func(context.Context) (string, error)
	certificate   func(*tls.CertificateRequestInfo) (*tls.Certificate, error)
}

// tokenWaitKey carries how long a caller waits for a token refresh in flight
// when it has no token to send, shorter than its own deadline. A user
// request sets it, so it is answered at once that the token host has not
// answered yet instead of waiting out the refresh.
type tokenWaitKey struct{}

func withTokenWait(ctx context.Context, limit time.Duration) context.Context {
	return context.WithValue(ctx, tokenWaitKey{}, limit)
}

// tokenRefreshTimeout bounds one background token refresh, the same bound a
// cloud token client puts on one request.
const tokenRefreshTimeout = 10 * time.Second

type expiringBearerSource struct {
	mu                sync.Mutex
	refresh           func(context.Context) (string, time.Time, error)
	now               func() time.Time
	skew              time.Duration
	header            string
	expires           time.Time
	principal         string
	principalVerified bool
	retryAt           time.Time
	failures          int
	// unreachable is set while the last refresh got no definite answer from
	// the token host, so a call inside its backoff says the same.
	unreachable bool
	// inflight is closed when the refresh running now ends; nil when none
	// runs. Refreshes run on lifetime, which closeSource cancels.
	inflight       chan struct{}
	lifetime       context.Context
	stop           context.CancelFunc
	closed         bool
	refreshTimeout time.Duration
	current        atomic.Pointer[expiringToken]
}

// expiringToken is the current token and its expiry, read without the
// refresh lock so the epoch can record a token's expiry without waiting on a
// refresh in progress.
type expiringToken struct {
	header  string
	expires time.Time
}

// rotating reports that the source renews its token on its own.
func (source *expiringBearerSource) rotating() bool { return true }

// credentialExpiry is header's recorded expiry, or zero when header is not
// the current token.
func (source *expiringBearerSource) credentialExpiry(header string) time.Time {
	current := source.current.Load()
	if current == nil || header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(current.header)) != 1 {
		return time.Time{}
	}
	return current.expires
}

// currentCredential reports whether header is the current token. A source
// that has issued none yet accepts any header, as a test or a constructor
// presenting its own token does.
func (source *expiringBearerSource) currentCredential(header string) bool {
	current := source.current.Load()
	return current == nil || subtle.ConstantTimeCompare([]byte(header), []byte(current.header)) == 1
}

func (source *expiringBearerSource) credentialPrincipal() string {
	if !source.principalVerified {
		return ""
	}
	return source.principal
}

type credentialEpoch struct {
	certificate            *tls.Certificate
	requestGeneration      atomic.Uint64
	history                atomic.Value
	reviewPrincipal        string
	reviewFingerprint      [32]byte
	reviewUnsupported      bool
	unsupportedFingerprint [32]byte
	mu                     sync.Mutex
	ctx                    context.Context
	cancel                 context.CancelFunc
	initialized            bool
	fingerprint            [32]byte
	principal              [32]byte
	known                  bool
	retired                bool
	renewal                bool
	// source is where a proved source's principal came from; it never
	// changes within an epoch.
	source principalSource
	// servingHeader is the token requests send for a proved source, with its
	// recorded expiry; servingRejected is set once the cluster answered it 401.
	servingHeader   string
	servingExpires  time.Time
	servingRejected bool
	// upgradeReady marks an unknown epoch whose token was proven after
	// start; the next commit or cluster lookup ends it. upgraded marks it
	// ended that way; its successor starts with no backoff and keeps the
	// warm inventory.
	upgradeReady bool
	upgraded     bool
	// unreachable is set when the epoch was retired at its start because the
	// token host gave no definite answer, so a later check of the retired
	// epoch keeps saying the host is retrying rather than naming the
	// credential unavailable for good.
	unreachable bool
	// upgradePrincipal is the digest of the principal the late proof
	// returned; a successor on renewed bytes takes the warm inventory only
	// when it is proven for this same principal.
	upgradePrincipal [32]byte
	// proofFailing is set while requests fail because a renewal cannot be
	// proved, with the status code and since when.
	proofFailing      bool
	proofFailingCode  string
	proofFailingSince time.Time
}

type credentialEpochContextKey struct{}

func (service *Service) captureCredentialID() string {
	return service.captureCredentialIDAtGeneration(service.client.authGeneration.Load())
}

// Recovery generations fence process-local reads, not persistent identity.
// Generation zero retains the existing initial-boot storage key format.
func (service *Service) captureCredentialIDAtGeneration(generation uint64) string {
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	identity := epoch.fingerprint
	mode := "credential"
	if service.client.credentials == nil {
		mode = "anonymous"
	} else if epoch.known {
		identity = epoch.principal
		mode = "provider-principal"
	}
	endpoint := ""
	if service.client.base != nil {
		endpoint = service.client.base.String()
	}
	encoded, _ := json.Marshal([]string{"capture-scope-v1", endpoint, service.configuredCredentialID, mode, hex.EncodeToString(identity[:]), strconv.FormatUint(generation, 10)})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// credentialFailure marks a failure of the credential itself: it could not be
// resolved, refreshed, proved or accepted. It is the only kind of capture
// failure, beside the sentinel credential answers, that may close the
// credential recovery gate. The cause keeps its own meaning and text.
type credentialFailure struct{ cause error }

func (err *credentialFailure) Error() string { return err.cause.Error() }
func (err *credentialFailure) Unwrap() error { return err.cause }

// credentialFault types a failure raised while resolving or accepting a
// credential, including a timeout inside that work.
func credentialFault(err error) error {
	var typed *credentialFailure
	if err == nil || errors.As(err, &typed) {
		return err
	}
	return &credentialFailure{cause: err}
}

func (service *Service) credentialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bound, cancel := context.WithCancel(context.WithValue(ctx, credentialEpochContextKey{}, service))
	stop := context.AfterFunc(service.identity.ctx, cancel)
	return bound, func() { stop(); cancel() }
}

func (service *Service) validateCredential(ctx context.Context) error {
	if service == nil {
		return ErrInvalidArguments
	}
	if service.changes != nil {
		service.changes.mu.Lock()
		stopped := service.changes.stopped[service.indexScope()]
		service.changes.mu.Unlock()
		if stopped {
			return ErrClusterUnavailable
		}
	}
	if service.identity == nil || service.client == nil || service.client.credentials == nil {
		return ctx.Err()
	}
	epoch := service.identity
	epoch.mu.Lock()
	if epoch.retired || epoch.ctx.Err() != nil {
		defer epoch.mu.Unlock()
		return retiredEpochErrorLocked(epoch)
	}
	epoch.mu.Unlock()
	// The token is fetched off the epoch lock, so a caller waiting on a
	// token host never holds up the others; its wait ends on its own
	// deadline.
	header, err := service.client.credentials.Authorization(ctx)
	epoch.mu.Lock()
	if epoch.retired || epoch.ctx.Err() != nil {
		defer epoch.mu.Unlock()
		return retiredEpochErrorLocked(epoch)
	}
	if err != nil {
		defer epoch.mu.Unlock()
		if abandonedAttempt(ctx, err) {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) {
			epoch.retired = true
			epoch.cancel()
		}
		if errors.Is(err, ErrTokenEndpointUnavailable) {
			// The token host gave no definite answer: the fault keeps saying so,
			// so it is reported as retrying rather than as a credential to fix.
			return credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
		}
		return ErrCredentialUnavailable
	}
	if source, ok := service.client.credentials.(provenCredentialSource); ok {
		// A proof may be in flight; it is waited for off the epoch lock.
		epoch.mu.Unlock()
		_, err := service.admitProven(ctx, source, header)
		return err
	}
	defer epoch.mu.Unlock()
	return service.acceptCredentialLocked(ctx, header)
}

// retiredEpochErrorLocked is the answer of a retired epoch, which keeps the
// token endpoint cause when the token host gave no answer.
func retiredEpochErrorLocked(epoch *credentialEpoch) error {
	epoch.retired = true
	if epoch.unreachable {
		return credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
	}
	return ErrCredentialUnavailable
}

func (service *Service) acceptCredential(ctx context.Context, header string) error {
	_, _, _, err := service.acceptCredentialForRequest(ctx, header)
	return err
}

// acceptCredentialForRequest admits header for one request and returns the
// header the request must send. For a source whose renewals are proved that
// can be the previous, proven token while header is still a candidate.
func (service *Service) acceptCredentialForRequest(ctx context.Context, header string) (string, uint64, *tls.Certificate, error) {
	if source, ok := service.client.credentials.(provenCredentialSource); ok {
		admission, err := service.admitProven(ctx, source, header)
		if err != nil {
			return "", 0, nil, err
		}
		return admission.header, admission.generation, admission.certificate, nil
	}
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	if err := service.acceptCredentialLocked(ctx, header); err != nil {
		return "", 0, nil, err
	}
	return header, epoch.requestGeneration.Load(), epoch.certificate, nil
}

// commitConflictError reports that a listing could not be committed because the
// credential epoch changed while it was being read. The listing was not wrong;
// it belongs to a retired identity, so the next cycle has to read it again.
type commitConflictError struct{ cause error }

func (err commitConflictError) Error() string {
	return "kubernetes: credential epoch changed before commit"
}
func (err commitConflictError) Unwrap() error { return err.cause }
func (err commitConflictError) Class() string { return "commit_conflict" }

func (service *Service) commitCredential(ctx context.Context, commit func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	epoch := service.identity
	// The token is fetched before the epoch lock is taken, so a commit that
	// waits on a token host never holds up the readers behind that lock.
	var header string
	var authErr error
	credentials := service.client != nil && service.client.credentials != nil
	if credentials {
		header, authErr = service.client.credentials.Authorization(ctx)
	}
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	// The epoch lock is shared by every writer. A deadline spent waiting for it
	// is contention, not a credential answer, so it is returned before the
	// credential is consulted and stays untyped.
	if err := ctx.Err(); err != nil {
		return err
	}
	if epoch.retired || epoch.ctx.Err() != nil {
		return commitConflictError{cause: ErrCredentialUnavailable}
	}
	if credentials {
		if authErr != nil {
			return authErr
		}
		if source, ok := service.client.credentials.(provenCredentialSource); ok {
			// A commit never waits on a proof: with no token allowed to
			// serve yet, it fails fast and is retried.
			_, wait, err := service.admitProvenLocked(ctx, source, header, true)
			if err != nil {
				return err
			}
			if wait != nil {
				return credentialFault(&proofError{class: "pending"})
			}
		} else if err := service.acceptCredentialLocked(ctx, header); err != nil {
			return err
		}
	}
	if service.client != nil {
		service.client.authMu.Lock()
		defer service.client.authMu.Unlock()
		if epoch.ctx.Err() != nil {
			return commitConflictError{cause: ErrCredentialUnavailable}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return commit()
}

func (service *Service) acceptCredentialLocked(ctx context.Context, header string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	epoch := service.identity
	if epoch.retired || epoch.ctx.Err() != nil {
		return ErrCredentialUnavailable
	}
	certificate, err := service.client.credentials.ClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		epoch.retired = true
		epoch.cancel()
		return ErrCredentialUnavailable
	}
	fingerprint := credentialFingerprint(header, certificate)
	principal := ""
	if source, ok := service.client.credentials.(interface {
		verifiedCredentialPrincipal(context.Context, string) (string, proofOutcome, error)
	}); ok {
		principal, _, err = source.verifiedCredentialPrincipal(ctx, header)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				epoch.retired = true
				epoch.cancel()
				return credentialFault(err)
			}
			// On a review-based epoch the provider proof is not this epoch's
			// principal source: its failure changes nothing, and review
			// decides below.
			if epoch.reviewPrincipal == "" {
				return credentialFault(err)
			}
			principal = ""
		}
	} else if source, ok := service.client.credentials.(interface{ credentialPrincipal() string }); ok {
		principal = source.credentialPrincipal()
	}
	previousReview := epoch.reviewPrincipal
	// A Kubernetes-authenticated UserInfo includes groups, UID and extras.
	// A same subject or provider display name alone cannot prove equivalence.
	if epoch.reviewPrincipal != "" && subtle.ConstantTimeCompare(fingerprint[:], epoch.reviewFingerprint[:]) == 1 {
		principal = epoch.reviewPrincipal
	} else if !epoch.reviewUnsupported || subtle.ConstantTimeCompare(fingerprint[:], epoch.unsupportedFingerprint[:]) != 1 {
		reviewed, reviewErr := service.client.selfSubjectPrincipal(ctx, header, certificate)
		if reviewErr != nil {
			if errors.Is(reviewErr, errSelfSubjectReviewUnsupported) {
				epoch.reviewUnsupported, epoch.unsupportedFingerprint = true, fingerprint
			}
			if errors.Is(reviewErr, ErrUnauthorized) {
				epoch.retired = true
				epoch.cancel()
				return credentialFault(reviewErr)
			}
			if epoch.reviewPrincipal != "" || ctx.Err() != nil {
				return credentialFault(reviewErr)
			}
			// A server without identity-review support can still collect under
			// a byte-bound epoch. It cannot coalesce an unproved bearer change.
		}
		if reviewed != "" {
			principal = reviewed
			epoch.reviewPrincipal, epoch.reviewFingerprint = reviewed, fingerprint
		} else if epoch.reviewPrincipal != "" {
			// A previously authenticated principal must not be silently replaced
			// by a weaker provider hint when review becomes unavailable.
			return ErrCredentialUnavailable
		}
	} else if epoch.reviewPrincipal != "" {
		return ErrCredentialUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if epoch.ctx.Err() != nil {
		epoch.retired = true
		return ErrCredentialUnavailable
	}
	principalHash := sha256.Sum256([]byte(principal))
	known := principal != ""
	sameFingerprint := subtle.ConstantTimeCompare(fingerprint[:], epoch.fingerprint[:]) == 1
	samePrincipal := subtle.ConstantTimeCompare(principalHash[:], epoch.principal[:]) == 1
	if epoch.initialized && ((!sameFingerprint && !(known && epoch.known && samePrincipal)) || known != epoch.known || !samePrincipal) {
		if known && epoch.known && !samePrincipal {
			service.client.noteIdentityChange(service.scope.ClusterID, changedIdentityFields(previousReview, principal))
		}
		epoch.renewal = true
		epoch.retired = true
		epoch.cancel()
		return ErrCredentialUnavailable
	}
	if !epoch.initialized || !sameFingerprint {
		if epoch.initialized && known {
			service.client.noteIdentityKept(service.scope.ClusterID)
		}
		epoch.requestGeneration.Add(1)
	}
	epoch.initialized, epoch.fingerprint, epoch.principal, epoch.known = true, fingerprint, principalHash, known
	epoch.certificate = certificate
	history := fingerprint
	if known {
		history = principalHash
	}
	epoch.history.Store(hex.EncodeToString(history[:]))
	return ctx.Err()
}

func (client *Client) certificateHTTP(certificate *tls.Certificate, bases ...*http.Client) (*http.Client, func(), error) {
	if client == nil {
		return nil, nil, ErrCredentialUnavailable
	}
	base := client.http
	if len(bases) > 0 {
		base = bases[0]
	}
	if base == nil {
		return nil, nil, ErrCredentialUnavailable
	}
	if certificate == nil || len(certificate.Certificate) == 0 {
		return base, func() {}, nil
	}
	transport, ok := base.Transport.(*http.Transport)
	if !ok {
		return nil, nil, ErrCredentialUnavailable
	}
	pinned := transport.Clone()
	if pinned.TLSClientConfig == nil {
		pinned.TLSClientConfig = &tls.Config{}
	} else {
		pinned.TLSClientConfig = pinned.TLSClientConfig.Clone()
	}
	pinned.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return certificate, nil }
	pinned.TLSClientConfig.Certificates = nil
	pinned.DisableKeepAlives = true
	httpClient := *base
	httpClient.Transport = pinned
	return &httpClient, pinned.CloseIdleConnections, nil
}

// reviewedUserInfo is the authenticated identity a SelfSubjectReview returns.
type reviewedUserInfo struct {
	Username string              `json:"username"`
	UID      string              `json:"uid"`
	Groups   []string            `json:"groups"`
	Extra    map[string][]string `json:"extra"`
}

func (client *Client) selfSubjectPrincipal(ctx context.Context, header string, certificate *tls.Certificate) (string, error) {
	if client == nil || client.http == nil {
		return "", nil
	}
	target, err := client.requestURL("/apis/authentication.k8s.io/v1/selfsubjectreviews")
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, min(client.timeout, 5*time.Second))
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`))
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	request.Header.Set("Authorization", header)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	reviewer, closeTransport, err := client.certificateHTTP(certificate)
	if err != nil {
		return "", err
	}
	defer closeTransport()
	response, err := reviewer.Do(request)
	if err != nil {
		// The typed failure still unwraps to ErrCredentialUnavailable.
		return "", proofTransportError(err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden:
		return "", errSelfSubjectReviewUnsupported
	case http.StatusUnauthorized:
		return "", ErrUnauthorized
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 {
		return "", proofStatusError(response, time.Now())
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", ErrCredentialUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil || len(raw) > 64<<10 {
		return "", ErrCredentialUnavailable
	}
	var result struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Status     struct {
			UserInfo reviewedUserInfo `json:"userInfo"`
		} `json:"status"`
	}
	if json.Unmarshal(raw, &result) != nil || result.APIVersion != "authentication.k8s.io/v1" || result.Kind != "SelfSubjectReview" || result.Status.UserInfo.Username == "" {
		return "", ErrCredentialUnavailable
	}
	user := result.Status.UserInfo
	if user.Username == "system:anonymous" {
		return "", ErrUnauthorized
	}
	// This standard field identifies the token, not its authenticated
	// principal; retaining it would rebuild on every projected-token renewal.
	delete(user.Extra, "authentication.kubernetes.io/credential-id")
	// Providers whose authenticator reports per-session fields remove them
	// here, so a routine credential refresh keeps the same principal.
	if normalizer, ok := client.credentials.(interface{ normalizeReviewedUser(*reviewedUserInfo) }); ok {
		normalizer.normalizeReviewedUser(&user)
	}
	sort.Strings(user.Groups)
	for key := range user.Extra {
		sort.Strings(user.Extra[key])
	}
	identity, err := json.Marshal(user)
	if err != nil {
		return "", ErrCredentialUnavailable
	}
	return "self-subject-review:" + string(identity), nil
}

// Authorization returns the current token while it is valid with a margin
// before its expiry, and never waits on a refresh to do so. A refresh starts
// in the background when the token enters its renewal window; it runs on the
// source's own context, bounded by tokenRefreshTimeout, so its outcome is
// always recorded and its backoff spaces the next one. A caller with no
// servable token waits for the refresh in flight up to its own deadline; at
// that deadline it is told the token host has not answered, and nothing is
// recorded. A failed refresh with no servable token answers its recorded
// class at once.
func (source *expiringBearerSource) Authorization(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source.mu.Lock()
	now := source.now()
	if source.servableLocked(now) {
		header := source.header
		source.startRefreshLocked(now)
		source.mu.Unlock()
		return header, nil
	}
	source.startRefreshLocked(now)
	wait := source.inflight
	if wait == nil {
		err := source.recordedErrorLocked()
		source.mu.Unlock()
		return "", err
	}
	source.mu.Unlock()
	var bound <-chan time.Time
	if limit, ok := ctx.Value(tokenWaitKey{}).(time.Duration); ok && limit > 0 {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		bound = timer.C
	}
	select {
	case <-wait:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", ctx.Err()
		}
		return "", credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
	case <-bound:
		return "", credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.servableLocked(source.now()) {
		return source.header, nil
	}
	return "", source.recordedErrorLocked()
}

// servableLocked reports whether the current token may be sent: it was not
// rejected and stays valid for proofGraceMargin, so transit time or a
// cluster clock a little ahead cannot turn it into a rejection.
func (source *expiringBearerSource) servableLocked(now time.Time) bool {
	return source.header != "" && now.Add(proofGraceMargin).Before(source.expires)
}

// startRefreshLocked starts one background refresh when the token is
// missing or inside its renewal window, none is running, the backoff has
// passed and the source is open.
func (source *expiringBearerSource) startRefreshLocked(now time.Time) {
	renewAt := max(source.skew, proofGraceMargin)
	if source.header != "" && now.Add(renewAt).Before(source.expires) {
		return
	}
	if source.inflight != nil || now.Before(source.retryAt) || source.closed {
		return
	}
	if source.lifetime == nil {
		source.lifetime, source.stop = context.WithCancel(context.Background())
	}
	timeout := source.refreshTimeout
	if timeout <= 0 {
		timeout = tokenRefreshTimeout
	}
	ctx, cancel := context.WithTimeout(source.lifetime, timeout)
	done := make(chan struct{})
	source.inflight = done
	go func() {
		defer cancel()
		token, expires, err := source.refresh(ctx)
		source.mu.Lock()
		defer close(done)
		defer source.mu.Unlock()
		source.inflight = nil
		source.recordLocked(ctx, token, expires, err)
	}()
}

// recordLocked records a refresh outcome. A refresh the source's Close ended
// records nothing.
func (source *expiringBearerSource) recordLocked(ctx context.Context, token string, expires time.Time, err error) {
	now := source.now()
	if err == nil {
		header, headerErr := bearerHeader(token)
		if headerErr != nil || !expires.After(now) {
			err = errors.New("kubernetes: credential provider returned an invalid token")
		} else {
			if now.Add(proofGraceMargin).Before(expires) {
				source.header, source.expires = header, expires
				source.current.Store(&expiringToken{header: header, expires: expires})
			}
			if now.Add(max(source.skew, proofGraceMargin)).Before(expires) {
				source.retryAt, source.failures, source.unreachable = time.Time{}, 0, false
				return
			}
			// A token that arrives already inside its renewal window would
			// start the next refresh at once. It counts as an invalid answer
			// from the provider, so the backoff spaces the next refresh.
			err = errors.New("kubernetes: credential provider returned a token that expires too soon")
		}
	}
	if source.closed {
		return
	}
	source.failures++
	source.retryAt = now.Add(time.Second * time.Duration(1<<min(source.failures-1, 6)))
	// No answer from the host (a transport error, the refresh's own timeout,
	// 429 or 5xx) is retried as a host outage; anything else is definite.
	source.unreachable = errors.Is(err, ErrTokenEndpointUnavailable) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil
}

// pendingError is nil while the current token may be sent. Otherwise it is
// what a caller would be told now: the token class while a refresh waits on
// the token host, is about to start, or the host gave no answer; the
// recorded definite answer during its backoff.
func (source *expiringBearerSource) pendingError() error {
	source.mu.Lock()
	defer source.mu.Unlock()
	now := source.now()
	if source.servableLocked(now) {
		return nil
	}
	if source.inflight == nil && now.Before(source.retryAt) && !source.unreachable {
		return source.recordedErrorLocked()
	}
	return credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
}

// recordedErrorLocked is the answer while no token is servable and no
// refresh is running.
func (source *expiringBearerSource) recordedErrorLocked() error {
	if source.unreachable {
		return credentialFault(fmt.Errorf("%w: %w", ErrCredentialUnavailable, ErrTokenEndpointUnavailable))
	}
	return credentialFault(ErrCredentialUnavailable)
}

// closeSource stops the refresh in flight and any later one.
func (source *expiringBearerSource) closeSource() {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.closed = true
	if source.stop != nil {
		source.stop()
	}
}

// rejectBearer invalidates only the token that actually received the rejection.
// A refresh in progress cannot be invalidated by it: its newer token is kept.
func (source *expiringBearerSource) rejectBearer(header string) bool {
	source.mu.Lock()
	defer source.mu.Unlock()
	if header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(source.header)) != 1 {
		return false
	}
	source.header, source.expires = "", time.Time{}
	source.current.Store(nil)
	return true
}

func (source *expiringBearerSource) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

func (source *credentialSource) Authorization(ctx context.Context) (string, error) {
	if source == nil || source.authorization == nil {
		return "", nil
	}
	header, err := source.authorization(ctx)
	return header, credentialFault(err)
}

// rotating reports false: a static token or certificate changes only when
// the operator changes it.
func (source *credentialSource) rotating() bool { return false }

func (source *credentialSource) ClientCertificate(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if source == nil || source.certificate == nil {
		return new(tls.Certificate), nil
	}
	return source.certificate(info)
}

func staticTokenCredential(token string) (CredentialSource, error) {
	header, err := bearerHeader(token)
	if err != nil {
		return nil, err
	}
	return &credentialSource{authorization: func(context.Context) (string, error) { return header, nil }}, nil
}

func tokenFileCredential(path string) CredentialSource {
	return &tokenFileSource{path: path}
}

type tokenFileSource struct {
	path     string
	verifier *projectedTokenVerifier
}

func (source *tokenFileSource) Authorization(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := readBoundedFile(source.path, maxTokenBytes)
	if err != nil {
		return "", credentialFault(ErrCredentialUnavailable)
	}
	header, err := bearerHeader(string(data))
	return header, credentialFault(err)
}

func (source *tokenFileSource) rejectBearer(rejected string) bool {
	current, err := source.Authorization(context.Background())
	// An unavailable file still retires the rejected epoch. A newer file does not.
	return err != nil || subtle.ConstantTimeCompare([]byte(current), []byte(rejected)) == 1
}

func (source *tokenFileSource) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

// rotating reports that the token file is re-read and may change.
func (source *tokenFileSource) rotating() bool { return true }

func (source *tokenFileSource) verifiedCredentialPrincipal(ctx context.Context, header string) (string, proofOutcome, error) {
	if source.verifier == nil {
		return "", proofNotApplicable, nil
	}
	principal, err := source.verifier.verify(ctx, strings.TrimPrefix(header, "Bearer "))
	switch {
	case err != nil:
		return "", proofUnavailable, credentialFault(err)
	case principal == "":
		return "", proofNotApplicable, nil
	}
	return principal, proofProven, nil
}

func clientCertificateCredential(certificateFile, keyFile string, certificateData, keyData []byte) (CredentialSource, error) {
	if len(certificateData) > 0 || len(keyData) > 0 {
		if len(certificateData) == 0 || len(keyData) == 0 || certificateFile != "" || keyFile != "" {
			return nil, errors.New("kubernetes: invalid client certificate configuration")
		}
		certificate, err := tls.X509KeyPair(certificateData, keyData)
		if err != nil {
			return nil, errors.New("kubernetes: invalid client certificate")
		}
		return &credentialSource{certificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &certificate, nil }}, nil
	}
	if certificateFile == "" || keyFile == "" {
		return nil, errors.New("kubernetes: invalid client certificate configuration")
	}
	cache := &certificateFileCache{certificateFile: certificateFile, keyFile: keyFile}
	if _, err := cache.load(); err != nil {
		return nil, err
	}
	return &credentialSource{certificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cache.load() }}, nil
}

type certificateFileCache struct {
	mu              sync.Mutex
	certificateFile string
	keyFile         string
	certificate     *tls.Certificate
	certificateMod  int64
	keyMod          int64
}

func (cache *certificateFileCache) load() (*tls.Certificate, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	certificateInfo, err := os.Stat(cache.certificateFile)
	if err != nil {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	keyInfo, err := os.Stat(cache.keyFile)
	if err != nil || keyInfo.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	certificateMod := certificateInfo.ModTime().UnixNano()
	keyMod := keyInfo.ModTime().UnixNano()
	if cache.certificate != nil && cache.certificateMod == certificateMod && cache.keyMod == keyMod {
		return cache.certificate, nil
	}
	certificatePEM, err := readBoundedFile(cache.certificateFile, maxCredentialFileBytes)
	if err != nil {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	keyPEM, err := readBoundedFile(cache.keyFile, maxCredentialFileBytes)
	if err != nil {
		return nil, errors.New("kubernetes: client certificate unavailable")
	}
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, errors.New("kubernetes: invalid client certificate")
	}
	cache.certificate = &certificate
	cache.certificateMod = certificateMod
	cache.keyMod = keyMod
	return cache.certificate, nil
}

func bearerHeader(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > int(maxTokenBytes) || strings.ContainsAny(token, "\r\n") {
		return "", ErrCredentialUnavailable
	}
	return "Bearer " + token, nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("bounded file read failed")
	}
	return data, nil
}

func readSecureBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("credential file is not private")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, errors.New("bounded credential file read failed")
	}
	return data, nil
}
