package kubernetes

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

func TestDiagnoseErrorReturnsSafeActionableClasses(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      string
		retryable bool
	}{
		{name: "invalid arguments", err: ErrInvalidArguments, code: "invalid_arguments"},
		{name: "expired list cursor", err: ErrCursorExpired, code: "cursor_expired"},
		{name: "history deadline", err: historyStoreError(context.DeadlineExceeded), code: "history_store_timeout", retryable: true},
		{name: "history store", err: historyStoreError(errors.New("private database response")), code: "history_store_unavailable", retryable: true},
		{name: "history converting", err: historyStoreError(fmt.Errorf("read: %w", storage.ErrEvidenceNotReady)), code: "history_upgrading", retryable: true},
		{name: "history authority", err: errHistoryAuthority, code: "history_authority_denied"},
		{name: "history mismatch", err: errHistoryMismatch, code: "authority_mismatch"},
		{name: "invalid endpoint", err: ErrInvalidEndpoint, code: "connector_configuration_invalid"},
		{name: "credentials", err: ErrCredentialUnavailable, code: "credential_unavailable"},
		{name: "authentication", err: ErrUnauthorized, code: "cluster_authentication_failed"},
		{name: "permission", err: ErrForbidden, code: "cluster_permission_denied"},
		{name: "missing resource", err: ErrNotFound, code: "resource_unavailable"},
		{name: "kubernetes timeout", err: markKubernetesTimeout(fmt.Errorf("kubernetes: request failed: %w", context.DeadlineExceeded)), code: "request_timeout", retryable: true},
		{name: "deadline spent inside versus", err: context.DeadlineExceeded, code: "server_timeout", retryable: true},
		{name: "store deadline", err: markStoreFailure(fmt.Errorf("storage: read blob: %w", context.DeadlineExceeded)), code: "history_store_timeout", retryable: true},
		{name: "store unreachable", err: markStoreFailure(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), code: "history_store_unavailable", retryable: true},
		{name: "store converting", err: markStoreFailure(fmt.Errorf("read: %w", storage.ErrEvidenceNotReady)), code: "history_upgrading", retryable: true},
		{name: "budget", err: ErrOperationBudget, code: "operation_budget_exhausted", retryable: true},
		{name: "incomplete graph", err: ErrGraphIncomplete, code: "graph_incomplete", retryable: true},
		{name: "graph limit", err: ErrCompleteGraphLimit, code: "graph_limit_exceeded", retryable: false},
		{name: "response cap", err: ErrResponseTooLarge, code: "response_too_large"},
		{name: "redirect", err: ErrRedirect, code: "redirect_refused"},
		{name: "certificate", err: x509.UnknownAuthorityError{}, code: "tls_verification_failed"},
		{name: "dns", err: &net.DNSError{Err: "not found", Name: "redacted.invalid"}, code: "dns_resolution_failed", retryable: true},
		{name: "connection", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private detail")}, code: "connection_failed", retryable: true},
		{name: "fallback", err: errors.New("secret provider response"), code: "read_failed", retryable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detail := DiagnoseError(test.err)
			if detail.Code != test.code || detail.Message == "" || detail.Action == "" || detail.Retryable != test.retryable {
				t.Fatalf("detail = %+v", detail)
			}
			if detail.Message == test.err.Error() || detail.Action == test.err.Error() {
				t.Fatalf("raw cause crossed diagnostic boundary: %+v", detail)
			}
		})
	}
}

// eventDeadline is a context whose deadline passes when the test says so, not
// on a timer.
type eventDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newEventDeadline(parent context.Context) *eventDeadline {
	return &eventDeadline{Context: parent, done: make(chan struct{})}
}

func (deadline *eventDeadline) Done() <-chan struct{} { return deadline.done }

func (deadline *eventDeadline) Err() error {
	select {
	case <-deadline.done:
		return context.DeadlineExceeded
	default:
		return deadline.Context.Err()
	}
}

func (deadline *eventDeadline) pass() { deadline.once.Do(func() { close(deadline.done) }) }

// A deadline is blamed on the Kubernetes API server only when it passes while
// the server holds the request. A deadline that already passed before the
// request was sent was spent inside Versus, and is not named as a Kubernetes
// timeout or sent to connectors.kubernetes.timeout.
func TestKubernetesTimeoutIsOnlyADeadlineThatPassedDuringTheRoundTrip(t *testing.T) {
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		arrived <- struct{}{}
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	during := newEventDeadline(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := client.get(during, "/api/v1/namespaces", "application/json")
		result <- err
	}()
	waitFor(t, arrived, "the API server to receive the request")
	during.pass()
	err = waitFor(t, result, "the request to end")
	if detail := DiagnoseError(err); detail.Code != "request_timeout" {
		t.Fatalf("deadline during the round trip = %+v (%v), want request_timeout", detail, err)
	}

	before := newEventDeadline(t.Context())
	before.pass()
	_, err = client.get(before, "/api/v1/namespaces", "application/json")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired request = %v, want a deadline", err)
	}
	if detail := DiagnoseError(err); detail.Code == "request_timeout" {
		t.Fatalf("a deadline spent before the request was sent was blamed on the cluster: %+v", detail)
	}
}

