package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestChangeCaptureConfig(t *testing.T) {
	for _, mode := range []string{"continuous", "demand", "invalid"} {
		connector, err := decodeConnectors(map[string]any{"kubernetes": map[string]any{"change_capture": mode}})
		if mode == "invalid" {
			if err == nil {
				t.Fatal("invalid mode accepted")
			}
			continue
		}
		if err != nil || connector.Kubernetes.ChangeCapture != mode || cloneConnectorsConfig(connector).Kubernetes.ChangeCapture != mode {
			t.Fatalf("capture mode clone: %+v %v", connector, err)
		}
	}
	connector, err := decodeConnectors(nil)
	if err != nil || connector.Kubernetes.ChangeCapture != "continuous" {
		t.Fatalf("capture default: %+v %v", connector, err)
	}
}

func TestKubernetesMultipleValidation(t *testing.T) {
	entry := func(id, endpoint string) map[string]any {
		return map[string]any{"cluster_id": id, "endpoint": endpoint}
	}
	cases := []map[string]any{
		{"clusters": []any{entry("one", "https://one.example")}},
		{"multiple": false, "clusters": []any{}},
		{"multiple": true, "clusters": []any{}},
		{"multiple": true, "endpoint": "sensitive-value", "clusters": []any{entry("one", "https://one.example")}},
		{"multiple": true, "auth": map[string]any{}, "clusters": []any{entry("one", "https://one.example")}},
		{"multiple": true, "actions": map[string]any{"enable": false}, "clusters": []any{entry("one", "https://one.example")}},
		{"multiple": true, "clusters": []any{entry("", "https://one.example")}},
		{"multiple": true, "clusters": []any{entry("BAD", "https://one.example")}},
		{"multiple": true, "clusters": []any{entry(strings.Repeat("a", 64), "https://one.example")}},
		{"multiple": true, "clusters": []any{entry("one", "https://one.example"), entry("one", "https://two.example")}},
		{"multiple": true, "clusters": []any{entry("one", "https://one.example"), entry("two", "https://one.example/")}},
		{"multiple": true, "clusters": []any{entry("one", "https://one.example"), entry("two", "https://ONE.example:443/")}},
		{"multiple": true, "clusters": []any{map[string]any{"cluster_id": "one", "auth": map[string]any{"mode": "in_cluster"}}, map[string]any{"cluster_id": "two", "auth": map[string]any{"mode": "in_cluster"}}}},
		{"multiple": true, "clusters": []any{map[string]any{"cluster_id": "one", "unknown": "sensitive-value"}}},
		{"multiple": true, "clusters": []any{map[string]any{"cluster_id": "one", "auth": map[string]any{"unknown": "sensitive-value"}}}},
	}
	tooMany := []any{}
	for index := 0; index < 17; index++ {
		tooMany = append(tooMany, entry(fmt.Sprintf("cluster-%d", index), fmt.Sprintf("https://cluster-%d.example", index)))
	}
	cases = append(cases, map[string]any{"multiple": true, "clusters": tooMany})
	for index, input := range cases {
		_, err := decodeConnectors(map[string]any{"kubernetes": input})
		if err == nil || strings.Contains(err.Error(), "sensitive-value") {
			t.Fatalf("case %d: unsafe or missing rejection: %v", index, err)
		}
	}
	t.Setenv("CLUSTER_CIDRS", "10.0.0.0/8, 2001:db8::/32")
	loaded, err := decodeConnectors(map[string]any{"kubernetes": map[string]any{"multiple": true, "clusters": []any{map[string]any{"cluster_id": "one", "endpoint_cidrs": "${CLUSTER_CIDRS}", "auth": map[string]any{"mode": "token", "token": "reader"}, "actions": map[string]any{"enable": true, "auth": map[string]any{"mode": "token", "token": "actor"}}}}}})
	if err != nil || len(loaded.Kubernetes.Resolved()) != 1 || loaded.Kubernetes.Clusters[0].Actions.Auth.Token != "actor" || len(loaded.Kubernetes.Clusters[0].EndpointCIDRs) != 2 {
		t.Fatalf("valid single-entry fleet: %+v %v", loaded.Kubernetes.Clusters, err)
	}
	cloned := cloneConnectorsConfig(loaded)
	cloned.Kubernetes.Clusters[0].EndpointCIDRs[0] = "changed"
	cloned.Kubernetes.Clusters[0].Auth.Token = "changed"
	if loaded.Kubernetes.Clusters[0].EndpointCIDRs[0] != "10.0.0.0/8" || loaded.Kubernetes.Clusters[0].Auth.Token != "reader" {
		t.Fatal("cluster clone mutated source")
	}
}

