package index

import "time"

// SyncProgress describes a cluster's first inventory sync while no generation
// has been published yet. It only reports what the background sync has already
// observed; it never claims the data is complete.
type SyncProgress struct {
	// Step is the step in progress, one of SyncSteps.
	Step      string   `json:"step"`
	StepIndex int      `json:"step_index"`
	StepTotal int      `json:"step_total"`
	StepsDone []string `json:"steps_done"`
	// Counts holds records listed so far per Kubernetes kind. A kind appears
	// once its list completes.
	Counts         map[string]int `json:"counts"`
	KindsDone      int            `json:"kinds_done"`
	KindsTotal     int            `json:"kinds_total"`
	ClusterVersion string         `json:"cluster_version,omitempty"`
	NodeCount      *int           `json:"node_count,omitempty"`
	StartedAt      time.Time      `json:"started_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// SyncSteps are the first-sync steps in execution order: connect to the
// cluster, list resources, build the projections that are published, and read
// CPU and memory usage (which runs after the first publication).
var SyncSteps = []string{"connected", "listing", "building", "metrics"}
