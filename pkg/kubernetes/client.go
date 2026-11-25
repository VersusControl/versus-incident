// Package kubernetes provides bounded, read-only access to Kubernetes APIs.
package kubernetes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

const (
	defaultTimeout      = 10 * time.Second
	defaultMaxBodyBytes = int64(4 << 20)
	maxTokenBytes       = int64(64 << 10)
	maxCABytes          = int64(4 << 20)
)

var (
	ErrInvalidEndpoint       = errors.New("kubernetes: invalid endpoint")
	ErrUnauthorized          = errors.New("kubernetes: unauthorized")
	ErrForbidden             = errors.New("kubernetes: forbidden")
	ErrNotFound              = errors.New("kubernetes: not found")
	ErrResponseTooLarge      = errors.New("kubernetes: response too large")
	errPreviousUnavailable   = errors.New("kubernetes: previous container unavailable")
	ErrRedirect              = errors.New("kubernetes: redirect refused")
	ErrInvalidArguments      = errors.New("kubernetes: invalid arguments")
	ErrOperationBudget       = errors.New("kubernetes: operation budget exhausted")
	ErrGraphIncomplete       = errors.New("kubernetes: complete graph could not be verified")
	ErrCompleteGraphLimit    = errors.New("kubernetes: complete graph safety limit exceeded")
	ErrCredentialUnavailable = errors.New("kubernetes: credential unavailable")
)

// Config defines one Kubernetes API connection. Credentials are read from
// TokenFile for every request so projected service-account rotation is honored.
type Config struct {
	Namespace            string
	Endpoint             string
	TokenFile            string
	CAFile               string
	CAData               []byte
	ServerName           string
	Credentials          CredentialSource
	Timeout              time.Duration
	MaxBodyBytes         int64
	AllowLoopbackHTTP    bool
	AllowLoopback        bool
	AllowPrivateNetworks bool
	EndpointCIDRs        []string
}

type endpointPolicy struct {
	allowLoopback bool
	allowPrivate  bool
	allowedCIDRs  []*net.IPNet
}

type ipResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type contextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// productionIndexRate and productionIndexBurst pace a client's inventory
// LIST requests.
const (
	productionIndexRate  = 2
	productionIndexBurst = 4
)

// Client is a read-only, bounded Kubernetes API transport.
type Client struct {
	namespace    string
	base         *url.URL
	credentials  CredentialSource
	timeout      time.Duration
	maxBodyBytes int64
	http         *http.Client
	streamHTTP   *http.Client
	indexLimiter *rate.Limiter
	// discoveryLimiter paces background discovery reads apart from inventory
	// LISTs, so a discovery walk never waits behind the LIST pace.
	discoveryLimiter *rate.Limiter
	// discoveryBound replaces discoveryWalkBound for one client when set; a
	// test fixture serving many API groups uses it on a loaded machine. Zero
	// keeps the production bound.
	discoveryBound time.Duration
	metadataMu     sync.Mutex
	metadataDenied map[metadataDenyKey]metadataDenyMemo
	now            func() time.Time
	backoffMu      sync.Mutex
	backoffUntil   time.Time
	// systemBackoffUntil is the API server's throttle on system reads. It is
	// kept apart from backoffUntil, so a 429 on a background system read
	// never stalls the user's reads, and theirs never stall system reads.
	systemBackoffUntil time.Time
	authMu             sync.Mutex
	authGeneration     atomic.Uint64
	authContext        context.Context
	authCancel         context.CancelFunc
	// proofs caches identity proofs per token for sources whose renewals
	// are proved; it is created on first use.
	proofsMu sync.Mutex
	proofs   *credentialProofs
	// identityChanges counts principal-change rebuilds in a row, with the
	// changed field names, for the warning that the identity is not stable.
	identityMu      sync.Mutex
	identityChanges int
	identityFields  map[string]bool
}

func (client *Client) credentialEpochContext() (context.Context, context.CancelFunc) {
	client.authMu.Lock()
	defer client.authMu.Unlock()
	if client.authContext == nil {
		client.authContext, client.authCancel = context.WithCancel(context.Background())
	}
	return context.WithCancel(client.authContext)
}

func (client *Client) rejectCredential() {
	client.authMu.Lock()
	defer client.authMu.Unlock()
	client.authGeneration.Add(1)
	if client.authCancel != nil {
		client.authCancel()
	}
	client.authContext, client.authCancel = nil, nil
}

