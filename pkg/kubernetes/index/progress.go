package index

import "time"

// SyncProgress describes a cluster's first inventory sync until the first ready generation
// has been published. It only reports what the background sync has already
// observed; it never claims the data is complete.
type SyncProgress struct {
	// Step is the step in progress, one of SyncSteps.
	Step      string   `json:"step"`
	StepIndex int      `json:"step_index"`
	StepTotal int      `json:"step_total"`
	StepsDone []string `json:"steps_done"`
	// Counts holds records listed so far per Kubernetes kind. A kind appears
	// once its list completes.
	Counts map[string]int `json:"counts"`
	// KindsDone counts resolved checks, including absent APIs and unreadable kinds;
	// Counts contains only successfully listed records.
	KindsDone      int                `json:"kinds_done"`
	KindsTotal     int                `json:"kinds_total"`
	ClusterVersion string             `json:"cluster_version,omitempty"`
	NodeCount      *int               `json:"node_count,omitempty"`
	StartedAt      time.Time          `json:"started_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	RemainingKinds []string           `json:"remaining_kinds,omitempty"`
	KindStatus     map[string]string  `json:"kind_status,omitempty"`
	PhaseStatus    map[string]string  `json:"phase_status,omitempty"`
	PhaseSeconds   map[string]float64 `json:"phase_seconds,omitempty"`
}

// SyncSteps are the first-sync steps in execution order: connect to the
// cluster, list resources, build the projections that are published, and read
// CPU and memory usage (which runs after the first publication).
var SyncSteps = []string{"connected", "listing", "building", "metrics"}
