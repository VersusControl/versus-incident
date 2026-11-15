package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests cover what a default `helm install` and the one-file kubectl
// manifest give a new user: a sign-in secret that exists and survives
// upgrades, install notes that say how to reach the UI, and a committed
// manifest that matches the chart.

// gatewaySecretLookup is the exact lookup the chart uses to keep the generated
// sign-in secret across upgrades. The upgrade test swaps it for a fixed
// stored Secret; if the template changes, the test must change with it.
const gatewaySecretLookup = `lookup "v1" "Secret" .Release.Namespace (printf "%s-secrets" (include "versus-incident.fullname" .))`

// copyChart copies the chart, subcharts included, into a test directory so a
// test can patch templates without touching the real chart.
func copyChart(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "versus-incident")
	err := filepath.WalkDir(chartDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(chartDir, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func helmTemplate(t *testing.T, chart string, args ...string) []byte {
	t.Helper()
	command := exec.Command("helm", append([]string{"template", "versus", chart, "--namespace", "versus"}, args...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr.String())
	}
	return output
}

// renderedSignInSecret returns the base64 gateway_secret stored in the
// chart-managed Secret.
func renderedSignInSecret(t *testing.T, rendered []byte) string {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var document struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Data     map[string]string     `yaml:"data"`
		}
		if err := decoder.Decode(&document); err != nil {
			break
		}
		if document.Kind == "Secret" && strings.HasSuffix(document.Metadata.Name, "-secrets") {
			return document.Data["gateway_secret"]
		}
	}
	t.Fatal("no chart-managed Secret in the rendered output")
	return ""
}

func TestHelmGeneratesSignInSecretOnFirstInstall(t *testing.T) {
	requireRenderableChart(t)
	first := renderedSignInSecret(t, helmTemplate(t, chartDir))
	second := renderedSignInSecret(t, helmTemplate(t, chartDir))
	for _, encoded := range []string{first, second} {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).Match(decoded) {
			t.Fatalf("generated sign-in secret is not 32 random bytes in hex: %q (%v)", decoded, err)
		}
	}
	if first == second {
		t.Fatal("two first installs generated the same sign-in secret")
	}
}

func TestHelmKeepsGeneratedSignInSecretOnUpgrade(t *testing.T) {
	requireRenderableChart(t)
	chart := copyChart(t)
	helpers := filepath.Join(chart, "templates", "_helpers.tpl")
	raw, err := os.ReadFile(helpers)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), gatewaySecretLookup) {
		t.Fatal("the chart no longer reads the stored sign-in secret with the expected lookup; update this test")
	}
	stored := base64.StdEncoding.EncodeToString([]byte("secret-from-the-previous-release"))
	// The upgrade sees the Secret the previous release created.
	patched := strings.Replace(string(raw), gatewaySecretLookup, `(dict "data" (dict "gateway_secret" "`+stored+`"))`, 1)
	if err := os.WriteFile(helpers, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := renderedSignInSecret(t, helmTemplate(t, chart)); got != stored {
		t.Fatalf("upgrade replaced the stored sign-in secret: got %q", got)
	}
	user := base64.StdEncoding.EncodeToString([]byte("chosen-by-the-user"))
	if got := renderedSignInSecret(t, helmTemplate(t, chart, "--set", "gatewaySecret=chosen-by-the-user")); got != user {
		t.Fatalf("a user-set sign-in secret must win over the stored one: got %q", got)
	}
}

