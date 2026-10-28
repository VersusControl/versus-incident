package core

import (
	"context"
	"time"
)

// BaselineRequest carries trusted scope and bounded selectors to a read-only provider.
type BaselineRequest struct {
	OrgID     string        `json:"-"`
	Service   string        `json:"-"`
	Signal    string        `json:"-"`
	PatternID string        `json:"-"`
	Window    time.Duration `json:"-"`
	Limit     int           `json:"-"`
	At        time.Time     `json:"-"`
}

// BaselineRecord is a provider-neutral learned expectation without raw observations.
type BaselineRecord struct {
	Service          string      `json:"service"`
	Operation        string      `json:"operation,omitempty"`
	Signal           string      `json:"signal"`
	Family           string      `json:"family"`
	SourceType       string      `json:"source_type"`
	PatternID        string      `json:"pattern_id,omitempty"`
	ExpectedMean     float64     `json:"expected_mean"`
	ExpectedStd      float64     `json:"expected_std"`
	CurrentValue     *float64    `json:"current_value"`
	Unit             string      `json:"unit"`
	ObservationCount int         `json:"observation_count"`
	Confident        bool        `json:"confident"`
	LastTrainedAt    time.Time   `json:"last_trained_at"`
	Availability     HealthState `json:"availability"`
	ReasonCode       string      `json:"reason_code,omitempty"`
	Provenance       []string    `json:"provenance"`
}

// BaselineCoverage reports one contributing surface even when it returned no records.
type BaselineCoverage struct {
	Family       string      `json:"family"`
	SourceType   string      `json:"source_type"`
	Availability HealthState `json:"availability"`
	ReasonCode   string      `json:"reason_code,omitempty"`
}

// BaselineResult is a deterministic bounded projection from one or more providers.
type BaselineResult struct {
	Availability HealthState        `json:"availability"`
	ReasonCode   string             `json:"reason_code,omitempty"`
	Found        bool               `json:"found"`
	Truncated    bool               `json:"truncated"`
	Omitted      int                `json:"omitted_records,omitempty"`
	Records      []BaselineRecord   `json:"records"`
	Coverage     []BaselineCoverage `json:"coverage"`
}

// BaselineProvider returns authorized learned expectations for trusted scope.
type BaselineProvider interface {
	DescribeBaselines(context.Context, BaselineRequest) (BaselineResult, error)
}
