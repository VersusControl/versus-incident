package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	kubernetespkg "github.com/VersusControl/versus-incident/pkg/kubernetes"
	"gopkg.in/yaml.v3"
)

// -----------------------------------------------------------------------------
// Helm chart <-> application config drift
// -----------------------------------------------------------------------------
//
// TestDefaultConfigMatchesSample keeps the embedded baseline and the documented
// sample in lockstep. This file is its chart-side analogue: it renders the Helm
// chart and checks that the config.yaml the chart produces can express every key
// the binary supports. Without it the chart silently falls behind pkg/config and
// shipped features become unreachable for Helm users.
//
// The comparison is structural (key sets, not values) because the chart is
// entitled to its own opinionated defaults — it is not entitled to omit a knob.

// chartDir is the chart under test, relative to this package.
const chartDir = "../../helm/versus-incident"

// coverageValues is the maximal scenario: every chart-expressible key turned on.
// It lives with the other chart scenarios so `tests/run.sh` renders it too.
const coverageValues = chartDir + "/tests/20-config-coverage.yaml"

// appKeysNotExposedByChart lists keys present in default_config.yaml that the
// chart deliberately does NOT render. Every entry needs a reason: the point of
// the list is to state intent, not to silence the test.
var appKeysNotExposedByChart = map[string]string{
	// Google Pub/Sub and Azure Service Bus are declared in the config schema
	// but have no consumer implementation yet. Rendering them would advertise
	// an inbound queue backend that does nothing.
	"queue.pubsub.enable": "roadmap-only backend; no consumer implemented",
	"queue.azbus.enable":  "roadmap-only backend; no consumer implemented",
}

// chartKeysNotInAppConfig lists keys the chart renders that do not appear in
// default_config.yaml. Same rule: each entry states why it is legitimate.
var chartKeysNotInAppConfig = map[string]string{
	// The loader hardcodes the sibling agent_sources.yaml path; this key is
	// documented in the operator docs and rendered for legibility, but the
	// binary ignores it.
	"agent.sources_path": "documented for operators; the loader hardcodes the sibling path",

	// Real StoragePostgresConfig field. The baseline ships the file backend,
	// so the postgres sub-block only appears once storage.type=postgres.
	"storage.postgres.dsn": "real StoragePostgresConfig field; the baseline ships the file backend",

	// Redis client tuning rendered for operator visibility. These have no
	// field in RedisConfig and no env override in the loader — they are
	// inert today and kept only for backward compatibility with existing
	// releases. Do not add more.
	"redis.connection_timeout": "inert: no RedisConfig field; kept for backward compatibility",
	"redis.read_timeout":       "inert: no RedisConfig field; kept for backward compatibility",
	"redis.write_timeout":      "inert: no RedisConfig field; kept for backward compatibility",
	"redis.max_retries":        "inert: no RedisConfig field; kept for backward compatibility",
	"redis.min_retry_backoff":  "inert: no RedisConfig field; kept for backward compatibility",
	"redis.max_retry_backoff":  "inert: no RedisConfig field; kept for backward compatibility",

	// redis.tls is a genuine RedisConfig field (a scalar; nil means TLS on),
	// deliberately absent from the baseline so an omitted key keeps the
	// secure default. The certificate paths are delivered to the client via
	// the REDIS_TLS_* env vars.
	"redis.tls":           "real RedisConfig field; omitted from the baseline so nil keeps TLS on",
	"redis.tls_ca_file":   "file path delivered via the REDIS_TLS_CA_FILE env var",
	"redis.tls_cert_file": "file path delivered via the REDIS_TLS_CERT_FILE env var",
	"redis.tls_key_file":  "file path delivered via the REDIS_TLS_KEY_FILE env var",
}

// chartScenario is one `helm template` invocation.
type chartScenario struct {
	name string
	args []string
}

