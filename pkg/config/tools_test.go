package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	kubernetespkg "github.com/VersusControl/versus-incident/pkg/kubernetes"
)

// TestLoadToolsFile asserts the tools.yaml loader parses the root-level
// tool-loop knobs and the recent_changes git repos, expands ${VAR}
// references, and round-trips the top-level `tools` key.
func TestLoadToolsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	content := `tools:
  tool_timeout: 30s
  parallel_tools: true
  recent_changes:
    git:
      auth:
        token: ${TOOLS_TEST_TOKEN}
        ssh_key_path: /etc/versus/global_key
      repos:
        - url: ${TOOLS_TEST_REPO}
          branch: main
          service: api
        - url: git@github.com:acme/web.git
          auth:
            ssh_key_path: /etc/versus/web_key
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tools.yaml: %v", err)
	}
	t.Setenv("TOOLS_TEST_REPO", "https://github.com/acme/api.git")
	t.Setenv("TOOLS_TEST_TOKEN", "secret-token")

	got, err := loadToolsFile(path)
	if err != nil {
		t.Fatalf("loadToolsFile: %v", err)
	}
	if got.ToolTimeout != "30s" {
		t.Fatalf("tool_timeout = %q, want 30s", got.ToolTimeout)
	}
	if !got.ParallelTools {
		t.Fatalf("parallel_tools = %v, want true", got.ParallelTools)
	}
	auth := got.RecentChanges.Git.Auth
	if auth.Token != "secret-token" {
		t.Fatalf("global auth.token = %q, want env-expanded secret-token", auth.Token)
	}
	if auth.SSHKeyPath != "/etc/versus/global_key" {
		t.Fatalf("global auth.ssh_key_path = %q", auth.SSHKeyPath)
	}
	repos := got.RecentChanges.Git.Repos
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2: %+v", len(repos), repos)
	}
	if repos[0].URL != "https://github.com/acme/api.git" {
		t.Fatalf("repos[0].url = %q, want env-expanded api URL", repos[0].URL)
	}
	if repos[0].Branch != "main" || repos[0].Service != "api" {
		t.Fatalf("repos[0] = %+v", repos[0])
	}
	if repos[1].URL != "git@github.com:acme/web.git" {
		t.Fatalf("repos[1].url = %q", repos[1].URL)
	}
	if repos[1].Auth.SSHKeyPath != "/etc/versus/web_key" {
		t.Fatalf("repos[1].auth.ssh_key_path = %q, want per-repo override", repos[1].Auth.SSHKeyPath)
	}
}

// TestLoadToolsFile_DescribeDependencies asserts the tools.yaml loader
// parses the describe_dependencies service-dependency graph.
func TestLoadToolsFile_DescribeDependencies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	content := `tools:
  describe_dependencies:
    services:
      - name: web
        depends_on:
          - api
      - name: api
        depends_on:
          - database
          - cache
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tools.yaml: %v", err)
	}
	got, err := loadToolsFile(path)
	if err != nil {
		t.Fatalf("loadToolsFile: %v", err)
	}
	svcs := got.DescribeDependencies.Services
	if len(svcs) != 2 {
		t.Fatalf("got %d services, want 2: %+v", len(svcs), svcs)
	}
	if svcs[0].Name != "web" || len(svcs[0].DependsOn) != 1 || svcs[0].DependsOn[0] != "api" {
		t.Fatalf("services[0] = %+v", svcs[0])
	}
	if svcs[1].Name != "api" || len(svcs[1].DependsOn) != 2 {
		t.Fatalf("services[1] = %+v", svcs[1])
	}
}

func TestLoadToolsFileKubernetesAuthentication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.yaml")
	content := "tools:\n" +
		"  kubernetes:\n" +
		"    endpoint: https://cluster.example\n" +
		"    auth:\n" +
		"      mode: token\n" +
		"      token: ${KUBERNETES_TEST_TOKEN}\n" +
		"      eks:\n" +
		"        cluster_name: production\n" +
		"        region: us-east-1\n" +
		"      aks:\n" +
		"        credential_mode: client_secret\n" +
		"        server_id: api://aks-server\n" +
		"        tenant_id: tenant\n" +
		"        client_id: client\n" +
		"        client_secret: ${AKS_TEST_SECRET}\n" +
		"      gke:\n" +
		"        credentials_file: /var/run/secrets/google/credentials.json\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	token := "quoted: #token\\value\nsecond line"
	clientSecret := "client: #secret\\value\nsecond line"
	t.Setenv("KUBERNETES_TEST_TOKEN", token)
	t.Setenv("AKS_TEST_SECRET", clientSecret)
	got, err := loadToolsFile(path)
	if err != nil {
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), clientSecret) {
			t.Fatal("configuration error exposed a secret")
		}
		t.Fatal(err)
	}
	if got.Kubernetes.Auth.Mode != "token" || got.Kubernetes.Auth.Token != token || got.Kubernetes.Auth.EKS.ClusterName != "production" {
		t.Fatalf("Kubernetes auth = %+v", got.Kubernetes.Auth)
	}
	if got.Kubernetes.Auth.AKS.CredentialMode != "client_secret" || got.Kubernetes.Auth.AKS.ClientSecret != clientSecret || got.Kubernetes.Auth.GKE.CredentialsFile != "/var/run/secrets/google/credentials.json" {
		t.Fatalf("native cloud auth = %+v", got.Kubernetes.Auth)
	}
}

