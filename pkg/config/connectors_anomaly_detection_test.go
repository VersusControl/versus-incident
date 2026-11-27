package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeConfig writes config.yaml (and tools.yaml when given) into a fresh
// directory and returns the config path.
func writeConfig(t *testing.T, main, tools string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if tools != "" {
		if err := os.WriteFile(filepath.Join(directory, "tools.yaml"), []byte(tools), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

const singleClusterWithBlock = `connectors:
  kubernetes:
    endpoint: https://single.example
    anomaly_detection:
      enabled: true
      minPods: 3
      lanes:
        metrics: {enabled: true, window: 10m}
        events: [Warning, BackOff]
`

const multiClusterWithBlock = `connectors:
  kubernetes:
    multiple: true
    anomaly_detection:
      enabled: true
      lanes: {metrics: {enabled: true}}
    clusters:
      - cluster_id: prod
        endpoint: https://prod.example
        anomaly_detection:
          lanes: {metrics: {enabled: false}}
      - cluster_id: staging
        endpoint: https://staging.example
`

func TestAnomalyDetectionBlockLoadsInBothForms(t *testing.T) {
	single, err := loadConfigFromPath(writeConfig(t, singleClusterWithBlock, ""))
	if err != nil {
		t.Fatalf("single-cluster load: %v", err)
	}
	wantSingle := map[string]any{
		"enabled": true,
		"minPods": 3,
		"lanes": map[string]any{
			"metrics": map[string]any{"enabled": true, "window": "10m"},
			"events":  []any{"Warning", "BackOff"},
		},
	}
	if got := single.Connectors.Kubernetes.AnomalyDetection; !reflect.DeepEqual(got, wantSingle) {
		t.Fatalf("single-cluster block = %#v, want %#v", got, wantSingle)
	}
	blocks := single.Connectors.Kubernetes.AnomalyDetectionBlocks()
	if len(blocks) != 1 || blocks[0].ClusterID != DefaultKubernetesClusterID ||
		!reflect.DeepEqual(blocks[0].Connector, wantSingle) || blocks[0].Cluster != nil {
		t.Fatalf("single-cluster blocks = %#v", blocks)
	}

	multi, err := loadConfigFromPath(writeConfig(t, multiClusterWithBlock, ""))
	if err != nil {
		t.Fatalf("multi-cluster load: %v", err)
	}
	connectorBlock := map[string]any{"enabled": true, "lanes": map[string]any{"metrics": map[string]any{"enabled": true}}}
	prodBlock := map[string]any{"lanes": map[string]any{"metrics": map[string]any{"enabled": false}}}
	want := []KubernetesAnomalyDetectionBlocks{
		{ClusterID: "prod", Connector: connectorBlock, Cluster: prodBlock},
		{ClusterID: "staging", Connector: connectorBlock},
	}
	if got := multi.Connectors.Kubernetes.AnomalyDetectionBlocks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-cluster blocks = %#v, want %#v", got, want)
	}

	// The returned maps are copies: changing them leaves the config alone.
	got := multi.Connectors.Kubernetes.AnomalyDetectionBlocks()
	got[0].Connector["lanes"].(map[string]any)["metrics"].(map[string]any)["enabled"] = "changed"
	got[0].Cluster["lanes"] = nil
	if !reflect.DeepEqual(multi.Connectors.Kubernetes.AnomalyDetectionBlocks(), want) {
		t.Fatal("changing the returned blocks changed the loaded config")
	}
}

func TestAnomalyDetectionBlockSurvivesCloneWithoutAliasing(t *testing.T) {
	for name, content := range map[string]string{"single": singleClusterWithBlock, "multiple": multiClusterWithBlock} {
		t.Run(name, func(t *testing.T) {
			source, err := loadConfigFromPath(writeConfig(t, content, ""))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			before := source.Connectors.Kubernetes.AnomalyDetectionBlocks()
			cloned := cloneConfig(source)
			if !reflect.DeepEqual(cloned.Connectors, source.Connectors) {
				t.Fatalf("clone lost the block:\n%#v\n%#v", cloned.Connectors, source.Connectors)
			}
			cloned.Connectors.Kubernetes.AnomalyDetection["enabled"] = false
			cloned.Connectors.Kubernetes.AnomalyDetection["lanes"].(map[string]any)["metrics"].(map[string]any)["enabled"] = false
			for index := range cloned.Connectors.Kubernetes.Clusters {
				if block := cloned.Connectors.Kubernetes.Clusters[index].AnomalyDetection; block != nil {
					block["lanes"].(map[string]any)["metrics"].(map[string]any)["enabled"] = true
				}
			}
			if !reflect.DeepEqual(source.Connectors.Kubernetes.AnomalyDetectionBlocks(), before) {
				t.Fatal("changing the clone changed the source config")
			}
		})
	}
}

// A config without the block loads exactly as before: both fields stay nil,
// and adding the block changes no other setting.
func TestConfigWithoutAnomalyDetectionBlockIsUnchanged(t *testing.T) {
	plain := strings.Replace(multiClusterWithBlock, `    anomaly_detection:
      enabled: true
      lanes: {metrics: {enabled: true}}
`, "", 1)
	plain = strings.Replace(plain, `        anomaly_detection:
          lanes: {metrics: {enabled: false}}
`, "", 1)
	if strings.Contains(plain, "anomaly_detection") {
		t.Fatalf("fixture still has the block:\n%s", plain)
	}
	without, err := loadConfigFromPath(writeConfig(t, plain, ""))
	if err != nil {
		t.Fatalf("load without block: %v", err)
	}
	if without.Connectors.Kubernetes.AnomalyDetection != nil {
		t.Fatal("connector block is not nil")
	}
	for _, cluster := range without.Connectors.Kubernetes.Clusters {
		if cluster.AnomalyDetection != nil {
			t.Fatal("cluster block is not nil")
		}
	}
	if cloned := cloneConfig(without); !reflect.DeepEqual(cloned.Connectors, without.Connectors) {
		t.Fatal("clone of a config without the block differs")
	}

	with, err := loadConfigFromPath(writeConfig(t, multiClusterWithBlock, ""))
	if err != nil {
		t.Fatalf("load with block: %v", err)
	}
	with.Connectors.Kubernetes.AnomalyDetection = nil
	for index := range with.Connectors.Kubernetes.Clusters {
		with.Connectors.Kubernetes.Clusters[index].AnomalyDetection = nil
	}
	if !reflect.DeepEqual(with, without) {
		t.Fatal("the block changed another setting")
	}
}

func TestAnomalyDetectionNeighboursStayStrict(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    string
	}{
		{"unknown sibling key", "connectors:\n  kubernetes:\n    anomaly_detectionn: {enabled: true}\n",
			`connectors.kubernetes: unknown key "anomaly_detectionn"`},
		{"unknown sibling key on a cluster", "connectors:\n  kubernetes:\n    multiple: true\n    clusters:\n      - cluster_id: prod\n        anomaly_detection: {}\n        anomaly: {}\n",
			`connectors.kubernetes.clusters[0]: unknown key "anomaly"`},
		{"other connector-level key in multiple mode", "connectors:\n  kubernetes:\n    multiple: true\n    anomaly_detection: {}\n    endpoint: https://one.example\n    clusters:\n      - cluster_id: prod\n",
			"connectors.kubernetes: configure each cluster under clusters"},
		{"block that is not a mapping", "connectors:\n  kubernetes:\n    anomaly_detection: [enabled]\n",
			"connectors.kubernetes.anomaly_detection: invalid value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadConfigFromPath(writeConfig(t, test.content, ""))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
		})
	}
}