// renderNotes renders NOTES.txt, which `helm template` skips, by wrapping it
// in a named template inside a copy of the chart.
func renderNotes(t *testing.T, args ...string) string {
	t.Helper()
	chart := copyChart(t)
	notes, err := os.ReadFile(filepath.Join(chart, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := "{{- define \"test.notes\" -}}\n" + string(notes) + "\n{{- end -}}\n"
	if err := os.WriteFile(filepath.Join(chart, "templates", "_test_notes.tpl"), []byte(wrapped), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "templates", "test-notes.yaml"), []byte("notes: {{ include \"test.notes\" . | toJson }}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered := helmTemplate(t, chart, append([]string{"--show-only", "templates/test-notes.yaml"}, args...)...)
	var document struct {
		Notes string `yaml:"notes"`
	}
	if err := yaml.Unmarshal(rendered, &document); err != nil {
		t.Fatal(err)
	}
	return document.Notes
}

func TestHelmNotesExplainKubernetesAndSignIn(t *testing.T) {
	requireRenderableChart(t)
	notes := renderNotes(t, "--set", "connectors.kubernetes.enabled=true")
	for _, want := range []string{
		"kubectl -n versus port-forward svc/versus-versus-incident 3000:3000",
		"http://localhost:3000",
		"kubectl -n versus get secret versus-versus-incident-secrets -o jsonpath='{.data.gateway_secret}' | base64 -d",
		"ServiceAccount versus/versus-versus-incident (auth.mode=in_cluster)",
		"ClusterRole versus-versus-incident-kubernetes-reader",
		"https://docs.versusincident.com/#/agent/kubernetes/quickstart",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("install notes miss %q:\n%s", want, notes)
		}
	}
	for _, stale := range []string{"NOT registered", "404"} {
		if strings.Contains(notes, stale) {
			t.Errorf("install notes still say %q", stale)
		}
	}
	// The connector is off by default: one line on how to turn it on, plus
	// the quickstart.
	off := renderNotes(t)
	if !strings.Contains(off, "The Kubernetes connector is off. Turn it on with --set connectors.kubernetes.enabled=true") ||
		!strings.Contains(off, "https://docs.versusincident.com/#/agent/kubernetes/quickstart") || strings.Contains(off, "ClusterRole") {
		t.Errorf("notes for the default, disabled connector:\n%s", off)
	}
	existing := renderNotes(t, "--set", "gatewaySecretExistingSecret=versus-incident")
	if !strings.Contains(existing, "kubectl -n versus create secret generic versus-incident --from-literal=gateway_secret=$(openssl rand -hex 32)") {
		t.Errorf("notes for a user-created Secret do not say how to create it:\n%s", existing)
	}
}

func TestKubectlManifestMatchesChart(t *testing.T) {
	requireRenderableChart(t)
	command := exec.Command("bash", "../../scripts/render_kubernetes_manifest.sh", "--check")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("examples/kubernetes/k8s-connector-in-cluster-enabled.yaml differs from a fresh render: %v\n%s", err, output)
	}
}

func TestKubectlManifestReadsUserCreatedSignInSecret(t *testing.T) {
	raw, err := os.ReadFile("../../examples/kubernetes/k8s-connector-in-cluster-enabled.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	found := false
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			break
		}
		encoded, _ := json.Marshal(document)
		text := string(encoded)
		if document["kind"] == "Secret" && strings.Contains(text, `"gateway_secret"`) {
			t.Fatal("the manifest must not carry a sign-in secret")
		}
		if document["kind"] != "Deployment" {
			continue
		}
		found = true
		for _, want := range []string{
			`"name":"GATEWAY_SECRET","valueFrom":{"secretKeyRef":{"key":"gateway_secret","name":"versus-incident","optional":true}}`,
			`"name":"GATEWAY_SECRET_REQUIRED","value":"true"`,
			`"image":"ghcr.io/versuscontrol/versus-incident:v`,
			`"replicas":1`,
			`"namespace":"versus"`,
		} {
			if !strings.Contains(text, want) {
				t.Errorf("manifest Deployment misses %s", want)
			}
		}
	}
	if !found {
		t.Fatal("the manifest has no Deployment")
	}
}

// A default install renders disabled channels as `enable: false` only, no
// proxy block and, with the Kubernetes connector off, no connector settings.
// The loader must accept that config as it is, with no error and no log
// output, and both body-logging switches must be off. Turning the connector
// on must load just as cleanly, reading the cluster with in_cluster.
func TestHelmDefaultRenderLoadsClean(t *testing.T) {
	requireRenderableChart(t)
	for _, test := range []struct {
		name, mode string
		args       []string
	}{
		{name: "defaults", mode: ""},
		{name: "connector-on", mode: "in_cluster", args: []string{"--set", "connectors.kubernetes.enabled=true"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertChartRenderLoadsClean(t, chartScenario{name: test.name, args: test.args}, test.mode)
		})
	}
}

func assertChartRenderLoadsClean(t *testing.T, scenario chartScenario, mode string) {
	t.Helper()
	files := renderChartFiles(t, scenario)
	if mode == "" && strings.Contains(files["connectors.yaml"], "connectors:") {
		t.Fatalf("connector settings rendered while the connector is off:\n%s", files["connectors.yaml"])
	}
	if strings.Contains(files["config.yaml"], "proxy:") || strings.Contains(files["config.yaml"], "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("default render still carries proxy or disabled-channel keys:\n%s", files["config.yaml"])
	}
	directory := t.TempDir()
	for name, body := range files {
		if strings.HasSuffix(name, ".yaml") {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	loaded, err := loadConfigFromPath(filepath.Join(directory, "config.yaml"))
	log.SetOutput(previous)
	if err != nil {
		t.Fatalf("default render does not load: %v", err)
	}
	if logged.Len() > 0 {
		t.Fatalf("loading the default render logged: %s", logged.String())
	}
	if loaded.Alert.DebugBody || loaded.Queue.DebugBody {
		t.Fatal("alert and queue body logging must be off by default")
	}
	if loaded.Alert.Telegram.Enable || loaded.Alert.Viber.Enable || loaded.Alert.Email.Enable || loaded.Alert.MSTeams.Enable || loaded.Alert.Lark.Enable || loaded.Proxy.URL != "" {
		t.Fatal("a channel or the proxy is on in the default render")
	}
	if loaded.Connectors.Kubernetes.Auth.Mode != mode {
		t.Fatalf("connector mode = %q", loaded.Connectors.Kubernetes.Auth.Mode)
	}
}