// waitingOnSharedDiscovery counts the goroutines waiting in Discover for a
// discovery another request started.
func waitingOnSharedDiscovery() int {
	buffer := make([]byte, 1<<20)
	stacks := string(buffer[:runtime.Stack(buffer, true)])
	waiting := 0
	for _, stack := range strings.Split(stacks, "\n\n") {
		lines := strings.SplitN(stack, "\n", 3)
		if len(lines) > 1 && strings.Contains(lines[0], "[select") && strings.Contains(lines[1], "kubernetes.(*Service).Discover(") {
			waiting++
		}
	}
	return waiting
}

// Two requests share one discovery against a slow API server. The one that
// started it times out in its own round trip and is told the cluster read
// timed out. The one that waits for it times out on its own deadline outside
// any call of its own, so it gets the neutral server_timeout: a deadline is
// named for a component only when it came out of that component's call.
func TestRequestsSharingASlowDiscoveryNameOnlyTheirOwnCall(t *testing.T) {
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		arrived <- struct{}{}
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "shared-discovery"}, time.Hour)
	first, second := newEventDeadline(t.Context()), newEventDeadline(t.Context())
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := service.Discover(first)
		firstResult <- err
	}()
	waitFor(t, arrived, "the API server to receive the first request's discovery")
	go func() {
		_, err := service.Discover(second)
		secondResult <- err
	}()
	eventually(t, "the second request to wait for the shared discovery", func() bool { return waitingOnSharedDiscovery() == 1 })
	second.pass()
	if detail := DiagnoseError(waitFor(t, secondResult, "the waiting request to end")); detail.Code != "server_timeout" {
		t.Fatalf("request waiting on the shared discovery = %+v, want server_timeout", detail)
	}
	first.pass()
	if detail := DiagnoseError(waitFor(t, firstResult, "the first request to end")); detail.Code != "request_timeout" {
		t.Fatalf("request that started the discovery = %+v, want request_timeout", detail)
	}
}

// starterRequest tags the context of the request that starts a discovery.
type starterRequest struct{}

// closingCheckGate is a working bearer token that stops the starting request
// at its discovery's closing credential check: the starter's second credential
// read made outside any API read (the first is the check on entry; reads run
// under an operation budget).
type closingCheckGate struct {
	mu      sync.Mutex
	calls   int
	closing chan struct{}
	proceed chan struct{}
}

func (gate *closingCheckGate) Authorization(ctx context.Context) (string, error) {
	if ctx.Value(starterRequest{}) != nil && operationBudgetFrom(ctx) == nil {
		gate.mu.Lock()
		gate.calls++
		closing := gate.calls == 2
		gate.mu.Unlock()
		if closing {
			close(gate.closing)
			<-gate.proceed
		}
	}
	return "Bearer closing-check", nil
}

func (gate *closingCheckGate) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

