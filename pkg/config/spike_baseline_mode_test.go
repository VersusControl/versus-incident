package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// catalogMapstructureTags returns the set of mapstructure tags declared on
// AgentCatalogConfig, so a test can assert on the exact config surface without
// depending on a live YAML load.
func catalogMapstructureTags(t *testing.T) map[string]struct{} {
	t.Helper()
	tags := map[string]struct{}{}
	rt := reflect.TypeOf(AgentCatalogConfig{})
	for i := 0; i < rt.NumField(); i++ {
		if tag := rt.Field(i).Tag.Get("mapstructure"); tag != "" {
			tags[tag] = struct{}{}
		}
	}
	return tags
}

// TestSpikeConfigKeysRemoved proves the retired spike_seasonal knob and the
// moved-to-storage spike_baseline_mode key are both gone from the catalog
// config surface, so no config can revive the old off/hour_of_day/hour_of_week
// granularity switch or set the baseline mode from YAML (it is now a
// storage-backed runtime setting).
func TestSpikeConfigKeysRemoved(t *testing.T) {
	tags := catalogMapstructureTags(t)
	if _, ok := tags["spike_seasonal"]; ok {
		t.Fatal("AgentCatalogConfig still declares the removed spike_seasonal key")
	}
	if _, ok := tags["spike_baseline_mode"]; ok {
		t.Fatal("AgentCatalogConfig still declares spike_baseline_mode — the baseline mode is now a storage-backed runtime setting, not a YAML key")
	}
}

// TestSpikeBaselineModeNotParsed proves a spike_baseline_mode key in a config
// no longer maps to any struct field (it is silently ignored, so an old config
// still loads without error) — the YAML surface no longer exposes it.
func TestSpikeBaselineModeNotParsed(t *testing.T) {
	v := viper.New()
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader(`
catalog:
  spike_baseline_mode: time_of_day
  spike_seasonal: hour_of_week
`)); err != nil {
		t.Fatalf("read yaml: %v", err)
	}
	var cat AgentCatalogConfig
	if err := v.UnmarshalKey("catalog", &cat); err != nil {
		t.Fatalf("unmarshal catalog: %v", err)
	}
	// The struct has no field for either legacy key, so the config still loads
	// and nothing is populated from them.
	if got := reflect.ValueOf(cat); got.IsZero() != true {
		// spike_z etc. are absent from this snippet, so the whole struct is the
		// zero value — proving neither removed key wrote a field.
		t.Fatalf("AgentCatalogConfig populated a field from a removed key: %+v", cat)
	}
}
