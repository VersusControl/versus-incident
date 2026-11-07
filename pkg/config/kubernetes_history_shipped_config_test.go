package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shipped files carry the 7-day retention and the 500-record save part
// size literally, so an operator reading either file sees the real default.
func TestShippedConfigFilesCarryHistoryDefaults(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "config", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"config/config.yaml": shipped, "pkg/config/default_config.yaml": defaultConfigYAML} {
		var doc struct {
			Kubernetes struct {
				Evidence map[string]any `yaml:"evidence"`
			} `yaml:"kubernetes"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := doc.Kubernetes.Evidence["retention"]; got != "168h" {
			t.Errorf("%s: kubernetes.evidence.retention = %#v, want \"168h\"", name, got)
		}
		if got := doc.Kubernetes.Evidence["save_chunk"]; got != 500 {
			t.Errorf("%s: kubernetes.evidence.save_chunk = %#v, want 500", name, got)
		}
	}
}

// Loading the shipped config.yaml through the real pipeline yields the
// defaults, the environment overrides the file, and no environment value can
// fail the load: an unreadable or out-of-range one falls back to 500.
func TestShippedConfigHistorySaveChunkResolution(t *testing.T) {
	shipped := filepath.Join("..", "..", "config", "config.yaml")
	for _, test := range []struct {
		name, env string
		want      int
	}{
		{"shipped file, no environment", "", 500},
		{"environment overrides the file", "64", 64},
		{"upper bound", "1024", 1024},
		{"lower bound", "1", 1},
		{"surrounding spaces are trimmed", " 600 ", 600},
		{"zero falls back", "0", 500},
		{"negative falls back", "-1", 500},
		{"just above the range falls back", "1025", 500},
		{"not a number falls back", "many", 500},
		{"fractional falls back", "1.5", 500},
		{"exponent falls back", "1e3", 500},
		{"overflow falls back", "99999999999999999999", 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_EVIDENCE_SAVE_CHUNK", test.env)
			t.Setenv("KUBERNETES_EVIDENCE_RETENTION", "")
			loaded, err := loadConfigFromPath(shipped)
			if err != nil {
				t.Fatalf("load shipped config with KUBERNETES_EVIDENCE_SAVE_CHUNK=%q: %v", test.env, err)
			}
			if loaded.Kubernetes.Evidence.SaveChunk != test.want || loaded.Kubernetes.Evidence.Retention != "168h" {
				t.Fatalf("save_chunk=%d retention=%q, want %d and 168h", loaded.Kubernetes.Evidence.SaveChunk, loaded.Kubernetes.Evidence.Retention, test.want)
			}
		})
	}
}

// A file value that is not a number, or is out of range, never fails the
// load either; a value set through ${VAR} expansion in the file is honoured.
func TestHistorySaveChunkFileEdgeValues(t *testing.T) {
	t.Setenv("KUBERNETES_EVIDENCE_SAVE_CHUNK", "")
	t.Setenv("K31_QA_SAVE_CHUNK", "128")
	for _, test := range []struct {
		name, value string
		want        int
	}{
		{"expanded from the environment", "${K31_QA_SAVE_CHUNK}", 128},
		{"empty string", `""`, 500},
		{"quoted number", `"256"`, 256},
		{"list", "[1, 2]", 500},
		{"null", "null", 500},
		{"exact upper bound", "1024", 1024},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := loadKubernetesHistoryConfig(t, "kubernetes:\n  evidence:\n    save_chunk: "+test.value+"\n").SaveChunk; got != test.want {
				t.Fatalf("save_chunk %s = %d, want %d", test.value, got, test.want)
			}
		})
	}
}
