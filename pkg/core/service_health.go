package core

import (
	"context"
	"time"
)

// HealthState is the canonical availability state for a health measure.
type HealthState string

const (
	HealthReady         HealthState = "ready"
	HealthPartial       HealthState = "partial"
	HealthNotConfigured HealthState = "not_configured"
	HealthCollecting    HealthState = "collecting"
	HealthNoData        HealthState = "no_data"
	HealthUnsupported   HealthState = "unsupported"
	HealthError         HealthState = "error"
	HealthStale         HealthState = "stale"
	HealthRestricted    HealthState = "restricted"
)

// MeasureAvailability separates operational state from a nullable value.
type MeasureAvailability struct {
	State      HealthState `json:"state"`
	ReasonCode string      `json:"reason_code,omitempty"`
	ActionID   string      `json:"action_id,omitempty"`
}

// HealthBudget is the server-clamped remaining allowance passed to a provider.
type HealthBudget struct {
	Requests          int
	RequestsPerSource int
	Rows              int
	ResponseBytes     int64
	CumulativeBytes   int64
	Concurrency       int
}

// HealthCollectRequest carries trusted scope and bounded collection inputs.
type HealthCollectRequest struct {
	OrgID       string
	SourceIDs   []string
	ServiceIDs  []string
	WindowStart time.Time
	WindowEnd   time.Time
	Deadline    time.Time
	Budget      HealthBudget
}

// SignalEvidence is a source-neutral nullable measure with explicit availability.
type SignalEvidence struct {
	Family       string
	Measure      string
	Value        *float64
	Unit         string
	Numerator    *float64
	Denominator  *float64
	Availability MeasureAvailability
	SourceRef    string
	ObservedAt   time.Time
	WindowStart  time.Time
	WindowEnd    time.Time
	FreshUntil   time.Time
	Provenance   string
}

// HealthProvider discovers capabilities and performs one bounded read. OSS
// registers no metric or trace provider; out-of-tree providers implement this seam.
type HealthProvider interface {
	Capabilities(context.Context, string) map[string]MeasureAvailability
	Collect(context.Context, HealthCollectRequest) ([]SignalEvidence, error)
}

// LogHealthObservation is the safe post-grouping projection accepted by
// Service Health. It deliberately has no raw signal, message, sample,
// credential, provider-field, or cursor field.
type LogHealthObservation struct {
	OrgID               string
	SourceID            string
	Service             string
	PatternID           string
	ObservedAt          time.Time
	Frequency           int
	StrongestSeverity   string
	NewPattern          bool
	InGrace             bool
	ExpectedRate        float64
	ExpectedSpread      float64
	LifecycleClass      string
	LifecycleClassified bool
}

// SourceHealthObservation records one worker pull outcome without its raw
// backend error or provider-specific delivery state.
type SourceHealthObservation struct {
	OrgID       string
	SourceID    string
	AttemptedAt time.Time
	Succeeded   bool
	ErrorClass  string
}

// LogHealthRecorder receives the worker's already-computed fold projection.
// Training passes LifecycleClassified=false rather than inventing a verdict.
type LogHealthRecorder interface {
	RecordLogHealth(context.Context, LogHealthObservation) error
	RecordSourceHealth(context.Context, SourceHealthObservation) error
}
