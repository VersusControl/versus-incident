package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func TestIndependentKubernetesRegistryConfigurationSharesCaptureLease(t *testing.T) {
	cfg := config.KubernetesConnectorConfig{KubernetesToolConfig: config.KubernetesToolConfig{Endpoint: "https://cluster.example", ClusterID: "cluster", CredentialID: "configured", ChangeCapture: "demand", Auth: config.KubernetesAuthConfig{Mode: "token", Token: "bootstrap-credential-canary"}}}
	provider := storage.NewMemory()
	var firstScope kubernetes.Scope
	for position := range 2 {
		registry, errs := NewKubernetesRegistry(cfg, tenancy.NewOrgScope("org"))
		defer registry.Close()
		if len(errs) != 0 {
			t.Fatal("configured registry did not construct")
		}
		service, err := registry.ResolveCluster("org", "cluster")
		if err != nil {
			t.Fatal(err)
		}
		service.SetChangeStorage(provider)
		active := service.ActiveScope()
		if position == 0 {
			firstScope = active
		} else if active != firstScope {
			t.Fatal("independent config bootstraps changed capture identity")
		}
		store := kubechanges.NewStore(provider, kubeindex.Scope{OrgID: active.OrgID, ClusterID: active.ClusterID, CredentialID: active.CredentialID})
		err = store.ClaimLease("owner-"+string(rune('a'+position)), time.Now().UTC(), time.Minute)
		if position == 0 && err != nil || position == 1 && err != kubechanges.ErrLeaseLost {
			t.Fatal("configured replicas did not compete for one lease")
		}
	}
}

func TestNewKubernetesRegistryKeepsFailedEntriesAndOwnCredentials(t *testing.T) {
	connector := config.KubernetesConnectorConfig{Multiple: true, Clusters: []config.KubernetesClusterConfig{
		{DisplayName: "One", KubernetesToolConfig: config.KubernetesToolConfig{ClusterID: "one", CredentialID: "credential-one", Endpoint: "https://one.example", Auth: config.KubernetesAuthConfig{Mode: "token", Token: "reader-one"}}},
		{KubernetesToolConfig: config.KubernetesToolConfig{ClusterID: "failed", Endpoint: "https://failed.example", Auth: config.KubernetesAuthConfig{Mode: "invalid"}}},
	}}
	registry, failures := NewKubernetesRegistry(connector, tenancy.NewOrgScope("licensed"))
	if !registry.Multiple() || len(failures) != 1 || len(registry.Clusters()) != 2 {
		t.Fatal("failed entry lost or blocked catalog")
	}
	service, err := registry.ResolveCluster("request-org", "one")
	if err != nil || service.Scope().CredentialID != "credential-one" || service.Scope().OrgID != "request-org" {
		t.Fatal("trusted scope drift")
	}
	summaries := registry.Summaries(context.Background(), "licensed", func(id string) bool { return id == "failed" })
	if len(summaries) != 1 || summaries[0].Health != "unreachable" {
		t.Fatal("failed construction not summarized")
	}
	count := 0
	registry.Each(func(*kubernetes.Service) { count++ })
	if count != 1 {
		t.Fatal("Each visited failed entry")
	}
}

func TestNewKubernetesServiceIsOptionalAndRejectsUnsafeConfiguration(t *testing.T) {
	service, err := NewKubernetesService(config.KubernetesToolConfig{}, tenancy.DefaultOrgScope())
	if err != nil || service != nil {
		t.Fatalf("empty config = %v, %v", service, err)
	}
	if _, err := NewKubernetesService(config.KubernetesToolConfig{Endpoint: "http://cluster.example"}, tenancy.DefaultOrgScope()); err == nil {
		t.Fatal("unsafe endpoint accepted")
	}
}

func TestNewKubernetesServiceSupportsExplicitAuthAndPreservesLegacyTokenFile(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := NewKubernetesService(config.KubernetesToolConfig{Endpoint: "https://cluster.example", TokenFile: tokenPath}, tenancy.DefaultOrgScope())
	if err != nil || legacy == nil {
		t.Fatalf("legacy token_file = %v, %v", legacy, err)
	}
	explicit, err := NewKubernetesService(config.KubernetesToolConfig{Endpoint: "https://cluster.example", Auth: config.KubernetesAuthConfig{Mode: "token_file", TokenFile: tokenPath}}, tenancy.DefaultOrgScope())
	if err != nil || explicit == nil {
		t.Fatalf("explicit token_file = %v, %v", explicit, err)
	}
	if _, err := NewKubernetesService(config.KubernetesToolConfig{Endpoint: "https://cluster.example", TokenFile: tokenPath, Auth: config.KubernetesAuthConfig{Mode: "token_file", TokenFile: tokenPath}}, tenancy.DefaultOrgScope()); err == nil {
		t.Fatal("legacy and explicit credentials were combined")
	}
}

