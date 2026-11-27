package kubernetes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestSelfSubjectReviewRejectsUnprovedEnvelopesAndAnonymous(t *testing.T) {
	for _, tc := range []struct {
		name, version, kind, username string
	}{
		{"missing-envelope", "", "", "reader"},
		{"wrong-version", "v1", "SelfSubjectReview", "reader"},
		{"wrong-kind", "authentication.k8s.io/v1", "Status", "reader"},
		{"anonymous", "authentication.k8s.io/v1", "SelfSubjectReview", "system:anonymous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": tc.version, "kind": tc.kind, "status": map[string]any{"userInfo": map[string]any{"username": tc.username}}})
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "review"})
			if principal, err := service.client.selfSubjectPrincipal(t.Context(), "Bearer test", nil); err == nil || principal != "" {
				t.Fatalf("unproved principal accepted: %q, %v", principal, err)
			}
			service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer test", nil }}
			err := service.acceptCredential(t.Context(), "Bearer test")
			if service.identity.known {
				t.Fatal("invalid review proved principal equivalence")
			}
			if tc.username == "system:anonymous" && (!errors.Is(err, ErrUnauthorized) || service.identity.ctx.Err() == nil || service.historyPrincipal() != "") {
				t.Fatal("authenticated source accepted anonymous authority")
			}
		})
	}
}

func TestUnsupportedSelfSubjectReviewIsCachedOnlyForAcceptedMaterial(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "review"})
			service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return "Bearer first", nil }}
			service = NewService(service.client, service.Scope(), 0)
			for range 4 {
				if err := service.validateCredential(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if calls.Load() != 1 || service.identity.known {
				t.Fatalf("unsupported review retried or guessed principal: calls=%d", calls.Load())
			}
			generation := service.identity.requestGeneration.Load()
			if err := service.acceptCredential(t.Context(), "Bearer second"); !errors.Is(err, ErrCredentialUnavailable) {
				t.Fatalf("unproved replacement accepted: %v", err)
			}
			if calls.Load() != 2 || service.identity.requestGeneration.Load() != generation || service.identity.ctx.Err() == nil {
				t.Fatal("changed material skipped review or escaped retirement fence")
			}
		})
	}
}

func TestHistoryScopePointerCannotCrossOrgOrTrustStoredOrg(t *testing.T) {
	provider := storage.NewMemory()
	first := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "first", ClusterID: "same"})
	first.SetChangeStorage(provider)
	first.changes.evidenceEnabled = true
	second := first.Scoped(Scope{OrgID: "second", ClusterID: "same"})
	if first.historyScopeKey() == second.historyScopeKey() {
		t.Fatal("history pointer is not org-bound")
	}
	scope := storage.EvidenceScope{OrgID: "first", ClusterID: "same", ClusterUID: "protected-physical"}
	if err := provider.(storage.EvidenceStore).CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "first", Epoch: "test", At: time.Now().UTC(), Principal: first.historyPrincipal(), Authority: map[string]storage.EvidenceAuthority{"Pod": {State: "allowed", ProvedAt: time.Now().UTC()}}}); err != nil {
		t.Fatal(err)
	}
	if err := first.rememberHistoryScope(scope); err != nil {
		t.Fatal(err)
	}
	resolved, _, err := second.persistedHistoryScope(t.Context())
	if !errors.Is(err, errHistoryNotEstablished) || resolved.ClusterUID != "" {
		t.Fatalf("other org learned physical history identity: %+v, %v", resolved, err)
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.WriteBlob(second.historyScopeKey(), raw); err != nil {
		t.Fatal(err)
	}
	if resolved, _, err := second.persistedHistoryScope(t.Context()); !errors.Is(err, errHistoryAuthority) || resolved.ClusterUID != "" {
		t.Fatalf("stored org was silently rewritten or exposed: %+v, %v", resolved, err)
	}
	if err := second.rememberHistoryScope(scope); !errors.Is(err, errHistoryAuthority) {
		t.Fatalf("foreign pointer was published: %v", err)
	}
	second.denyHistoryAuthority(t.Context(), "/api/v1/pods", false)
	cp, err := provider.(storage.EvidenceStore).EvidenceCheckpoint(t.Context(), scope)
	if err != nil || cp.Authority["Pod"].State != "allowed" {
		t.Fatal("foreign pointer redirected denial persistence across orgs")
	}
}

func TestRevokedCredentialPersistsDenialWithoutInventoryCache(t *testing.T) {
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "revoked"})
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	service.changes.evidenceEnabled = true
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "revoked", ClusterUID: "physical"}
	principal := service.historyPrincipal()
	if err := provider.(storage.EvidenceStore).CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "committed", Epoch: "test", At: time.Now().UTC(), Principal: principal, Authority: map[string]storage.EvidenceAuthority{"Pod": {State: "allowed", ProvedAt: time.Now().UTC()}}}); err != nil {
		t.Fatal(err)
	}
	if err := service.rememberHistoryScope(scope); err != nil {
		t.Fatal(err)
	}
	service.identity.cancel()
	service.denyHistoryAuthority(t.Context(), "/api/v1/pods", true)
	cp, err := provider.(storage.EvidenceStore).EvidenceCheckpoint(t.Context(), scope)
	if err != nil || cp.Authority["Pod"].State != "denied" {
		t.Fatalf("retirement prevented durable denial: %+v, %v", cp.Authority, err)
	}
}

func TestHistoryDenialBeforeFirstCommitOutranksMissingProof(t *testing.T) {
	for _, state := range []string{"unestablished", "revoked", "forbidden", "unauthorized"} {
		t.Run(state, func(t *testing.T) {
			service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "empty"})
			service.SetChangeStorage(storage.NewMemory())
			service.changes.evidenceEnabled = true
			want := errHistoryAuthority
			switch state {
			case "unestablished":
				want = errHistoryNotEstablished
			case "revoked":
				service.identity.cancel()
			default:
				service.denyHistoryAuthority(t.Context(), "/api/v1/pods", state == "unauthorized")
			}
			page, err := service.persistedHistoryQuery(t.Context(), storage.EvidenceQuery{})
			if !errors.Is(err, want) || len(page.Items) != 0 {
				t.Fatalf("precommit authority=%v, items=%d; want %v", err, len(page.Items), want)
			}
			if _, err := service.CommitStatus(t.Context()); !errors.Is(err, want) {
				t.Fatalf("commit status authority=%v; want %v", err, want)
			}
		})
	}
}

func TestHistoryWithoutKindAuthorityCannotExposeStoredPayload(t *testing.T) {
	service := newTestService(t, "http://127.0.0.1:1", Scope{OrgID: "default", ClusterID: "protected"})
	provider := storage.NewMemory()
	service.SetChangeStorage(provider)
	service.changes.evidenceEnabled = true
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "protected", ClusterUID: "physical"}
	at := time.Now().UTC()
	change := Change{ID: "protected", Cluster: "protected", Kind: "Pod", Namespace: "private", Name: "protected", UID: "pod", RootUID: "root", Type: kubechanges.Created, Source: "observed", At: at}
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	record := storage.EvidenceRecord{ID: "change:protected", Type: "change", Kind: "Pod", Namespace: "private", Name: "protected", UID: "pod", RootUID: "root", Rule: string(change.Type), At: at, ObservedAt: at, Epoch: "test", Payload: raw}
	if service.projectEvidence(record).Type != "change" {
		t.Fatal("fixture is not a valid protected payload")
	}
	if err := provider.(storage.EvidenceStore).CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "committed", At: at, Epoch: "test", Principal: service.historyPrincipal(), Records: []storage.EvidenceRecord{record}}); err != nil {
		t.Fatal(err)
	}
	if err := service.rememberHistoryScope(scope); err != nil {
		t.Fatal(err)
	}
	// Prove the fixture contains a payload before testing authority filtering.
	stored, err := provider.(storage.EvidenceStore).QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute)})
	if err != nil || len(stored.Items) != 1 {
		t.Fatalf("protected fixture was not stored: items=%d, %v", len(stored.Items), err)
	}
	page, err := service.persistedHistoryQuery(t.Context(), storage.EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute)})
	if err != nil && !errors.Is(err, errHistoryAuthority) && !errors.Is(err, errHistoryNotEstablished) {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.Type != "coverage_gap" || item.Namespace != "" || item.Name != "" || item.UID != "" || item.RootUID != "" || strings.Contains(string(item.Payload), "protected") || strings.Contains(string(item.Payload), "private") {
			t.Fatal("missing kind authority exposed protected payload")
		}
	}
	if err := service.persistedPodAuthority(t.Context(), "private"); !errors.Is(err, errHistoryAuthority) {
		t.Fatalf("saved log payload accepted missing authority: %v", err)
	}
}

func TestNamespaceFallbackRetainsGlobalDenialWithRealScopedProofTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/allowed/pods") {
			t.Errorf("unexpected fallback request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList", "metadata": map[string]any{}, "items": []any{}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "fallback"})
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	cycleStart := time.Now().Add(-time.Minute).UTC()
	deniedAt := time.Now().UTC()
	service.rememberHistoryDenials(storage.EvidenceScope{OrgID: "default", ClusterID: "fallback"}, service.historyPrincipal(), map[string]storage.EvidenceAuthority{"Pod": {State: "denied", ProvedAt: deniedAt}})
	generation := inventoryGeneration{View: kubeindex.Snapshot{Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "error", Error: "forbidden", LastAttemptAt: cycleStart}}}}
	cache := &inventoryCache{namespaces: map[string]bool{"allowed": true}}
	ctx := context.WithValue(t.Context(), indexDefinitionContextKey{}, map[string]ResourceDefinition{"Pod": {Version: "v1", Resource: "pods", Kind: "Pod", Namespaced: true}})
	started := time.Now().UTC()
	if err := service.collectScopedInventory(ctx, cache, &generation, []string{"Pod"}, cycleStart); err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	proofs := historyProofs(generation)
	if proofs["Pod"].State != "denied" {
		t.Fatal("namespace fallback erased cluster-wide denial")
	}
	scoped := proofs["Pod@allowed"]
	if scoped.State != "allowed" || scoped.ProvedAt.Before(started) || scoped.ProvedAt.After(finished) {
		t.Fatalf("scoped proof has manufactured or cycle-start freshness: %+v", scoped)
	}
	cp := storage.EvidenceCheckpoint{Authority: proofs}
	service.overlayHistoryDenials(storage.EvidenceScope{OrgID: "default", ClusterID: "fallback"}, service.historyPrincipal(), &cp)
	if effectiveHistoryProof(cp, "Pod", "allowed").State != "allowed" || effectiveHistoryProof(cp, "Pod", "other").State != "denied" {
		t.Fatal("actual later scoped proof did not supersede global denial only in its namespace")
	}
	// Reusing a prior scoped snapshot must not re-date its proof.
	stale := generation.Namespaces["allowed"].Kinds["Pod"]
	stale.ObservedAt = cycleStart
	snapshot := generation.Namespaces["allowed"]
	snapshot.Kinds["Pod"] = stale
	generation.Namespaces["allowed"] = snapshot
	cp.Authority = historyProofs(generation)
	service.overlayHistoryDenials(storage.EvidenceScope{OrgID: "default", ClusterID: "fallback"}, service.historyPrincipal(), &cp)
	if effectiveHistoryProof(cp, "Pod", "allowed").State != "denied" {
		t.Fatal("older namespace proof escaped positive denial")
	}
}

