package kubernetes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

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
	now = now.Add(61 * time.Second)
	if _, err := source.Authorization(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("refresh calls after skew = %d", calls)
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

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := source.Authorization(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("blocked waiter error = %v after %v", err, time.Since(started))
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
	if err := first.commitCredential(t.Context(), func() error { return first.changeStore().Commit([]Change{change}, nil, nil, "first", now) }); err != nil {
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
	page, err = replacement.Changes(t.Context(), ChangeQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("replacement exposed previous principal history")
	}
	if err := first.commitCredential(t.Context(), func() error { t.Fatal("retired principal committed"); return nil }); !errors.Is(err, ErrCredentialUnavailable) {
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
	if err := service.commitCredential(t.Context(), func() error { return store.RenewLease("capture-owner", time.Now().UTC(), time.Minute) }); err != nil {
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
	if err := service.commitCredential(t.Context(), func() error { t.Fatal("invalid token committed"); return nil }); !errors.Is(err, ErrCredentialUnavailable) {
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
	_, resolveErr := registry.resolveCluster(ctx, "org", "fast")
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
			if err := service.commitCredential(t.Context(), func() error {
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
				err = service.commitCredential(t.Context(), func() error { t.Fatal("rejected refresh renewed lease"); return nil })
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("definitive rejection hidden: %v", err)
			}
			select {
			case <-bound.Done():
			case <-time.After(time.Second):
				t.Fatal("alias stream context survived rejection")
			}
			for _, old := range []*Service{service, alias, shared} {
				if old.identity.ctx.Err() == nil {
					t.Fatal("shared client epoch survived rejection")
				}
				if page, err := old.Changes(t.Context(), ChangeQuery{}); err == nil || len(page.Items) != 0 {
					t.Fatal("retired epoch exposed history")
				}
				if err := old.commitCredential(t.Context(), func() error { t.Fatal("retired epoch committed"); return nil }); err == nil {
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
				if err := service.commitCredential(t.Context(), func() error { t.Fatal("expired freshness accepted commit"); return nil }); err == nil {
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
	config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path == "/api/v1/pods" {
				writer.WriteHeader(http.StatusUnauthorized)
				return
			}
			if blocked.Load() && request.URL.Path == "/openid/v1/jwks" {
				close(started)
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
	if err := service.commitCredential(ctx, func() error {
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
	config, sign := projectedAuthFixture(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if strings.HasSuffix(request.URL.Path, "/log") {
				writer.WriteHeader(http.StatusOK)
				writer.(http.Flusher).Flush()
				close(opened)
				<-request.Context().Done()
				close(stopped)
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
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("stream did not open")
	}
	var output any
	if err := client.GetJSON(t.Context(), "/api/v1/pods", &output); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("API did not reject credential")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("API rejection did not cancel upstream stream")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not stop")
	}
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