func TestKubernetesSingleModeUnchanged(t *testing.T) {
	for _, multiple := range []any{nil, false} {
		fields := map[string]any{"endpoint": "https://single.example", "cluster_id": "single"}
		if multiple != nil {
			fields["multiple"] = multiple
		}
		loaded, err := decodeConnectors(map[string]any{"kubernetes": fields})
		if err != nil || loaded.Kubernetes.Multiple || len(loaded.Kubernetes.Resolved()) != 1 || loaded.Kubernetes.Resolved()[0].Endpoint != "https://single.example" {
			t.Fatalf("single mode: %v", err)
		}
	}
}

func TestConnectorsLoad(t *testing.T) {
	directory := t.TempDir()
	main := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(main, []byte("connectors:\n  kubernetes:\n    endpoint: https://inline.example\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(main)
	if err != nil || loaded.Connectors.Kubernetes.Endpoint != "https://inline.example" {
		t.Fatalf("inline: %v", err)
	}
	t.Setenv("CONNECTOR_ENDPOINT", "https://sibling.example")
	if err := os.WriteFile(filepath.Join(directory, "connectors.yaml"), []byte("connectors:\n  kubernetes:\n    endpoint: ${CONNECTOR_ENDPOINT}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadConfigFromPath(main)
	if err != nil || loaded.Connectors.Kubernetes.Endpoint != "https://sibling.example" {
		t.Fatalf("sibling: %v", err)
	}
}

// A connectors.yaml without settings must not switch off connectors written
// inline in config.yaml; a mounted file with settings still replaces them.
func TestInlineConnectorsSurviveEmptyConnectorsFile(t *testing.T) {
	directory := t.TempDir()
	main := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(main, []byte("connectors:\n  kubernetes:\n    auth:\n      mode: in_cluster\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(directory, "connectors.yaml")
	for name, content := range map[string]string{
		"no file":       "",
		"empty file":    "\n",
		"comments only": "# connectors:\n#   kubernetes:\n#     auth:\n#       mode: token\n",
	} {
		if name == "no file" {
			_ = os.Remove(sibling)
		} else if err := os.WriteFile(sibling, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadConfigFromPath(main)
		if err != nil || loaded.Connectors.Kubernetes.Auth.Mode != "in_cluster" {
			t.Fatalf("%s: inline connectors were not used: %v", name, err)
		}
	}
	if err := os.WriteFile(sibling, []byte("connectors:\n  kubernetes:\n    auth:\n      mode: \"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(main)
	if err != nil || loaded.Connectors.Kubernetes.Auth.Mode != "" {
		t.Fatalf("a mounted connectors file with settings must replace inline connectors: %v", err)
	}
	if err := os.WriteFile(sibling, []byte("not: [valid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigFromPath(main); err == nil {
		t.Fatal("an invalid connectors file must still fail the load")
	}
}

// The repository's sample connectors.yaml carries comments only, so a build
// that ships it can never override inline connectors.
func TestShippedConnectorsSampleHasNoSettings(t *testing.T) {
	_, present, err := loadSiblingConnectorsFile("../../config/connectors.yaml")
	if err != nil || present {
		t.Fatalf("config/connectors.yaml must hold only comments: present=%v err=%v", present, err)
	}
}

func TestRequiredGatewaySecretStopsStartup(t *testing.T) {
	directory := t.TempDir()
	main := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(main, []byte("gateway_secret: ${GATEWAY_SECRET}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEWAY_SECRET", "")
	t.Setenv("GATEWAY_SECRET_REQUIRED", "")
	if _, err := loadConfigFromPath(main); err != nil {
		t.Fatalf("an empty sign-in secret is allowed unless required: %v", err)
	}
	t.Setenv("GATEWAY_SECRET_REQUIRED", "true")
	t.Setenv("GATEWAY_SECRET_HINT", "create it with: kubectl -n versus create secret generic versus-incident")
	_, err := loadConfigFromPath(main)
	if err == nil || !strings.Contains(err.Error(), "sign-in secret is missing") || !strings.Contains(err.Error(), "kubectl -n versus create secret generic versus-incident") {
		t.Fatalf("missing required sign-in secret: %v", err)
	}
	t.Setenv("GATEWAY_SECRET", "present")
	loaded, err := loadConfigFromPath(main)
	if err != nil || loaded.GatewaySecret != "present" {
		t.Fatalf("a supplied sign-in secret must load: %v", err)
	}
}

// A mistyped connector key must be named with its YAML path so the operator
// can fix it; the value is never echoed because it may be a credential.
func TestConnectorUnknownKeyIsNamedWithoutItsValue(t *testing.T) {
	for _, test := range []struct {
		fields map[string]any
		want   string
	}{
		{map[string]any{"endpointt": "sensitive-value"}, `connectors.kubernetes: unknown key "endpointt"`},
		{map[string]any{"auth": map[string]any{"mode": "eks", "eks": map[string]any{"cluster": "sensitive-value"}}}, `connectors.kubernetes.auth.eks: unknown key "cluster"`},
		{map[string]any{"actions": map[string]any{"auth": map[string]any{"tokn": "sensitive-value"}}}, `connectors.kubernetes.actions.auth: unknown key "tokn"`},
		{map[string]any{"multiple": true, "clusters": []any{map[string]any{"cluster_id": "one", "endpoint": "https://one.example", "auth": map[string]any{"mode": "token", "toke": "sensitive-value"}}}}, `connectors.kubernetes.clusters[0].auth: unknown key "toke"`},
		{map[string]any{"timeout": []any{"sensitive-value"}}, `connectors.kubernetes.timeout: invalid value`},
	} {
		_, err := decodeConnectors(map[string]any{"kubernetes": test.fields})
		if err == nil || err.Error() != test.want {
			t.Errorf("error = %v, want %s", err, test.want)
		}
		if err != nil && strings.Contains(err.Error(), "sensitive-value") {
			t.Errorf("error echoes a configured value: %v", err)
		}
	}
	_, err := decodeConnectors(map[string]any{"kubernetez": map[string]any{}})
	if err == nil || err.Error() != `connectors: unknown key "kubernetez"` {
		t.Errorf("top-level unknown key: %v", err)
	}
}

func TestCloneConnectors(t *testing.T) {
	source := &Config{Connectors: ConnectorsConfig{Kubernetes: KubernetesConnectorConfig{KubernetesToolConfig: KubernetesToolConfig{
		EndpointCIDRs: []string{"10.0.0.0/8"}, Auth: KubernetesAuthConfig{Mode: "token", Token: "reader"},
		Actions: KubernetesActionsToolConfig{Enable: true, Auth: KubernetesAuthConfig{Mode: "token", Token: "actor"}},
	}}}}
	cloned := cloneConfig(source)
	cloned.Connectors.Kubernetes.EndpointCIDRs[0] = "changed"
	if source.Connectors.Kubernetes.EndpointCIDRs[0] != "10.0.0.0/8" || cloned.Connectors.Kubernetes.Actions.Auth.Token != "actor" {
		t.Fatal("connector clone lost isolation or actor credentials")
	}
}

func TestConnectorErrorsAreSafe(t *testing.T) {
	for _, input := range []string{
		"agent.tools:\n  kubernetes: null\n",
		"connectors: {}\n---\nconnectors:\n  kubernetes:\n    unknown: sensitive-value\n",
		"agent:\n  tools:\n    kubernetes: null\n",
		"connectors: null\n",
		"connectors:\n  kubernetes:\n    auth: null\n",
		"agent:\n  tools:\n    kubernetes: {}\n",
		"tools:\n  kubernetes:\n    auth:\n      token: sensitive-value\n",
		"connectors:\n  kubernetes:\n    unknown: sensitive-value\n",
		"connectors:\n  kubernetes:\n    endpoint: [sensitive-value]\n",
	} {
		t.Run(input[:5], func(t *testing.T) {
			main := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(main, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := loadConfigFromPath(main)
			if err == nil || strings.Contains(err.Error(), "sensitive-value") {
				t.Fatalf("unsafe or missing rejection: %v", err)
			}
		})
	}
}

func TestConnectorCredentialsExpandOnce(t *testing.T) {
	t.Setenv("CONNECTOR_SECRET", "$UNSET_LITERAL_SECRET ${ALSO_LITERAL}")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("connectors:\n  kubernetes:\n    auth:\n      token: ${CONNECTOR_SECRET}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfigFromPath(path)
	if err != nil || loaded.Connectors.Kubernetes.Auth.Token != os.Getenv("CONNECTOR_SECRET") {
		t.Fatal("connector credential expanded more than once")
	}
}

func TestConnectorTypedEnvironmentScalars(t *testing.T) {
	t.Setenv("CONNECTOR_PRIVATE", "true")
	t.Setenv("CONNECTOR_REPLICAS", "12")
	path := filepath.Join(t.TempDir(), "connectors.yaml")
	if err := os.WriteFile(path, []byte("connectors:\n  kubernetes:\n    allow_private_networks: ${CONNECTOR_PRIVATE}\n    actions:\n      max_replicas: ${CONNECTOR_REPLICAS}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConnectorsFile(path)
	if err != nil || !loaded.Kubernetes.AllowPrivateNetworks || loaded.Kubernetes.Actions.MaxReplicas != 12 {
		t.Fatalf("typed env: %v", err)
	}
}

func TestConnectorEndpointCIDRsNormalize(t *testing.T) {
	t.Setenv("CONNECTOR_CIDRS", " 10.0.0.0/8, 2001:db8::/32 ")
	for _, input := range []string{
		"endpoint_cidrs: '10.0.0.0/8, 2001:db8::/32'",
		"endpoint_cidrs: ${CONNECTOR_CIDRS}",
		"endpoint_cidrs: [' 10.0.0.0/8 ', '2001:db8::/32']",
	} {
		for _, sibling := range []bool{false, true} {
			directory := t.TempDir()
			main := filepath.Join(directory, "config.yaml")
			path := main
			if sibling {
				if err := os.WriteFile(main, []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(directory, "connectors.yaml")
			}
			if err := os.WriteFile(path, []byte("connectors:\n  kubernetes:\n    "+input+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadConfigFromPath(main)
			if err != nil || !reflect.DeepEqual(loaded.Connectors.Kubernetes.EndpointCIDRs, []string{"10.0.0.0/8", "2001:db8::/32"}) {
				t.Fatalf("normalization sibling=%v: %v", sibling, err)
			}
		}
	}
	for _, raw := range []any{"sensitive-value", "10.0.0.0/8,", []any{"10.0.0.0/8", 42}, true, nil, []any{"${MISSING_CIDR}"}} {
		_, err := decodeConnectors(map[string]any{"kubernetes": map[string]any{"endpoint_cidrs": raw}})
		if err == nil || strings.Contains(err.Error(), "sensitive-value") {
			t.Fatalf("unsafe or missing CIDR rejection: %v", err)
		}
	}
	for _, raw := range []any{"", []any{}, []string{}} {
		loaded, err := decodeConnectors(map[string]any{"kubernetes": map[string]any{"endpoint_cidrs": raw}})
		if err != nil || len(loaded.Kubernetes.EndpointCIDRs) != 0 {
			t.Fatalf("empty CIDRs: %v", err)
		}
	}
}

// A connector names the resources it never lists and the namespaces it reads;
// both are bounded lists of valid names, and a clone owns its own copies.
func TestConnectorCollectionScope(t *testing.T) {
	connector, err := decodeConnectors(map[string]any{"kubernetes": map[string]any{
		"exclude_resources": []any{"secrets"}, "namespaces": []any{"shop", "payments"},
	}})
	if err != nil || strings.Join(connector.Kubernetes.ExcludeResources, ",") != "secrets" || strings.Join(connector.Kubernetes.Namespaces, ",") != "shop,payments" {
		t.Fatalf("collection scope = %+v %v", connector.Kubernetes, err)
	}
	for _, resource := range []string{"pods", "namespaces"} {
		if _, err := decodeConnectors(map[string]any{"kubernetes": map[string]any{"exclude_resources": []any{resource}}}); err == nil || !strings.Contains(err.Error(), "cannot exclude \""+resource+"\"") {
			t.Errorf("excluding %s answered %v", resource, err)
		}
	}
	for key, value := range map[string][]any{
		"namespaces":        {"Shop"},
		"exclude_resources": {"secrets", "secrets"},
	} {
		if _, err := decodeConnectors(map[string]any{"kubernetes": map[string]any{key: value}}); err == nil || !strings.Contains(err.Error(), "connectors.kubernetes."+key) {
			t.Errorf("%s %v answered %v", key, value, err)
		}
	}
	source := &Config{Connectors: connector}
	cloned := cloneConfig(source)
	cloned.Connectors.Kubernetes.Namespaces[0] = "changed"
	cloned.Connectors.Kubernetes.ExcludeResources[0] = "changed"
	if source.Connectors.Kubernetes.Namespaces[0] != "shop" || source.Connectors.Kubernetes.ExcludeResources[0] != "secrets" {
		t.Fatal("a clone shares the collection scope lists")
	}
}
