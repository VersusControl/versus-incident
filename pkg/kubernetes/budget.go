package kubernetes

import (
	"context"
	"sync"
	"time"
)

const (
	defaultOperationTimeout  = 20 * time.Second
	defaultOperationRequests = 64
	defaultOperationBytes    = int64(32 << 20)
	defaultOperationItems    = 10000
)

type operationBudgetKey struct{}

type operationBudget struct {
	mu       sync.Mutex
	deadline time.Time
	requests int
	bytes    int64
	items    int
}

func ensureOperationBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	if operationBudgetFrom(ctx) != nil {
		return ctx, func() {}
	}
	return withOperationBudget(ctx, defaultOperationTimeout, defaultOperationRequests, defaultOperationBytes, defaultOperationItems)
}

func withOperationBudget(ctx context.Context, timeout time.Duration, requests int, bytes int64, items int) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	budget := &operationBudget{deadline: time.Now().Add(timeout), requests: requests, bytes: bytes, items: items}
	return context.WithValue(ctx, operationBudgetKey{}, budget), cancel
}

func operationBudgetFrom(ctx context.Context) *operationBudget {
	budget, _ := ctx.Value(operationBudgetKey{}).(*operationBudget)
	return budget
}

func (budget *operationBudget) takeRequest() bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if time.Now().After(budget.deadline) || budget.requests <= 0 {
		return false
	}
	budget.requests--
	return true
}

func (budget *operationBudget) takeBytes(count int64) bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if count < 0 || count > budget.bytes {
		budget.bytes = 0
		return false
	}
	budget.bytes -= count
	return true
}

func (budget *operationBudget) takeItems(count int) int {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if count > budget.items {
		count = budget.items
	}
	budget.items -= count
	return count
}

func (budget *operationBudget) expired() bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return time.Now().After(budget.deadline)
}