// A request sharing a discovery gets exactly what the request that started it
// returns, after that request's closing credential check. The credential is
// retired after the discovery's reads finished and before its closing check,
// so the starter answers credential_unavailable; the sharing request must not
// be handed the discovery as if nothing happened.
func TestRequestsSharingADiscoveryGetTheStartersClosingAnswer(t *testing.T) {
	groupsAsked := make(chan struct{}, 1)
	releaseGroups := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method != http.MethodGet:
			// The identity review is not supported; the epoch stays
			// byte-bound.
			http.NotFound(writer, request)
		case request.URL.Path == "/api":
			_, _ = writer.Write([]byte(`{"versions":[]}`))
		case request.URL.Path == "/apis":
			groupsAsked <- struct{}{}
			select {
			case <-releaseGroups:
			case <-request.Context().Done():
				return
			}
			_, _ = writer.Write([]byte(`{"groups":[]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	gate := &closingCheckGate{closing: make(chan struct{}), proceed: make(chan struct{})}
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: time.Hour, Credentials: gate})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "shared-closing-check"}, time.Hour)
	starterResult, sharingResult := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := service.Discover(context.WithValue(t.Context(), starterRequest{}, true))
		starterResult <- err
	}()
	waitFor(t, groupsAsked, "the starter's last discovery read")
	go func() {
		_, err := service.Discover(t.Context())
		sharingResult <- err
	}()
	eventually(t, "the second request to share the discovery", func() bool { return waitingOnSharedDiscovery() == 1 })
	close(releaseGroups)
	waitFor(t, gate.closing, "the starter's closing credential check")
	var sharingErr error
	sharingDone := false
	if waitingOnSharedDiscovery() == 0 {
		// The discovery was handed out before its closing check; that answer
		// is final before the credential is retired.
		sharingErr, sharingDone = waitFor(t, sharingResult, "the sharing request to end"), true
	}
	service.identity.mu.Lock()
	service.identity.retired = true
	service.identity.mu.Unlock()
	service.identity.cancel()
	close(gate.proceed)
	starterErr := waitFor(t, starterResult, "the starter to end")
	if code := DiagnoseError(starterErr).Code; !errors.Is(starterErr, ErrCredentialUnavailable) || code != "credential_unavailable" {
		t.Fatalf("starter = %v (%s), want credential_unavailable", starterErr, code)
	}
	if !sharingDone {
		sharingErr = waitFor(t, sharingResult, "the sharing request to end")
	}
	if sharingErr == nil || DiagnoseError(sharingErr).Code != DiagnoseError(starterErr).Code {
		t.Fatalf("sharing request got %v, the starter got %v", sharingErr, starterErr)
	}
}

// A credential retired while a discovery is reading the API server is named as
// the credential, not as a failed read, and the discovery returns no resources.
func TestDiscoveryDuringCredentialRetirementNamesTheCredential(t *testing.T) {
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			// The identity review is not supported; the epoch stays
			// byte-bound.
			http.NotFound(writer, request)
			return
		}
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-request.Context().Done()
	}))
	defer server.Close()
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: time.Hour, Credentials: staticCredential(t, "discovery-test")})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "retired-discovery"}, time.Hour)
	type outcome struct {
		discovery Discovery
		err       error
	}
	result := make(chan outcome, 1)
	go func() {
		discovery, err := service.Discover(t.Context())
		result <- outcome{discovery, err}
	}()
	waitFor(t, arrived, "the discovery to reach the API server")
	service.identity.mu.Lock()
	service.identity.retired = true
	service.identity.mu.Unlock()
	service.identity.cancel()
	got := waitFor(t, result, "the discovery to end")
	if !errors.Is(got.err, ErrCredentialUnavailable) || DiagnoseError(got.err).Code != "credential_unavailable" {
		t.Fatalf("discovery during credential retirement = %v (%s), want credential_unavailable", got.err, DiagnoseError(got.err).Code)
	}
	if len(got.discovery.Resources) != 0 || len(got.discovery.Partial) != 0 || !got.discovery.ObservedAt.IsZero() {
		t.Fatalf("a failed discovery returned data: %+v", got.discovery)
	}
}

// failingTokenHost is a credential whose token endpoint does not answer. Its
// first call runs out the request's deadline, which then passes during the
// retry pause that follows the failed attempt.
type failingTokenHost struct{ deadline *eventDeadline }

func (source failingTokenHost) Authorization(context.Context) (string, error) {
	source.deadline.pass()
	return "", fmt.Errorf("token request: %w", ErrTokenEndpointUnavailable)
}

func (source failingTokenHost) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

// The pause before retrying an inventory read is Versus's own pacing, not a
// wait the API server asked for: a deadline that passes there is never named
// a Kubernetes timeout.
func TestDeadlineInTheInventoryRetryPauseIsNotAKubernetesTimeout(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	deadline := newEventDeadline(t.Context())
	client, err := NewClient(Config{Endpoint: server.URL, AllowLoopbackHTTP: true, Timeout: time.Hour, Credentials: failingTokenHost{deadline: deadline}})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{OrgID: "default", ClusterID: "retry-pause"}, time.Hour)
	var value map[string]any
	err = service.inventoryRead(deadline, "/api/v1/pods", &value, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read whose deadline passed in the retry pause = %v, want the deadline", err)
	}
	if code := DiagnoseError(err).Code; code == "request_timeout" {
		t.Fatalf("a deadline in Versus's own retry pause was named a Kubernetes timeout")
	}
}

// A read of the published generation whose context ended before the read is
// not a store answer: it is not named for the store.
func TestInventoryReadWithAnEndedContextIsNotAStoreTimeout(t *testing.T) {
	memory := storage.NewMemory()
	t.Cleanup(func() { _ = memory.Close() })
	expired := newEventDeadline(t.Context())
	expired.pass()
	_, _, err := readInventoryGenerationContext(expired, wrapStore(memory), Scope{OrgID: "default", ClusterID: "ended"}, "")
	if code := DiagnoseError(inventoryReadAnswer(err)).Code; code != "server_timeout" {
		t.Fatalf("read with an ended context = %s, want server_timeout", code)
	}
}