func TestCertificateReviewAndRequestsPinAcceptedMaterial(t *testing.T) {
	first := &tls.Certificate{Certificate: [][]byte{[]byte("accepted")}}
	second := &tls.Certificate{Certificate: [][]byte{[]byte("replacement")}}
	transport := &http.Transport{TLSClientConfig: &tls.Config{GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return second, nil }}}
	client := &Client{http: &http.Client{Transport: transport, Timeout: time.Second}}
	pinned, closeTransport, err := client.certificateHTTP(first)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	fixed := pinned.Transport.(*http.Transport)
	certificate, err := fixed.TLSClientConfig.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil || certificate != first || !fixed.DisableKeepAlives {
		t.Fatal("certificate review could reuse another credential's connection")
	}
	if pinned.Timeout != time.Second || transport.DisableKeepAlives {
		t.Fatal("request pinning mutated shared client")
	}
	if _, _, err = (*Client)(nil).certificateHTTP(first); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("nil client accepted")
	}
}

func TestDelayedCredentialDenialCannotRetireVerifiedReplacement(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var startedOnce sync.Once
			var releaseOnce sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/selfsubjectreviews") {
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview", "status": map[string]any{"userInfo": map[string]any{"username": "verified", "uid": "identity", "groups": []string{"readers"}}}})
					return
				}
				startedOnce.Do(func() { close(started) })
				<-release
				http.Error(w, "old request", status)
			}))
			defer server.Close()
			defer releaseOnce.Do(func() { close(release) })
			service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "rotation"})
			var bearer atomic.Value
			bearer.Store("Bearer original")
			service.client.credentials = &credentialSource{authorization: func(context.Context) (string, error) { return bearer.Load().(string), nil }}
			if err := service.acceptCredential(t.Context(), "Bearer original"); err != nil {
				t.Fatal(err)
			}
			provider := storage.NewMemory()
			service.SetChangeStorage(provider)
			service.changes.evidenceEnabled = true
			scope := storage.EvidenceScope{OrgID: "default", ClusterID: "rotation", ClusterUID: "physical"}
			at := time.Now().UTC()
			principal := service.historyPrincipal()
			if err := provider.(storage.EvidenceStore).CommitEvidence(t.Context(), scope, storage.EvidenceBatch{ID: "proof", At: at, Epoch: "stable", Principal: principal, Authority: map[string]storage.EvidenceAuthority{"Pod": {State: "allowed", ProvedAt: at}}}); err != nil {
				t.Fatal(err)
			}
			if err := service.rememberHistoryScope(scope); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := service.credentialContext(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var result struct {
					Items []json.RawMessage `json:"items"`
				}
				done <- service.client.GetMetadataJSON(ctx, "/api/v1/pods", &result)
			}()
			waitFor(t, started, "request did not start")
			bearer.Store("Bearer replacement")
			if err := service.acceptCredential(t.Context(), "Bearer replacement"); err != nil {
				t.Fatal(err)
			}
			releaseOnce.Do(func() { close(release) })
			if err := <-done; err == nil {
				t.Fatal("old failing response unexpectedly succeeded")
			}
			if service.identity.ctx.Err() != nil || service.historyPrincipal() != principal {
				t.Fatal("old response retired replacement")
			}
			cp, err := provider.(storage.EvidenceStore).EvidenceCheckpoint(t.Context(), scope)
			if err != nil || cp.Authority["Pod"].State != "allowed" {
				t.Fatal("old response revoked replacement authority")
			}
		})
	}
}

func TestUnverifiedProviderHintCannotProvePrincipalEquivalence(t *testing.T) {
	now := time.Now().UTC()
	token := "first"
	source := &expiringBearerSource{principal: "configured-client-id", now: func() time.Time { return now }, refresh: func(context.Context) (string, time.Time, error) { return token, now.Add(time.Minute), nil }}
	service := NewService(&Client{credentials: source}, Scope{OrgID: "default", ClusterID: "unknown"}, 0)
	if service.identity.known {
		t.Fatal("configured provider identity became proof")
	}
	now = now.Add(2 * time.Minute)
	token = "replacement"
	if err := service.validateCredential(t.Context()); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("unproved bearer change retained epoch: %v", err)
	}
	if service.identity.ctx.Err() == nil {
		t.Fatal("unproved principal retained live inventory")
	}
}

func TestSelfSubjectReviewProvesCompletePrincipalEquivalence(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" || r.Method != http.MethodPost {
			t.Error("unexpected identity request")
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		groups := []string{"team-a", "system:authenticated"}
		if r.Header.Get("Authorization") == "Bearer renewed" {
			groups = []string{"system:authenticated", "team-a"}
		}
		if r.Header.Get("Authorization") == "Bearer changed" {
			groups = []string{"system:authenticated", "team-b"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview", "status": map[string]any{"userInfo": map[string]any{"username": "same-subject", "uid": "principal-uid", "groups": groups, "extra": map[string][]string{"scope": {"a", "b"}}}}})
	}))
	defer server.Close()
	service := newTestService(t, server.URL, Scope{OrgID: "default", ClusterID: "principal"})
	service.client.credentials = &credentialSource{
		authorization: func(context.Context) (string, error) { return "Bearer initial", nil },
		certificate:   func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return nil, nil },
	}
	if err := service.acceptCredential(t.Context(), "Bearer initial"); err != nil {
		t.Fatal(err)
	}
	identity := service.historyPrincipal()
	if identity == "" {
		t.Fatal("no verified history binding")
	}
	for i := 0; i < 3; i++ {
		if err := service.acceptCredential(t.Context(), "Bearer initial"); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unchanged bearer repeated identity review: %d", calls.Load())
	}
	if err := service.acceptCredential(t.Context(), "Bearer renewed"); err != nil {
		t.Fatal(err)
	}
	if service.historyPrincipal() != identity || service.identity.ctx.Err() != nil {
		t.Fatal("equivalent verified principal retired epoch")
	}
	if err := service.acceptCredential(t.Context(), "Bearer changed"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("same subject with different groups accepted: %v", err)
	}
	if service.historyPrincipal() != "" || service.identity.ctx.Err() == nil {
		t.Fatal("principal change retained saved-history authority")
	}
}

func TestExpiringBearerSourceRefreshesOnceWithSkew(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	calls := 0
	source := &expiringBearerSource{
		now:  func() time.Time { return now },
		skew: time.Minute,
		refresh: func(context.Context) (string, time.Time, error) {
			calls++
			return "token", now.Add(2 * time.Minute), nil
		},
	}
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := source.Authorization(context.Background()); err != nil {
				t.Errorf("authorization: %v", err)
			}
		}()
	}
	wait.Wait()
	if calls != 1 {
		t.Fatalf("refresh calls = %d", calls)
	}
	// Inside the renewal window the token still valid with margin is served
	// at once, and one refresh runs in the background.
	now = now.Add(61 * time.Second)
	if _, err := source.Authorization(context.Background()); err != nil {
		t.Fatal(err)
	}
	settleRefresh(t, source)
	if calls != 2 {
		t.Fatalf("refresh calls after skew = %d", calls)
	}
}

// The renewal window scales with the token's lifetime. A short-lived token is
// renewed at half its life and served until the serving margin, never
// counted as a failure; only a token that expires within the margin is an
// invalid answer. Tokens of 15 minutes or more renew at the configured skew,
// exactly as before.
func TestExpiringBearerSourceRenewalWindowFollowsTokenLifetime(t *testing.T) {
	start := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		skew     time.Duration
		lifetime time.Duration
		window   time.Duration
	}{
		{"90s token", 2 * time.Minute, 90 * time.Second, 45 * time.Second},
		{"120s token", 2 * time.Minute, 120 * time.Second, 60 * time.Second},
		{"60s token", time.Minute, 60 * time.Second, 45 * time.Second},
		{"EKS 15m token", time.Minute, 15 * time.Minute, time.Minute},
		{"AKS 1h token", 2 * time.Minute, time.Hour, 2 * time.Minute},
		{"GKE 1h token", 2 * time.Minute, time.Hour, 2 * time.Minute},
		{"15m token, small skew", 10 * time.Second, 15 * time.Minute, proofGraceMargin},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := start
			var calls atomic.Int32
			source := &expiringBearerSource{now: func() time.Time { return now }, skew: test.skew, refresh: func(context.Context) (string, time.Time, error) {
				calls.Add(1)
				return "token-" + strconv.Itoa(int(calls.Load())), now.Add(test.lifetime), nil
			}}
			if got, err := authorizeRenewed(t, source); err != nil || got != "Bearer token-1" {
				t.Fatalf("first token = %q, %v", got, err)
			}
			if source.renewWindow != test.window || source.failures != 0 || !source.retryAt.IsZero() {
				t.Fatalf("window=%s failures=%d retryAt=%s, want window %s and no backoff", source.renewWindow, source.failures, source.retryAt, test.window)
			}
			if test.window != max(test.skew, proofGraceMargin) && test.lifetime >= 15*time.Minute {
				t.Fatalf("a long-lived token renews at %s, not at the skew", test.window)
			}
			expires := now.Add(test.lifetime)
			now = expires.Add(-test.window - time.Second)
			if got, err := authorizeRenewed(t, source); err != nil || got != "Bearer token-1" || calls.Load() != 1 {
				t.Fatalf("before the window: %q %v calls=%d", got, err, calls.Load())
			}
			now = expires.Add(-test.window + time.Second)
			if got, err := authorizeRenewed(t, source); err != nil || got != "Bearer token-2" || calls.Load() != 2 {
				t.Fatalf("inside the window: %q %v calls=%d", got, err, calls.Load())
			}
			if source.failures != 0 || source.unreachable {
				t.Fatalf("a renewal counted as a failure: failures=%d", source.failures)
			}
		})
	}
}