// chartScenarios covers every mutually exclusive branch in the templates, so
// their union is the full set of keys the chart can express. The on-call
// providers and the storage backends are each an either/or in one render.
var chartScenarios = []chartScenario{
	{name: "defaults"},
	{name: "kubernetes-in-cluster", args: kubernetesModeArgs("in_cluster")},
	{name: "kubernetes-token", args: kubernetesModeArgs("token")},
	{name: "kubernetes-token-file", args: kubernetesModeArgs("token_file")},
	{name: "kubernetes-client-certificate", args: kubernetesModeArgs("client_certificate")},
	{name: "kubernetes-kubeconfig", args: kubernetesModeArgs("kubeconfig")},
	{name: "kubernetes-eks", args: kubernetesModeArgs("eks")},
	{name: "kubernetes-aks-workload-identity", args: kubernetesModeArgs("aks", "agent.tools.kubernetes.auth.aks.credentialMode=workload_identity")},
	{name: "kubernetes-aks-client-secret", args: kubernetesModeArgs("aks", "agent.tools.kubernetes.auth.aks.credentialMode=client_secret")},
	{name: "kubernetes-aks-managed-identity", args: kubernetesModeArgs("aks", "agent.tools.kubernetes.auth.aks.credentialMode=managed_identity")},
	{name: "kubernetes-gke", args: kubernetesModeArgs("gke")},
	{name: "coverage", args: []string{"-f", coverageValues}},
	{name: "coverage-pagerduty", args: []string{"-f", coverageValues, "--set", "oncall.provider=pagerduty"}},
	{name: "coverage-servicenow", args: []string{"-f", coverageValues, "--set", "oncall.provider=servicenow"}},
	{name: "coverage-incident-io", args: []string{"-f", coverageValues, "--set", "oncall.provider=incident_io"}},
	{name: "postgres", args: []string{
		"--set", "storage.type=postgres",
		"--set", "storage.postgres.dsn=postgres://versus:pass@pg:5432/versus?sslmode=require",
	}},
	{name: "ha", args: []string{"-f", chartDir + "/tests/05-ha.yaml"}},
	{name: "tools-secrets", args: []string{"-f", chartDir + "/tests/14-tools-secrets.yaml"}},
}

func kubernetesModeArgs(mode string, overrides ...string) []string {
	values := []string{
		"agent.tools.kubernetes.auth.mode=" + mode,
		"agent.tools.kubernetes.endpoint=https://cluster.example",
		"agent.tools.kubernetes.caFile=/run/kubernetes/ca.crt",
		"agent.tools.kubernetes.serverName=cluster.internal",
		"agent.tools.kubernetes.auth.token=fixture-token",
		"agent.tools.kubernetes.auth.tokenFile=/run/kubernetes/token",
		"agent.tools.kubernetes.auth.clientCertificate.certificateFile=/run/kubernetes/client.crt",
		"agent.tools.kubernetes.auth.clientCertificate.keyFile=/run/kubernetes/client.key",
		"agent.tools.kubernetes.auth.kubeconfig.path=/run/kubernetes/config",
		"agent.tools.kubernetes.auth.kubeconfig.context=production",
		"agent.tools.kubernetes.auth.eks.clusterName=production",
		"agent.tools.kubernetes.auth.eks.region=us-east-1",
		"agent.tools.kubernetes.auth.eks.roleARN=arn:aws:iam::123456789012:role/reader",
		"agent.tools.kubernetes.auth.eks.profile=production",
		"agent.tools.kubernetes.auth.aks.credentialMode=client_secret",
		"agent.tools.kubernetes.auth.aks.serverID=api://aks-server",
		"agent.tools.kubernetes.auth.aks.tenantID=tenant",
		"agent.tools.kubernetes.auth.aks.clientID=client",
		"agent.tools.kubernetes.auth.aks.clientSecret=fixture-secret",
		"agent.tools.kubernetes.auth.aks.federatedTokenFile=/run/azure/token",
		"agent.tools.kubernetes.auth.aks.environment=government",
		"agent.tools.kubernetes.auth.gke.credentialsFile=/run/google/credentials.json",
	}
	values = append(values, overrides...)
	args := make([]string, 0, len(values)*2)
	for _, value := range values {
		args = append(args, "--set-string", value)
	}
	return args
}

// renderChartFiles runs `helm template` for one scenario and returns the config
// files the chart placed in its ConfigMap, keyed by filename.
func renderChartFiles(t *testing.T, sc chartScenario) map[string]string {
	t.Helper()

	args := append([]string{"template", "versus-drift", chartDir}, sc.args...)
	cmd := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("scenario %q: helm template failed: %v\n%s", sc.name, err, stderr.String())
	}

	dec := yaml.NewDecoder(bytes.NewReader(stdout.Bytes()))
	for {
		var doc struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
		}
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc.Kind == "ConfigMap" {
			if _, ok := doc.Data["config.yaml"]; ok {
				return doc.Data
			}
		}
	}
	t.Fatalf("scenario %q: no ConfigMap carrying config.yaml in the rendered output", sc.name)
	return nil
}