// rejectRequestCredential fences delayed failures from a previous client epoch.
func (client *Client) rejectRequestCredential(header string, generation uint64, recoveries ...*clusterRecovery) bool {
	client.authMu.Lock()
	defer client.authMu.Unlock()
	if client.authGeneration.Load() != generation {
		return false
	}
	if source, ok := client.credentials.(interface{ rejectBearer(string) bool }); ok && !source.rejectBearer(header) {
		return false
	}
	if len(recoveries) > 0 {
		// A rejected bearer belongs to the credential every read scope shares.
		recoveries[0].failed(ErrUnauthorized, client, "")
	}
	client.authGeneration.Add(1)
	if client.authCancel != nil {
		client.authCancel()
	}
	client.authContext, client.authCancel = nil, nil
	return true
}

type captureReadContextKey struct{}

func captureReadContext(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, captureReadContextKey{}, owner)
}

type systemReadContextKey struct{}

// systemRead marks the requests of a gated system read (see system_reads.go).
// The API server's answer to such a request is about that read alone: a 403
// is not recorded as a history authority denial. bodyLimit, when above the
// client's own limit, is the largest body the read accepts; it is bounded by
// the system read ceiling, never by the caller.
func systemRead(ctx context.Context, bodyLimit int64) context.Context {
	return context.WithValue(ctx, systemReadContextKey{}, systemReadMark{bodyLimit: bodyLimit})
}

type systemReadMark struct{ bodyLimit int64 }

func systemReadFrom(ctx context.Context) (systemReadMark, bool) {
	mark, ok := ctx.Value(systemReadContextKey{}).(systemReadMark)
	return mark, ok
}

type discoveryReadContextKey struct{}

// discoveryRead marks the reads of a discovery walk. In the background they
// take the discovery pace instead of the inventory LIST pace. Discovery is a
// bounded set of small metadata documents, cached between cycles; sharing the
// LIST pace made a full walk of a cluster with many API groups outlast its
// own time bound, so the groups listed last (installed CRD groups such as the
// Gateway API) were never read and their kinds looked unserved.
func discoveryRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, discoveryReadContextKey{}, true)
}

// newDiscoveryLimiter fits the largest walk discovery may make within the
// time bound a collection cycle gives it.
func newDiscoveryLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Limit(float64(discoveryRequests)/discoveryWalkBound.Seconds()), 16)
}

// discoveryWalkBound is the time a collection cycle gives one discovery walk.
const discoveryWalkBound = 10 * time.Second

// discoveryWalkTimeout is the time this client's collection cycle gives one
// discovery walk.
func (client *Client) discoveryWalkTimeout() time.Duration {
	if client != nil && client.discoveryBound > 0 {
		return client.discoveryBound
	}
	return discoveryWalkBound
}

// NewClient validates transport policy and constructs a read-only API client.
func NewClient(config Config) (*Client, error) {
	if config.Namespace != "" && (!safeSegment(config.Namespace) || len(config.Namespace) > 63) {
		return nil, ErrInvalidArguments
	}
	policy, err := newEndpointPolicy(config)
	if err != nil {
		return nil, err
	}
	base, err := validateEndpoint(config.Endpoint, policy)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(config.ServerName)}
	if config.CAFile != "" && len(config.CAData) > 0 {
		return nil, errors.New("kubernetes: conflicting CA configuration")
	}
	caPEM := config.CAData
	if config.CAFile != "" {
		caPEM, err = readBoundedFile(config.CAFile, maxCABytes)
		if err != nil {
			return nil, errors.New("kubernetes: CA unavailable")
		}
	}
	if len(caPEM) > int(maxCABytes) {
		return nil, errors.New("kubernetes: CA unavailable")
	}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("kubernetes: CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	credentials := config.Credentials
	if credentials != nil && config.TokenFile != "" {
		return nil, errors.New("kubernetes: conflicting credential configuration")
	}
	if credentials == nil && config.TokenFile != "" {
		credentials = tokenFileCredential(config.TokenFile)
	}
	if credentials != nil {
		tlsConfig.GetClientCertificate = credentials.ClientCertificate
	}
	timeout := config.Timeout
	if timeout <= 0 || timeout > time.Minute {
		timeout = defaultTimeout
	}
	maxBodyBytes := config.MaxBodyBytes
	if maxBodyBytes <= 0 || maxBodyBytes > 32<<20 {
		maxBodyBytes = defaultMaxBodyBytes
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	transport.Proxy = nil
	transport.DialContext = guardedDialContext(&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}, net.DefaultResolver, policy)
	streamTransport := transport.Clone()
	streamTransport.DialContext = guardedDialContext(&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}, net.DefaultResolver, policy)
	streamTransport.TLSHandshakeTimeout = 10 * time.Second
	streamTransport.ResponseHeaderTimeout = 10 * time.Second
	streamTransport.MaxResponseHeaderBytes = 64 << 10
	client := &Client{
		namespace: config.Namespace,
		base:      base, credentials: credentials, timeout: timeout, maxBodyBytes: maxBodyBytes,
		indexLimiter: rate.NewLimiter(productionIndexRate, productionIndexBurst), discoveryLimiter: newDiscoveryLimiter(), metadataDenied: map[metadataDenyKey]metadataDenyMemo{},
		http:       &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRedirect }},
		streamHTTP: &http.Client{Transport: streamTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrRedirect }},
	}
	if source, ok := credentials.(*tokenFileSource); ok {
		client.credentials = &tokenFileSource{path: source.path, verifier: &projectedTokenVerifier{client: client}}
	}
	return client, nil
}