// A token that expires within the serving margin cannot be sent and counts
// as an invalid answer, spaced by the backoff. A token that lives longer than
// the margin but no longer than its renewal window is served, and the next
// refresh waits half its usable life, so a provider that always issues such
// tokens is not asked once a second.
func TestExpiringBearerSourceShortTokensAtTheMargin(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	lifetime := proofGraceMargin
	var calls atomic.Int32
	source := &expiringBearerSource{now: func() time.Time { return now }, skew: 2 * time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		calls.Add(1)
		return "token", now.Add(lifetime), nil
	}}
	if _, err := authorizeRenewed(t, source); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("a token inside the serving margin was served: %v", err)
	}
	if source.failures != 1 || !source.retryAt.Equal(now.Add(time.Second)) {
		t.Fatalf("failures=%d retryAt=%s, want one failure spaced by the backoff", source.failures, source.retryAt)
	}
	now = now.Add(time.Second)
	lifetime = 40 * time.Second
	usable := lifetime - proofGraceMargin
	if got, err := authorizeRenewed(t, source); err != nil || got != "Bearer token" {
		t.Fatalf("a 40s token = %q, %v", got, err)
	}
	if source.failures != 0 || !source.retryAt.Equal(now.Add(usable/2)) {
		t.Fatalf("failures=%d retryAt=%s, want no failure and the next refresh half the usable life later", source.failures, source.retryAt)
	}
	// Reads continue every 100 ms for a simulated minute against a provider
	// that always issues 40 s tokens: it is asked about once per half usable
	// life, and every read is answered with a token.
	start, before := now, calls.Load()
	for now.Before(start.Add(time.Minute)) {
		if _, err := authorizeRenewed(t, source); err != nil {
			t.Fatalf("read at +%s: %v", now.Sub(start), err)
		}
		now = now.Add(100 * time.Millisecond)
	}
	if asked, limit := calls.Load()-before, int32(time.Minute/(usable/2))+1; asked > limit || asked < 2 {
		t.Fatalf("a constant 40s provider was asked %d times in a minute, want 2..%d", asked, limit)
	}
}

// authorizeRenewed asks source for a token once its renewal window has
// begun: the call starts the background refresh, which is waited for, and
// the answer is what the source then serves, without starting another.
func authorizeRenewed(t *testing.T, source CredentialSource) (string, error) {
	t.Helper()
	var expiring *expiringBearerSource
	switch typed := source.(type) {
	case *expiringBearerSource:
		expiring = typed
	case *gkeCredentialSource:
		expiring = typed.expiringBearerSource
	case *aksCredentialSource:
		expiring = typed.expiringBearerSource
	default:
		t.Fatalf("source %T does not renew in the background", source)
	}
	_, _ = source.Authorization(t.Context())
	settleRefresh(t, expiring)
	expiring.mu.Lock()
	defer expiring.mu.Unlock()
	if expiring.servableLocked(expiring.now()) {
		return expiring.header, nil
	}
	return "", expiring.recordedErrorLocked()
}

// settleRefresh waits until source has no refresh in flight.
func settleRefresh(t *testing.T, source *expiringBearerSource) {
	t.Helper()
	// A refresh against a failing provider runs the SDK's jittered retries,
	// which can take several seconds; the refresh timeout is its upper bound.
	for wait := polling(t, "token refresh did not finish"); ; wait.next() {
		source.mu.Lock()
		idle := source.inflight == nil
		source.mu.Unlock()
		if idle {
			return
		}
	}
}

func TestExpiringBearerSourceCanceledWaiterReturns(t *testing.T) {
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	source := &expiringBearerSource{
		now: time.Now,
		refresh: func(context.Context) (string, time.Time, error) {
			close(refreshStarted)
			<-releaseRefresh
			return "token", time.Now().Add(time.Hour), nil
		},
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := source.Authorization(context.Background())
		firstDone <- err
	}()
	<-refreshStarted

	// A waiter whose deadline passes while another caller's token request is
	// still in flight returns at once: the token host has not answered in
	// time. A waiter that is cancelled returns only the cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := source.Authorization(ctx)
	if !errors.Is(err, ErrTokenEndpointUnavailable) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("blocked waiter error = %v after %v", err, time.Since(started))
	}
	cancelled, cancelWaiter := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancelWaiter)
	started = time.Now()
	_, err = source.Authorization(cancelled)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTokenEndpointUnavailable) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("cancelled waiter error = %v after %v", err, time.Since(started))
	}
	close(releaseRefresh)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func projectedAuthFixture(t *testing.T, wrappers ...func(http.Handler) http.Handler) (Config, func(string, func(*projectedTokenClaims)) string) {
	t.Helper()
	return projectedAuthAlgorithmFixture(t, jose.ES256, "https://kubernetes.default.svc", wrappers...)
}

func projectedAuthAlgorithmFixture(t *testing.T, algorithm jose.SignatureAlgorithm, issuer string, wrappers ...func(http.Handler) http.Handler) (Config, func(string, func(*projectedTokenClaims)) string) {
	t.Helper()
	var key, publicKey any
	if algorithm == jose.RS256 {
		generated, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		key, publicKey = generated, &generated.PublicKey
	} else {
		generated, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		key, publicKey = generated, &generated.PublicKey
	}
	var handler http.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(writer, map[string]string{"issuer": issuer, "jwks_uri": issuer + "/openid/v1/jwks"})
		case "/openid/v1/jwks":
			writeJSON(writer, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: publicKey, KeyID: "cluster-key", Algorithm: string(algorithm), Use: "sig"}}})
		default:
			http.NotFound(writer, request)
		}
	})
	for _, wrap := range wrappers {
		handler = wrap(handler)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	config := Config{Endpoint: server.URL, CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), AllowLoopback: true}
	sign := func(id string, alter func(*projectedTokenClaims)) string {
		t.Helper()
		now := time.Now().UTC()
		claims := projectedTokenClaims{Claims: jwt.Claims{Issuer: issuer, Subject: "system:serviceaccount:default:reader", Audience: jwt.Audience{issuer}, ID: id, Expiry: jwt.NewNumericDate(now.Add(time.Hour)), NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)), IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute))}}
		claims.Kubernetes.Namespace = "default"
		claims.Kubernetes.ServiceAccount.Name, claims.Kubernetes.ServiceAccount.UID = "reader", "service-account-uid"
		if alter != nil {
			alter(&claims)
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: algorithm, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "cluster-key"))
		if err != nil {
			t.Fatal(err)
		}
		token, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	return config, sign
}

func TestProjectedNativeClaimsServiceConstruction(t *testing.T) {
	config, sign := projectedAuthAlgorithmFixture(t, jose.RS256, "https://kubernetes.default.svc.cluster.local")
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	token := sign("native-shaped", func(claims *projectedTokenClaims) {
		claims.Subject = "system:serviceaccount:versus-system:versus-versus-incident"
		claims.Kubernetes.Namespace = "versus-system"
		claims.Kubernetes.ServiceAccount.Name = "versus-versus-incident"
		claims.Kubernetes.ServiceAccount.UID = "d5091046-0ed5-41fb-97f8-c1e509e132e2"
		claims.Expiry = jwt.NewNumericDate(time.Now().Add(365 * 24 * time.Hour))
	})
	if err := os.WriteFile(config.TokenFile, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	if status := client.credentials.(*tokenFileSource).verifier.verificationStatus(); status.Stage != "verified" {
		t.Fatalf("native-shaped RS256 rejection stage = %s", status.Stage)
	}
	if service.CaptureStatus().Mode != "continuous" || !service.identity.known {
		t.Fatal("native-shaped RS256 principal failed service propagation")
	}
}

func TestProjectedNativeJWKSContentNegotiation(t *testing.T) {
	config, sign := projectedAuthAlgorithmFixture(t, jose.RS256, "https://kubernetes.default.svc.cluster.local", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/openid/v1/jwks" && request.Header.Get("Accept") != "application/jwk-set+json" {
				writer.WriteHeader(http.StatusNotAcceptable)
				return
			}
			if request.URL.Path == "/.well-known/openid-configuration" && request.Header.Get("Accept") != "application/json" {
				t.Error("discovery content negotiation changed")
			}
			next.ServeHTTP(writer, request)
		})
	})
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(config.TokenFile, []byte(sign("reader", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	if service.CaptureStatus().Mode != "continuous" {
		status := client.credentials.(*tokenFileSource).verifier.verificationStatus()
		t.Fatalf("native JWKS rejected at %s HTTP %d", status.Stage, status.HTTPStatus)
	}
}

func TestProjectedReaderVerificationStatus(t *testing.T) {
	for _, test := range []struct {
		name  string
		path  string
		code  int
		stage string
	}{
		{"discovery_unauthorized", "/.well-known/openid-configuration", http.StatusUnauthorized, "discovery_response"},
		{"jwks_forbidden", "/openid/v1/jwks", http.StatusForbidden, "jwks_response"},
		{"jwks_not_acceptable", "/openid/v1/jwks", http.StatusNotAcceptable, "jwks_response"},
		{"reader_accepted", "", 0, "verified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var reader string
			config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Header.Get("Authorization") != "Bearer "+reader {
						t.Error("metadata request did not use the reader bearer")
					}
					if request.URL.Path == test.path {
						writer.WriteHeader(test.code)
						return
					}
					next.ServeHTTP(writer, request)
				})
			})
			reader = sign("reader", nil)
			config.TokenFile = filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(config.TokenFile, []byte(reader), 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{ClusterID: "cluster"}, 0)
			service.SetChangeStorage(storage.NewMemory())
			status := client.credentials.(*tokenFileSource).verifier.verificationStatus()
			if status.Stage != test.stage || (test.code != 0 && status.HTTPStatus != test.code) {
				t.Fatalf("verification stage/status = %s/%d", status.Stage, status.HTTPStatus)
			}
			capture := service.CaptureStatus()
			if test.code != 0 {
				if capture.Mode != "demand" || capture.Reason != "stable_principal_unverified" || capture.Verification == nil || *capture.Verification != status {
					t.Fatal("rejection did not propagate safely through service construction")
				}
			} else if capture.Mode != "continuous" || capture.Verification != nil {
				t.Fatal("verified reader principal did not propagate through service construction")
			}
		})
	}
}

