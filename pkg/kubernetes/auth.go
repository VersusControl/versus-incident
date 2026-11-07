package kubernetes

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type expiringBearerSource struct {
	lockOnce          sync.Once
	lock              chan struct{}
	refresh           func(context.Context) (string, time.Time, error)
	now               func() time.Time
	skew              time.Duration
	header            string
	expires           time.Time
	principal         string
	principalVerified bool
	retryAt           time.Time
	failures          int
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
	defer epoch.mu.Unlock()
	if epoch.retired || epoch.ctx.Err() != nil {
		epoch.retired = true
		return ErrCredentialUnavailable
	}
	header, err := service.client.credentials.Authorization(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) {
			epoch.retired = true
			epoch.cancel()
		}
		return ErrCredentialUnavailable
	}
	return service.acceptCredentialLocked(ctx, header)
}

func (service *Service) acceptCredential(ctx context.Context, header string) error {
	_, _, err := service.acceptCredentialForRequest(ctx, header)
	return err
}

func (service *Service) acceptCredentialForRequest(ctx context.Context, header string) (uint64, *tls.Certificate, error) {
	epoch := service.identity
	epoch.mu.Lock()
	defer epoch.mu.Unlock()
	if err := service.acceptCredentialLocked(ctx, header); err != nil {
		return 0, nil, err
	}
	return epoch.requestGeneration.Load(), epoch.certificate, nil
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
	if service.client != nil && service.client.credentials != nil {
		header, err := service.client.credentials.Authorization(ctx)
		if err != nil {
			return err
		}
		if err := service.acceptCredentialLocked(ctx, header); err != nil {
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
	digest := sha256.New()
	digest.Write([]byte(header))
	if certificate != nil {
		for _, encoded := range certificate.Certificate {
			digest.Write(encoded)
		}
	}
	var fingerprint [32]byte
	copy(fingerprint[:], digest.Sum(nil))
	principal := ""
	if source, ok := service.client.credentials.(interface {
		verifiedCredentialPrincipal(context.Context, string) (string, error)
	}); ok {
		principal, err = source.verifiedCredentialPrincipal(ctx, header)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				epoch.retired = true
				epoch.cancel()
			}
			return credentialFault(err)
		}
	} else if source, ok := service.client.credentials.(interface{ credentialPrincipal() string }); ok {
		principal = source.credentialPrincipal()
	}
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
		epoch.renewal = true
		epoch.retired = true
		epoch.cancel()
		return ErrCredentialUnavailable
	}
	if !epoch.initialized || !sameFingerprint {
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
		return "", ErrCredentialUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden:
		return "", errSelfSubjectReviewUnsupported
	case http.StatusUnauthorized:
		return "", ErrUnauthorized
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
func (source *expiringBearerSource) Authorization(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	source.lockOnce.Do(func() {
		source.lock = make(chan struct{}, 1)
		source.lock <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-source.lock:
	}
	defer func() { source.lock <- struct{}{} }()
	now := source.now()
	if now.Before(source.retryAt) {
		return "", credentialFault(ErrCredentialUnavailable)
	}
	if source.header != "" && now.Add(source.skew).Before(source.expires) {
		return source.header, nil
	}
	token, expires, err := source.refresh(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", credentialFault(err)
		}
		source.failures++
		source.retryAt = now.Add(time.Second * time.Duration(1<<min(source.failures-1, 6)))
		return "", credentialFault(ErrCredentialUnavailable)
	}
	header, err := bearerHeader(token)
	if err != nil || !expires.After(now) {
		return "", credentialFault(errors.New("kubernetes: credential provider returned an invalid token"))
	}
	source.header = header
	source.expires = expires
	source.retryAt, source.failures = time.Time{}, 0
	return header, nil
}

// rejectBearer invalidates only the token that actually received the rejection.
// Waiting for an in-progress refresh cannot invalidate its newer result.
func (source *expiringBearerSource) rejectBearer(header string) bool {
	source.lockOnce.Do(func() { source.lock = make(chan struct{}, 1); source.lock <- struct{}{} })
	<-source.lock
	defer func() { source.lock <- struct{}{} }()
	if header == "" || subtle.ConstantTimeCompare([]byte(header), []byte(source.header)) != 1 {
		return false
	}
	source.header, source.expires = "", time.Time{}
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

func (source *tokenFileSource) verifiedCredentialPrincipal(ctx context.Context, header string) (string, error) {
	if source.verifier == nil {
		return "", nil
	}
	principal, err := source.verifier.verify(ctx, strings.TrimPrefix(header, "Bearer "))
	return principal, credentialFault(err)
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
