package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