func TestProjectedServiceAccountReplicasRotationAndReplacement(t *testing.T) {
	config, sign := projectedAuthFixture(t)
	provider := storage.NewMemory()
	paths := []string{filepath.Join(t.TempDir(), "token"), filepath.Join(t.TempDir(), "token")}
	services := make([]*Service, 2)
	for position, path := range paths {
		if err := os.WriteFile(path, []byte(sign(path, nil)), 0o600); err != nil {
			t.Fatal(err)
		}
		copy := config
		copy.TokenFile = path
		client, err := NewClient(copy)
		if err != nil {
			t.Fatal(err)
		}
		services[position] = NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
		services[position].SetChangeStorage(provider)
		if services[position].CaptureStatus().Mode != "continuous" {
			t.Fatal("verified projected token fell back to demand")
		}
	}
	first, second := services[0], services[1]
	if first.indexScope() != second.indexScope() {
		t.Fatal("different Pod tokens split one verified SA principal")
	}
	for range 2 {
		for _, replica := range services {
			identity, scope := replica.identity, replica.indexScope()
			verifier := replica.client.credentials.(*tokenFileSource).verifier
			verifier.mu.Lock()
			verifier.until = time.Now().Add(-time.Second)
			verifier.mu.Unlock()
			if err := replica.validateCredential(t.Context()); err != nil || replica.identity != identity || replica.indexScope() != scope {
				t.Fatalf("periodic refresh split replica epoch: %v", err)
			}
		}
	}
	now := time.Now().UTC()
	if err := first.changeStore().ClaimLease("first", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := second.changeStore().ClaimLease("second", now, time.Minute); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatal("two replicas acquired one principal")
	}
	change := Change{ID: "history", Kind: "Pod", UID: "pod", Name: "api", Cluster: "cluster", Type: kubechanges.Created, At: now}
	if err := first.commitCredential(t.Context(), func(context.Context) error {
		return first.changeStore().Commit([]Change{change}, nil, nil, "first", now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths[0], []byte(sign("rotation", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.validateCredential(t.Context()); err != nil {
		t.Fatal("verified principal rotation retired its epoch")
	}
	page, err := second.Changes(t.Context(), ChangeQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != change.ID {
		t.Fatal("rotation lost shared history")
	}
	registry := NewServiceRegistry(first)
	t.Cleanup(registry.Close)
	if err := os.WriteFile(paths[0], []byte(sign("replacement", func(claims *projectedTokenClaims) { claims.Kubernetes.ServiceAccount.UID = "replacement-uid" })), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := registry.ResolveCluster("org", "cluster")
	if err != nil || replacement.indexScope() == first.indexScope() {
		t.Fatal("different SA UID reused history scope")
	}
	if !replacement.registered {
		t.Fatal("a replaced service lost its registry membership, so stored records could be served before capture proves them")
	}
	page, err = replacement.Changes(t.Context(), ChangeQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("replacement exposed previous principal history")
	}
	if err := first.commitCredential(t.Context(), func(context.Context) error { t.Fatal("retired principal committed"); return nil }); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("old write not fenced")
	}
	if page, err := first.Changes(t.Context(), ChangeQuery{}); err == nil || len(page.Items) != 0 {
		t.Fatal("old principal served history")
	}
}

func TestProjectedRotationCanceledDiscoveryPreservesCapture(t *testing.T) {
	var blocked atomic.Bool
	started := make(chan struct{}, 1)
	config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if blocked.Load() && request.URL.Path == "/.well-known/openid-configuration" {
				started <- struct{}{}
				<-request.Context().Done()
				return
			}
			next.ServeHTTP(writer, request)
		})
	})
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(config.TokenFile, []byte(sign("initial", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	scope := service.indexScope()
	now := time.Now().UTC()
	store := service.changeStore()
	if err := store.ClaimLease("capture-owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	change := Change{ID: "history", Kind: "Pod", UID: "pod", Name: "api", Cluster: "cluster", Type: kubechanges.Created, At: now}
	if err := store.Commit([]Change{change}, nil, nil, "capture-owner", now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.TokenFile, []byte(sign("rotation", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- service.validateCredential(ctx) }()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("discovery did not start")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verification error = %v", err)
	}
	if service.identity.ctx.Err() != nil || service.indexScope() != scope {
		t.Fatal("transient discovery retired or changed verified identity")
	}
	blocked.Store(false)
	if err := service.commitCredential(t.Context(), func(context.Context) error { return store.RenewLease("capture-owner", time.Now().UTC(), time.Minute) }); err != nil {
		t.Fatalf("same-token retry did not preserve owner: %v", err)
	}
	page, err := service.Changes(t.Context(), ChangeQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != change.ID || service.indexScope() != scope {
		t.Fatalf("retry lost stable history: %+v %v", page, err)
	}
}

func TestProjectedCaptureTransientRenewalPreservesOwnerUntilExpiry(t *testing.T) {
	for _, recoverVerification := range []bool{true, false} {
		name := "expires"
		if recoverVerification {
			name = "recovers"
		}
		t.Run(name, func(t *testing.T) {
			var unavailable atomic.Bool
			started := make(chan struct{}, 1)
			config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/apis/authentication.k8s.io/v1/selfsubjectreviews" {
						http.NotFound(writer, request)
						return
					}
					if unavailable.Load() && request.URL.Path == "/.well-known/openid-configuration" {
						select {
						case started <- struct{}{}:
						default:
						}
						http.Error(writer, "unavailable", http.StatusServiceUnavailable)
						return
					}
					if request.URL.Path != "/.well-known/openid-configuration" && request.URL.Path != "/openid/v1/jwks" {
						t.Error("capture unexpectedly started a resource read")
					}
					next.ServeHTTP(writer, request)
				})
			})
			config.TokenFile = filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(config.TokenFile, []byte(sign("initial", nil)), 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{ClusterID: "cluster"}, 0)
			service.SetChangeStorage(storage.NewMemory())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			loop := &captureLoop{ctx: ctx, cancel: cancel, done: make(chan struct{}), owner: "capture-owner", leaseTTL: 500 * time.Millisecond, renewInterval: 20 * time.Millisecond, next: map[string]time.Time{}, success: map[string]time.Time{}, gapFrom: map[string]time.Time{}, status: CaptureStatus{Mode: "continuous", Behind: true}}
			for kind := range captureCadences {
				loop.next[kind] = time.Now().Add(time.Hour)
			}
			service.changes.captures[service.indexScope()] = loop
			go service.capture(loop)
			t.Cleanup(service.StopCapture)
			ready := time.NewTicker(time.Millisecond)
			defer ready.Stop()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			var owned context.Context
			for owned == nil {
				loop.mu.Lock()
				owned = loop.ownedContext
				loop.mu.Unlock()
				if owned != nil {
					break
				}
				select {
				case <-ready.C:
				case <-deadline.C:
					t.Fatal("capture never acquired lease")
				}
			}
			if err := os.WriteFile(config.TokenFile, []byte(sign("rotated", nil)), 0o600); err != nil {
				t.Fatal(err)
			}
			unavailable.Store(true)
			select {
			case <-started:
			case <-deadline.C:
				t.Fatal("renewal did not verify rotated token")
			}
			if recoverVerification {
				unavailable.Store(false)
				if err := service.validateCredential(t.Context()); err != nil {
					t.Fatal(err)
				}
				loop.mu.Lock()
				owner, stillOwned := loop.owner, loop.owned
				loop.mu.Unlock()
				if owner != "capture-owner" || !stillOwned || owned.Err() != nil {
					t.Fatal("transient renewal churned owner")
				}
			} else {
				select {
				case <-owned.Done():
				case <-deadline.C:
					t.Fatal("capture continued past unextended lease expiry")
				}
				if err := service.changeStore().Commit(nil, nil, nil, "capture-owner", time.Now().UTC()); !errors.Is(err, kubechanges.ErrLeaseLost) {
					t.Fatalf("expired owner not fenced: %v", err)
				}
			}
			if service.identity.ctx.Err() != nil {
				t.Fatal("transient renewal retired verified identity")
			}
		})
	}
}

func TestProjectedVerificationTransientResponsesRetry(t *testing.T) {
	for _, path := range []string{"/.well-known/openid-configuration", "/openid/v1/jwks"} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
			t.Run(path+"/"+http.StatusText(status), func(t *testing.T) {
				var unavailable atomic.Bool
				var reads atomic.Int32
				config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.URL.Path == path {
							reads.Add(1)
							if unavailable.Load() {
								writer.Header().Set("Retry-After", "1")
								http.Error(writer, "unavailable", status)
								return
							}
						}
						next.ServeHTTP(writer, request)
					})
				})
				config.TokenFile = filepath.Join(t.TempDir(), "token")
				if err := os.WriteFile(config.TokenFile, []byte(sign("initial", nil)), 0o600); err != nil {
					t.Fatal(err)
				}
				client, err := NewClient(config)
				if err != nil {
					t.Fatal(err)
				}
				service := NewService(client, Scope{ClusterID: "cluster"}, 0)
				registry := NewServiceRegistry(service)
				t.Cleanup(registry.Close)
				scope, fingerprint := service.indexScope(), service.identity.fingerprint
				if err := os.WriteFile(config.TokenFile, []byte(sign("rotated", nil)), 0o600); err != nil {
					t.Fatal(err)
				}
				unavailable.Store(true)
				_, err = registry.ResolveCluster("org", "cluster")
				if err == nil || service.identity.ctx.Err() != nil || service.identity.fingerprint != fingerprint || service.indexScope() != scope {
					t.Fatalf("transient response accepted token or retired identity: %v", err)
				}
				if status == http.StatusTooManyRequests {
					var throttled *ErrThrottled
					if !errors.As(err, &throttled) {
						t.Fatalf("throttle error hidden: %v", err)
					}
					before := reads.Load()
					ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
					defer cancel()
					if err := service.validateCredential(ctx); !errors.Is(err, context.DeadlineExceeded) || reads.Load() != before {
						t.Fatalf("retry ignored backoff: %v reads=%d", err, reads.Load()-before)
					}
				}
				unavailable.Store(false)
				if err := service.validateCredential(t.Context()); err != nil || service.identity.fingerprint == fingerprint || service.indexScope() != scope {
					t.Fatalf("successful retry was negatively cached: %v", err)
				}
			})
		}
	}
}

