package k8s

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testActorCredential struct{}

func (testActorCredential) Authorization(context.Context) (string, error) {
	return "Bearer actor-token", nil
}

func (testActorCredential) ClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return new(tls.Certificate), nil
}

func TestActorClientRequiresExplicitCredentialAndHTTPS(t *testing.T) {
	if _, err := NewClient(ClientConfig{Endpoint: "https://cluster.example"}); err == nil {
		t.Fatal("missing actor credential was accepted")
	}
	if _, err := NewClient(ClientConfig{Endpoint: "http://cluster.example", Credentials: testActorCredential{}}); err == nil {
		t.Fatal("remote HTTP endpoint was accepted")
	}
}

func TestActorClientUsesCredentialAndRefusesRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer actor-token" {
			t.Errorf("actor Authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Method != http.MethodPatch || request.Header.Get("Content-Type") != "application/strategic-merge-patch+json" {
			t.Errorf("request = %s content-type=%q", request.Method, request.Header.Get("Content-Type"))
		}
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer source.Close()
	api, err := NewClient(ClientConfig{Endpoint: source.URL, Credentials: testActorCredential{}, AllowLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.Do(context.Background(), http.MethodPatch, "/apis/apps/v1/namespaces/shop/deployments/api", url.Values{"dryRun": {"All"}}, []byte(`{"spec":{}}`))
	if err == nil || redirected.Load() {
		t.Fatalf("redirect error=%v target reached=%v", err, redirected.Load())
	}
}

func TestActorClientBlocksPrivateEgressAndUnsafeRequests(t *testing.T) {
	api, err := NewClient(ClientConfig{Endpoint: "https://10.0.0.1", Credentials: testActorCredential{}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := api.Do(ctx, http.MethodGet, "/api/v1/nodes/worker-1", nil, nil); !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("private endpoint error=%v", err)
	}
	for _, test := range []struct {
		path string
		body []byte
	}{{path: "/api/v1/../secrets"}, {path: "/api/v1/pods/%2f.."}, {path: "/api/v1/jobs", body: []byte(strings.Repeat("x", maxRequestBytes+1))}} {
		if _, err := api.Do(context.Background(), http.MethodPost, test.path, nil, test.body); !errors.Is(err, ErrRequestFailed) {
			t.Errorf("unsafe request %q error=%v", test.path, err)
		}
	}
}