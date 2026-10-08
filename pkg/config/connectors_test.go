package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
	source := &Config{Connectors: ConnectorsConfig{Kubernetes: KubernetesToolConfig{
		EndpointCIDRs: []string{"10.0.0.0/8"}, Auth: KubernetesAuthConfig{Mode: "token", Token: "reader"},
		Actions: KubernetesActionsToolConfig{Enable: true, Auth: KubernetesAuthConfig{Mode: "token", Token: "actor"}},
	}}}
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
