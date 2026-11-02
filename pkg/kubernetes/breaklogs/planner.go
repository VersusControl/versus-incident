package breaklogs

import (
	"context"
	"sync"
	"time"
)

type linkedBudgetKey struct{}
type linkedBudget struct {
	mu        sync.Mutex
	remaining int
}

// AdmitLinkedCall is shared by all pod reads and time splits in one query.
func AdmitLinkedCall(ctx context.Context) bool {
	budget, _ := ctx.Value(linkedBudgetKey{}).(*linkedBudget)
	if budget == nil {
		return true
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.remaining == 0 {
		return false
	}
	budget.remaining--
	return true
}

// ReadWindow spends a whole-query budget, not a per-split line budget.
// Only disjoint, non-truncated leaves can claim coverage.
// Three logical queries and 6000 returned lines are ceilings, not a promise of
// completeness. Native adapters batch the verified Pod set in each query and
// separately bound pages/lifecycle requests; truncation leaves explicit gaps.
func ReadWindow(ctx context.Context, reader LinkedReader, query LinkedQuery) (Result, error) {
	if err := query.Validate(); err != nil {
		return Result{}, err
	}
	pending := []LinkedQuery{query}
	ctx = context.WithValue(ctx, linkedBudgetKey{}, &linkedBudget{remaining: 3})
	result := Result{}
	linesLeft := 6000
	for calls := 0; calls < 3 && len(pending) > 0 && linesLeft > 0; calls++ {
		next := pending[0]
		pending = pending[1:]
		next.Limit = min(2000, linesLeft)
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		part, err := reader.Read(callCtx, next)
		cancel()
		if err != nil {
			result.Truncated = true
			return result, err
		}
		if len(part.Lines) > next.Limit {
			part.Lines = part.Lines[:next.Limit]
			part.Truncated = true
		}
		linesLeft -= len(part.Lines)
		for _, line := range part.Lines {
			if !line.At.Before(next.From) && !line.At.After(next.To) {
				result.Lines = append(result.Lines, line)
			}
		}
		if !part.Truncated {
			result.Complete = append(result.Complete, Gap{From: next.From, To: next.To})
			continue
		}
		middle := next.From.Add(next.To.Sub(next.From) / 2)
		if !middle.After(next.From) || !next.To.After(middle) {
			result.Truncated = true
			continue
		}
		newest, oldest := next, next
		newest.From = middle
		oldest.To = middle.Add(-time.Nanosecond)
		pending = append([]LinkedQuery{newest, oldest}, pending...)
	}
	result.Truncated = result.Truncated || len(pending) > 0
	return result, nil
}