func TestNewKubernetesServiceRequiresModeForEveryNestedAuthGroup(t *testing.T) {
	for _, test := range []struct {
		name string
		auth config.KubernetesAuthConfig
	}{
		{name: "token", auth: config.KubernetesAuthConfig{Token: "secret"}},
		{name: "token file", auth: config.KubernetesAuthConfig{TokenFile: "/token"}},
		{name: "client certificate", auth: config.KubernetesAuthConfig{ClientCertificate: config.KubernetesClientCertificateConfig{CertificateFile: "/certificate"}}},
		{name: "kubeconfig", auth: config.KubernetesAuthConfig{Kubeconfig: config.KubernetesKubeconfigConfig{Path: "/config"}}},
		{name: "EKS", auth: config.KubernetesAuthConfig{EKS: config.KubernetesEKSConfig{ClusterName: "production"}}},
		{name: "AKS", auth: config.KubernetesAuthConfig{AKS: config.KubernetesAKSConfig{ClientID: "client"}}},
		{name: "GKE", auth: config.KubernetesAuthConfig{GKE: config.KubernetesGKEConfig{CredentialsFile: "/credentials"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewKubernetesService(config.KubernetesToolConfig{Endpoint: "https://cluster.example", Auth: test.auth}, tenancy.DefaultOrgScope())
			if err == nil || !strings.Contains(err.Error(), "auth.mode is required when auth.* is configured") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

// kubernetesGoroutines counts live goroutines whose stack runs code from the
// Kubernetes connector package.
func kubernetesGoroutines(t *testing.T) int {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, stack := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(stack, "versus-incident/pkg/kubernetes.") {
			n++
		}
	}
	return n
}

// TestKubernetesRegistryWithoutStartCaptureStartsNothing proves that building
// the cluster registry the way a composition root does (construction, change
// storage, scrubber, log-stream context, issue-alert sink, and resolving a
// cluster) starts no inventory sync, no capture and no connector goroutine:
// those, and the recovery supervisor (the only caller of the restart path in
// superviseCluster), start only from StartCapture. Construction does make one
// synchronous identity review (SelfSubjectReview) to pin the credential's
// principal; it is the only request allowed before StartCapture. The control
// half calls StartCapture and sees reads and goroutines appear, so the probe
// is able to notice them.
func TestKubernetesRegistryWithoutStartCaptureStartsNothing(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer server.Close()
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	cfg := config.KubernetesConnectorConfig{KubernetesToolConfig: config.KubernetesToolConfig{
		Endpoint: server.URL, CAData: base64.StdEncoding.EncodeToString(caPEM), AllowLoopback: true,
		ClusterID: "cluster", Auth: config.KubernetesAuthConfig{Mode: "token", Token: "construction-probe"},
	}}

	before := kubernetesGoroutines(t)
	registry, errs := NewKubernetesRegistry(cfg, tenancy.DefaultOrgScope())
	if len(errs) != 0 {
		t.Fatalf("registry did not build: %v", errs)
	}
	defer registry.Close()
	t.Logf("requests after construction: %v", requests())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scrubber, _ := NewRedactor(false, nil)
	registry.Each(func(service *kubernetes.Service) {
		service.SetChangeStorage(storage.NewMemory())
		service.SetScrubber(scrubber)
		service.SetLogStreamContext(ctx)
	})
	ConfigureKubernetesIssueAlerts(registry, func(context.Context, string, kubernetes.IssueAlert) (kubernetes.Outcome, error) {
		return kubernetes.Outcome{}, nil
	})
	if _, err := registry.ResolveCluster(storage.DefaultOrgID, "cluster"); err != nil {
		t.Fatalf("resolve cluster: %v", err)
	}
	t.Logf("requests after resolve: %v", requests())
	// Longer than the recovery supervisor's 1 s ticker, so a supervisor or an
	// inventory start would have made its first request by now.
	time.Sleep(1500 * time.Millisecond)
	const identityReview = "/apis/authentication.k8s.io/v1/selfsubjectreviews"
	reviews := 0
	for _, path := range requests() {
		if path != identityReview {
			t.Fatalf("building the registry without StartCapture read %s; all requests: %v", path, requests())
		}
		reviews++
	}
	if reviews > 1 {
		t.Fatalf("construction repeated the identity review %d times", reviews)
	}
	if after := kubernetesGoroutines(t); after > before {
		t.Fatalf("building the registry without StartCapture left %d connector goroutines running (before: %d)", after-before, before)
	}

	registry.StartCapture(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for len(requests()) == reviews && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(requests()) == reviews {
		t.Fatal("control: StartCapture sent no API request; the probe cannot see a sync")
	}
	if kubernetesGoroutines(t) <= before {
		t.Fatal("control: StartCapture started no connector goroutine; the probe cannot see one")
	}
}
