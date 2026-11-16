package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests cover what a default `helm install` and the one-file kubectl
// manifest give a new user: a sign-in secret that exists and survives
// upgrades, and a committed manifest that reads the user's sign-in Secret. The
// install notes and the upgrade are chart scenarios 98 to 102; the chart CI job
// checks the committed manifests against a fresh render.

// gatewaySecretLookup is the exact lookup the chart uses to keep the generated
// sign-in secret across upgrades. Chart scenarios 101 and 102 render an upgrade
// by swapping it for a stored Secret (helm/versus-incident/tests/run.sh); if the
// template changes, they must change with it.
const gatewaySecretLookup = `lookup "v1" "Secret" .Release.Namespace (printf "%s-secrets" (include "versus-incident.fullname" .))`

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

// Two first installs, chart scenarios 01 and 21, both leave the sign-in secret
// unset, so each generates its own.
func TestHelmGeneratesSignInSecretOnFirstInstall(t *testing.T) {
	first := renderedSignInSecret(t, chartRender(t, "01-defaults"))
	second := renderedSignInSecret(t, chartRender(t, "21-kubernetes-default"))
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

// The upgrade itself is rendered by chart scenarios 101 and 102; this keeps
// the lookup they replace in the chart.
func TestHelmReadsStoredSignInSecretOnUpgrade(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), gatewaySecretLookup) {
		t.Fatal("the chart no longer reads the stored sign-in secret with the expected lookup; update this test and SIGN_IN_SECRET_LOOKUP in helm/versus-incident/tests/run.sh")
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
			`"image":"ghcr.io/versuscontrol/versus-incident"`,
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
	for _, test := range []struct{ name, scenario, mode string }{
		{name: "defaults", scenario: "01-defaults", mode: ""},
		{name: "connector-on", scenario: "21-kubernetes-default", mode: "in_cluster"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertChartRenderLoadsClean(t, test.scenario, test.mode)
		})
	}
}

func assertChartRenderLoadsClean(t *testing.T, scenario string, mode string) {
	t.Helper()
	files := renderChartFiles(t, scenario)
	if mode == "" && strings.Contains(files["connectors.yaml"], "connectors:") {
		t.Fatalf("connector settings rendered while the connector is off:\n%s", files["connectors.yaml"])
	}
	if strings.Contains(files["config.yaml"], "proxy:") || strings.Contains(files["config.yaml"], "TELEGRAM_BOT_TOKEN") {
		t.Fatalf("default render still carries proxy or disabled-channel keys:\n%s", files["config.yaml"])
	}
	path := writeRenderedConfig(t, files)
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	loaded, err := loadConfigFromPath(path)
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