func TestToolsEnvironmentScalarExpansionSemantics(t *testing.T) {
	t.Setenv("TOOLS_VALUE", "value: #literal\nnext")
	input := map[string]any{
		"braced":  "${TOOLS_VALUE}",
		"plain":   "$TOOLS_VALUE",
		"unset":   "${TOOLS_UNSET}",
		"dollars": "$$",
		"nested":  []any{map[string]any{"value": "prefix-${TOOLS_VALUE}-suffix"}},
	}
	got := expandEnvironmentScalars(input).(map[string]any)
	if got["braced"] != "value: #literal\nnext" || got["plain"] != got["braced"] || got["unset"] != "" || got["dollars"] != "" {
		t.Fatalf("expanded scalars = %#v", got)
	}
	nested := got["nested"].([]any)[0].(map[string]any)["value"]
	if nested != "prefix-value: #literal\nnext-suffix" {
		t.Fatalf("nested scalar = %#v", nested)
	}
}

func TestLoadToolsFileRejectsUnknownKubernetesAuthenticationField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.yaml")
	content := "tools:\n  kubernetes:\n    auth:\n      mode: aks\n      exec_timeout: 15s\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadToolsFile(path); err == nil {
		t.Fatal("unknown Kubernetes authentication field accepted")
	}
}

func TestLoadToolsFileAcceptsCommaStringSlice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.yaml")
	if err := os.WriteFile(path, []byte("tools:\n  kubernetes:\n    endpoint_cidrs: 10.0.0.0/8,192.168.0.0/16\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadToolsFile(path)
	if err != nil || !reflect.DeepEqual(config.Kubernetes.EndpointCIDRs, []string{"10.0.0.0/8", "192.168.0.0/16"}) {
		t.Fatalf("endpoint_cidrs = %#v, %v", config.Kubernetes.EndpointCIDRs, err)
	}
}

func TestCopiedAKSExamplesStrictlyDecodeAndResolve(t *testing.T) {
	t.Setenv("KUBERNETES_AKS_CLIENT_SECRET", "fixture-client-secret")
	for name, body := range map[string]string{
		"client secret": `tools:
  kubernetes:
    endpoint: https://production.example.azmk8s.io
    ca_file: /run/secrets/kubernetes/ca.crt
    auth:
      mode: aks
      aks:
        credential_mode: client_secret
        environment: public
        server_id: api://AKS_SERVER_APP_ID
        tenant_id: TENANT_ID
        client_id: CLIENT_ID
        client_secret: ${KUBERNETES_AKS_CLIENT_SECRET}
`,
		"managed identity": `tools:
  kubernetes:
    endpoint: https://production.example.azmk8s.io
    ca_file: /run/secrets/kubernetes/ca.crt
    auth:
      mode: aks
      aks:
        credential_mode: managed_identity
        environment: public
        server_id: api://AKS_SERVER_APP_ID
        client_id: OPTIONAL_USER_ASSIGNED_IDENTITY_CLIENT_ID
`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tools.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			tools, err := loadToolsFile(path)
			if err != nil {
				t.Fatalf("strict decode: %v", err)
			}
			configuration := tools.Kubernetes
			auth := configuration.Auth.AKS
			resolved, err := kubernetespkg.ResolveAuthentication(kubernetespkg.AuthOptions{
				Mode: configuration.Auth.Mode, Endpoint: configuration.Endpoint, CAFile: configuration.CAFile,
				AKSCredentialMode: auth.CredentialMode, AKSServerID: auth.ServerID, AKSTenantID: auth.TenantID,
				AKSClientID: auth.ClientID, AKSClientSecret: auth.ClientSecret, AKSFederatedTokenFile: auth.FederatedTokenFile, AKSEnvironment: auth.Environment,
			})
			if err != nil || resolved.Endpoint != configuration.Endpoint || resolved.CAFile != configuration.CAFile {
				t.Fatalf("resolved = %+v, %v", resolved, err)
			}
		})
	}
}

// TestLoadToolsFile_Empty asserts an empty/absent tools block parses to a
// zero ToolsConfig without error.
func TestLoadToolsFile_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tools.yaml")
	if err := os.WriteFile(path, []byte("tools: {}\n"), 0o600); err != nil {
		t.Fatalf("write tools.yaml: %v", err)
	}
	got, err := loadToolsFile(path)
	if err != nil {
		t.Fatalf("loadToolsFile: %v", err)
	}
	if !reflect.DeepEqual(got, ToolsConfig{}) {
		t.Fatalf("got = %+v, want zero ToolsConfig", got)
	}
}

// TestLoadToolsFile_ReadError asserts a missing file surfaces a read
// error (the caller only invokes the loader after a successful os.Stat).
func TestLoadToolsFile_ReadError(t *testing.T) {
	_, err := loadToolsFile(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error reading a missing tools.yaml")
	}
}
