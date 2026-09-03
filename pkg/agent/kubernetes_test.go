package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

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
