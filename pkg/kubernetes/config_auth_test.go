package kubernetes

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestResolveAuthenticationModesAndInClusterEnvironment(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.20.30.40")
	t.Setenv("KUBERNETES_SERVICE_PORT", "7443")
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "6443")
	resolved, err := ResolveAuthentication(AuthOptions{Mode: "in_cluster"})
	if err != nil || resolved.Endpoint != "https://10.20.30.40:7443" || resolved.CAFile != inClusterCAFile {
		t.Fatalf("in-cluster = %+v, %v", resolved, err)
	}
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	resolved, err = ResolveAuthentication(AuthOptions{Mode: "in_cluster"})
	if err != nil || resolved.Endpoint != "https://10.20.30.40:6443" {
		t.Fatalf("in-cluster HTTPS fallback = %+v, %v", resolved, err)
	}
	t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "")
	resolved, err = ResolveAuthentication(AuthOptions{Mode: "in_cluster"})
	if err != nil || resolved.Endpoint != "https://10.20.30.40:443" {
		t.Fatalf("in-cluster default = %+v, %v", resolved, err)
	}

	for _, options := range []AuthOptions{
		{Mode: "token", Endpoint: "https://cluster.example", Token: "a", TokenFile: "b"},
		{Mode: "in_cluster", Endpoint: "https://cluster.example"},
		{Mode: "aks", Endpoint: "https://cluster.example", AKSCredentialMode: "device_code", AKSServerID: "server", AKSClientID: "client"},
		{Mode: "eks", Endpoint: "https://cluster.example", EKSRegion: "us-east-1"},
		{Mode: "unknown", Endpoint: "https://cluster.example"},
	} {
		if _, err := ResolveAuthentication(options); err == nil {
			t.Fatalf("accepted conflicting/unsupported options: %+v", options)
		}
	}
}

func TestKubeconfigResolutionAndUnsafeFeatures(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "token"), []byte("rotating"), 0o600); err != nil {
		t.Fatal(err)
	}
	caData := base64.StdEncoding.EncodeToString([]byte("ca-data"))
	valid := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: prod
clusters:
- name: cluster
  cluster:
    server: https://cluster.example
    certificate-authority-data: %s
contexts:
- name: prod
  context: {cluster: cluster, user: reader}
users:
- name: reader
  user: {tokenFile: token}
`, caData)
	path := filepath.Join(directory, "config")
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path})
	if err != nil || result.Endpoint != "https://cluster.example" || string(result.CAData) != "ca-data" {
		t.Fatalf("resolved = %+v, %v", result, err)
	}
	header, err := result.Credentials.Authorization(context.Background())
	if err != nil || header != "Bearer rotating" {
		t.Fatalf("relative token = %q, %v", header, err)
	}
	for _, options := range []AuthOptions{
		{Mode: "kubeconfig", KubeconfigPath: path, ServerName: "ignored.example"},
		{Mode: "kubeconfig", KubeconfigPath: path, CertificateFile: "ignored.crt", KeyFile: "ignored.key"},
		{Mode: "kubeconfig", KubeconfigPath: path, CertificateData: base64.StdEncoding.EncodeToString([]byte("ignored")), KeyData: base64.StdEncoding.EncodeToString([]byte("ignored"))},
	} {
		if _, err := ResolveAuthentication(options); err == nil {
			t.Fatalf("accepted top-level kubeconfig conflict: %+v", options)
		}
	}

	for name, fragment := range map[string]string{
		"proxy":    "    proxy-url: http://proxy.example\n",
		"insecure": "    insecure-skip-tls-verify: true\n",
		"unknown":  "    mystery-field: true\n",
		"dual-ca":  "    certificate-authority: ca.crt\n",
	} {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(valid, "    certificate-authority-data:", fragment+"    certificate-authority-data:", 1)
			if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path}); err == nil {
				t.Fatal("unsafe kubeconfig accepted")
			}
		})
	}

	badUsers := []string{
		"    username: user\n    password: pass",
		"    token: one\n    tokenFile: token",
		"    auth-provider: {name: gcp}",
		"    exec: {command: /bin/sh, args: [-c, id], interactiveMode: Never}",
	}
	for index, user := range badUsers {
		bad := strings.Replace(valid, "  user: {tokenFile: token}", "  user:\n"+user, 1)
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path}); err == nil {
			t.Fatalf("unsafe user %d accepted", index)
		}
	}
}

func TestResolveAuthenticationRejectsInactiveCrossModeFields(t *testing.T) {
	for _, options := range []AuthOptions{
		{Mode: "token", Endpoint: "https://cluster.example", Token: "token", KubeconfigContext: "ignored"},
		{Mode: "client_certificate", Endpoint: "https://cluster.example", CertificateFile: "client.crt", KeyFile: "client.key", KubeconfigContext: "ignored"},
		{Mode: "aks", Endpoint: "https://cluster.example", AKSCredentialMode: "managed_identity", AKSServerID: "server", EKSRegion: "us-east-1"},
		{Mode: "gke", Endpoint: "https://cluster.example", EKSRoleARN: "arn:aws:iam::123456789012:role/ignored"},
		{Mode: "gke", Endpoint: "https://cluster.example", EKSProfile: "ignored"},
		{Mode: "token", Endpoint: "https://cluster.example", Token: "token", CAFile: "ca.crt", CAData: base64.StdEncoding.EncodeToString([]byte("ca"))},
	} {
		if _, err := ResolveAuthentication(options); err == nil {
			t.Fatalf("accepted inactive or duplicate fields: %+v", options)
		}
	}
}

func TestKubeconfigAcceptsStandardMetadataAndRejectsAdditionalDocuments(t *testing.T) {
	directory := t.TempDir()
	config := `apiVersion: v1
