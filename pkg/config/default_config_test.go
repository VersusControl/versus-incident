package config

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/spf13/viper"
)

// parseRawYAML loads YAML into a viper instance WITHOUT env expansion, so
// ${VAR} placeholders are compared as literal strings.
func parseRawYAML(t *testing.T, data []byte) *viper.Viper {
	t.Helper()
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(bytes.NewReader(data)); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	return v
}

// TestDefaultConfigMatchesSample enforces B33: the embedded best-practice
// baseline (default_config.yaml) and the documented sample (config/config.yaml)
// must carry the same keys and the same values, so a change to one can never
// silently diverge from the other.
//
// The comparison is value-level (comments and formatting are ignored) and runs
// BEFORE env expansion, so ${VAR} placeholders compare literally.
func TestDefaultConfigMatchesSample(t *testing.T) {
	sampleRaw, err := os.ReadFile("../../config/config.yaml")
	if err != nil {
		t.Fatalf("read sample config: %v", err)
	}

	def := parseRawYAML(t, defaultConfigYAML)
	sample := parseRawYAML(t, sampleRaw)

	seen := map[string]struct{}{}
	for _, k := range def.AllKeys() {
		seen[k] = struct{}{}
	}
	for _, k := range sample.AllKeys() {
		seen[k] = struct{}{}
	}

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		inDef, inSample := def.IsSet(k), sample.IsSet(k)
		switch {
		case inDef && !inSample:
			t.Errorf("key %q is in default_config.yaml but missing from config/config.yaml — remove it or add it to the sample", k)
		case !inDef && inSample:
			t.Errorf("key %q is in config/config.yaml but missing from default_config.yaml — add it to the embedded default", k)
		default:
			if dv, sv := fmt.Sprint(def.Get(k)), fmt.Sprint(sample.Get(k)); dv != sv {
				t.Errorf("key %q diverged: default_config.yaml=%q vs config/config.yaml=%q", k, dv, sv)
			}
		}
	}
}
