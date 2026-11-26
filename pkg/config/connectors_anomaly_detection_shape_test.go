package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// A block that is not a mapping stops the load, and the error names the key
// at the level where it was written.
func TestAnomalyDetectionBlockMustBeAMapping(t *testing.T) {
	values := map[string]string{
		"list":           "[enabled, true]",
		"string":         "enabled",
		"quoted string":  `"{enabled: true}"`,
		"number":         "3",
		"bool":           "true",
		"null":           "null",
		"empty value":    "",
		"block sequence": "\n      - enabled",
	}
	for name, value := range values {
		t.Run("connector level "+name, func(t *testing.T) {
			content := "connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection: " + value + "\n"
			_, err := loadConfigFromPath(writeConfig(t, content, ""))
			if err == nil || err.Error() != "connectors.kubernetes.anomaly_detection: invalid value" {
				t.Fatalf("error = %v, want the connector-level key named", err)
			}
		})
		t.Run("cluster level "+name, func(t *testing.T) {
			indented := strings.ReplaceAll(value, "\n      ", "\n          ")
			content := "connectors:\n  kubernetes:\n    multiple: true\n    clusters:\n      - cluster_id: prod\n        endpoint: https://prod.example\n        anomaly_detection: " + indented + "\n"
			_, err := loadConfigFromPath(writeConfig(t, content, ""))
			if err == nil || err.Error() != "connectors.kubernetes.clusters[0].anomaly_detection: invalid value" {
				t.Fatalf("error = %v, want the cluster-level key named", err)
			}
		})
	}
}

// An empty mapping is kept as an empty block, distinct from an absent one, so
// an extension can tell "configured with defaults" from "not configured".
func TestAnomalyDetectionEmptyMappingIsKeptDistinctFromAbsent(t *testing.T) {
	content := "connectors:\n  kubernetes:\n    multiple: true\n    anomaly_detection: {}\n    clusters:\n      - cluster_id: prod\n        endpoint: https://prod.example\n        anomaly_detection: {}\n      - cluster_id: staging\n        endpoint: https://staging.example\n"
	loaded, err := loadConfigFromPath(writeConfig(t, content, ""))
	if err != nil {
		t.Fatal(err)
	}
	blocks := loaded.Connectors.Kubernetes.AnomalyDetectionBlocks()
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if blocks[0].Connector == nil || len(blocks[0].Connector) != 0 || blocks[0].Cluster == nil || len(blocks[0].Cluster) != 0 {
		t.Fatalf("prod blocks = %#v, want two empty, non-nil maps", blocks[0])
	}
	if blocks[1].Connector == nil || blocks[1].Cluster != nil {
		t.Fatalf("staging blocks = %#v, want the connector default and no cluster block", blocks[1])
	}
}

// deepBlockYAML is a block nested depth levels deep, alternating mappings and
// lists of mappings. Every level also holds a mapping keyed by quoted HTTP
// status codes, the accepted way to write numeric keys.
func deepBlockYAML(depth int) string {
	var b strings.Builder
	b.WriteString("connectors:\n  kubernetes:\n    multiple: true\n    anomaly_detection:\n")
	indent := "      "
	for level := range depth {
		fmt.Fprintf(&b, "%sname%d: value%d\n", indent, level, level)
		fmt.Fprintf(&b, "%scodes%d: {\"%d\": page, \"404\": ignore}\n", indent, level, 500+level)
		fmt.Fprintf(&b, "%slist%d: [a, {inner: x%d}]\n", indent, level, level)
		fmt.Fprintf(&b, "%snext:\n", indent)
		indent += "  "
	}
	fmt.Fprintf(&b, "%sleaf: true\n", indent)
	b.WriteString("    clusters:\n      - cluster_id: prod\n        endpoint: https://prod.example\n")
	return b.String()
}

// mutateEveryLevel walks the block and changes every map and list it finds,
// at every depth, so any shared reference shows up in the source.
func mutateEveryLevel(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if typed == nil {
			return
		}
		for key, nested := range typed {
			mutateEveryLevel(nested)
			if _, scalar := nested.(string); scalar {
				typed[key] = "changed"
			}
		}
		typed["added"] = "changed"
	case map[any]any:
		if typed == nil {
			return
		}
		for key, nested := range typed {
			mutateEveryLevel(nested)
			if _, scalar := nested.(string); scalar {
				typed[key] = "changed"
			}
		}
		typed["added"] = "changed"
	case []any:
		for index, nested := range typed {
			mutateEveryLevel(nested)
			if _, scalar := nested.(string); scalar {
				typed[index] = "changed"
			}
		}
	}
}

