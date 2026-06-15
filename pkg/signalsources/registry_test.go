package signalsources

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// stubSource is a minimal core.SignalSource for registry tests.
type stubSource struct{ name string }

func (s stubSource) Name() string { return s.name }
func (s stubSource) Pull(context.Context, time.Time) ([]core.Signal, time.Time, error) {
	return nil, time.Time{}, nil
}

func TestRegistry_RegisterAndLookup(t *testing.T) {
	const typeName = "test-source-registry"
	if _, ok := Lookup(typeName); ok {
		t.Fatalf("type %q already registered before test", typeName)
	}

	var gotName string
	var gotOpts map[string]any
	Register(typeName, func(name string, options map[string]any) (core.SignalSource, error) {
		gotName = name
		gotOpts = options
		return stubSource{name: name}, nil
	})

	f, ok := Lookup(typeName)
	if !ok {
		t.Fatalf("Lookup did not find registered type %q", typeName)
	}
	src, err := f("inst", map[string]any{"address": "http://x"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if src.Name() != "inst" {
		t.Errorf("source name = %q", src.Name())
	}
	if gotName != "inst" || gotOpts["address"] != "http://x" {
		t.Errorf("factory received name=%q opts=%v", gotName, gotOpts)
	}

	found := false
	for _, n := range Registered() {
		if n == typeName {
			found = true
		}
	}
	if !found {
		t.Errorf("Registered() missing %q: %v", typeName, Registered())
	}
}

func TestRegistry_RequiresEnterprise(t *testing.T) {
	for _, tn := range []string{"prometheus", "traces"} {
		if !RequiresEnterprise(tn) {
			t.Errorf("RequiresEnterprise(%q) = false, want true", tn)
		}
	}
	if RequiresEnterprise("file") {
		t.Error("RequiresEnterprise(\"file\") = true, want false")
	}

	err := ErrRequiresEnterprise("prometheus")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "enterprise") {
		t.Errorf("ErrRequiresEnterprise should mention enterprise, got %v", err)
	}
}

func TestRegistry_RegisterPanics(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		fn()
	}
	assertPanics("empty type", func() { Register("", func(string, map[string]any) (core.SignalSource, error) { return nil, nil }) })
	assertPanics("nil factory", func() { Register("x", nil) })

	const dup = "test-source-dup"
	Register(dup, func(string, map[string]any) (core.SignalSource, error) { return nil, nil })
	assertPanics("duplicate", func() {
		Register(dup, func(string, map[string]any) (core.SignalSource, error) { return nil, nil })
	})
}
