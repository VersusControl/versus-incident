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

// HealthUsage reports the provider work charged against the shared tick budget.
type HealthUsage struct {
	Sources              int
	Requests             int
	Rows                 int
	ResponseBytes        int64
	LargestResponseBytes int64
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
	OrgID        string              `json:"org_id"`
	Service      string              `json:"service"`
	Operation    string              `json:"operation,omitempty"`
	Family       string              `json:"family"`
	Measure      string              `json:"measure"`
	Value        *float64            `json:"value"`
	Unit         string              `json:"unit,omitempty"`
	Numerator    *float64            `json:"numerator,omitempty"`
	Denominator  *float64            `json:"denominator,omitempty"`
	Availability MeasureAvailability `json:"availability"`
	SourceRef    string              `json:"source_ref"`
	SignalRef    string              `json:"signal_ref,omitempty"`
	ObservedAt   time.Time           `json:"observed_at"`
	WindowStart  time.Time           `json:"window_start"`
	WindowEnd    time.Time           `json:"window_end"`
	FreshUntil   time.Time           `json:"fresh_until,omitempty"`
	Provenance   string              `json:"provenance,omitempty"`
}

// HealthCapability reports per-measure availability for one signal family.
type HealthCapability struct {
	Family   string                         `json:"family"`
	Measures map[string]MeasureAvailability `json:"measures"`
}

// HealthCollection is one provider result plus the work it consumed.
type HealthCollection struct {
	Evidence         []SignalEvidence
	AssessmentInputs []HealthAssessmentInput
	Usage            HealthUsage
	Partial          bool
}

// HealthAssessmentInput is an ephemeral expected range paired with evidence
// from the same provider read. Collectors pass it to assessors but never persist it.
type HealthAssessmentInput struct {
	OrgID             string
	Service           string
	Operation         string
	Family            string
	Measure           string
	SourceRef         string
	SignalRef         string
	ExpectedMean      float64
	ExpectedStd       float64
	ObservationCount  int
	MaturityThreshold int
}

// AssessmentDriver identifies one normalized input that influenced an assessment.
type AssessmentDriver struct {
	Family    string  `json:"family"`
	Measure   string  `json:"measure"`
	Operation string  `json:"operation,omitempty"`
	Weight    float64 `json:"weight,omitempty"`
}

// HealthAssessment is an optional source-neutral enrichment. Nil score and
// silent values mean withheld or unknown; pointers preserve a real zero or false.
type HealthAssessment struct {
	OrgID             string             `json:"org_id"`
	Service           string             `json:"service"`
	RegressionScore   *float64           `json:"regression_score"`
	Regressing        bool               `json:"regressing,omitempty"`
	Silent            *bool              `json:"silent"`
	Confidence        float64            `json:"confidence"`
	ReasonCode        string             `json:"reason_code,omitempty"`
	Drivers           []AssessmentDriver `json:"drivers,omitempty"`
	IncludedFamilies  []string           `json:"included_families,omitempty"`
	AlgorithmVersion  string             `json:"algorithm_version"`
	BaselineReference string             `json:"baseline_reference,omitempty"`
	AssessedAt        time.Time          `json:"assessed_at"`
	FreshUntil        time.Time          `json:"fresh_until,omitempty"`
	Severity          string             `json:"severity,omitempty"`
	RaiseSeverity     bool               `json:"raise_severity,omitempty"`
	AlertStateKnown   bool               `json:"alert_state_known,omitempty"`
}

// HealthProvider discovers capabilities and performs one bounded read. OSS
// registers no metric or trace provider; out-of-tree providers implement this seam.
type HealthProvider interface {
	Capabilities(context.Context, string) []HealthCapability
	Collect(context.Context, HealthCollectRequest) (HealthCollection, error)
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
