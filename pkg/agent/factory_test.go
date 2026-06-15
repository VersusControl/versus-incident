package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/signalsources"
)

type fakeRegisteredSource struct{ name string }

func (s fakeRegisteredSource) Name() string { return s.name }
func (s fakeRegisteredSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return nil, time.Time{}, nil
}

// TestBuildSources_EnterpriseTypeRequiresRegistration asserts that a
// prometheus/traces source configured on a build where nothing registered
// the type fails with an actionable "requires Versus Enterprise" error
// rather than a generic unknown-type error.
func TestBuildSources_EnterpriseTypeRequiresRegistration(t *testing.T) {
	cfg := config.AgentConfig{
		Sources: []config.AgentSourceConfig{
			{Name: "metrics", Type: "prometheus", Enable: true},
		},
	}
	sources, errs := BuildSources(cfg)
	if len(sources) != 0 {
		t.Fatalf("expected no sources, got %d", len(sources))
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %v", errs)
	}
	if !strings.Contains(strings.ToLower(errs[0].Error()), "enterprise") {
		t.Errorf("error should mention enterprise, got %v", errs[0])
	}
}

// TestBuildSources_UnknownTypeStillReported asserts a genuine typo still
// yields an unknown-type error (not the enterprise message).
func TestBuildSources_UnknownTypeStillReported(t *testing.T) {
	cfg := config.AgentConfig{
		Sources: []config.AgentSourceConfig{
			{Name: "oops", Type: "promethues", Enable: true}, // typo
		},
	}
	_, errs := BuildSources(cfg)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "unknown type") {
		t.Fatalf("expected unknown-type error, got %v", errs)
	}
}

// TestBuildSources_RegisteredTypeResolves asserts the factory consults the
// registration hook for a type it does not build in itself, passing the
// generic Options block through to the registered Factory.
func TestBuildSources_RegisteredTypeResolves(t *testing.T) {
	const typeName = "test-build-source"
	var gotOpts map[string]any
	signalsources.Register(typeName, func(name string, options map[string]any) (core.SignalSource, error) {
		gotOpts = options
		return fakeRegisteredSource{name: name}, nil
	})

	cfg := config.AgentConfig{
		Sources: []config.AgentSourceConfig{
			{Name: "x", Type: typeName, Enable: true, Options: map[string]interface{}{"address": "http://x"}},
		},
	}
	sources, errs := BuildSources(cfg)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(sources) != 1 || sources[0].Name() != "x" {
		t.Fatalf("expected 1 resolved source, got %+v", sources)
	}
	if gotOpts["address"] != "http://x" {
		t.Errorf("Options not threaded to factory: %v", gotOpts)
	}
}
