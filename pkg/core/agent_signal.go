package core

import (
	"context"
	"time"
)

// Canonical Signal.Fields keys shared across the agent pipeline. They name
// the two attributes every signal type can carry regardless of source: the
// discovered service and the logical signal name (a metric's golden-signal,
// a trace operation, or a log template label). They live in OSS so any
// consumer — the worker's learn-exclusion chokepoint, an enterprise
// metric/trace brain, a standing data source — keys off ONE definition.
// Enterprise packages (pkg/intel, pkg/datasource) keep their own copies to
// stay decoupled; a cross-package drift test pins them equal.
const (
	// FieldService is the Signal.Fields key holding the discovered service
	// name (string). Empty when the source did not stamp one.
	FieldService = "service"
	// FieldSignal is the Signal.Fields key holding the logical signal name
	// (string) — e.g. a metric golden-signal or trace operation label.
	FieldSignal = "signal"
)

// Signal is one normalized observation pulled from a SignalSource.
//
// Fields holds best-effort structured fields; Raw is the original source
// document (capped to AgentConfig.SignalMaxBytes by the SignalSource so the
// pipeline cannot OOM on large documents).
type Signal struct {
	Source    string // e.g. "elasticsearch:prod-app"
	Timestamp time.Time
	Severity  string                 // raw severity from source, best-effort
	Message   string                 // primary text payload
	Fields    map[string]interface{} // structured fields
	Raw       map[string]interface{} // original document (capped size)
}

// SignalSource is a puller for one external observability backend
// (Elasticsearch, Loki, CloudWatch, ...).
type SignalSource interface {
	// Name uniquely identifies the configured source instance. It is used as
	// part of Redis cursor keys, so it must be stable across restarts.
	Name() string

	// Pull returns all signals strictly newer than `since`, plus the new
	// cursor (== max timestamp seen, or `since` if no signals were returned).
	//
	// Tailing invariant: the returned cursor MUST NOT exceed the wall-clock
	// time at pull, and a cursor-driven source MUST NOT scan past `now`.
	// Document timestamps are untrusted producer data; a single future-dated
	// record would otherwise push the cursor ahead of real time and make every
	// following `>= cursor` query match nothing until that time arrives — the
	// source stops emitting until its cursor is wiped. Sources bound their scan
	// at `now` (Loki `end`, Graylog `to`, Splunk `latest`, Elasticsearch `lte`,
	// CloudWatch `EndTime`) and clamp the returned cursor via
	// signalsources.ClampCursor.
	Pull(ctx context.Context, since time.Time) ([]Signal, time.Time, error)
}

// SourceRewinder is the OPTIONAL capability a SignalSource implements when it
// keeps its OWN internal read position — a byte offset, page token, or similar
// — that is INDEPENDENT of the poll cursor the worker passes to Pull as
// `since`. The file source is the canonical example: it tails a log file by
// byte offset (persisted in a sidecar) and ignores `since` entirely.
//
// Clearing the learned catalog rewinds the worker's poll cursor, which makes
// cursor-driven sources (Elasticsearch, Loki, CloudWatch, …) re-read their
// lookback window on the next tick. A source that tracks its own position must
// additionally rewind THAT position here — otherwise the clear leaves it pinned
// past the already-consumed data and it never re-emits, so the SAME running
// worker stops learning until the process is recreated (which reconstructs the
// source from scratch). Rewind reconciles the two cursors of truth so a clear
// behaves like a fresh process start in-place.
//
// Sources whose position is driven purely by `since` do not implement it; the
// worker skips them. Implementations must be safe to call concurrently with
// Pull and must leave the source in the state a freshly-constructed instance
// would have.
type SourceRewinder interface {
	Rewind(ctx context.Context) error
}

// AgentVerdict is the classification a Detector pipeline assigns to a batch
// of signals that share a fingerprint.
type AgentVerdict int

const (
	// VerdictKnownPattern means the pattern is in the catalog and current
	// frequency is within baseline — suppress (no AI, no incident).
	VerdictKnownPattern AgentVerdict = iota
	// VerdictUnknown means the pattern was never seen during training —
	// forward to AI for analysis.
	VerdictUnknown
	// VerdictSpike means the pattern is known but frequency exceeds the
	// baseline by more than the configured threshold — forward to AI.
	VerdictSpike
)

// String renders an AgentVerdict for logging.
func (v AgentVerdict) String() string {
	switch v {
	case VerdictKnownPattern:
		return "known"
	case VerdictUnknown:
		return "unknown"
	case VerdictSpike:
		return "spike"
	default:
		return "invalid"
	}
}

