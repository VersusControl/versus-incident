package agent

import (
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func newBuildCatalog(t *testing.T) (*Catalog, storage.Provider) {
	t.Helper()
	store := storage.NewMemory()
	cat, err := LoadCatalog(store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	return cat, store
}

// TestBuildAIs_ResolverConstructsIdleBundle proves item-3 constructibility:
// an off-at-boot binary (cfg.AI.Enable=false) with a model configured AND a
// registered AISettingsResolver still builds the bundle, so the runtime
// enable flag has an idle Detect agent to switch on.
func TestBuildAIs_ResolverConstructsIdleBundle(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{
			Enable: false,
			Model:  "gpt-4o-mini",
		},
	}

	SetAISettingsResolver(&stubAISettings{})
	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect == nil {
		t.Fatal("Detect = nil; want a constructed (idle) detect agent when a resolver is registered + model configured")
	}
}

// TestBuildAIs_NoResolver_ZeroBundle proves the OSS path is unchanged: with
// no resolver and Enable=false the bundle is zero, exactly as before.
func TestBuildAIs_NoResolver_ZeroBundle(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{Enable: false, Model: "gpt-4o-mini"},
	}

	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect != nil || bundle.Router != nil {
		t.Fatalf("want zero bundle in OSS (Enable=false, no resolver), got Detect=%v Router=%v", bundle.Detect, bundle.Router)
	}
}

// TestBuildAIs_ResolverButNoModel_ZeroBundle proves we never build a
// nil-key client: a resolver is registered but no model is configured, so
// the bundle stays zero rather than constructing a client that only errors
// at call time.
func TestBuildAIs_ResolverButNoModel_ZeroBundle(t *testing.T) {
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{Enable: false}, // Model empty
	}

	SetAISettingsResolver(&stubAISettings{})
	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect != nil {
		t.Fatal("Detect != nil; want zero bundle when no model is configured (avoid nil-key client)")
	}
}

// TestBuildAIs_EnabledConstructs proves the pre-seam happy path is intact:
// Enable=true + model configured builds the detect agent regardless of any
// resolver.
func TestBuildAIs_EnabledConstructs(t *testing.T) {
	SetAISettingsResolver(nil)
	t.Cleanup(func() { SetAISettingsResolver(nil) })
	cat, store := newBuildCatalog(t)

	cfg := config.AgentConfig{
		AI: config.AgentAIConfig{Enable: true, Model: "gpt-4o-mini"},
	}

	bundle := BuildAIs(cfg, cat, store, nil)
	if bundle.Detect == nil {
		t.Fatal("Detect = nil; want a detect agent when AI is enabled at boot")
	}
}
