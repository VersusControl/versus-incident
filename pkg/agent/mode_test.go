package agent

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
)

// stubModeResolver returns a sequence of (mode, ok) results, one per call,
// so a test can prove the worker re-reads the resolver every tick. Once the
// sequence is exhausted the last entry repeats.
type stubModeResolver struct {
	results []stubModeResult
	calls   int
}

type stubModeResult struct {
	mode string
	ok   bool
}

func (s *stubModeResolver) Mode(_ context.Context, _ string) (string, bool) {
	i := s.calls
	if i >= len(s.results) {
		i = len(s.results) - 1
	}
	s.calls++
	r := s.results[i]
	return r.mode, r.ok
}

func newModeWorker(yaml string) *Worker {
	return &Worker{cfg: config.AgentConfig{Mode: yaml}}
}

// TestEffectiveMode_NoResolver_OSSUnchanged proves that with no resolver
// registered the effective mode equals the YAML mode on every tick — the
// byte-for-byte OSS path.
func TestEffectiveMode_NoResolver_OSSUnchanged(t *testing.T) {
	SetModeResolver(nil)
	t.Cleanup(func() { SetModeResolver(nil) })

	cases := map[string]string{
		"training": "training",
		"shadow":   "shadow",
		"detect":   "detect",
		"":         "training", // empty YAML defaults to training
	}
	for yaml, want := range cases {
		w := newModeWorker(yaml)
		for tick := 0; tick < 3; tick++ {
			if got := w.effectiveMode(context.Background()); got != want {
				t.Fatalf("yaml=%q tick=%d: effectiveMode=%q want %q", yaml, tick, got, want)
			}
		}
	}
}

// TestEffectiveMode_HotSwitch proves a registered resolver whose answer
// changes between ticks flips the worker's effective mode on the next tick
// with no restart.
func TestEffectiveMode_HotSwitch(t *testing.T) {
	w := newModeWorker("training")

	res := &stubModeResolver{results: []stubModeResult{
		{"training", true},
		{"shadow", true},
		{"detect", true},
	}}
	SetModeResolver(res)
	t.Cleanup(func() { SetModeResolver(nil) })

	want := []string{"training", "shadow", "detect", "detect"}
	for tick, w2 := range want {
		if got := w.effectiveMode(context.Background()); got != w2 {
			t.Fatalf("tick=%d: effectiveMode=%q want %q", tick, got, w2)
		}
	}
}

// TestEffectiveMode_FailClosed proves the resolver fails closed to the YAML
// floor for both ok=false ("no opinion") and an invalid mode string.
func TestEffectiveMode_FailClosed(t *testing.T) {
	t.Cleanup(func() { SetModeResolver(nil) })

	t.Run("ok=false keeps YAML", func(t *testing.T) {
		w := newModeWorker("detect")
		SetModeResolver(&stubModeResolver{results: []stubModeResult{{"shadow", false}}})
		if got := w.effectiveMode(context.Background()); got != "detect" {
			t.Fatalf("effectiveMode=%q want detect (ok=false ignored)", got)
		}
	})

	t.Run("invalid mode keeps YAML", func(t *testing.T) {
		w := newModeWorker("detect")
		SetModeResolver(&stubModeResolver{results: []stubModeResult{{"bogus", true}}})
		if got := w.effectiveMode(context.Background()); got != "detect" {
			t.Fatalf("effectiveMode=%q want detect (invalid mode ignored)", got)
		}
	})
}

// stubOrgModeResolver implements both ModeResolver.Mode (unused here) and the
// optional Get(org) capability EffectiveModeForOrg reads.
type stubOrgModeResolver struct {
	byOrg map[string]string // org -> override mode; absent => ok=false
}

func (s *stubOrgModeResolver) Mode(context.Context, string) (string, bool) {
	return "", false
}

func (s *stubOrgModeResolver) Get(org string) (string, bool) {
	m, ok := s.byOrg[org]
	return m, ok
}

// TestEffectiveModeForOrg_NoResolver proves the read-only display helper
// returns the YAML floor unchanged when no resolver is registered (OSS).
func TestEffectiveModeForOrg_NoResolver(t *testing.T) {
	SetModeResolver(nil)
	t.Cleanup(func() { SetModeResolver(nil) })
	if got := EffectiveModeForOrg("default", "training"); got != "training" {
		t.Fatalf("no resolver = %q, want training (YAML floor)", got)
	}
}

// TestEffectiveModeForOrg_OverrideAndFallback proves the helper reports the
// per-org override when present and valid, and falls back to the YAML floor
// for an absent override or an invalid stored value.
func TestEffectiveModeForOrg_OverrideAndFallback(t *testing.T) {
	res := &stubOrgModeResolver{byOrg: map[string]string{
		"acme":    "detect",
		"globex":  "shadow",
		"corrupt": "bogus", // invalid -> ignored, YAML floor
	}}
	SetModeResolver(res)
	t.Cleanup(func() { SetModeResolver(nil) })

	if got := EffectiveModeForOrg("acme", "training"); got != "detect" {
		t.Fatalf("acme override = %q, want detect", got)
	}
	if got := EffectiveModeForOrg("globex", "training"); got != "shadow" {
		t.Fatalf("globex override = %q, want shadow", got)
	}
	if got := EffectiveModeForOrg("corrupt", "training"); got != "training" {
		t.Fatalf("corrupt override = %q, want training (invalid ignored)", got)
	}
	if got := EffectiveModeForOrg("default", "training"); got != "training" {
		t.Fatalf("no override = %q, want training (YAML floor)", got)
	}
}

func TestIsValidMode(t *testing.T) {
	valid := []string{"training", "shadow", "detect"}
	for _, m := range valid {
		if !isValidMode(m) {
			t.Errorf("isValidMode(%q)=false want true", m)
		}
	}
	invalid := []string{"", "TRAINING", "bogus", "Detect", "off"}
	for _, m := range invalid {
		if isValidMode(m) {
			t.Errorf("isValidMode(%q)=true want false", m)
		}
	}
}

// TestSetModeResolver_LastWins proves the single slot is last-wins and that
// nil clears it back to community mode.
func TestSetModeResolver_LastWins(t *testing.T) {
	t.Cleanup(func() { SetModeResolver(nil) })

	if modeResolver() != nil {
		t.Fatal("expected nil resolver before registration")
	}
	first := &stubModeResolver{results: []stubModeResult{{"shadow", true}}}
	second := &stubModeResolver{results: []stubModeResult{{"detect", true}}}
	SetModeResolver(first)
	SetModeResolver(second)
	if modeResolver() != second {
		t.Fatal("expected the second resolver to win")
	}
	SetModeResolver(nil)
	if modeResolver() != nil {
		t.Fatal("expected nil after clearing the slot")
	}
}
