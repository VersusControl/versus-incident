package kubernetes

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

const usageInterestDebounce = 100 * time.Millisecond
const usageInterestTouchInterval = time.Minute

// Cache IO must not hold the credential mutex and delay unrelated API reads.
// A Provider call already in flight cannot be interrupted; it may finish in
// the old epoch's private cache, which a retired service cannot serve. This is
// only for disposable usage caches, not state or action transactions.
func (service *Service) guardedUsageCacheWrite(ctx context.Context, write func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if service.identity.ctx.Err() != nil {
		return ErrCredentialUnavailable
	}
	service.changes.mu.Lock()
	stopped := service.changes.stopped[service.indexScope()]
	service.changes.mu.Unlock()
	if stopped {
		return ErrClusterUnavailable
	}
	return write()
}

type usageInterestWorker struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	wake           chan struct{}
	pending        map[string]usageInterest
	recent         map[string]usageInterest
	fetched        map[string]time.Time
	failures       atomic.Int64
	overflowLogged bool
}

// Requests only enqueue bounded memory work. There is one coalescing worker
// per scoped service, never one goroutine or manifest CAS per drawer request.
func (service *Service) queueUsageInterest(item usageInterest) {
	service.changes.mu.Lock()
	if service.changes.stopped[service.indexScope()] {
		service.changes.mu.Unlock()
		return
	}
	lifecycle := service.changes.lifecycle
	if service.identity.ctx.Err() != nil || lifecycle != nil && lifecycle.Err() != nil || service.logStreamContext != nil && service.logStreamContext.Err() != nil {
		service.changes.mu.Unlock()
		return
	}
	state := service.usageHistory
	state.Lock()
	key := service.cacheKey()
	worker := state.workers[key]
	if worker == nil {
		if len(state.workers) >= 16 {
			state.Unlock()
			service.changes.mu.Unlock()
			log.Printf("kubernetes: usage history interest worker limit reached")
			return
		}
		ctx, cancel := context.WithCancel(service.identity.ctx)
		stops := []func() bool{}
		if lifecycle != nil {
			stops = append(stops, context.AfterFunc(lifecycle, cancel))
		}
		if service.logStreamContext != nil {
			stops = append(stops, context.AfterFunc(service.logStreamContext, cancel))
		}
		worker = &usageInterestWorker{ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), pending: make(map[string]usageInterest), recent: make(map[string]usageInterest), fetched: make(map[string]time.Time)}
		state.workers[key] = worker
		go func() {
			defer close(worker.done)
			defer func() {
				for _, stop := range stops {
					stop()
				}
				state.Lock()
				if state.workers[key] == worker {
					delete(state.workers, key)
				}
				state.Unlock()
			}()
			service.runUsageInterestWorker(worker)
		}()
	}
	worker.mu.Lock()
	id := item.Namespace + "/" + item.Pod
	previous, exists := worker.pending[id]
	if !exists && len(worker.pending) >= usageHistoryPods {
		if !worker.overflowLogged {
			log.Printf("kubernetes: usage history interest queue limit reached")
			worker.overflowLogged = true
		}
	} else {
		if !exists || !previous.Opened.After(item.Opened) {
			worker.pending[id] = item
		}
		select {
		case worker.wake <- struct{}{}:
		default:
		}
	}
	worker.mu.Unlock()
	state.Unlock()
	service.changes.mu.Unlock()
}

func (service *Service) stopUsageInterestWorker() {
	state := service.usageHistory
	if state == nil {
		return
	}
	state.Lock()
	worker := state.workers[service.cacheKey()]
	if worker != nil {
		worker.cancel()
	}
	state.Unlock()
	if worker != nil {
		// Provider blob methods have no context. An in-flight call must return
		// before we join; do not abandon it in a detached/leaking goroutine.
		<-worker.done
	}
}

func (worker *usageInterestWorker) failed(stage string, err error) {
	if worker.ctx.Err() != nil {
		return
	}
	worker.failures.Add(1)
	code := DiagnoseError(err).Code
	if errors.Is(err, errUsageInterestContention) {
		code = "storage_conflict"
	}
	log.Printf("kubernetes: usage history bridge %s failed: code=%s; pending interests retained", stage, code)
}

