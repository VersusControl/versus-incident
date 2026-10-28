package kubernetes

import (
	"sort"
	"sync"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// SyncProgress is the first-sync progress attached to a view's sync status.
type SyncProgress = kubeindex.SyncProgress

// inventoryProgress records what the background worker has done during a
// cluster's first ready publication. Completed phases and counts survive retries;
// the current phase can change while the remaining kinds fill in.
type inventoryProgress struct {
	mu          sync.Mutex
	started     time.Time
	updated     time.Time
	step        int
	counts      map[string]int
	kindsTotal  int
	version     string
	done        map[string]bool
	statuses    map[string]string
	durations   map[string]float64
	phaseStatus map[string]string
	phaseAt     time.Time
}

func (progress *inventoryProgress) advance(step string, now time.Time) {
	position := -1
	for index, name := range kubeindex.SyncSteps {
		if name == step {
			position = index + 1
		}
	}
	if position < 0 {
		return
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.started.IsZero() {
		progress.started = now
	}
	if position != progress.step || progress.phaseAt.IsZero() {
		if progress.durations == nil {
			progress.durations = map[string]float64{}
		}
		if progress.step > 0 && !progress.phaseAt.IsZero() {
			progress.durations[kubeindex.SyncSteps[progress.step-1]] += now.Sub(progress.phaseAt).Seconds()
		}
		progress.step, progress.updated, progress.phaseAt = position, now, now
	}
}

// pause excludes the worker retry/cadence wait from measured phase durations.
func (progress *inventoryProgress) pause(now time.Time) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.step > 0 && !progress.phaseAt.IsZero() {
		progress.durations[kubeindex.SyncSteps[progress.step-1]] += now.Sub(progress.phaseAt).Seconds()
		progress.phaseAt = time.Time{}
	}
}

func (progress *inventoryProgress) complete(step string, now time.Time) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.done == nil {
		progress.done = map[string]bool{}
	}
	progress.done[step], progress.updated = true, now
	delete(progress.phaseStatus, step)
}

func (progress *inventoryProgress) phaseResult(phase, status string) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.phaseStatus == nil {
		progress.phaseStatus = map[string]string{}
	}
	progress.phaseStatus[phase] = status
}

func (progress *inventoryProgress) due(kinds int, now time.Time) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.statuses == nil {
		progress.statuses = map[string]string{}
	}
	for kind := range indexedResourceIDs {
		if progress.statuses[kind] == "" {
			progress.statuses[kind] = "pending"
		}
	}
	if kinds > progress.kindsTotal {
		progress.kindsTotal, progress.updated = kinds, now
	}
}

func (progress *inventoryProgress) kindObserved(kind string, state kubeindex.KindStatus, now time.Time) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.statuses == nil {
		progress.statuses = map[string]string{}
	}
	status := state.State
	if state.Absent {
		status = "absent"
	} else if state.Error != "" {
		status = state.Error
	}
	progress.statuses[kind], progress.updated = status, now
}

func (progress *inventoryProgress) kindListed(kind string, records int, now time.Time) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.counts == nil {
		progress.counts = map[string]int{}
	}
	if progress.statuses == nil {
		progress.statuses = map[string]string{}
	}
	progress.statuses[kind] = "ready"
	progress.counts[kind] = max(records, progress.counts[kind])
	progress.updated = now
}

func (progress *inventoryProgress) clusterVersion(version string) {
	if version == "" {
		return
	}
	progress.mu.Lock()
	progress.version = version
	progress.mu.Unlock()
}

func (progress *inventoryProgress) snapshot() *SyncProgress {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.step == 0 {
		return nil
	}
	result := &SyncProgress{
		Step: kubeindex.SyncSteps[progress.step-1], StepIndex: progress.step, StepTotal: len(kubeindex.SyncSteps),
		StepsDone: []string{}, Counts: make(map[string]int, len(progress.counts)),
		KindsTotal:     max(progress.kindsTotal, len(progress.statuses)),
		ClusterVersion: progress.version, StartedAt: progress.started, UpdatedAt: progress.updated,
	}
	result.KindStatus = make(map[string]string, len(progress.statuses))
	result.PhaseStatus = make(map[string]string, len(progress.phaseStatus))
	for phase, status := range progress.phaseStatus {
		result.PhaseStatus[phase] = status
	}
	result.PhaseSeconds = make(map[string]float64, len(kubeindex.SyncSteps))
	for _, step := range kubeindex.SyncSteps {
		if progress.done[step] {
			result.StepsDone = append(result.StepsDone, step)
		}
		result.PhaseSeconds[step] = progress.durations[step]
	}
	if !progress.phaseAt.IsZero() {
		result.PhaseSeconds[result.Step] += time.Since(progress.phaseAt).Seconds()
	}
	for kind, status := range progress.statuses {
		result.KindStatus[kind] = status
		if status != "ready" && status != "absent" && status != "forbidden" {
			result.RemainingKinds = append(result.RemainingKinds, kind)
		} else {
			result.KindsDone++
		}
	}
	sort.Strings(result.RemainingKinds)
	for kind, count := range progress.counts {
		result.Counts[kind] = count
	}
	if nodes, exists := progress.counts["Node"]; exists {
		result.NodeCount = &nodes
	}
	return result
}

func (service *Service) inventoryCacheEntry() *inventoryCache {
	if service == nil || service.changes == nil {
		return nil
	}
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	return service.changes.inventories[service.indexScope()]
}

// coldSyncProgress retains worker progress until the first ready publication,
// including the interval after the core-only generation is published.
func (service *Service) coldSyncProgress() *SyncProgress {
	cache := service.inventoryCacheEntry()
	if cache == nil {
		return nil
	}
	service.changes.mu.Lock()
	started := cache.started
	service.changes.mu.Unlock()
	if !started {
		return nil
	}
	cache.mu.RLock()
	published := !cache.generation.FirstReadyAt.IsZero()
	cache.mu.RUnlock()
	if published {
		return nil
	}
	return cache.progress.snapshot()
}

// withSyncProgress attaches first-sync progress to a syncing view status.
func (service *Service) withSyncProgress(status SyncStatus) SyncStatus {
	if status.State == "syncing" {
		status.Progress = service.coldSyncProgress()
	}
	return status
}