kind: Config
preferences: {colors: true, extensions: [{name: kubectl.example/preferences, extension: {theme: dark}}]}
current-context: production
clusters: [{name: production, cluster: {server: https://cluster.example, disable-compression: true, extensions: [{name: provider.example/cluster, extension: {managed: true}}]}}]
contexts: [{name: production, context: {cluster: production, user: operator, namespace: default, extensions: [{name: kubectl.example/context, extension: {source: cli}}]}}]
users: [{name: operator, user: {token: fixture-token, extensions: [{name: provider.example/user, extension: {account: fixture}}]}}]
extensions: [{name: kubectl.example/config, extension: {version: 1}}]
`
	path := filepath.Join(directory, "config")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path}); err != nil {
		t.Fatalf("standard kubeconfig rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte(config+"---\napiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path}); err == nil {
		t.Fatal("additional YAML document accepted")
	}
}

func TestProviderGeneratedKubeconfigCredentials(t *testing.T) {
	for _, testCase := range []struct {
		name string
		exec kubeExec
	}{
		{name: "eks", exec: kubeExec{APIVersion: "client.authentication.k8s.io/v1beta1", Command: "aws", Args: []string{"eks", "get-token", "--cluster-name", "production"}, InteractiveMode: "Never"}},
		{name: "aks", exec: kubeExec{APIVersion: "client.authentication.k8s.io/v1beta1", Command: "kubelogin", Args: []string{"get-token", "--login", "workloadidentity"}, InteractiveMode: "Never"}},
		{name: "gke", exec: kubeExec{APIVersion: "client.authentication.k8s.io/v1beta1", Command: "gke-gcloud-auth-plugin", InteractiveMode: "Never"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeKubeconfigDocument(t, kubeconfigDocumentForUser(kubeUser{Exec: &testCase.exec}))
			if _, err := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path}); err == nil || !strings.Contains(err.Error(), "configure auth.mode as eks, aks, or gke") {
				t.Fatalf("provider exec credential error = %v", err)
			}
		})
	}

	path := writeKubeconfigDocument(t, kubeconfigDocumentForUser(kubeUser{Token: "fixture-token"}))
	if _, err := ResolveAuthentication(AuthOptions{Mode: "kubeconfig", KubeconfigPath: path}); err != nil {
		t.Fatalf("static provider credential rejected: %v", err)
	}
}

func kubeconfigDocumentForUser(user kubeUser) kubeconfigDocument {
	return kubeconfigDocument{
		APIVersion:     "v1",
		Kind:           "Config",
		CurrentContext: "production",
		Clusters:       []namedCluster{{Name: "production", Cluster: kubeCluster{Server: "https://cluster.example"}}},
		Contexts:       []namedContext{{Name: "production", Context: kubeContext{Cluster: "production", User: "operator"}}},
		Users:          []namedUser{{Name: "operator", User: user}},
	}
}

func writeKubeconfigDocument(t *testing.T, document kubeconfigDocument) string {
	t.Helper()
	data, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInClusterResolutionConstructsPrivateIPv4AndIPv6Clients(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	certificate := server.Certificate()
	ca := fmt.Sprintf("-----BEGIN CERTIFICATE-----\n%s\n-----END CERTIFICATE-----\n", base64.StdEncoding.EncodeToString(certificate.Raw))
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, []byte(ca), 0o600); err != nil {
		t.Fatal(err)
	}
	originalCA := inClusterCAFile
	inClusterCAFile = caPath
	t.Cleanup(func() { inClusterCAFile = originalCA })
	for _, host := range []string{"10.20.30.40", "fd00::10"} {
		t.Run(host, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", host)
			t.Setenv("KUBERNETES_SERVICE_PORT_HTTPS", "443")
			resolved, err := ResolveAuthentication(AuthOptions{Mode: "in_cluster"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewClient(resolved); err != nil {
				t.Fatalf("private in-cluster client: %v", err)
			}
		})
	}
}
