package cost

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// RollupReconcile reports one ReconcileRollups call.
type RollupReconcile struct {
	// Done is true once this process reads totals from the cost rollups.
	Done bool
	// Compared is true when this call compared the rollups with the cost
	// lines; false when this process had reconciled already or could not
	// take the replica lock.
	Compared bool
	Repair   storage.FinOpsRollupRepair
	Took     time.Duration
}

// ReconcileRollups brings the cost rollups in line with the cost lines once
// per process, holding the replica lock, so it never runs beside a cost-line
// write or another replica's reconcile. Until it has finished, totals read
// the cost lines. When another refresh, drill-down or replica holds the lock
// it does nothing and reports not done; the scheduler tries again on its
// next tick. Any holder of the replica lock runs it first as well.
func (a *Adapter) ReconcileRollups(ctx context.Context) (RollupReconcile, error) {
	if a.rollupsDone.Load() {
		return RollupReconcile{Done: true}, nil
	}
	return a.reconcileLocked(ctx, false)
}

// reconcileAfterRefresh reconciles again, after a scheduled refresh: every
// writer since the last reconcile, this version's or not, is caught within
// one refresh cycle. A busy lock skips it until the next refresh.
func (a *Adapter) reconcileAfterRefresh(ctx context.Context) (RollupReconcile, error) {
	return a.reconcileLocked(ctx, true)
}

// reconcileLocked takes this process's and the replicas' locks and
// reconciles; again reconciles even when this process has before.
func (a *Adapter) reconcileLocked(ctx context.Context, again bool) (RollupReconcile, error) {
	if !a.mu.TryLock() {
		return RollupReconcile{Done: a.rollupsDone.Load()}, nil
	}
	defer a.mu.Unlock()
	unlock, err := a.acquireReplicas(ctx, 0)
	if errors.Is(err, ErrBusy) {
		return RollupReconcile{Done: a.rollupsDone.Load()}, nil
	}
	if err != nil {
		return RollupReconcile{Done: a.rollupsDone.Load()}, err
	}
	defer unlock()
	return a.reconcileRollups(ctx, again)
}

// reconcileRollups runs the reconcile; the caller holds the replica lock.
// Without again it does nothing once this process has reconciled.
func (a *Adapter) reconcileRollups(ctx context.Context, again bool) (RollupReconcile, error) {
	a.rollupsMu.Lock()
	defer a.rollupsMu.Unlock()
	if !again && a.rollupsDone.Load() {
		return RollupReconcile{Done: true}, nil
	}
	store, err := a.cache.tables()
	if errors.Is(err, ErrNoFinOpsTables) {
		// No cost tables, no rollups to read.
		a.rollupsDone.Store(true)
		return RollupReconcile{Done: true}, nil
	}
	if err != nil {
		return RollupReconcile{Done: a.rollupsDone.Load()}, err
	}
	rollups, ok := store.(storage.FinOpsRollupStore)
	if !ok {
		// A store without rollup upkeep never reads totals from a rollup.
		a.rollupsDone.Store(true)
		return RollupReconcile{Done: true}, nil
	}
	began := time.Now()
	repair, err := rollups.ReconcileFinOpsRollups(ctx)
	took := time.Since(began)
	if err != nil {
		return RollupReconcile{Done: a.rollupsDone.Load(), Compared: true, Took: took}, err
	}
	if repair.RebuiltDays > 0 || repair.RebuiltMonths > 0 {
		log.Printf("aws cost: cost rollups reconciled in %s: %d days and %d months rebuilt",
			took.Round(time.Millisecond), repair.RebuiltDays, repair.RebuiltMonths)
	}
	a.rollupsDone.Store(true)
	return RollupReconcile{Done: true, Compared: true, Repair: repair, Took: took}, nil
}