func (service *Service) runUsageInterestWorker(worker *usageInterestWorker) {
	backoff := 250 * time.Millisecond
	for {
		select {
		case <-worker.ctx.Done():
			return
		case <-worker.wake:
		}
		delay := usageInterestDebounce
		for {
			timer := time.NewTimer(delay)
			select {
			case <-worker.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if service.flushUsageInterests(worker) {
				backoff = 250 * time.Millisecond
				break
			}
			delay = backoff
			backoff = min(30*time.Second, backoff*2)
		}
	}
}

func (service *Service) flushUsageInterests(worker *usageInterestWorker) bool {
	now := time.Now().UTC()
	worker.mu.Lock()
	batch := make(map[string]usageInterest, len(worker.pending))
	updates := []usageInterest{}
	for key, item := range worker.pending {
		batch[key] = item
		recent, exists := worker.recent[key]
		if !exists || recent.UID != item.UID || item.Opened.Sub(recent.Opened) >= usageInterestTouchInterval {
			updates = append(updates, item)
		}
	}
	worker.mu.Unlock()
	if len(batch) == 0 {
		return true
	}
	provider := service.usageProvider()
	if provider == nil {
		worker.failed("interest_write", ErrClusterUnavailable)
		return false
	}
	if len(updates) > 0 {
		if err := service.updateUsageManifest(worker.ctx, provider, updates, now); err != nil {
			worker.failed("interest_write", err)
			return false
		}
	}
	if worker.ctx.Err() != nil {
		return true
	}
	worker.mu.Lock()
	for _, item := range updates {
		worker.recent[item.Namespace+"/"+item.Pod] = item
	}
	for key, item := range worker.recent {
		if now.Sub(item.Opened) > 2*usageInterestTouchInterval {
			delete(worker.recent, key)
			delete(worker.fetched, key)
		}
	}
	for len(worker.recent) > usageHistoryPods {
		oldestKey := ""
		oldest := now.Add(time.Hour)
		for key, item := range worker.recent {
			if item.Opened.Before(oldest) {
				oldestKey, oldest = key, item.Opened
			}
		}
		delete(worker.recent, oldestKey)
		delete(worker.fetched, oldestKey)
	}
	worker.mu.Unlock()
	readFailed := false
	for key, item := range batch {
		if worker.ctx.Err() != nil {
			return true
		}
		worker.mu.Lock()
		fresh := now.Sub(worker.fetched[key]) < 15*time.Second
		worker.mu.Unlock()
		if !fresh {
			loaded, err := service.refreshSharedUsage(worker.ctx, provider, item, now)
			if err != nil {
				worker.failed("snapshot_read", err)
				readFailed = true
				continue
			}
			if loaded {
				worker.mu.Lock()
				worker.fetched[key] = now
				worker.mu.Unlock()
			}
		}
		worker.mu.Lock()
		if latest, exists := worker.pending[key]; exists && latest.UID == item.UID && !latest.Opened.After(item.Opened) {
			delete(worker.pending, key)
		}
		if len(worker.pending) < usageHistoryPods {
			worker.overflowLogged = false
		}
		worker.mu.Unlock()
	}
	return !readFailed
}

func (service *Service) refreshSharedUsage(ctx context.Context, provider storage.Provider, item usageInterest, now time.Time) (bool, error) {
	seen := make(map[string]bool)
	for _, org := range append([]string{service.scope.OrgID}, service.readOrgs...) {
		if seen[org] {
			continue
		}
		seen[org] = true
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		scope := org + "\x00" + service.scope.ClusterID + "\x00" + service.scope.CredentialID
		record, _, err := readUsageBridgeRecord(provider, scope, item.Namespace, item.Pod, now)
		if err != nil {
			return false, err
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if len(record.Samples) == 0 || item.UID != "" && item.UID != record.UID {
			continue
		}
		state := service.usageHistory
		state.Lock()
		local := state.scope(service.cacheKey())
		entry := local.pods[item.Namespace+"/"+item.Pod]
		if entry != nil && (entry.uid == "" || entry.uid == record.UID) {
			entry.samples = mergeUsageSamples(record.Samples, entry.samples, now)
			if record.Since.Before(entry.since) {
				entry.since = record.Since
			}
		}
		state.Unlock()
		return true, nil
	}
	return false, nil
}
