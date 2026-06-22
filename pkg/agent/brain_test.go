package agent

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// fakeBrain is a trivial Learner+Detector used to exercise the registry and the
// worker's brain-selection path without the log brain's catalog machinery.
type fakeBrain struct {
	kind     string
	grouped  []core.Observation
	verdict  core.TypedVerdict
	learned  int
	received []core.Signal // every signal handed to Group, in order
}

func (b *fakeBrain) Kind() string { return b.kind }

func (b *fakeBrain) Group(_ context.Context, sigs []core.Signal) ([]core.Observation, error) {
	b.received = append(b.received, sigs...)
	return b.grouped, nil
}

func (b *fakeBrain) Expected(context.Context, string, time.Time) (float64, float64, bool) {
	return 0, 0, true
}

func (b *fakeBrain) Learn(_ context.Context, obs []core.Observation) error {
	b.learned += len(obs)
	return nil
}

func (b *fakeBrain) Classify(core.Observation, float64, float64, bool) core.TypedVerdict {
	return b.verdict
}

func TestRegisterTypedBrain_LookupRoundTrip(t *testing.T) {
	const typ = "test-brain-roundtrip"
	if _, ok := lookupTypedBrain(typ); ok {
		t.Fatalf("type %q already registered before test", typ)
	}

	fb := &fakeBrain{kind: "metrics"}
	RegisterTypedBrain(typ, func(name string, opts map[string]any) (core.SignalLearner, core.SignalDetector, error) {
		if name != "inst" {
			t.Errorf("factory name = %q, want inst", name)
		}
		if opts["k"] != "v" {
			t.Errorf("factory opts = %v, want k=v", opts)
		}
		return fb, fb, nil
	})

	f, ok := lookupTypedBrain(typ)
	if !ok {
		t.Fatalf("type %q not found after register", typ)
	}
	l, d, err := f("inst", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if l.Kind() != "metrics" || d.Kind() != "metrics" {
		t.Fatalf("kind = (%q,%q), want metrics", l.Kind(), d.Kind())
	}

	found := false
	for _, k := range RegisteredTypedBrains() {
		if k == typ {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q missing from RegisteredTypedBrains() = %v", typ, RegisteredTypedBrains())
	}
}

func TestRegisterTypedBrain_PanicsOnDuplicate(t *testing.T) {
	const typ = "test-brain-dup"
	RegisterTypedBrain(typ, func(string, map[string]any) (core.SignalLearner, core.SignalDetector, error) {
		return nil, nil, nil
	})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	RegisterTypedBrain(typ, func(string, map[string]any) (core.SignalLearner, core.SignalDetector, error) {
		return nil, nil, nil
	})
}

func TestRegisterTypedBrain_PanicsOnNilFactory(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on nil factory")
		}
	}()
	RegisterTypedBrain("test-brain-nil", nil)
}

func TestRegisterTypedBrain_PanicsOnEmptyType(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty type")
		}
	}()
	RegisterTypedBrain("", func(string, map[string]any) (core.SignalLearner, core.SignalDetector, error) {
		return nil, nil, nil
	})
}