func TestProjectedCanceledConstructorDoesNotPoisonVerifiedService(t *testing.T) {
	var blocked atomic.Bool
	var attempts atomic.Int32
	config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if blocked.Load() && request.URL.Path == "/.well-known/openid-configuration" {
				attempts.Add(1)
				<-request.Context().Done()
				return
			}
			next.ServeHTTP(writer, request)
		})
	})
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(config.TokenFile, []byte(sign("initial", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	verified := NewService(client, Scope{ClusterID: "cluster"}, 0)
	if err := os.WriteFile(config.TokenFile, []byte(sign("rotated", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	unavailable := newService(ctx, client, verified.Scope(), 0)
	if unavailable.identity.known || attempts.Load() != 1 || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("canceled constructor accepted unverified token")
	}
	blocked.Store(false)
	if err := verified.validateCredential(t.Context()); err != nil || verified.identity.ctx.Err() != nil {
		t.Fatalf("constructor retired old verified identity: %v", err)
	}
}

func TestProjectedRotationInvalidSignatureFencesEpoch(t *testing.T) {
	config, sign := projectedAuthFixture(t)
	_, untrustedSign := projectedAuthFixture(t)
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(config.TokenFile, []byte(sign("initial", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{ClusterID: "cluster"}, 0)
	if err := os.WriteFile(config.TokenFile, []byte(untrustedSign("invalid-signature", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.validateCredential(t.Context()); !errors.Is(err, ErrCredentialUnavailable) || service.identity.ctx.Err() == nil {
		t.Fatalf("invalid signature did not fence old epoch: %v", err)
	}
	if err := service.commitCredential(t.Context(), func(context.Context) error { t.Fatal("invalid token committed"); return nil }); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("invalid signature permitted write: %v", err)
	}
}

func TestProjectedDiscoveryJWKSMetadataNeverSelectsDestination(t *testing.T) {
	var externalReads atomic.Int32
	external := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		externalReads.Add(1)
		t.Error("advertised destination received a request")
	}))
	defer external.Close()
	for _, advertised := range []string{"https://172.19.0.6:6443/openid/v1/jwks", external.URL + "/openid/v1/jwks", "http://untrusted.invalid/keys?redirect=external", "not a URL"} {
		t.Run(advertised, func(t *testing.T) {
			var fixedReads atomic.Int32
			config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/.well-known/openid-configuration" {
						writeJSON(writer, map[string]string{"issuer": "https://kubernetes.default.svc", "jwks_uri": advertised})
						return
					}
					if request.URL.Path == "/openid/v1/jwks" {
						fixedReads.Add(1)
					}
					next.ServeHTTP(writer, request)
				})
			})
			client, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			verifier := &projectedTokenVerifier{client: client}
			principal, err := verifier.verify(t.Context(), sign("native", nil))
			if err != nil || principal == "" || fixedReads.Load() != 1 || externalReads.Load() != 0 {
				t.Fatalf("fixed-origin verification failed: principal=%t error=%v fixed=%d external=%d", principal != "", err, fixedReads.Load(), externalReads.Load())
			}
		})
	}
}

func TestUnverifiedProjectedTokensExplicitDemand(t *testing.T) {
	config, sign := projectedAuthFixture(t)
	for _, test := range []struct {
		name  string
		alter func(*projectedTokenClaims)
	}{
		{"issuer", func(claims *projectedTokenClaims) { claims.Issuer = "https://untrusted.invalid" }},
		{"audience", func(claims *projectedTokenClaims) { claims.Audience = jwt.Audience{"wrong"} }},
		{"expired", func(claims *projectedTokenClaims) { claims.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Minute)) }},
		{"future", func(claims *projectedTokenClaims) { claims.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour)) }},
		{"issued_future", func(claims *projectedTokenClaims) { claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) }},
		{"missing_uid", func(claims *projectedTokenClaims) { claims.Kubernetes.ServiceAccount.UID = "" }},
		{"subject", func(claims *projectedTokenClaims) { claims.Subject = "system:serviceaccount:other:reader" }},
		{"missing_expiry", func(claims *projectedTokenClaims) { claims.Expiry = nil }},
		{"unsigned", nil},
		{"untrusted_tls", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			token := sign(test.name, test.alter)
			if test.name == "unsigned" {
				token = "eyJhbGciOiJub25lIn0.e30."
			}
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			copy := config
			copy.TokenFile = path
			if test.name == "untrusted_tls" {
				copy.CAData = nil
			}
			client, err := NewClient(copy)
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{ClusterID: "cluster"}, 0)
			provider := storage.NewMemory()
			service.SetChangeStorage(provider)
			for range 2 {
				service.StartCapture(t.Context())
				status := service.CaptureStatus()
				if status.Mode != "demand" || status.Reason != "stable_principal_unverified" || status.Behind {
					t.Fatalf("silent continuous claim: %+v", status)
				}
				if projected := captureIndexStatus(status); projected.Mode != "demand" || projected.Reason != status.Reason {
					t.Fatal("public index status lost effective demand reason")
				}
			}
			service.StopCapture()
			blobs, err := provider.ListBlobs("kubernetes/changes/")
			if err != nil || len(blobs) != 0 {
				t.Fatal("unverified token acquired lease")
			}
		})
	}
}

func TestRegistryBlockedProjectedVerificationKeepsOtherClusterResponsive(t *testing.T) {
	config, sign := projectedAuthFixture(t)
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	var keys jose.JSONWebKeySet
	if err := client.GetJSON(t.Context(), "/openid/v1/jwks", &keys); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}, 1), make(chan struct{})
	var blocked atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/.well-known/openid-configuration" {
			writeJSON(writer, map[string]string{"issuer": "https://kubernetes.default.svc", "jwks_uri": "https://kubernetes.default.svc/openid/v1/jwks"})
			return
		}
		if blocked.Load() {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		writeJSON(writer, keys)
	}))
	defer server.Close()
	config.Endpoint = server.URL
	config.CAData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(config.TokenFile, []byte(sign("before", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err = NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	slow := NewService(client, Scope{ClusterID: "slow"}, 0)
	fast := NewService(nil, Scope{ClusterID: "fast"}, 0)
	registry := NewClusterRegistry(true, []ClusterEntry{{Info: ClusterInfo{ID: "slow"}, Service: slow}, {Info: ClusterInfo{ID: "fast"}, Service: fast}})
	defer registry.Close()
	blocked.Store(true)
	if err := os.WriteFile(config.TokenFile, []byte(sign("renewed", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, resolveErr := registry.ResolveCluster("org", "slow"); done <- resolveErr }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("projected JWKS verification did not block")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	before := time.Now()
	_, resolveErr := registry.resolveCluster(ctx, "org", "fast", nil)
	close(release)
	if resolveErr != nil {
		t.Fatalf("blocked JWKS stopped another cluster: %v", resolveErr)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	t.Logf("blocked_tls_jwks=true other_cluster_resolution=%s bound=200ms", time.Since(before))
}

func TestProjectedPositiveFreshnessBound(t *testing.T) {
	config, sign := projectedAuthFixture(t)
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &projectedTokenVerifier{client: client}
	token := sign("long-lived", func(claims *projectedTokenClaims) {
		claims.Expiry = jwt.NewNumericDate(time.Now().Add(365 * 24 * time.Hour))
	})
	principal, err := verifier.verify(t.Context(), token)
	after := time.Now()
	if err != nil || principal == "" {
		t.Fatalf("verification failed: %v", err)
	}
	if verifier.until.After(after.Add(30 * time.Second)) {
		t.Fatalf("positive freshness exceeds 30 seconds: %s", verifier.until.Sub(after))
	}
	expiry := jwt.NewNumericDate(time.Now().Add(10 * time.Second))
	principal, err = verifier.verify(t.Context(), sign("short-lived", func(claims *projectedTokenClaims) {
		claims.Expiry = expiry
	}))
	if err != nil || principal == "" || !verifier.until.Equal(expiry.Time()) {
		t.Fatalf("positive freshness exceeded JWT expiry: %v", err)
	}
}

func TestProjectedUnauthorizedFencesHistoryAndRenewal(t *testing.T) {
	for _, mode := range []string{"read", "stream", "history_refresh", "lease_refresh"} {
		t.Run(mode, func(t *testing.T) {
			var rejected atomic.Bool
			var discoveryReads atomic.Int32
			config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/.well-known/openid-configuration" {
						discoveryReads.Add(1)
					}
					if rejected.Load() {
						writer.WriteHeader(http.StatusUnauthorized)
						return
					}
					next.ServeHTTP(writer, request)
				})
			})
			config.TokenFile = filepath.Join(t.TempDir(), "token")
			token := sign("long-lived", func(claims *projectedTokenClaims) {
				claims.Expiry = jwt.NewNumericDate(time.Now().Add(365 * 24 * time.Hour))
			})
			if err := os.WriteFile(config.TokenFile, []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "configured"}, 0)
			provider := storage.NewMemory()
			service.SetChangeStorage(provider)
			alias := service.Scoped(service.Scope())
			shared := NewService(client, service.Scope(), 0)
			shared.SetChangeStorage(provider)
			bound, cancel := alias.credentialContext(t.Context())
			defer cancel()
			now := time.Now().UTC()
			store := service.changeStore()
			if err := store.ClaimLease("owner", now, time.Minute); err != nil {
				t.Fatal(err)
			}
			change := Change{ID: "history", Kind: "Pod", UID: "pod", Name: "api", Cluster: "cluster", Type: kubechanges.Created, At: now}
			if err := service.commitCredential(t.Context(), func(context.Context) error {
				return store.Commit([]Change{change}, nil, nil, "owner", now)
			}); err != nil {
				t.Fatal(err)
			}
			verifier := client.credentials.(*tokenFileSource).verifier
			if mode == "history_refresh" || mode == "lease_refresh" {
				verifier.mu.Lock()
				verifier.until = time.Now().Add(-time.Second)
				verifier.mu.Unlock()
			}
			rejected.Store(true)
			switch mode {
			case "read":
				var output any
				err = client.GetJSON(t.Context(), "/api/v1/pods", &output)
			case "stream":
				_, err = client.openPodLogStream(t.Context(), "/api/v1/namespaces/default/pods/api/log", false)
			case "history_refresh":
				_, err = service.Changes(t.Context(), ChangeQuery{})
			case "lease_refresh":
				err = service.commitCredential(t.Context(), func(context.Context) error { t.Fatal("rejected refresh renewed lease"); return nil })
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("definitive rejection hidden: %v", err)
			}
			waitFor(t, bound.Done(), "alias stream context survived rejection")
			for _, old := range []*Service{service, alias, shared} {
				if old.identity.ctx.Err() == nil {
					t.Fatal("shared client epoch survived rejection")
				}
				if page, err := old.Changes(t.Context(), ChangeQuery{}); err == nil || len(page.Items) != 0 {
					t.Fatal("retired epoch exposed history")
				}
				if err := old.commitCredential(t.Context(), func(context.Context) error { t.Fatal("retired epoch committed"); return nil }); err == nil {
					t.Fatal("retired epoch accepted renewal")
				}
			}
			registry := NewServiceRegistry(service)
			defer registry.Close()
			if _, err := registry.ResolveCluster("org", "cluster"); err == nil {
				t.Fatal("unauthorized resolve accepted fingerprint fallback")
			}
			before := discoveryReads.Load()
			freshClient, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			fresh := NewService(freshClient, service.Scope(), 0)
			fresh.SetChangeStorage(provider)
			if discoveryReads.Load() <= before || fresh.identity.known {
				t.Fatal("fresh same-SA client did not receive actual authentication rejection")
			}
			if page, err := fresh.Changes(t.Context(), ChangeQuery{}); err == nil || len(page.Items) != 0 {
				t.Fatal("fresh unauthorized client read retired history")
			}
		})
	}
}

