package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func awsAccount(accountID string, fields map[string]any) map[string]any {
	account := map[string]any{"account_id": accountID}
	for key, value := range fields {
		account[key] = value
	}
	return account
}

func TestAWSConnectorConfigShape(t *testing.T) {
	t.Setenv("AWS_EXTERNAL_ID", "external-value")
	loaded, err := decodeConnectors(map[string]any{"aws": map[string]any{
		"enable": true,
		"accounts": []any{
			awsAccount("111122223333", map[string]any{
				"display_name": "Production",
				"regions":      "auto",
				"auth": map[string]any{
					"mode":        "assume_role",
					"role_arn":    "arn:aws:iam::111122223333:role/versus-reader",
					"external_id": "${AWS_EXTERNAL_ID}",
				},
			}),
			awsAccount("444455556666", map[string]any{
				"regions": []any{"us-east-1", "eu-west-1"},
				"auth":    map[string]any{"mode": "default_chain"},
			}),
		},
		"tags":    map[string]any{"value_allowlist": []any{"Name", "app", "service"}},
		"budgets": map[string]any{"max_requests_per_second": "4"},
		"optional_data": map[string]any{
			"cost_explorer":   false,
			"security_hub":    true,
			"config":          false,
			"trusted_advisor": false,
		},
		"health_check_interval": "10m",
	}})
	if err != nil {
		t.Fatal(err)
	}
	connector := loaded.AWS
	if !connector.Enable || len(connector.Accounts) != 2 || connector.Budgets.MaxRequestsPerSecond != 4 || connector.HealthCheckInterval != "10m" {
		t.Fatalf("aws connector = %+v", connector)
	}
	first, second := connector.Accounts[0], connector.Accounts[1]
	if first.AccountID != "111122223333" || first.DisplayName != "Production" || len(first.Regions) != 0 || first.Auth.Mode != "assume_role" || first.Auth.ExternalID != "external-value" {
		t.Fatalf("first account = %+v", first)
	}
	if !reflect.DeepEqual(second.Regions, []string{"us-east-1", "eu-west-1"}) || second.Auth.Mode != "default_chain" {
		t.Fatalf("second account = %+v", second)
	}
	if !connector.OptionalData.SecurityHub || connector.OptionalData.CostExplorer || len(connector.Tags.ValueAllowlist) != 3 {
		t.Fatalf("aws options = %+v", connector)
	}

	cloned := cloneConnectorsConfig(loaded)
	cloned.AWS.Accounts[1].Regions[0] = "changed"
	cloned.AWS.Tags.ValueAllowlist[0] = "changed"
	cloned.AWS.Accounts[0].Auth.ExternalID = "changed"
	if loaded.AWS.Accounts[1].Regions[0] != "us-east-1" || loaded.AWS.Tags.ValueAllowlist[0] != "Name" || loaded.AWS.Accounts[0].Auth.ExternalID != "external-value" {
		t.Fatal("aws clone mutated source")
	}
}