func TestAnomalyDetectionDeepBlockClonesWithoutAliasingAtAnyLevel(t *testing.T) {
	const depth = 12
	source, err := loadConfigFromPath(writeConfig(t, deepBlockYAML(depth), ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	block := source.Connectors.Kubernetes.AnomalyDetection
	// The deepest leaf is reachable, so the whole block survived the load.
	cursor := block
	for level := range depth {
		next, ok := cursor["next"].(map[string]any)
		if !ok {
			t.Fatalf("level %d: next = %T %#v", level, cursor["next"], cursor["next"])
		}
		if codes, ok := cursor[fmt.Sprintf("codes%d", level)].(map[string]any); !ok || codes["404"] != "ignore" {
			t.Fatalf("level %d: codes = %#v, want a text-keyed mapping", level, cursor[fmt.Sprintf("codes%d", level)])
		}
		cursor = next
	}
	if cursor["leaf"] != true {
		t.Fatalf("leaf = %#v", cursor)
	}
	before := fmt.Sprintf("%#v", source.Connectors.Kubernetes.AnomalyDetectionBlocks())
	beforeBlock := fmt.Sprintf("%#v", block)

	cloned := cloneConfig(source)
	if !reflect.DeepEqual(cloned.Connectors.Kubernetes.AnomalyDetection, block) {
		t.Fatal("clone lost part of the deep block")
	}
	mutateEveryLevel(cloned.Connectors.Kubernetes.AnomalyDetection)
	for _, entry := range source.Connectors.Kubernetes.AnomalyDetectionBlocks() {
		mutateEveryLevel(entry.Connector)
		mutateEveryLevel(entry.Cluster)
	}
	if fmt.Sprintf("%#v", source.Connectors.Kubernetes.AnomalyDetection) != beforeBlock {
		t.Fatal("changing the clone or the returned blocks changed the source block")
	}
	if after := fmt.Sprintf("%#v", source.Connectors.Kubernetes.AnomalyDetectionBlocks()); after != before {
		t.Fatal("changing a copy changed what AnomalyDetectionBlocks returns")
	}
}

// A large flat block clones in full.
func TestAnomalyDetectionLargeBlockClonesInFull(t *testing.T) {
	var b strings.Builder
	b.WriteString("connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection:\n")
	const keys = 5000
	for index := range keys {
		fmt.Fprintf(&b, "      key%d: {threshold: %d, tags: [t%d]}\n", index, index, index)
	}
	source, err := loadConfigFromPath(writeConfig(t, b.String(), ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := len(source.Connectors.Kubernetes.AnomalyDetection); got != keys {
		t.Fatalf("keys = %d, want %d", got, keys)
	}
	cloned := cloneConfig(source)
	mutateEveryLevel(cloned.Connectors.Kubernetes.AnomalyDetection)
	entry, _ := source.Connectors.Kubernetes.AnomalyDetection["key4999"].(map[string]any)
	if tags, _ := entry["tags"].([]any); len(source.Connectors.Kubernetes.AnomalyDetection) != keys || len(tags) != 1 || tags[0] != "t4999" {
		t.Fatalf("source changed through the clone: %#v", entry)
	}
}

// A numeric key written bare decodes as a non-text key; it is rejected at
// load with the path that holds it, at any depth and at either level.
func TestAnomalyDetectionBareNumericKeyIsRejectedWithItsPath(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"nested mapping", "connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection:\n      codes: {500: page, 404: ignore}\n",
			"connectors.kubernetes.anomaly_detection.codes: key 404 must be text; quote it"},
		{"top of the block", "connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection:\n      500: page\n",
			"connectors.kubernetes.anomaly_detection: key 500 must be text; quote it"},
		{"inside a list", "connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection:\n      rules: [{name: a}, {true: page}]\n",
			"connectors.kubernetes.anomaly_detection.rules[1]: key true must be text; quote it"},
		{"cluster block", "connectors:\n  kubernetes:\n    multiple: true\n    clusters:\n      - cluster_id: prod\n        endpoint: https://prod.example\n        anomaly_detection:\n          lanes: {metrics: {codes: {503: page}}}\n",
			"connectors.kubernetes.clusters[0].anomaly_detection.lanes.metrics.codes: key 503 must be text; quote it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadConfigFromPath(writeConfig(t, tc.content, ""))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %s", err, tc.want)
			}
		})
	}
}

// Quoted numeric keys load as text and are copied: a change to a clone or to
// a block AnomalyDetectionBlocks returned leaves the loaded config alone.
func TestAnomalyDetectionQuotedNumericKeyMappingIsCopied(t *testing.T) {
	content := "connectors:\n  kubernetes:\n    endpoint: https://single.example\n    anomaly_detection:\n      codes: {\"500\": page, \"404\": ignore}\n"
	source, err := loadConfigFromPath(writeConfig(t, content, ""))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	codes := func(block map[string]any) map[string]any {
		typed, ok := block["codes"].(map[string]any)
		if !ok {
			t.Fatalf("codes = %T, want map[string]any", block["codes"])
		}
		return typed
	}
	codes(cloneConfig(source).Connectors.Kubernetes.AnomalyDetection)["500"] = "changed by the clone"
	codes(source.Connectors.Kubernetes.AnomalyDetectionBlocks()[0].Connector)["404"] = "changed by a caller"
	if got := codes(source.Connectors.Kubernetes.AnomalyDetection); got["500"] != "page" || got["404"] != "ignore" || len(got) != 2 {
		t.Fatalf("source block changed through a copy: %#v", got)
	}
}
