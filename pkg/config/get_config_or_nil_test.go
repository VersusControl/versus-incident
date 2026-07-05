package config

import "testing"

// TestGetConfigOrNil_UnloadedReturnsNilNoPanic proves the safe accessor never
// panics when config was never loaded — it returns nil so callers can degrade
// to "unconfigured" instead of crashing (contrast GetConfig, which panics).
func TestGetConfigOrNil_UnloadedReturnsNilNoPanic(t *testing.T) {
	prev := cfg
	t.Cleanup(func() { cfg = prev })

	cfg = nil
	if got := GetConfigOrNil(); got != nil {
		t.Fatalf("GetConfigOrNil() = %v, want nil when config is unloaded", got)
	}
}

// TestGetConfigOrNil_LoadedReturnsSamePointer proves that once config is loaded
// the safe accessor returns the very same *Config that GetConfig does, so a
// caller switching from GetConfig to GetConfigOrNil sees identical loaded state.
func TestGetConfigOrNil_LoadedReturnsSamePointer(t *testing.T) {
	prev := cfg
	t.Cleanup(func() { cfg = prev })

	cfg = &Config{Name: "loaded-test"}
	if got := GetConfigOrNil(); got != cfg {
		t.Fatalf("GetConfigOrNil() = %p, want same pointer as global cfg %p", got, cfg)
	}
	if got := GetConfigOrNil(); got != GetConfig() {
		t.Fatalf("GetConfigOrNil() and GetConfig() disagree when config is loaded")
	}
}