func TestProjectedExpiredFreshnessTransientAndForbidden(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var failed atomic.Bool
			var reads atomic.Int32
			config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.URL.Path == "/api/v1/secrets" {
						writer.WriteHeader(http.StatusForbidden)
						return
					}
					reads.Add(1)
					if failed.Load() {
						writer.WriteHeader(code)
						return
					}
					next.ServeHTTP(writer, request)
				})
			})
			config.TokenFile = filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(config.TokenFile, []byte(sign("unchanged", nil)), 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{OrgID: "org", ClusterID: "cluster"}, 0)
			service.SetChangeStorage(storage.NewMemory())
			identity, scope := service.identity, service.indexScope()
			var output any
			if err := client.GetJSON(t.Context(), "/api/v1/secrets", &output); !errors.Is(err, ErrForbidden) || identity.ctx.Err() != nil {
				t.Fatal("kind-specific 403 retired capture")
			}
			if err := service.validateCredential(t.Context()); err != nil {
				t.Fatal(err)
			}
			verifier := client.credentials.(*tokenFileSource).verifier
			verifier.mu.Lock()
			verifier.until = time.Now().Add(-time.Second)
			verifier.mu.Unlock()
			failed.Store(true)
			for range 2 {
				if err := service.commitCredential(t.Context(), func(context.Context) error { t.Fatal("expired freshness accepted commit"); return nil }); err == nil {
					t.Fatal("expired principal accepted request")
				}
				if code == http.StatusTooManyRequests {
					client.backoffMu.Lock()
					client.backoffUntil = time.Time{}
					client.backoffMu.Unlock()
				}
			}
			if service.identity != identity || identity.ctx.Err() != nil || service.indexScope() != scope || !identity.known {
				t.Fatal("failed refresh retired known scoped status")
			}
			before := reads.Load()
			failed.Store(false)
			if err := service.validateCredential(t.Context()); err != nil || reads.Load() <= before || service.indexScope() != scope {
				t.Fatalf("failed verification negatively cached: %v", err)
			}
		})
	}
}

func TestProjectedLateVerificationCannotResurrectRejectedCache(t *testing.T) {
	var blocked atomic.Bool
	started, release := make(chan struct{}), make(chan struct{})
	var startedOnce sync.Once
	config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/api/v1/pods" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			if blocked.Load() && request.URL.Path == "/openid/v1/jwks" {
				startedOnce.Do(func() { close(started) })
				select {
				case <-release:
				case <-request.Context().Done():
					return
				}
			}
			next.ServeHTTP(writer, request)
		})
	})
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &projectedTokenVerifier{client: client}
	token := sign("unchanged", nil)
	if principal, err := verifier.verify(t.Context(), token); err != nil || principal == "" {
		t.Fatal("initial verification failed")
	}
	verifier.until = time.Now().Add(-time.Second)
	blocked.Store(true)
	done := make(chan error, 1)
	go func() { _, err := verifier.verify(t.Context(), token); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("verification did not start")
	}
	var output any
	err = client.GetJSON(t.Context(), "/api/v1/pods", &output)
	close(release)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("API rejection missing: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("late verification accepted rejected generation: %v", err)
	}
	blocked.Store(false)
	if verifier.generation == client.authGeneration.Load() {
		t.Fatal("late verification resurrected cache generation")
	}
	if principal, err := verifier.verify(t.Context(), token); err != nil || principal == "" {
		t.Fatalf("generation rejection poisoned fresh verification: %v", err)
	}
}

func TestProjectedUnchangedTokenRefreshRejectsChangedTrust(t *testing.T) {
	for _, change := range []string{"removed_key", "replaced_key", "issuer"} {
		t.Run(change, func(t *testing.T) {
			var changed atomic.Bool
			var reads atomic.Int32
			other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					reads.Add(1)
					if changed.Load() {
						if change == "issuer" && request.URL.Path == "/.well-known/openid-configuration" {
							writeJSON(writer, map[string]string{"issuer": "https://other.invalid"})
							return
						}
						if change != "issuer" && request.URL.Path == "/openid/v1/jwks" {
							keys := jose.JSONWebKeySet{}
							if change == "replaced_key" {
								keys.Keys = []jose.JSONWebKey{{Key: &other.PublicKey, KeyID: "cluster-key", Algorithm: "ES256", Use: "sig"}}
							}
							writeJSON(writer, keys)
							return
						}
					}
					next.ServeHTTP(writer, request)
				})
			})
			config.TokenFile = filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(config.TokenFile, []byte(sign("initial", nil)), 0o600); err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(config)
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(client, Scope{ClusterID: "cluster"}, 0)
			before := reads.Load()
			if err := os.WriteFile(config.TokenFile, []byte(sign("new-raw-same-SA", nil)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := service.validateCredential(t.Context()); err != nil || reads.Load() <= before {
				t.Fatal("new raw token inherited cached verification")
			}
			changed.Store(true)
			if err := service.validateCredential(t.Context()); err != nil {
				t.Fatal("fresh bounded cache unexpectedly refreshed")
			}
			verifier := client.credentials.(*tokenFileSource).verifier
			verifier.mu.Lock()
			verifier.until = time.Now().Add(-time.Second)
			verifier.mu.Unlock()
			if err := service.validateCredential(t.Context()); !errors.Is(err, ErrCredentialUnavailable) || service.identity.ctx.Err() == nil {
				t.Fatalf("unchanged token survived changed trust after TTL: %v", err)
			}
		})
	}
}