// AgentResult is the output of a Detector for one batch of signals that share
// the same fingerprint.
type AgentResult struct {
	Verdict       AgentVerdict
	PatternID     string   // empty when VerdictUnknown and pattern not yet stored
	Template      string   // human-readable pattern template ("Failed to ... at <*>:<*>")
	SampleSignals []Signal // representative signals (capped, post-redaction)
	Frequency     int      // matches in the current tick / window
	Baseline      float64  // EWMA baseline for the pattern (0 when unknown)
	// RuleSeverity is the strongest operator-declared severity carried by the
	// grouped signals (e.g. an anomaly rule's `severity: critical`). Empty for
	// auto-discovered signals with no declared severity. It acts as a floor:
	// the AI may escalate but must not silently demote below it.
	RuleSeverity string
}

// Detector consumes signals and emits AgentResults.
type Detector interface {
	Name() string
	Process(ctx context.Context, batch []Signal) ([]AgentResult, error)
}

// AIFinding is the structured response returned by an AISRE for an
// unknown / spiking pattern. Used by detect mode.
//
// Detect mode populates Title, Summary, Severity, Category,
// Confidence, Suggestions, SampleIDs. The lower block (RootCauseHypotheses,
// Evidence, RelatedPatternIDs, NextSteps) is populated by analyze mode
// only; detect leaves them empty and they marshal as omitempty.
type AIFinding struct {
	Title       string
	Summary     string
	Severity    string  // critical|high|medium|low
	Category    string  // e.g. "database", "auth", "deploy"
	Confidence  float64 // 0..1
	Suggestions []string
	SampleIDs   []string // signal IDs / pattern IDs for traceability

	// Analyze-mode fields. All optional; omitempty so detect-mode
	// payloads stay byte-compatible with the detect-only shape.
	RootCauseHypotheses []RootCauseHypothesis `json:"root_cause_hypotheses,omitempty"`
	Evidence            []EvidenceItem        `json:"evidence,omitempty"`
	RelatedPatternIDs   []string              `json:"related_pattern_ids,omitempty"`
	NextSteps           []string              `json:"next_steps,omitempty"`
}

// RootCauseHypothesis is one candidate explanation produced by an
// analyze-mode agent. Confidence is in [0, 1]; Rationale is a short
// human-readable justification.
type RootCauseHypothesis struct {
	Hypothesis string  `json:"hypothesis"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale,omitempty"`
}

// EvidenceItem records one piece of supporting evidence the analyze
// agent gathered (typically from a tool call). Source identifies where
// it came from (e.g. "tool:get_related_logs"); Summary is a one-liner
// shown in the UI; Detail is the long form (capped on persist).
type EvidenceItem struct {
	Source  string `json:"source"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

// AICallResult bundles the parsed finding with the inputs and outputs
// of the underlying model call. The trace fields (UserPrompt,
// RawResponse, DurationMs, Model) are persisted into the detect log so
// operators can audit what was sent to the model and what came back.
//
// SystemPrompt is intentionally omitted — it is a constant per build
// and would bloat every record. Operators can fetch the current
// assembled system prompt via the agent admin API.
type AICallResult struct {
	Finding     *AIFinding
	UserPrompt  string
	RawResponse string
	DurationMs  int64
	Model       string

	// ToolCalls is populated by tool-using agents (analyze). It is
	// nil for tool-free agents (detect).
	ToolCalls []ToolCallTrace
}

// ToolCallTrace is one model-issued tool round-trip captured for audit.
type ToolCallTrace struct {
	Name       string
	Args       string
	Output     string
	DurationMs int64
	Error      string
}

// Readiness is how close a signal is to its settled/known state — the point at
// which the agent's judgment of it is final rather than provisional. It is a
// generic, type-agnostic value produced at each read boundary (the log catalog
// reader in OSS, the enterprise metric/trace baseline reader). It is a plain
// value type with no behaviour: presentation (remaining evidence, ETA, progress
// bar) is DERIVED from these facts by the UI — the server ships facts, not
// formatted strings.
//
// Derivations the UI does: remaining = max(0, Needed-Seen) (when Needed>0);
// etaMinutes = remaining / RatePerMin (when RatePerMin>0, Needed>0, !Ready);
// progress = Seen/Needed (when Needed>0).
type Readiness struct {
	// Ready reports the signal has reached its settled/known state.
	Ready bool `json:"ready"`
	// Seen is the evidence folded so far (log sightings / folded samples).
	Seen int `json:"seen"`
	// Needed is the evidence required to reach Ready. 0 is the INDETERMINATE
	// sentinel: no count gate applies (e.g. logs with AutoPromoteAfter<=0 —
	// count-promotion is disabled, promotion is manual-only). The UI shows
	// "Manual only", never "X of 0".
	Needed int `json:"needed"`
	// RatePerMin is the observed arrival rate of NEW evidence for this key,
	// in evidence/minute, used to derive the ETA. 0 is the UNKNOWN/STALLED
	// sentinel: no honest rate yet, or the signal stopped flowing — the UI
	// shows no ETA (never ∞).
	RatePerMin float64 `json:"rate_per_min"`
}