// renderChartConfig returns just the rendered config.yaml for a scenario.
func renderChartConfig(t *testing.T, sc chartScenario) string {
	t.Helper()
	return renderChartFiles(t, sc)["config.yaml"]
}

// TestHelmChartRendersLoadableConfig parses each rendered config file back
// through the real loader, so a block whose SHAPE disagrees with the Go structs
// (a map where a scalar is expected, say) fails here instead of crashing the
// pod at boot. The sibling agent_sources.yaml and tools.yaml are written next
// to config.yaml exactly as the Deployment mounts them, so the loader picks
// them up and validates their shapes too.
func TestHelmChartRendersLoadableConfig(t *testing.T) {
	requireRenderableChart(t)

	for _, sc := range chartScenarios {
		t.Run(sc.name, func(t *testing.T) {
			files := renderChartFiles(t, sc)

			dir := t.TempDir()
			for name, body := range files {
				if !strings.HasSuffix(name, ".yaml") {
					continue // message templates, not config
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatalf("write rendered %s: %v", name, err)
				}
			}

			path := filepath.Join(dir, "config.yaml")
			if _, err := loadConfigFromPath(path); err != nil {
				t.Errorf("chart shape drift: the config rendered by scenario %q does not unmarshal into the Go config structs: %v\n--- config.yaml ---\n%s", sc.name, err, files["config.yaml"])
			}
		})
	}
}

