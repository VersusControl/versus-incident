package kubernetes

import (
	"sync"
	"time"

	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

// SyncProgress is the first-sync progress attached to a view's sync status.
type SyncProgress = kubeindex.SyncProgress

// inventoryProgress records what the background worker has done during a
// cluster's first sync. It only moves forward: a retried cycle never lowers the
// step or a count, so viewers polling it see monotonic progress.
type inventoryProgress struct {
	mu         sync.Mutex
	started    time.Time
	updated    time.Time
	step       int
	counts     map[string]int
	listed     map[string]bool
	kindsTotal int
	version    string
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
	if position > progress.step {
		progress.step, progress.updated = position, now
	}
}

func (progress *inventoryProgress) due(kinds int, now time.Time) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if kinds > progress.kindsTotal {
		progress.kindsTotal, progress.updated = kinds, now
	}
}

func (progress *inventoryProgress) kindListed(kind string, records int, now time.Time) {
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.counts == nil {
		progress.counts, progress.listed = map[string]int{}, map[string]bool{}
	}
	progress.listed[kind] = true
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
		StepsDone: append([]string{}, kubeindex.SyncSteps[:progress.step-1]...), Counts: make(map[string]int, len(progress.counts)),
		KindsDone: len(progress.listed), KindsTotal: max(progress.kindsTotal, len(progress.listed)),
		ClusterVersion: progress.version, StartedAt: progress.started, UpdatedAt: progress.updated,
	}
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

// coldSyncProgress reports first-sync progress only while this scope's
// background sync runs and has not published its first generation. Once a
// generation exists, views serve it and progress is no longer reported.
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
	published := len(cache.generation.View.Kinds) > 0
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