func TestProjectedExpiredFreshnessCancellationDoesNotExtendLease(t *testing.T) {
	var blocked atomic.Bool
	config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if blocked.Load() {
				<-request.Context().Done()
				return
			}
			next.ServeHTTP(writer, request)
		})
	})
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(config.TokenFile, []byte(sign("unchanged", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{ClusterID: "cluster"}, 0)
	service.SetChangeStorage(storage.NewMemory())
	store := service.changeStore()
	now, ttl := time.Now().UTC(), time.Minute
	if err := store.ClaimLease("owner", now, ttl); err != nil {
		t.Fatal(err)
	}
	verifier := client.credentials.(*tokenFileSource).verifier
	verifier.until = time.Now().Add(-time.Second)
	blocked.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := service.commitCredential(ctx, func(context.Context) error {
		t.Fatal("expired freshness renewed lease during timeout")
		return store.RenewLease("owner", now.Add(time.Second), ttl)
	}); !errors.Is(err, context.DeadlineExceeded) || service.identity.ctx.Err() != nil || !service.identity.known {
		t.Fatalf("timeout changed known identity: %v", err)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if err := service.validateCredential(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation accepted expired principal: %v", err)
	}
	if err := store.ClaimLease("competitor", now.Add(ttl/2), ttl); !errors.Is(err, kubechanges.ErrLeaseLost) {
		t.Fatal("transient failure released existing lease early")
	}
	if err := store.ClaimLease("competitor", now.Add(ttl+time.Second), ttl); err != nil {
		t.Fatalf("transient failure extended existing lease: %v", err)
	}
	blocked.Store(false)
	if err := service.validateCredential(t.Context()); err != nil {
		t.Fatalf("timeout poisoned verification retry: %v", err)
	}
}

func TestProjectedAPIRejectionCancelsOpenStream(t *testing.T) {
	opened, stopped := make(chan struct{}), make(chan struct{})
	var openedOnce sync.Once
	var stoppedOnce sync.Once
	config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if strings.HasSuffix(request.URL.Path, "/log") {
				writer.WriteHeader(http.StatusOK)
				writer.(http.Flusher).Flush()
				openedOnce.Do(func() { close(opened) })
				<-request.Context().Done()
				stoppedOnce.Do(func() { close(stopped) })
				return
			}
			if request.URL.Path == "/api/v1/pods" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(writer, request)
		})
	})
	config.TokenFile = filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(config.TokenFile, []byte(sign("unchanged", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(client, Scope{ClusterID: "cluster"}, 0)
	done := make(chan error, 1)
	go func() {
		done <- service.streamPodLogs(t.Context(), PodLogStreamOptions{Namespace: "default", Pod: "api"}, func(PodLogStreamEvent) error { return nil }, time.Second, time.Second, time.Second, 1024)
	}()
	waitFor(t, opened, "stream did not open")
	var output any
	if err := client.GetJSON(t.Context(), "/api/v1/pods", &output); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("API did not reject credential")
	}
	waitFor(t, stopped, "API rejection did not cancel upstream stream")
	waitFor(t, done, "stream did not stop")
}

func TestProjectedVerifierRejectsUntrustedKeysAndDiscovery(t *testing.T) {
	config, sign := projectedAuthFixture(t)
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	var trustedKeys jose.JSONWebKeySet
	if err := client.GetJSON(t.Context(), "/openid/v1/jwks", &trustedKeys); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"redirect", "jwks_redirect", "oversized", "wrong_signature", "unknown_key", "duplicate_key", "wrong_key_use", "wrong_key_algorithm"} {
		t.Run(name, func(t *testing.T) {
			var jwksReads atomic.Int32
			var externalReads atomic.Int32
			external := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				externalReads.Add(1)
			}))
			defer external.Close()
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/.well-known/openid-configuration" {
					if name == "redirect" {
						http.Redirect(writer, request, external.URL+"/openid/v1/jwks", http.StatusFound)
						return
					}
					if name == "oversized" {
						_, _ = writer.Write([]byte(strings.Repeat("x", (64<<10)+1)))
						return
					}
					writeJSON(writer, map[string]string{"issuer": "https://kubernetes.default.svc", "jwks_uri": "https://kubernetes.default.svc/openid/v1/jwks"})
					return
				}
				jwksReads.Add(1)
				if name == "jwks_redirect" {
					http.Redirect(writer, request, external.URL+"/openid/v1/jwks", http.StatusFound)
					return
				}
				key := trustedKeys.Keys[0]
				keys := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{key}}
				switch name {
				case "wrong_signature":
					other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
					if err != nil {
						t.Error(err)
						return
					}
					keys.Keys[0].Key = &other.PublicKey
				case "unknown_key":
					keys.Keys[0].KeyID = "other"
				case "duplicate_key":
					keys.Keys = append(keys.Keys, key)
				case "wrong_key_use":
					keys.Keys[0].Use = "enc"
				case "wrong_key_algorithm":
					keys.Keys[0].Algorithm = "RS256"
				}
				writeJSON(writer, keys)
			}))
			defer server.Close()
			copy := config
			copy.Endpoint = server.URL
			copy.CAData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			client, err := NewClient(copy)
			if err != nil {
				t.Fatal(err)
			}
			verifier := &projectedTokenVerifier{client: client}
			if principal, _ := verifier.verify(t.Context(), sign(name, nil)); principal != "" {
				t.Fatal("untrusted principal accepted")
			}
			if (name == "redirect" || name == "oversized") && jwksReads.Load() != 0 {
				t.Fatal("unsafe discovery followed")
			}
			if externalReads.Load() != 0 {
				t.Fatal("verification redirected credentials off origin")
			}
		})
	}
}

func TestRejectedBearerRefreshCoalescesAndStaleRejectionCannotRetireSuccessor(t *testing.T) {
	var calls atomic.Int32
	source := &expiringBearerSource{now: time.Now, skew: time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		count := calls.Add(1)
		return "synthetic-" + string(rune('a'+count)), time.Now().Add(time.Hour), nil
	}}
	first, err := source.Authorization(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{credentials: source}
	original, _ := client.credentialEpochContext()
	generation := client.authGeneration.Load()
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); client.rejectRequestCredential(first, generation) }()
	}
	workers.Wait()
	if original.Err() == nil {
		t.Fatal("rejected epoch was not canceled")
	}
	second, err := source.Authorization(t.Context())
	if err != nil || second == first {
		t.Fatal("source reused rejected bearer")
	}
	successor, _ := client.credentialEpochContext()
	client.rejectRequestCredential(first, generation)
	if successor.Err() != nil {
		t.Fatal("stale rejection retired successor")
	}
	if calls.Load() != 2 {
		t.Fatalf("refresh count=%d", calls.Load())
	}
	if source.rejectBearer(first) {
		t.Fatal("stale bearer invalidated current cache")
	}
	current, err := source.Authorization(t.Context())
	if err != nil || current != second {
		t.Fatal("new bearer was discarded")
	}
}

func TestRecoveryCooldownSurvivesRequestWakesAndOnlyPublicationResets(t *testing.T) {
	r := &clusterRecovery{}
	r.failed(ErrCredentialUnavailable, nil, "org")
	deadline := r.deadline()
	for i := 0; i < 100; i++ {
		r.failed(ErrCredentialUnavailable, nil, "org")
	}
	if !r.deadline().Equal(deadline) || r.failures != 1 {
		t.Fatal("concurrent failures reset or multiplied cooldown")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitRecovery(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	r.committed("committed-revision")
	if !r.deadline().IsZero() || r.failures != 0 {
		t.Fatal("publication did not reset recovery")
	}
}

func TestUncommittedCredentialReplacementsKeepTheirStableCooldown(t *testing.T) {
	recovery := &clusterRecovery{}
	recovery.reserveReplacement(true)
	first := recovery.deadline()
	recovery.reserveReplacement(true)
	if recovery.replacementStreak != 2 || !recovery.deadline().After(first) {
		t.Fatal("success-less epoch changes reset replacement backoff")
	}
	reserved := recovery.deadline()
	recovery.failed(ErrUnauthorized, nil, "org")
	if recovery.deadline().Before(reserved) {
		t.Fatal("a rejection shortened the stable replacement cooldown")
	}
	recovery.committed("publication")
	if recovery.replacementStreak != 0 || !recovery.deadline().IsZero() {
		t.Fatal("committed recovery did not reset the replacement cooldown")
	}
}

// reviewIdentity is the review answer a fake cluster gives for one token.
type reviewIdentity struct {
	username, uid string
	groups        []string
	extra         map[string][]string
}

// identityReviewCluster answers identity reviews from a per-token table and
// counts them.
type identityReviewCluster struct {
	server  *httptest.Server
	mu      sync.Mutex
	answers map[string]reviewIdentity
	reviews atomic.Int64
}

func newIdentityReviewCluster(t *testing.T) *identityReviewCluster {
	t.Helper()
	cluster := &identityReviewCluster{answers: map[string]reviewIdentity{}}
	cluster.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/selfsubjectreviews" {
			writeJSON(w, map[string]any{"items": []any{}})
			return
		}
		cluster.reviews.Add(1)
		cluster.mu.Lock()
		answer, ok := cluster.answers[r.Header.Get("Authorization")]
		cluster.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		user := map[string]any{"username": answer.username, "groups": answer.groups}
		if answer.uid != "" {
			user["uid"] = answer.uid
		}
		if answer.extra != nil {
			user["extra"] = answer.extra
		}
		writeJSON(w, map[string]any{"apiVersion": "authentication.k8s.io/v1", "kind": "SelfSubjectReview", "status": map[string]any{"userInfo": user}})
	}))
	t.Cleanup(cluster.server.Close)
	return cluster
}

func (cluster *identityReviewCluster) answer(header string, identity reviewIdentity) {
	cluster.mu.Lock()
	cluster.answers[header] = identity
	cluster.mu.Unlock()
}

// rotatingProviderService connects a provider source to the cluster. rotate
// makes the source issue header next, as an hourly refresh does.
func rotatingProviderService(t *testing.T, cluster *identityReviewCluster, provider string) (*Service, func(string)) {
	t.Helper()
	var next atomic.Value
	next.Store("first-token")
	base := &expiringBearerSource{now: time.Now, skew: time.Minute, refresh: func(context.Context) (string, time.Time, error) {
		return next.Load().(string), time.Now().Add(time.Hour), nil
	}}
	var source CredentialSource = &aksCredentialSource{expiringBearerSource: base}
	if provider == "gke" {
		source = &gkeCredentialSource{expiringBearerSource: base}
	}
	service := newTestService(t, cluster.server.URL, Scope{OrgID: "default", ClusterID: "identity", CredentialID: "configured"})
	service.client.credentials = source
	rotate := func(token string) {
		current, _ := base.Authorization(t.Context())
		next.Store(token)
		base.rejectBearer(current)
	}
	return service, rotate
}

// validateRenewal validates the current token, waits for its proof and
// validates again: the previous token may serve while the proof runs.
func validateRenewal(t *testing.T, service *Service) error {
	t.Helper()
	if err := service.validateCredential(t.Context()); err != nil {
		return err
	}
	settleProofs(t, service.client)
	return service.validateCredential(t.Context())
}

// Field rule: AKS and GKE renewals whose review carries per-token extras keep
// one epoch and one storage key across 100 refreshes.
func TestAKSAndGKERenewalsWithPerTokenExtrasKeepOneEpoch(t *testing.T) {
	for _, provider := range []string{"aks", "gke"} {
		t.Run(provider, func(t *testing.T) {
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			cluster := newIdentityReviewCluster(t)
			service, rotate := rotatingProviderService(t, cluster, provider)
			identity := func(n int) reviewIdentity {
				if provider == "aks" {
					return reviewIdentity{username: "abababab-0000-4000-8000-000000000007", groups: []string{"cdcdcdcd-0000-4000-8000-000000000008", "system:authenticated"},
						extra: map[string][]string{"oid": {"abababab-0000-4000-8000-000000000007"}, "authentication.kubernetes.io/credential-id": {fmt.Sprintf("JTI=%d", n)}, "xms_session": {fmt.Sprintf("session-%d", n)}}}
				}
				return reviewIdentity{username: "100000000000000000001", groups: []string{"system:authenticated"},
					extra: map[string][]string{"iam.gke.io/user-assertion": {fmt.Sprintf("assertion-a-%d", n)}, "user-assertion.cloud.google.com": {fmt.Sprintf("assertion-b-%d", n)}}}
			}
			cluster.answer("Bearer first-token", identity(0))
			if err := service.validateCredential(t.Context()); err != nil {
				t.Fatal(err)
			}
			key := service.captureCredentialID()
			for n := 1; n <= 100; n++ {
				token := fmt.Sprintf("token-%d", n)
				cluster.answer("Bearer "+token, identity(n))
				rotate(token)
				if err := validateRenewal(t, service); err != nil {
					t.Fatalf("refresh %d retired the epoch: %v", n, err)
				}
			}
			if service.identity.ctx.Err() != nil || service.captureCredentialID() != key || service.identity.servingHeader != "Bearer token-100" {
				t.Fatal("per-token review fields changed the epoch or its storage key")
			}
		})
	}
}