func TestHelmChartKubernetesAuthenticationModesAreIsolated(t *testing.T) {
	requireRenderableChart(t)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.20.30.40")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	for _, scenario := range chartScenarios[1:11] {
		t.Run(scenario.name, func(t *testing.T) {
			files := renderChartFiles(t, scenario)
			directory := t.TempDir()
			for name, body := range files {
				if strings.HasSuffix(name, ".yaml") {
					if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			loaded, err := loadConfigFromPath(filepath.Join(directory, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			configuration := loaded.Agent.Tools.Kubernetes
			mode := configuration.Auth.Mode
			if mode != "eks" && configuration.Auth.EKS != (KubernetesEKSConfig{}) || mode != "aks" && configuration.Auth.AKS != (KubernetesAKSConfig{}) || mode != "gke" && configuration.Auth.GKE != (KubernetesGKEConfig{}) {
				t.Fatalf("mode %q retained inactive cloud auth: %+v", mode, configuration.Auth)
			}
			if (mode == "in_cluster" || mode == "kubeconfig") && (configuration.Endpoint != "" || configuration.CAFile != "" || configuration.CAData != "" || configuration.ServerName != "") {
				t.Fatalf("mode %q retained top-level endpoint/TLS fields: %+v", mode, configuration)
			}
			if mode == "in_cluster" {
				resolved, resolveErr := kubernetespkg.ResolveAuthentication(kubernetespkg.AuthOptions{Mode: mode})
				if resolveErr != nil || resolved.Endpoint != "https://10.20.30.40:443" {
					t.Fatalf("rendered in-cluster auth = %+v, %v", resolved, resolveErr)
				}
			}
		})
	}
}

// TestHelmChartCoversDefaultConfig is the chart-side analogue of
// TestDefaultConfigMatchesSample: it fails when the chart cannot express a key
// the application supports (a shipped feature unreachable for Helm users), and
// when the chart renders a key the application does not know about.
func TestHelmChartCoversDefaultConfig(t *testing.T) {
	requireRenderableChart(t)

	appKeys := map[string]struct{}{}
	for _, k := range parseRawYAML(t, defaultConfigYAML).AllKeys() {
		appKeys[k] = struct{}{}
	}

	chartKeys := map[string][]string{} // key -> scenarios that produced it
	for _, sc := range chartScenarios {
		for _, k := range parseRawYAML(t, []byte(renderChartConfig(t, sc))).AllKeys() {
			chartKeys[k] = append(chartKeys[k], sc.name)
		}
	}

	for _, k := range sortedKeys(appKeys) {
		if _, ok := appKeysNotExposedByChart[k]; ok {
			if _, rendered := chartKeys[k]; rendered {
				t.Errorf("stale exclusion: %q is listed in appKeysNotExposedByChart (%s) but the chart now renders it — drop the exclusion", k, appKeysNotExposedByChart[k])
			}
			continue
		}
		if !coveredBy(chartKeys, k) {
			t.Errorf("chart drift: the application supports %q (pkg/config/default_config.yaml) but no chart scenario renders it — add it to helm/versus-incident/templates/configmap.yaml with a matching values.yaml key, or record why it is not chart-exposed in appKeysNotExposedByChart", k)
		}
	}

	for _, k := range sortedKeys(chartKeys) {
		if _, ok := chartKeysNotInAppConfig[k]; ok {
			continue
		}
		if !coveredByPrefix(appKeys, k) {
			t.Errorf("chart drift: the chart renders %q (scenarios: %s) but the application has no such key in pkg/config/default_config.yaml — remove it from the chart or record why it is legitimate in chartKeysNotInAppConfig", k, strings.Join(chartKeys[k], ", "))
		}
	}
}

// chartTestsEnv makes an unrenderable chart a failure instead of a skip.
// helm/versus-incident/tests/run.sh sets it after `helm dependency build`, so
// the chart CI job can never silently stop checking for drift.
const chartTestsEnv = "VERSUS_HELM_CHART_TESTS"

// requireRenderableChart skips unless `helm template` can actually run. The
// plain `go test ./...` job has Helm on PATH but has not vendored the
// subcharts, and a missing subchart is a broken environment rather than drift.
func requireRenderableChart(t *testing.T) {
	t.Helper()

	var unmet error
	if _, err := exec.LookPath("helm"); err != nil {
		unmet = fmt.Errorf("helm is not on PATH; install Helm 3.x")
	} else {
		unmet = chartDependenciesVendored()
	}
	if unmet == nil {
		return
	}
	if os.Getenv(chartTestsEnv) == "1" {
		t.Fatalf("%s=1 but the chart cannot be rendered: %v", chartTestsEnv, unmet)
	}
	t.Skipf("chart drift tests need a renderable chart: %v — run helm/versus-incident/tests/run.sh", unmet)
}

// chartDependenciesVendored reports whether every subchart Chart.yaml declares
// is present in charts/. `helm template` refuses to render without them.
func chartDependenciesVendored() error {
	raw, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		return fmt.Errorf("read Chart.yaml: %w", err)
	}
	var meta struct {
		Dependencies []struct {
			Name string `yaml:"name"`
		} `yaml:"dependencies"`
	}
	if err := yaml.Unmarshal(raw, &meta); err != nil {
		return fmt.Errorf("parse Chart.yaml: %w", err)
	}
	for _, dep := range meta.Dependencies {
		if tgz, _ := filepath.Glob(filepath.Join(chartDir, "charts", dep.Name+"-*.tgz")); len(tgz) > 0 {
			continue
		}
		if fi, err := os.Stat(filepath.Join(chartDir, "charts", dep.Name)); err == nil && fi.IsDir() {
			continue
		}
		return fmt.Errorf("subchart %q is not vendored in charts/; run `helm dependency build %s`", dep.Name, chartDir)
	}
	return nil
}

// coveredBy reports whether the chart expresses key k. A chart is allowed to
// render a deeper leaf than the baseline declares (the baseline carries an
// empty `agent.ai.analyze:`, the chart renders `agent.ai.analyze.model`), so a
// descendant counts as coverage.
func coveredBy(chartKeys map[string][]string, k string) bool {
	if _, ok := chartKeys[k]; ok {
		return true
	}
	for ck := range chartKeys {
		if strings.HasPrefix(ck, k+".") {
			return true
		}
	}
	return false
}

// coveredByPrefix is the mirror image: a chart key is known to the application
// when it, or any of its ancestors, is declared in the baseline.
func coveredByPrefix(appKeys map[string]struct{}, k string) bool {
	for {
		if _, ok := appKeys[k]; ok {
			return true
		}
		i := strings.LastIndex(k, ".")
		if i < 0 {
			return false
		}
		k = k[:i]
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
