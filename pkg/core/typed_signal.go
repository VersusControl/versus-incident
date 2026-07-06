package core

import (
	"context"
	"time"
)

// -----------------------------------------------------------------------------
// Per-type Learner/Detector seam.
//
// One shared worker lifecycle — training → shadow → detect → analyze — driving
// a per-type pair of plug-ins:
//
//   - SignalLearner owns the LEARNED MODEL (keying + folding + the "what is
//     normal" estimate). One implementation per signal type.
//   - SignalDetector owns the SCORING POLICY (observation + learned expectation
//     → verdict). Pure and trivially testable.
//
// The interfaces live in OSS so the enterprise metric/trace learners can
// implement them and register via agent.RegisterTypedBrain. OSS ships the LOG
// brain (drain-miner catalog Learner + frequency-novelty Detector) as the
// built-in default; OSS never imports the enterprise implementations.
//
// See plans/productization/sre/phase-8-per-type-ai-flow.md §2 for the design.
// -----------------------------------------------------------------------------

// Observation is one keyed unit a typed pipeline learns/scores for a tick.
//
//	logs    → one drain-template bucket            (Value = tick frequency)
//	metrics → one (service, golden-signal) window  (Value = latest sample / p99 / error-ratio)
//	traces  → one (service, operation) window       (Value = window p99 latency or error-ratio)
type Observation struct {
	Key       string    // stable per-type fingerprint
	Service   string    // attribution (feeds grace + routing)
	Signal    string    // human label: template | "latency_p99" | "GET /checkout error_rate"
	Timestamp time.Time // observation time (seasonality needs it)
	Value     float64   // the scalar the learner folds + the detector scores
	Frequency int       // tick count (logs) / sample count (metrics/traces)
	Samples   []Signal  // representative raw signals for the AI-analyze handoff
	// IsNew reports whether Group saw this Key for the first time during this
	// tick (a freshly discovered template / metric / operation). It is used
	// only for discovery logging and never affects classification. (Additive
	// to the design-doc struct.)
	IsNew bool
}

// TypedVerdict is the per-type classification of one observation.
type TypedVerdict struct {
	Class     AgentVerdict // reuse the enum: VerdictKnownPattern | VerdictUnknown | VerdictSpike
	Confident bool         // false ⇒ this key is still TRAINING ⇒ lifecycle suppresses, any mode
	Score     float64      // z-score (metrics/traces) or spike ratio (logs)
	Baseline  float64      // learned expected value (for the finding text)
	Reason    string       // "p99 4.2σ above the learned Tue-14:00 baseline (≈ 180ms)"
}

// SignalLearner owns the per-type LEARNED MODEL: keying + folding + the
// "what is normal" estimate. Implementations persist their derived state via
// the model-state seam (opaque bytes, keyed by OrgID). One implementation per
// signal type (logs in OSS; metrics/traces in Versus Enterprise).
type SignalLearner interface {
	// Kind identifies the signal type: "logs" | "metrics" | "traces".
	Kind() string

	// Group keys a tick's batch into observations WITHOUT mutating the model.
	Group(ctx context.Context, batch []Signal) ([]Observation, error)

	// Expected returns the learned mean/spread for a key at ts, and whether
	// the model is confident enough to score (the per-key training gate).
	// It must be a pure read of pre-fold state: the worker snapshots it
	// BEFORE Learn folds the tick, so a deviation is judged against the
	// pre-tick baseline.
	Expected(ctx context.Context, key string, ts time.Time) (mean, std float64, confident bool)

	// Learn folds observations into the model. Called every tick in EVERY
	// mode (training/shadow/detect) so the baseline keeps adapting to drift.
	Learn(ctx context.Context, obs []Observation) error
}

// SignalDetector is the pure SCORING POLICY: a function of (observation,
// learned expectation) → verdict. The per-type knob (z threshold, spike
// multiplier, sustained-tick debounce) lives here, not in the worker. The
// detector scores against the PASSED expectation (the pre-fold snapshot),
// never by re-reading the learner, so the result is independent of whether
// the worker has already folded the tick.
type SignalDetector interface {
	// Kind identifies the signal type and must match its paired Learner.
	Kind() string

	// Classify scores one observation against the learned expectation
	// (mean, std, confident) captured by SignalLearner.Expected.
	Classify(obs Observation, mean, std float64, confident bool) TypedVerdict
}