// GetJSON performs one authenticated, bounded GET and decodes its JSON body.
func (client *Client) GetJSON(ctx context.Context, apiPath string, output any) error {
	data, err := client.get(ctx, apiPath, "application/json")
	if err != nil {
		return err
	}
	if list, ok := output.(*validatedInventoryList); ok {
		return list.UnmarshalJSON(data)
	}
	if err := json.Unmarshal(data, output); err != nil {
		var invalid *inventoryListValidationError
		if errors.As(err, &invalid) {
			return invalid
		}
		return errors.New("kubernetes: invalid API response")
	}
	return nil
}

func (client *Client) GetMetadataJSON(ctx context.Context, apiPath string, output any) error {
	data, err := client.get(ctx, apiPath, "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1")
	if err != nil {
		return err
	}
	if err = validateInventoryListEnvelope(data, true); err != nil {
		return err
	}
	if json.Unmarshal(data, output) != nil {
		return invalidInventoryList()
	}
	return nil
}

func (client *Client) get(ctx context.Context, apiPath, accept string) ([]byte, error) {
	if client == nil {
		return nil, ErrCredentialUnavailable
	}
	data, truncated, err := client.getBounded(ctx, apiPath, accept, client.maxBodyBytes)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

// extendBackoff pushes back the throttle the request on ctx belongs to.
func (client *Client) extendBackoff(ctx context.Context, until time.Time) {
	client.backoffMu.Lock()
	defer client.backoffMu.Unlock()
	if _, system := systemReadFrom(ctx); system {
		client.systemBackoffUntil = maxTime(client.systemBackoffUntil, until)
		return
	}
	client.backoffUntil = maxTime(client.backoffUntil, until)
}

// systemThrottle answers a system read at once while the API server's
// throttle on system reads lasts, instead of waiting it out: a background
// read has no user waiting and tries again on its next cycle.
func (client *Client) systemThrottle() error {
	if client == nil {
		return ErrClusterUnavailable
	}
	client.backoffMu.Lock()
	until := client.systemBackoffUntil
	client.backoffMu.Unlock()
	if delay := time.Until(until); delay > 0 {
		return &ErrThrottled{RetryAfter: delay}
	}
	return nil
}

func (client *Client) waitBackoff(ctx context.Context) error {
	_, system := systemReadFrom(ctx)
	for {
		client.backoffMu.Lock()
		until := client.backoffUntil
		if system {
			until = client.systemBackoffUntil
		}
		client.backoffMu.Unlock()
		delay := time.Until(until)
		if delay <= 0 {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			// The API server asked for this wait.
			return markKubernetesTimeout(ctx.Err())
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (client *Client) getBounded(ctx context.Context, apiPath, accept string, maxBytes int64) ([]byte, bool, error) {
	if client == nil || client.http == nil {
		return nil, false, errors.New("kubernetes: client unavailable")
	}
	ctx, cancelOperation := ensureOperationBudget(ctx)
	defer cancelOperation()
	if err := client.waitBackoff(ctx); err != nil {
		return nil, false, err
	}
	if _, background := ctx.Value(captureReadContextKey{}).(string); background {
		limiter := client.indexLimiter
		if discovery, _ := ctx.Value(discoveryReadContextKey{}).(bool); discovery {
			limiter = client.discoveryLimiter
		}
		if limiter != nil {
			if err := limiter.Wait(ctx); err != nil {
				return nil, false, err
			}
		}
	}
	budget := operationBudgetFrom(ctx)
	if !budget.takeRequest() {
		return nil, false, ErrOperationBudget
	}
	requestURL, err := client.requestURL(apiPath)
	if err != nil {
		return nil, false, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	if service, ok := ctx.Value(credentialEpochContextKey{}).(*Service); ok {
		stopIdentity := context.AfterFunc(service.identity.ctx, cancel)
		defer stopIdentity()
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("kubernetes: create request: %w", err)
	}
	request.Header.Set("Accept", accept)
	authGeneration := client.authGeneration.Load()
	var requestService *Service
	var requestCredentialGeneration uint64
	var requestCertificate *tls.Certificate
	if service, ok := ctx.Value(credentialEpochContextKey{}).(*Service); ok {
		requestService = service
		requestCredentialGeneration = service.identity.requestGeneration.Load()
	}
	if client.credentials != nil {
		if err := client.requireCredentialEpoch(ctx); err != nil {
			return nil, false, err
		}
		authorization, credentialErr := client.credentials.Authorization(requestCtx)
		if credentialErr != nil {
			return nil, false, credentialErr
		}
		if service, ok := ctx.Value(credentialEpochContextKey{}).(*Service); ok {
			// The admitted header can be the previous, proven token while
			// the renewed one is still being proved.
			admitted, accepted, certificate, err := service.acceptCredentialForRequest(ctx, authorization)
			if err != nil {
				return nil, false, err
			}
			authorization = admitted
			requestService, requestCredentialGeneration = service, accepted
			requestCertificate = certificate
		} else {
			requestCertificate, err = client.credentials.ClientCertificate(&tls.CertificateRequestInfo{})
			if err != nil {
				return nil, false, ErrCredentialUnavailable
			}
		}
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
	}
	requestClient, closeTransport, err := client.certificateHTTP(requestCertificate)
	if err != nil {
		return nil, false, err
	}
	defer closeTransport()
	// A deadline that already passed before the request was sent was spent
	// inside Versus (a lock, the credential, the history store), so only one
	// that fires during the round trip is a Kubernetes timeout.
	sentInTime := requestCtx.Err() == nil
	response, err := requestClient.Do(request)
	if err != nil {
		if errors.Is(err, ErrRedirect) {
			return nil, false, ErrRedirect
		}
		if budget.expired() {
			return nil, false, ErrOperationBudget
		}
		err = fmt.Errorf("kubernetes: request failed: %w", err)
		if sentInTime {
			err = markKubernetesTimeout(err)
		}
		return nil, false, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if hint := response.Header.Get("Retry-After"); hint != "" {
			delay := parseRetryAfter(hint, time.Now().UTC())
			client.extendBackoff(ctx, time.Now().Add(delay))
		}
		if response.StatusCode == http.StatusUnauthorized && podLogPath(apiPath) {
			// For a Pod log the API server relays the kubelet's answer: a 401
			// can be one node's kubelet refusing the API server while this
			// credential is good everywhere else. It fails this read only.
			return nil, false, ErrUnauthorized
		}
		currentRequest := requestService == nil || requestService.identity.requestGeneration.Load() == requestCredentialGeneration
		if response.StatusCode == http.StatusUnauthorized && requestService != nil {
			requestService.noteRejectedCredential(request.Header.Get("Authorization"))
		}
		if response.StatusCode == http.StatusUnauthorized && currentRequest {
			if client.rejectRequestCredential(request.Header.Get("Authorization"), authGeneration, recoveryFromContext(ctx)) && requestService != nil {
				requestService.denyHistoryAuthority(ctx, apiPath, true)
			}
		}
		if _, system := systemReadFrom(ctx); response.StatusCode == http.StatusForbidden && currentRequest && requestService != nil && !system {
			requestService.denyHistoryAuthority(ctx, apiPath, false)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			delay := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			client.extendBackoff(ctx, time.Now().UTC().Add(delay))
			return nil, false, &ErrThrottled{RetryAfter: delay}
		}
		if response.StatusCode == http.StatusBadRequest {
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			message := strings.ToLower(string(body))
			if strings.Contains(message, "previous terminated container") && strings.Contains(message, "not found") {
				return nil, false, errPreviousUnavailable
			}
		}
		if response.StatusCode == http.StatusForbidden {
			// Only the denied verb, resource, namespace and user are kept; the
			// body itself never leaves this function.
			body, readErr := io.ReadAll(io.LimitReader(response.Body, forbiddenStatusLimit+1))
			if readErr == nil {
				if denied := permissionDeniedFromStatus(body); denied != nil {
					return nil, false, denied
				}
			}
			return nil, false, ErrForbidden
		}
		return nil, false, classifyStatus(response.StatusCode)
	}
	ceiling := client.maxBodyBytes
	if mark, system := systemReadFrom(ctx); system && mark.bodyLimit > ceiling {
		ceiling = min(mark.bodyLimit, systemMetricsResponseCeiling)
	}
	if maxBytes <= 0 || maxBytes > ceiling {
		maxBytes = ceiling
	}
	limited := io.LimitReader(response.Body, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, markKubernetesTimeout(fmt.Errorf("kubernetes: read response: %w", err))
	}
	if !budget.takeBytes(int64(len(data))) {
		return nil, false, ErrOperationBudget
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	if service, ok := ctx.Value(credentialEpochContextKey{}).(*Service); ok {
		if err := service.validateCredential(ctx); err != nil {
			return nil, false, err
		}
	}
	return data, truncated, nil
}

// requireCredentialEpoch refuses a data request that is not bound to a
// credential epoch when the source's renewed tokens must be proved first. Only
// an epoch knows which token is proven; an unbound request would send whatever
// the source returned last, including a candidate that has not been proved.
func (client *Client) requireCredentialEpoch(ctx context.Context) error {
	if _, proven := client.credentials.(provenCredentialSource); !proven {
		return nil
	}
	if _, bound := ctx.Value(credentialEpochContextKey{}).(*Service); bound {
		return nil
	}
	return ErrCredentialUnavailable
}

func (client *Client) requestURL(apiPath string) (string, error) {
	if !strings.HasPrefix(apiPath, "/") || strings.ContainsAny(apiPath, "\r\n") {
		return "", ErrInvalidEndpoint
	}
	reference, err := url.Parse(apiPath)
	if err != nil || reference.IsAbs() || reference.Host != "" {
		return "", ErrInvalidEndpoint
	}
	result := *client.base
	result.Path = path.Clean("/" + strings.TrimPrefix(reference.Path, "/"))
	result.RawQuery = reference.RawQuery
	return result.String(), nil
}

func newEndpointPolicy(config Config) (endpointPolicy, error) {
	policy := endpointPolicy{allowLoopback: config.AllowLoopbackHTTP || config.AllowLoopback, allowPrivate: config.AllowPrivateNetworks}
	for _, value := range config.EndpointCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return endpointPolicy{}, ErrInvalidEndpoint
		}
		policy.allowedCIDRs = append(policy.allowedCIDRs, network)
	}
	return policy, nil
}

func validateEndpoint(value string, policy endpointPolicy) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return nil, ErrInvalidEndpoint
	}
	if parsed.Scheme != "https" {
		if parsed.Scheme != "http" || !policy.allowLoopback || !isLoopbackHost(parsed.Hostname()) {
			return nil, ErrInvalidEndpoint
		}
	}
	if address := net.ParseIP(parsed.Hostname()); address != nil && !policy.allows(address) {
		return nil, policy.refusal(address)
	}
	parsed.Path = ""
	return parsed, nil
}

func guardedDialContext(dialer contextDialer, resolver ipResolver, policy endpointPolicy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, ErrInvalidEndpoint
		}
		addresses := []net.IPAddr{{IP: net.ParseIP(host)}}
		if addresses[0].IP == nil {
			addresses, err = resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, &connectionError{cause: err}
			}
			if len(addresses) == 0 {
				return nil, ErrInvalidEndpoint
			}
		}
		for _, candidate := range addresses {
			if !policy.allows(candidate.IP) {
				return nil, policy.refusal(candidate.IP)
			}
		}
		var lastErr error
		for _, candidate := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

func (policy endpointPolicy) allows(address net.IP) bool {
	if address == nil || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	if address.IsLoopback() {
		return policy.allowLoopback
	}
	if !address.IsPrivate() {
		return true
	}
	if policy.allowPrivate {
		return true
	}
	for _, network := range policy.allowedCIDRs {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

// refusal names a refused address and the setting that would allow it.
func (policy endpointPolicy) refusal(address net.IP) error {
	refused := &endpointRefusedError{address: address}
	switch {
	case address == nil:
		return ErrInvalidEndpoint
	case address.IsUnspecified(), address.IsMulticast(), address.IsLinkLocalUnicast(), address.IsLinkLocalMulticast():
		refused.reason = "an unspecified, multicast or link-local address"
	case address.IsLoopback():
		refused.reason, refused.setting = "a loopback address", "allow_loopback"
	default:
		refused.reason, refused.setting = "on a private network", "allow_private_networks"
	}
	return refused
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func classifyStatus(status int) error {
	switch status {
	case http.StatusBadRequest:
		return ErrInvalidArguments
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		return &ErrThrottled{RetryAfter: 30 * time.Second}
	default:
		return fmt.Errorf("kubernetes: API status %d", status)
	}
}

// metadataDenyKey scopes a permission memo to one organization, cluster,
// credential epoch, kind, and namespace, so a denial observed for one tenant can
// never suppress a read for another.
type metadataDenyKey struct {
	OrgID        string
	ClusterID    string
	CredentialID string
	Kind         string
	Namespace    string
}

// metadataDenyMemo is how long a metadata permission memo keeps its answer. It
// is always bounded: a granted permission has to become visible without waiting
// for a restart, and a denied one must not be re-attempted on every retry cycle.
type metadataDenyMemo struct {
	Until time.Time
	// Err is the denial the memo remembered. It is replayed as-is, so a dead
	// credential is never re-reported as a missing permission.
	Err error
}

const (
	// metadataDenyTTL is the longest a denial suppresses a retry. It is four
	// retry cycles, so a grant is picked up within a minute while a persistent
	// denial costs at most one cluster request per kind per minute.
	metadataDenyTTL = time.Minute
	// maxMetadataDenyEntries bounds the memo map itself.
	maxMetadataDenyEntries = 512
)

// clock returns the client's time source, so a test can move time.
func (client *Client) clock() time.Time {
	if client != nil && client.now != nil {
		return client.now()
	}
	return time.Now()
}

// metadataDenial reports the denial a memo still honours, and whether it
// applies. The remembered error keeps its own class.
func (client *Client) metadataDenial(key metadataDenyKey) (error, bool) {
	client.metadataMu.Lock()
	defer client.metadataMu.Unlock()
	memo, exists := client.metadataDenied[key]
	if !exists || !client.clock().Before(memo.Until) {
		return nil, false
	}
	if memo.Err == nil {
		return ErrForbidden, true
	}
	return memo.Err, true
}

// memoMetadataDenial records a denial for a bounded time.
func (client *Client) memoMetadataDenial(key metadataDenyKey, denial error) {
	client.metadataMu.Lock()
	defer client.metadataMu.Unlock()
	if client.metadataDenied == nil {
		client.metadataDenied = map[metadataDenyKey]metadataDenyMemo{}
	}
	if len(client.metadataDenied) >= maxMetadataDenyEntries {
		// Bounded bookkeeping: forgetting every memo costs at most one probe per
		// key, which is exactly what the next retry cycle does anyway.
		clear(client.metadataDenied)
	}
	client.metadataDenied[key] = metadataDenyMemo{Until: client.clock().Add(metadataDenyTTL), Err: denial}
}

// clearMetadataDenial forgets a denial once a read succeeds.
func (client *Client) clearMetadataDenial(key metadataDenyKey) {
	client.metadataMu.Lock()
	defer client.metadataMu.Unlock()
	delete(client.metadataDenied, key)
}

// podLogPath reports a request for a Pod's log subresource: exactly
// /api/v1/namespaces/<namespace>/pods/<pod>/log, query aside. A Pod named
// "log", or an object named "log" in a namespace named "pods", is not one.
func podLogPath(apiPath string) bool {
	path, _, _ := strings.Cut(apiPath, "?")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) == 7 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "namespaces" && parts[3] != "" && parts[4] == "pods" && parts[5] != "" && parts[6] == "log"
}

type ErrThrottled struct{ RetryAfter time.Duration }

func (*ErrThrottled) Error() string { return "kubernetes: API throttled" }

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds <= 0 {
			return time.Second
		}
		if seconds >= 300 {
			return 5 * time.Minute
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		delay := at.Sub(now)
		if delay <= 0 {
			return time.Second
		}
		if delay > 5*time.Minute {
			return 5 * time.Minute
		}
		return delay
	}
	return 30 * time.Second
}

func recoveryFromContext(ctx context.Context) *clusterRecovery {
	if service, ok := ctx.Value(credentialEpochContextKey{}).(*Service); ok {
		return service.recovery
	}
	return nil
}