// A changed username, UID or group set retires the epoch, and the debug line
// names the changed field, never its value.
func TestReviewedIdentityChangeRetiresAndLogsTheFieldName(t *testing.T) {
	base := reviewIdentity{username: "user-value-1", uid: "uid-value-1", groups: []string{"group-value-1", "system:authenticated"}, extra: map[string][]string{"oid": {"oid-value-1"}}}
	for field, changed := range map[string]reviewIdentity{
		"username":  {username: "user-value-2", uid: base.uid, groups: base.groups, extra: base.extra},
		"uid":       {username: base.username, uid: "uid-value-2", groups: base.groups, extra: base.extra},
		"groups":    {username: base.username, uid: base.uid, groups: []string{"group-value-2", "system:authenticated"}, extra: base.extra},
		"extra:oid": {username: base.username, uid: base.uid, groups: base.groups, extra: map[string][]string{"oid": {"oid-value-2"}}},
	} {
		t.Run(field, func(t *testing.T) {
			logs := captureLogLines(t, "debug")
			cluster := newIdentityReviewCluster(t)
			service, rotate := rotatingProviderService(t, cluster, "aks")
			cluster.answer("Bearer first-token", base)
			if err := service.validateCredential(t.Context()); err != nil {
				t.Fatal(err)
			}
			cluster.answer("Bearer changed-token", changed)
			rotate("changed-token")
			if err := validateRenewal(t, service); !errors.Is(err, ErrCredentialUnavailable) || service.identity.ctx.Err() == nil {
				t.Fatalf("a changed %s kept the epoch: %v", field, err)
			}
			var found bool
			for _, line := range logs.all() {
				if strings.Contains(line, "value-") {
					t.Fatalf("an identity value reached the log: %q", line)
				}
				if strings.Contains(line, "identity changed") && strings.Contains(line, "fields="+field+";") {
					found = true
				}
			}
			if !found {
				t.Fatalf("no debug line named the changed field %s: %q", field, logs.all())
			}
		})
	}
}

// A review extra key that is not on the allowlist is ignored, so a new key
// appearing on a renewal does not retire the epoch.
func TestUnknownNewExtraKeyDoesNotRetire(t *testing.T) {
	for _, provider := range []string{"aks", "gke"} {
		t.Run(provider, func(t *testing.T) {
			cluster := newIdentityReviewCluster(t)
			service, rotate := rotatingProviderService(t, cluster, provider)
			identity := reviewIdentity{username: "reader", groups: []string{"system:authenticated"}, extra: map[string][]string{"oid": {"object-id"}}}
			cluster.answer("Bearer first-token", identity)
			if err := service.validateCredential(t.Context()); err != nil {
				t.Fatal(err)
			}
			identity.extra = map[string][]string{"oid": {"object-id"}, "new.example.com/per-token": {"anything"}, "authentication.kubernetes.io/pod-name": {"pod"}}
			cluster.answer("Bearer second-token", identity)
			rotate("second-token")
			if err := validateRenewal(t, service); err != nil || service.identity.ctx.Err() != nil {
				t.Fatalf("an unknown extra key retired the epoch: %v", err)
			}
		})
	}
}

// The allowlist matches keys exactly and only deletes keys; an answer with no
// extra left and one with no extra at all give the same principal.
func TestReviewAllowlistMatchesExactlyAndEmptyExtraEqualsMissing(t *testing.T) {
	cluster := newIdentityReviewCluster(t)
	service, _ := rotatingProviderService(t, cluster, "aks")
	principal := func(identity reviewIdentity) string {
		cluster.answer("Bearer probe", identity)
		value, err := service.client.selfSubjectPrincipal(t.Context(), "Bearer probe", nil)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	user := reviewIdentity{username: "reader", uid: "uid", groups: []string{"b", "a"}}
	missing := principal(user)
	user.extra = map[string][]string{}
	empty := principal(user)
	user.extra = map[string][]string{"OID": {"object-id"}, "oid ": {"object-id"}, "pod-name": {"pod"}}
	dropped := principal(user)
	if missing != empty || missing != dropped {
		t.Fatalf("empty, missing and dropped extras differ:\n%s\n%s\n%s", missing, empty, dropped)
	}
	if want := `self-subject-review:{"username":"reader","uid":"uid","groups":["a","b"],"extra":null}`; missing != want {
		t.Fatalf("principal = %s, want %s", missing, want)
	}
	user.extra = map[string][]string{"oid": {"object-id", "second"}}
	if kept := principal(user); !strings.Contains(kept, `"extra":{"oid":["object-id","second"]}`) {
		t.Fatalf("the allowlisted key lost values: %s", kept)
	}
	gke, _ := rotatingProviderService(t, cluster, "gke")
	cluster.answer("Bearer probe", reviewIdentity{username: "reader", uid: "uid", groups: []string{"b", "a"}, extra: map[string][]string{"oid": {"object-id"}}})
	if value, err := gke.client.selfSubjectPrincipal(t.Context(), "Bearer probe", nil); err != nil || value != missing {
		t.Fatalf("GKE kept an extra key: %s %v", value, err)
	}
}

// Three principal-change rebuilds in a row write exactly one warning at the
// default level; a single rebuild writes nothing there.
func TestIdentityChangesInARowWarnOnce(t *testing.T) {
	for _, changes := range []int{1, 4} {
		t.Run(fmt.Sprintf("%d changes", changes), func(t *testing.T) {
			logs := captureLogLines(t, "info")
			isolateProblems(t, 10*time.Minute, newFakeClock().Now)
			cluster := newIdentityReviewCluster(t)
			service, rotate := rotatingProviderService(t, cluster, "aks")
			groups := func(n int) reviewIdentity {
				return reviewIdentity{username: "reader", groups: []string{fmt.Sprintf("group-%d", n%2), "system:authenticated"}, extra: map[string][]string{"oid": {"object-id"}}}
			}
			cluster.answer("Bearer first-token", groups(0))
			if err := service.validateCredential(t.Context()); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= changes; n++ {
				token := fmt.Sprintf("token-%d", n)
				cluster.answer("Bearer "+token, groups(n))
				rotate(token)
				if err := validateRenewal(t, service); !errors.Is(err, ErrCredentialUnavailable) {
					t.Fatalf("group change %d kept the epoch: %v", n, err)
				}
				service = newService(t.Context(), service.client, service.Scope(), time.Minute)
				if !service.identity.known {
					t.Fatalf("the successor after change %d was not proven", n)
				}
			}
			lines := visible(logs.all())
			want := 0
			if changes >= unstableIdentityRebuilds {
				want = 1
			}
			if len(lines) != want {
				t.Fatalf("%d changes wrote %d lines at default level: %q", changes, len(lines), lines)
			}
			if want == 1 && (!strings.HasPrefix(lines[0], `WARN kubernetes: cluster "identity" identity is not stable: groups changed on 3 credential renewals in a row`) || strings.Contains(lines[0], "group-")) {
				t.Fatalf("warning = %q", lines[0])
			}
		})
	}
}

// Sources other than AKS and GKE keep today's principal for the same review
// answer: only the credential-id field is dropped, every other extra stays.
func TestReviewPrincipalIsUnchangedForOtherModes(t *testing.T) {
	cluster := newIdentityReviewCluster(t)
	answer := reviewIdentity{username: "system:serviceaccount:versus:versus-reader", uid: "sa-uid", groups: []string{"system:serviceaccounts", "system:authenticated"},
		extra: map[string][]string{"authentication.kubernetes.io/credential-id": {"JTI=1"}, "authentication.kubernetes.io/pod-name": {"versus-0"}, "authentication.kubernetes.io/node-name": {"node-1"}}}
	cluster.answer("Bearer probe", answer)
	want := `self-subject-review:{"username":"system:serviceaccount:versus:versus-reader","uid":"sa-uid","groups":["system:authenticated","system:serviceaccounts"],"extra":{"authentication.kubernetes.io/node-name":["node-1"],"authentication.kubernetes.io/pod-name":["versus-0"]}}`
	for name, source := range map[string]CredentialSource{
		"in_cluster or token_file": &tokenFileSource{path: "/nonexistent"},
		"token":                    &credentialSource{},
		"client_certificate":       &credentialSource{certificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return new(tls.Certificate), nil }},
		"eks":                      &eksCredentialSource{expiringBearerSource: &expiringBearerSource{}},
	} {
		service := newTestService(t, cluster.server.URL, Scope{OrgID: "default", ClusterID: "modes"})
		service.client.credentials = source
		if principal, err := service.client.selfSubjectPrincipal(t.Context(), "Bearer probe", nil); err != nil || principal != want {
			t.Fatalf("%s principal changed:\n%s\nwant\n%s (%v)", name, principal, want, err)
		}
	}
}

// Replicas of different releases read each other's saved history only if they
// derive the same history principal and scope key for one connector. These
// values were computed with the v1.4.36 formulas for a connector without
// credentials: a SHA-256 of "anonymous", NUL and the endpoint for the
// principal, and of org, cluster, endpoint and credential ID for the key, each
// in unpadded URL-safe base64. A replica that reaches the same cluster at
// another address derives other values, and its reads are denied.
func TestHistoryPrincipalKeepsTheReleasedFormat(t *testing.T) {
	scope := Scope{OrgID: "default", ClusterID: "east", CredentialID: "east-reader"}
	service := newTestService(t, "https://127.0.0.1:19671", scope)
	if got := service.historyPrincipal(); got != "JQHe--VW1Jro-82t2bJneOeshMUlZ7H7UUiGI206DgQ" {
		t.Fatalf("history principal %q differs from the v1.4.36 value", got)
	}
	if got := service.historyScopeKey(); got != "kubernetes/history-scope/RXAuWcLOm22dDFi4sSuGPQQ9LqeHXBaCQ09tJdEKpHM" {
		t.Fatalf("history scope key %q differs from the v1.4.36 value", got)
	}
	elsewhere := newTestService(t, "https://host.docker.internal:19671", scope)
	if elsewhere.historyPrincipal() == service.historyPrincipal() {
		t.Fatal("a replica configured with another endpoint shares the history principal")
	}
}