func TestAWSConnectorDefaults(t *testing.T) {
	loaded, err := decodeConnectors(map[string]any{"aws": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.AWS.Budgets.MaxRequestsPerSecond != 2 || loaded.AWS.HealthCheckInterval != "5m" || loaded.AWS.Enable {
		t.Fatalf("aws defaults = %+v", loaded.AWS)
	}
}

func TestAWSConnectorRejections(t *testing.T) {
	t.Setenv("AWS_EXTERNAL_ID", "external-value")
	base := func(fields map[string]any) map[string]any {
		account := map[string]any{
			"account_id": "111122223333",
			"auth": map[string]any{
				"mode":        "assume_role",
				"role_arn":    "arn:aws:iam::111122223333:role/versus-reader",
				"external_id": "${AWS_EXTERNAL_ID}",
			},
		}
		for key, value := range fields {
			account[key] = value
		}
		return map[string]any{"aws": map[string]any{"accounts": []any{account}}}
	}
	cases := []map[string]any{
		{"aws": map[string]any{"actions": map[string]any{"enable": true}, "accounts": []any{}}},
		base(map[string]any{"account_id": "11112222333"}),
		base(map[string]any{"account_id": "sensitive-value"}),
		base(map[string]any{"display_name": strings.Repeat("a", 129)}),
		base(map[string]any{"regions": []any{}}),
		base(map[string]any{"regions": "manual"}),
		base(map[string]any{"regions": []any{"us-east", "sensitive-value"}}),
		base(map[string]any{"auth": map[string]any{"mode": "assume_role"}}),
		base(map[string]any{"auth": map[string]any{"mode": "assume_role", "role_arn": "arn:aws-iso:iam::111122223333:role/reader"}}),
		base(map[string]any{"auth": map[string]any{"mode": "assume_role", "role_arn": "arn:aws:iam::111122223333:role/reader", "external_id": "sensitive-value"}}),
		base(map[string]any{"auth": map[string]any{"mode": "assume_role", "role_arn": "arn:aws:iam::111122223333:role/reader", "external_id": "${MISSING"}}),
		base(map[string]any{"auth": map[string]any{"mode": "assume_role", "role_arn": "arn:aws:iam::111122223333:role/reader", "external_id": ""}}),
		base(map[string]any{"auth": map[string]any{"mode": "default_chain", "role_arn": "arn:aws:iam::111122223333:role/reader"}}),
		base(map[string]any{"auth": map[string]any{"mode": "web_identity", "external_id": "${AWS_EXTERNAL_ID}"}}),
		base(map[string]any{"auth": map[string]any{"mode": "sensitive-value"}}),
		{"aws": map[string]any{"accounts": []any{
			awsAccount("111122223333", map[string]any{"auth": map[string]any{"mode": "default_chain"}}),
			awsAccount("111122223333", map[string]any{"auth": map[string]any{"mode": "default_chain"}}),
		}}},
		{"aws": map[string]any{"budgets": map[string]any{"max_requests_per_second": -1}}},
		{"aws": map[string]any{"budgets": map[string]any{"max_requests_per_second": 2000}}},
		{"aws": map[string]any{"health_check_interval": "5s"}},
		{"aws": map[string]any{"health_check_interval": "sensitive-value"}},
		{"aws": map[string]any{"tags": map[string]any{"value_allowlist": []any{"Name", ""}}}},
		{"aws": map[string]any{"tags": map[string]any{"value_allowlist": []any{"sensitive-value,other"}}}},
	}
	for index, input := range cases {
		_, err := decodeConnectors(input)
		if err == nil || strings.Contains(err.Error(), "sensitive-value") {
			t.Fatalf("case %d: unsafe or missing rejection: %v", index, err)
		}
	}
}

func TestAWSConnectorEnableEnvironmentOverride(t *testing.T) {
	directory := t.TempDir()
	main := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(main, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "connectors.yaml"), []byte("connectors:\n  aws:\n    enable: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSUS_CONNECTORS_AWS_ENABLE", "true")
	loaded, err := loadConfigFromPath(main)
	if err != nil || !loaded.Connectors.AWS.Enable {
		t.Fatalf("enable override: %v", err)
	}
	t.Setenv("VERSUS_CONNECTORS_AWS_ENABLE", "false")
	loaded, err = loadConfigFromPath(main)
	if err != nil || loaded.Connectors.AWS.Enable {
		t.Fatalf("disable override: %v", err)
	}
}

func TestAWSCollectConfigShapeDefaultsAndClone(t *testing.T) {
	loaded, err := decodeConnectors(map[string]any{"aws": map[string]any{
		"collect": map[string]any{
			"enable":    "false",
			"intervals": map[string]any{"inventory": "30m", "health": "1m"},
			"history":   map[string]any{"max_generations": "10", "change_retention": "48h"},
		},
		"applications": map[string]any{"sources": []any{"tags", "appregistry"}, "tag_keys": []any{"app", "component"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	collect := loaded.AWS.Collect
	if collect.Enable || collect.InventoryInterval() != 30*time.Minute || collect.HealthInterval() != time.Minute || collect.ChangesInterval() != DefaultAWSChangesInterval {
		t.Fatalf("collect = %+v", collect)
	}
	if collect.MaxGenerations() != 10 || collect.MaxAge() != DefaultAWSHistoryMaxAge || collect.ChangeRetention() != 48*time.Hour {
		t.Fatalf("history = %+v", collect.History)
	}
	if !reflect.DeepEqual(loaded.AWS.Applications.EffectiveSources(), []string{"tags", "appregistry"}) || !reflect.DeepEqual(loaded.AWS.Applications.TagKeys, []string{"app", "component"}) {
		t.Fatalf("applications = %+v", loaded.AWS.Applications)
	}

	cloned := cloneConnectorsConfig(loaded)
	cloned.AWS.Applications.Sources[0] = "changed"
	cloned.AWS.Applications.TagKeys[0] = "changed"
	cloned.AWS.Collect.Intervals.Health = "9m"
	if loaded.AWS.Applications.Sources[0] != "tags" || loaded.AWS.Applications.TagKeys[0] != "app" || loaded.AWS.Collect.Intervals.Health != "1m" {
		t.Fatal("aws collect clone mutated source")
	}

	defaults, err := decodeConnectors(nil)
	if err != nil || !defaults.AWS.Collect.Enable {
		t.Fatalf("collection must default on: %+v, %v", defaults.AWS.Collect, err)
	}
	empty, _ := decodeConnectors(map[string]any{"aws": map[string]any{}})
	if !empty.AWS.Collect.Enable || empty.AWS.Collect.Intervals.Inventory != "15m0s" || !reflect.DeepEqual(empty.AWS.Applications.Sources, DefaultAWSApplicationSources()) {
		t.Fatalf("collect defaults = %+v / %+v", empty.AWS.Collect, empty.AWS.Applications)
	}
	var zero AWSCollectConfig
	if zero.InventoryInterval() != DefaultAWSInventoryInterval || zero.MaxGenerations() != DefaultAWSHistoryGenerations {
		t.Fatal("zero collect config has no defaults")
	}
}

func TestAWSCollectConfigRejections(t *testing.T) {
	cases := []map[string]any{
		{"collect": map[string]any{"intervals": map[string]any{"health": "5s"}}},
		{"collect": map[string]any{"intervals": map[string]any{"changes": "sensitive-value"}}},
		{"collect": map[string]any{"intervals": map[string]any{"inventory": "48h"}}},
		{"collect": map[string]any{"history": map[string]any{"max_generations": 20000}}},
		{"collect": map[string]any{"history": map[string]any{"max_age": "1m"}}},
		{"collect": map[string]any{"unknown": true}},
		{"applications": map[string]any{"sources": []any{"sensitive-value"}}},
		{"applications": map[string]any{"sources": []any{"tags", "tags"}}},
		{"applications": map[string]any{"tag_keys": []any{"a,b"}}},
		{"applications": map[string]any{"tag_keys": []any{""}}},
	}
	for index, aws := range cases {
		_, err := decodeConnectors(map[string]any{"aws": aws})
		if err == nil || strings.Contains(err.Error(), "sensitive-value") {
			t.Fatalf("case %d: unsafe or missing rejection: %v", index, err)
		}
	}
}

func TestAWSCollectEnvironmentOverrides(t *testing.T) {
	directory := t.TempDir()
	main := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(main, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "connectors.yaml"), []byte("connectors:\n  aws:\n    enable: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_ENABLE", "false")
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_HEALTH_INTERVAL", "90s")
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_HISTORY_MAX_GENERATIONS", "7")
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_HISTORY_MAX_AGE", "24h")
	loaded, err := loadConfigFromPath(main)
	if err != nil {
		t.Fatal(err)
	}
	collect := loaded.Connectors.AWS.Collect
	if collect.Enable || collect.HealthInterval() != 90*time.Second || collect.MaxGenerations() != 7 || collect.MaxAge() != 24*time.Hour {
		t.Fatalf("env overrides = %+v", collect)
	}
	t.Setenv("VERSUS_CONNECTORS_AWS_COLLECT_ENABLE", "true")
	if loaded, err = loadConfigFromPath(main); err != nil || !loaded.Connectors.AWS.Collect.Enable {
		t.Fatalf("enable override: %v", err)
	}
	for name, value := range map[string]string{
		"VERSUS_CONNECTORS_AWS_COLLECT_ENABLE":                  "sensitive-value",
		"VERSUS_CONNECTORS_AWS_COLLECT_HEALTH_INTERVAL":         "1s",
		"VERSUS_CONNECTORS_AWS_COLLECT_HISTORY_MAX_GENERATIONS": "-3",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			_, err := loadConfigFromPath(main)
			if err == nil || strings.Contains(err.Error(), "sensitive-value") {
				t.Fatalf("invalid override accepted or echoed: %v", err)
			}
		})
	}
}
