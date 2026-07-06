package agent

import (
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// TestLogReadiness_DriftAgainstClassify is the anti-drift guard the design
// requires. It drives a log pattern to a range of states across
// thresholds {below-default, at-default, below-custom, at-custom, disabled(0),
// disabled(negative), already-known} and asserts THREE views of "known" all
// agree for every row:
//
//  1. isLogKnown(...)            — the single extracted predicate
//  2. LogReadiness(...).Ready    — the read-side readiness view
//  3. Classify(...)              — the classifier's VerdictKnownPattern decision
//
// Spike is disabled everywhere (SpikeMultiplier == 0) so Classify's ONLY
// "known" signal is VerdictKnownPattern — a spike would otherwise mask the
// count decision under test. If any of the three ever diverge, the promotion
// rule has drifted between the classifier and the readiness view.
func TestLogReadiness_DriftAgainstClassify(t *testing.T) {
	cases := []struct {
		name      string
		threshold int
		// build drives the brain/catalog to the state under test and returns
		// the LAST Classify verdict observed.
		build func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict
	}{
		{
			name:      "uncurated below default threshold",
			threshold: 100,
			build: func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict {
				var v core.TypedVerdict
				for i := 0; i < 50; i++ {
					v = classifyOnce(t, b, logObs("p", 1))
				}
				return v
			},
		},
		{
			name:      "uncurated reaches default threshold",
			threshold: 100,
			build: func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict {
				var v core.TypedVerdict
				for i := 0; i < 100; i++ {
					v = classifyOnce(t, b, logObs("p", 1))
				}
				return v
			},
		},
		{
			name:      "uncurated below custom threshold",
			threshold: 50,
			build: func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict {
				var v core.TypedVerdict
				for i := 0; i < 30; i++ {
					v = classifyOnce(t, b, logObs("p", 1))
				}
				return v
			},
		},
		{
			name:      "uncurated reaches custom threshold",
			threshold: 50,
			build: func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict {
				var v core.TypedVerdict
				for i := 0; i < 50; i++ {
					v = classifyOnce(t, b, logObs("p", 1))
				}
				return v
			},
		},
		{
			name:      "auto-promote disabled (0), high count never known",
			threshold: 0,
			build: func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict {
				var v core.TypedVerdict
				for i := 0; i < 120; i++ {
					v = classifyOnce(t, b, logObs("p", 1))
				}
				return v
			},
		},
		{
			name:      "auto-promote disabled (negative), high count never known",
			threshold: -1,
			build: func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict {
				var v core.TypedVerdict
				for i := 0; i < 120; i++ {
					v = classifyOnce(t, b, logObs("p", 1))
				}
				return v
			},
		},
		{
			name:      "operator-marked known stays known even with promotion disabled",
			threshold: 0,
			build: func(t *testing.T, b *logBrain, c *Catalog) core.TypedVerdict {
				// Create the pattern, hand-mark it known, then re-classify: the
				// prevVerdict=="known" clause must win independently of the
				// (disabled) count clause.
				classifyOnce(t, b, logObs("p", 1))
				if !c.MarkKnown("p") {
					t.Fatalf("MarkKnown(p) returned false")
				}
				return classifyOnce(t, b, logObs("p", 1))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, c := newLogBrainForTest(t, config.AgentCatalogConfig{
				AutoPromoteAfter: tc.threshold,
				SpikeMultiplier:  0, // disable spike so it can't mask the known decision
			})
			v := tc.build(t, b, c)

			p := c.Get("p")
			if p == nil {
				t.Fatalf("pattern p missing after build")
			}

			classifyKnown := v.Class == core.VerdictKnownPattern
			predicate := isLogKnown(p.Verdict, p.Count, tc.threshold)
			r := LogReadiness(p, tc.threshold, 30*time.Second)

			if predicate != classifyKnown {
				t.Errorf("isLogKnown=%v but Classify known=%v (verdict=%v) — predicate drifted from classifier",
					predicate, classifyKnown, v.Class)
			}
			if r.Ready != classifyKnown {
				t.Errorf("LogReadiness.Ready=%v but Classify known=%v (verdict=%v) — readiness drifted from classifier",
					r.Ready, classifyKnown, v.Class)
			}
			if r.Ready != predicate {
				t.Errorf("LogReadiness.Ready=%v but isLogKnown=%v — readiness drifted from predicate",
					r.Ready, predicate)
			}
		})
	}
}

// TestLogReadiness_EdgeCases pins the sentinel behaviour the design's §6 locked
// edge-case table requires, plus the exact per-minute rate derivation.
func TestLogReadiness_EdgeCases(t *testing.T) {
	poll := 30 * time.Second // 0.5 min

	t.Run("nil pattern yields zero readiness", func(t *testing.T) {
		r := LogReadiness(nil, 100, poll)
		if r != (core.Readiness{}) {
			t.Fatalf("LogReadiness(nil) = %+v, want zero value", r)
		}
	})

	t.Run("Needed carries a positive threshold", func(t *testing.T) {
		p := &Pattern{Count: 40, BaselineFrequency: 0}
		r := LogReadiness(p, 100, poll)
		if r.Needed != 100 {
			t.Errorf("Needed = %d, want 100 (the count gate)", r.Needed)
		}
		if r.Seen != 40 {
			t.Errorf("Seen = %d, want 40 (the pattern count)", r.Seen)
		}
		if r.Ready {
			t.Errorf("Ready = true, want false (40 < 100, not marked)")
		}
	})

	t.Run("AutoPromoteAfter<=0 → Needed=0 indeterminate, not ready", func(t *testing.T) {
		for _, threshold := range []int{0, -1, -100} {
			p := &Pattern{Count: 500, BaselineFrequency: 0} // huge count, still not known
			r := LogReadiness(p, threshold, poll)
			if r.Needed != 0 {
				t.Errorf("threshold %d: Needed = %d, want 0 (indeterminate sentinel — manual-only)", threshold, r.Needed)
			}
			if r.Ready {
				t.Errorf("threshold %d: Ready = true, want false (count promotion disabled, verdict empty)", threshold)
			}
			if r.RatePerMin != 0 {
				t.Errorf("threshold %d: RatePerMin = %v, want 0 (no baseline frequency yet)", threshold, r.RatePerMin)
			}
		}
	})

	t.Run("already-known → Ready, RatePerMin=0 (no countdown)", func(t *testing.T) {
		// Even with a high BaselineFrequency, a Ready pattern reports RatePerMin=0
		// because the ETA countdown only applies while still learning.
		p := &Pattern{Count: 100, Verdict: "known", BaselineFrequency: 12}
		r := LogReadiness(p, 100, poll)
		if !r.Ready {
			t.Errorf("Ready = false, want true (verdict==known)")
		}
		if r.RatePerMin != 0 {
			t.Errorf("RatePerMin = %v, want 0 (Ready ⇒ no ETA)", r.RatePerMin)
		}
	})

	t.Run("brand-new pattern, no baseline yet → RatePerMin=0 (no ETA)", func(t *testing.T) {
		p := &Pattern{Count: 3, BaselineFrequency: 0}
		r := LogReadiness(p, 100, poll)
		if r.RatePerMin != 0 {
			t.Errorf("RatePerMin = %v, want 0 (no rate yet)", r.RatePerMin)
		}
	})

	t.Run("poll interval unset → RatePerMin=0 (safe degrade)", func(t *testing.T) {
		p := &Pattern{Count: 10, BaselineFrequency: 6}
		r := LogReadiness(p, 100, 0)
		if r.RatePerMin != 0 {
			t.Errorf("RatePerMin = %v, want 0 (no poll interval wired)", r.RatePerMin)
		}
	})

	t.Run("learning with a live baseline → RatePerMin = EWMA/pollMinutes", func(t *testing.T) {
		// BaselineFrequency is sightings/tick; poll=30s=0.5min ⇒ 6/0.5 = 12/min.
		p := &Pattern{Count: 10, BaselineFrequency: 6}
		r := LogReadiness(p, 100, poll)
		if r.RatePerMin != 12 {
			t.Errorf("RatePerMin = %v, want 12 (6 sightings/tick ÷ 0.5 min/tick)", r.RatePerMin)
		}
	})
}
