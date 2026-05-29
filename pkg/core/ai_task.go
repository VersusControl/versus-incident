package core

import "context"

// AITaskKind identifies what an AIAgent is being asked to do.
//
// The dispatcher uses the kind to pick the right agent, cache, and
// rate limiter. New kinds are added by extending this enum and the
// router's wiring.
type AITaskKind string

const (
	// AITaskDetect is a cheap, tool-free, single-call classification of
	// an unknown or spiking log pattern. The output is an AIFinding
	// emitted as an incident through services.CreateIncidentFromFinding.
	AITaskDetect AITaskKind = "detect"

	// AITaskAnalyze is an operator-triggered, tool-using investigation
	// of a single incident. The output is an AIFinding persisted to the
	// analyses storage blob. Analyze NEVER fans out to notification
	// channels.
	AITaskAnalyze AITaskKind = "analyze"
)

// AITask is the input to an AIAgent.Run call. Each concrete task type
// carries the inputs needed by its kind and exposes a CacheKey() the
// router uses for memoisation. An empty CacheKey disables caching for
// that call.
type AITask interface {
	Kind() AITaskKind
	CacheKey() string
}

// DetectTask wraps an AgentResult for detect-mode classification.
type DetectTask struct {
	Result AgentResult
}

// Kind implements AITask.
func (DetectTask) Kind() AITaskKind { return AITaskDetect }

// CacheKey implements AITask. Detect memoisation is keyed by pattern
// id; an empty pattern id (unknown pattern not yet stored) disables
// caching for that call.
func (t DetectTask) CacheKey() string { return t.Result.PatternID }

// AnalyzeTask wraps an on-demand analysis request. The snapshot
// carries the incident payload the agent inspects; richer context
// (related logs, pattern history) is fetched via the agent's
// read-only tools at run time, not pre-loaded here.
type AnalyzeTask struct {
	Snapshot AnalyzeIncidentSnapshot
}

// Kind implements AITask.
func (AnalyzeTask) Kind() AITaskKind { return AITaskAnalyze }

// CacheKey implements AITask. Empty disables caching — operators
// expect a fresh tool walk on every analyze request.
func (t AnalyzeTask) CacheKey() string { return "" }

// AIAgent is one concrete model + prompt + (optional) tool wiring,
// dedicated to a single AITaskKind. Implementations live under
// pkg/agent/ai (e.g. detect, analyze).
//
// Run is expected to be self-contained: it owns the model call, but
// the router handles cache / rate-limit / persistence around it.
type AIAgent interface {
	Name() string
	Kind() AITaskKind
	Run(ctx context.Context, task AITask) (*AICallResult, error)
}
