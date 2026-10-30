package kubernetes

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"sync"
	"time"
)

// clusterRecovery survives credential epochs. Neither request traffic nor a
// successor's construction is evidence of recovery; only publication resets it.
type clusterRecovery struct {
	mu                sync.Mutex
	failures          int
	replacementStreak int
	next              time.Time
	lastCommitted     time.Time
	revision          string
	cancel            context.CancelFunc
	done              chan struct{}
	stages            map[string]recoveryStage
	workers           int
	firstReady        map[string]time.Time
	attempts          uint64
}

func (r *clusterRecovery) deadline() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

func (r *clusterRecovery) failed(err error, client *Client) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	// Concurrent failures from a single attempt do not multiply the streak.
	if now.Before(r.next) && r.failures > 0 {
		return
	}
	r.failures++
	delay := time.Second * time.Duration(1<<min(r.failures-1, 6))
	delay += time.Duration(rand.Int63n(int64(delay/4 + 1)))
	r.next = maxTime(r.next, now.Add(delay))
	if client != nil {
		client.backoffMu.Lock()
		r.next = maxTime(r.next, client.backoffUntil)
		client.backoffMu.Unlock()
	}
	log.Printf("kubernetes: recovery stage=credential_recovery reason=%s attempt=%d retry_ms=%d", recoveryReason(err, "credential_recovery"), r.failures, time.Until(r.next).Milliseconds())
}

func (r *clusterRecovery) committed(revision string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures, r.next, r.lastCommitted, r.revision = 0, time.Time{}, time.Now().UTC(), revision
	r.replacementStreak = 0
}

func waitRecovery(ctx context.Context, r *clusterRecovery) error {
	for {
		delay := time.Until(r.deadline())
		if delay <= 0 {
			return ctx.Err()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (registry *ServiceRegistry) superviseCluster(ctx context.Context, clusterID string, recovery *clusterRecovery) {
	defer close(recovery.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := waitRecovery(ctx, recovery); err != nil {
			return
		}
		registry.mu.Lock()
		var current *Service
		for _, entry := range registry.entries {
			if entry.Info.ID == clusterID {
				current = entry.Service
				break
			}
		}
		registry.mu.Unlock()
		if current == nil {
			return
		}
		// A producer exit is not itself a credential failure. Once its joined
		// done channel closes, restart in the same still-authorized scope and keep
		// the committed generation. Credential retirement takes the separate path.
		current.changes.mu.Lock()
		cache := current.changes.inventories[current.indexScope()]
		stopped := current.changes.stopped[current.indexScope()]
		restart := false
		if cache != nil && cache.done != nil && !stopped && current.identity.ctx.Err() == nil {
			select {
			case <-cache.done:
				cache.started = false
				restart = true
			default:
			}
		}
		current.changes.mu.Unlock()
		if stopped {
			return
		}
		if restart {
			current.startInventory(ctx)
			recovery.stage("worker_recovery", time.Now(), nil)
		}

		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := registry.resolveCluster(attempt, current.scope.OrgID, clusterID)
		cancel()
		if err != nil && ctx.Err() == nil {
			recovery.failed(err, current.client)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *clusterRecovery) published(generation inventoryGeneration, orgID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !generation.FirstReadyAt.IsZero() {
		if r.firstReady == nil {
			r.firstReady = map[string]time.Time{}
		}
		if r.firstReady[orgID].IsZero() {
			r.firstReady[orgID] = generation.FirstReadyAt
		}
	}
	fresh := false
	for _, state := range generation.View.Kinds {
		if state.State == "ready" && !state.Partial && state.CompletedAt.After(r.lastCommitted) {
			fresh = true
			break
		}
	}
	if !fresh {
		return
	}
	r.failures, r.next, r.lastCommitted, r.revision = 0, time.Time{}, time.Now().UTC(), generation.Revision
	r.replacementStreak = 0
}

type recoveryStage struct {
	reason string
	at     time.Time
}

func recoveryReason(err error, stage string) string {
	switch {
	case err == nil:
		return "ready"
	case errors.Is(err, errInventoryLeaseLost):
		return "lease_lost"
	case errors.Is(err, ErrCredentialUnavailable):
		return "credential_unavailable"
	case errors.Is(err, ErrUnauthorized):
		return "credential_rejected"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	}
	if stage == "payload" || stage == "cas" || stage == "journal" || stage == "lease" {
		return "storage_unavailable"
	}
	return "collection_unavailable"
}

func (r *clusterRecovery) stage(stage string, started time.Time, err error) {
	if r == nil {
		return
	}
	reason := recoveryReason(err, stage)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stages == nil {
		r.stages = map[string]recoveryStage{}
	}
	previous := r.stages[stage]
	if previous.reason == reason && time.Since(previous.at) < time.Minute {
		return
	}
	r.stages[stage] = recoveryStage{reason: reason, at: time.Now()}
	age := int64(-1)
	if !r.lastCommitted.IsZero() {
		age = int64(time.Since(r.lastCommitted).Seconds())
	}
	log.Printf("kubernetes: capture stage=%s reason=%s duration_ms=%d committed_age_s=%d failures=%d workers=%d attempts=%d", stage, reason, time.Since(started).Milliseconds(), age, r.failures, r.workers, r.attempts)
}

func (r *clusterRecovery) workerStarted() func() {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.workers++
	r.mu.Unlock()
	return func() { r.mu.Lock(); r.workers--; r.mu.Unlock() }
}
func (r *clusterRecovery) attempted() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.attempts++
	r.mu.Unlock()
}

func (r *clusterRecovery) firstPublication(orgID string) time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstReady[orgID]
}

func (r *clusterRecovery) reserveReplacement(renewal bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replacementStreak++
	streak := max(r.replacementStreak, r.failures)
	delay := time.Second * time.Duration(1<<min(streak-1, 6))
	delay += time.Duration(rand.Int63n(int64(delay/4 + 1)))
	r.next = maxTime(r.next, time.Now().Add(delay))
	reason := "credential_recovery"
	if renewal {
		reason = "credential_renewal"
	}
	log.Printf("kubernetes: recovery stage=epoch_rebuild reason=%s attempt=%d retry_ms=%d", reason, r.replacementStreak, time.Until(r.next).Milliseconds())
}
