package core

import (
	"context"
	"time"
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
	Pull(ctx context.Context, since time.Time) ([]Signal, time.Time, error)
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
}

// Detector consumes signals and emits AgentResults.
type Detector interface {
	Name() string
	Process(ctx context.Context, batch []Signal) ([]AgentResult, error)
}

// AIFinding is the structured response returned by an AIAnalyzer for an
// unknown / spiking pattern. Used by detect mode.
type AIFinding struct {
	Title       string
	Summary     string
	Severity    string  // critical|high|medium|low
	Category    string  // e.g. "database", "auth", "deploy"
	Confidence  float64 // 0..1
	Suggestions []string
	SampleIDs   []string // signal IDs / pattern IDs for traceability
}

// AIAnalyzer turns an AgentResult into an AIFinding. Concrete analyzers
// (OpenAI-compatible chat/completions, etc.) implement this interface.
type AIAnalyzer interface {
	Name() string
	Analyze(ctx context.Context, result AgentResult) (*AIFinding, error)
}