// The tools file has no anomaly-detection setting: the key under its
// kubernetes entry still stops the load.
func TestAnomalyDetectionKeyInToolsFileIsRejected(t *testing.T) {
	path := writeConfig(t, "connectors:\n  kubernetes:\n    endpoint: https://single.example\n",
		"tools:\n  kubernetes:\n    anomaly_detection:\n      enabled: true\n")
	if _, err := loadConfigFromPath(path); err == nil || !strings.Contains(err.Error(), "Kubernetes configuration must use connectors.kubernetes") {
		t.Fatalf("tools.yaml with anomaly_detection loaded: %v", err)
	}
	toolsPath := filepath.Join(filepath.Dir(path), "tools.yaml")
	if _, err := loadToolsFile(toolsPath); err == nil {
		t.Fatal("loadToolsFile accepted anomaly_detection under kubernetes")
	}
}

// Every mapping in the block, at any depth, must have text keys; the first
// offending key is named by its path.
func TestAnomalyDetectionNonTextKeysAreRejectedAtEveryLevel(t *testing.T) {
	for _, test := range []struct {
		name, block, want string
	}{
		{"top level", "{500: page}", "connectors.kubernetes.anomaly_detection: key 500 must be text; quote it"},
		{"nested", "{codes: {500: page, 404: ignore}}", "connectors.kubernetes.anomaly_detection.codes: key 404 must be text; quote it"},
		{"inside a list", "{rules: [{name: a}, {true: x}]}", "connectors.kubernetes.anomaly_detection.rules[1]: key true must be text; quote it"},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := "connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection: " + test.block + "\n"
			if _, err := loadConfigFromPath(writeConfig(t, content, "")); err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %s", err, test.want)
			}
		})
	}
	quoted := "connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection: {codes: {\"500\": page}}\n"
	if _, err := loadConfigFromPath(writeConfig(t, quoted, "")); err != nil {
		t.Fatalf("quoted keys rejected: %v", err)
	}
}

// The clone copies a mapping with non-text keys too, for every other caller
// of the generic copy.
func TestCloneAnyValueCopiesNonTextKeyMappings(t *testing.T) {
	source := map[string]any{"codes": map[any]any{500: "page", "list": []any{map[any]any{1: "x"}}}}
	cloned := cloneAnyMap(source)
	codes := cloned["codes"].(map[any]any)
	codes[500] = "changed"
	codes["list"].([]any)[0].(map[any]any)[1] = "changed"
	if got := source["codes"].(map[any]any); got[500] != "page" || got["list"].([]any)[0].(map[any]any)[1] != "x" {
		t.Fatalf("source changed through the clone: %#v", source)
	}
}
