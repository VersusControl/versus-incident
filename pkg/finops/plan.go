package finops

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// The request planner keeps paid billing API use under one daily cap per
// deployment. It is provider-neutral: an adapter describes its calls, the
// planner prices and trims the plan before the first call, and the meter
// counts every request (each page included) and refuses past the cap.

// DefaultMaxPaidRequests is the daily cap per deployment.
const DefaultMaxPaidRequests = 20

// ErrCapReached is returned by the meter when a request would pass the cap.
var ErrCapReached = errors.New("finops: daily paid-request cap reached")

// Call is one planned paid call.
type Call struct {
	// Name is a stable call name ("cost_by_account", "forecast").
	Name string `json:"name"`
	// Group is the commit unit, usually one payer and day. The required
	// calls of a group commit together or not at all.
	Group string `json:"group"`
	// Priority orders execution; lower runs first.
	Priority int `json:"priority"`
	// Requests is the estimate, pages included.
	Requests int `json:"requests"`
	// Required calls make up the day's data. Optional calls are dropped
	// first when the plan does not fit.
	Required bool `json:"required"`
	// DropOrder orders optional calls for dropping: lower drops first.
	DropOrder int `json:"drop_order,omitempty"`
}

// Skip reasons.
const (
	SkipCap          = "cap"
	SkipGroupFailed  = "group_failed"
	SkipCapMidRun    = "cap_reached"
	SkipNotAttempted = "not_attempted"
)

// Skipped is a call left out of a plan or run.
type Skipped struct {
	Call   Call   `json:"call"`
	Reason string `json:"reason"`
}

// Plan is the priced, trimmed plan.
type Plan struct {
	Run       []Call    `json:"run"`
	Skipped   []Skipped `json:"skipped,omitempty"`
	Estimate  int       `json:"estimate"`  // requests in Run
	Requested int       `json:"requested"` // requests in the full plan
	Remaining int       `json:"remaining"` // budget before the run
}

// BuildPlan prices the full plan and trims it to the remaining budget
// before any call is made. Optional calls drop first, by DropOrder (then
// latest priority first); if the required calls still do not fit, whole
// groups drop, lowest priority group first, so no day is half-fetched.
func BuildPlan(calls []Call, remaining int) Plan {
	p := Plan{Remaining: remaining}
	run := append([]Call(nil), calls...)
	sort.SliceStable(run, func(i, j int) bool {
		if run[i].Priority != run[j].Priority {
			return run[i].Priority < run[j].Priority
		}
		return run[i].Name < run[j].Name
	})
	total := 0
	for _, c := range run {
		total += c.Requests
	}
	p.Requested = total

	drop := func(idx int, reason string) {
		total -= run[idx].Requests
		p.Skipped = append(p.Skipped, Skipped{Call: run[idx], Reason: reason})
		run = append(run[:idx], run[idx+1:]...)
	}

	for total > remaining {
		idx := -1
		for i, c := range run {
			if c.Required {
				continue
			}
			if idx < 0 || c.DropOrder < run[idx].DropOrder ||
				(c.DropOrder == run[idx].DropOrder && c.Priority >= run[idx].Priority) {
				idx = i
			}
		}
		if idx < 0 {
			break
		}
		drop(idx, SkipCap)
	}
	for total > remaining && len(run) > 0 {
		// Drop the group of the lowest-priority remaining call.
		g := run[len(run)-1].Group
		for i := len(run) - 1; i >= 0; i-- {
			if run[i].Group == g {
				drop(i, SkipCap)
			}
		}
	}
	p.Run = run
	p.Estimate = total
	return p
}

// Meter counts paid requests for one day and refuses past the cap. The cap
// is checked here, never by callers. Implementations persist the count;
// MemoryMeter is the in-process one.
type Meter interface {
	// Take reserves n requests or returns ErrCapReached without taking any.
	Take(ctx context.Context, n int) error
	// Used and Cap report today's use.
	Used(ctx context.Context) int
	Cap() int
}

// MemoryMeter is a Meter for one day, safe for concurrent use.
type MemoryMeter struct {
	mu   sync.Mutex
	cap  int
	used int
}

// NewMemoryMeter returns a meter with the cap and the requests already used
// today.
func NewMemoryMeter(capRequests, used int) *MemoryMeter {
	return &MemoryMeter{cap: capRequests, used: used}
}

func (m *MemoryMeter) Take(_ context.Context, n int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n <= 0 {
		return nil
	}
	if m.used+n > m.cap {
		return ErrCapReached
	}
	m.used += n
	return nil
}

func (m *MemoryMeter) Used(context.Context) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.used
}

func (m *MemoryMeter) Cap() int { return m.cap }

// CallResult is the outcome of one call.
type CallResult struct {
	Call      Call   `json:"call"`
	OK        bool   `json:"ok"`
	ErrorCode string `json:"error_code,omitempty"`
	Skipped   string `json:"skipped,omitempty"`
}

// RunOutcome is the outcome of executing a plan.
type RunOutcome struct {
	Results []CallResult `json:"results"`
	// Commit lists groups whose required calls all succeeded; only those
	// days may be written. Other groups keep their old data, marked stale.
	Commit map[string]bool `json:"commit"`
}

// CallFunc runs one call. It must take every request, each page included,
// from the meter before sending it, and it returns a closed-vocabulary
// error code with a non-nil error on failure. It never retries.
type CallFunc func(ctx context.Context, c Call, m Meter) (code string, err error)

// Execute runs the plan in priority order. A failed required call skips the
// rest of its group's required calls (the day cannot commit, so spending
// more on it is waste); optional calls commit on their own. Reaching the cap
// stops the run. Nothing is retried; the next scheduled refresh tries again.
func Execute(ctx context.Context, p Plan, m Meter, run CallFunc) RunOutcome {
	out := RunOutcome{Commit: map[string]bool{}}
	failed := map[string]bool{}
	groups := map[string]bool{}
	for _, c := range p.Run {
		if c.Required {
			groups[c.Group] = true
		}
	}
	capHit := false
	for _, c := range p.Run {
		r := CallResult{Call: c}
		switch {
		case capHit:
			r.Skipped = SkipCapMidRun
		case c.Required && failed[c.Group]:
			r.Skipped = SkipGroupFailed
		case ctx.Err() != nil:
			r.Skipped, r.ErrorCode = SkipNotAttempted, "timeout"
		default:
			code, err := run(ctx, c, m)
			if err == nil {
				r.OK = true
			} else {
				r.ErrorCode = code
				if errors.Is(err, ErrCapReached) {
					r.ErrorCode = SkipCapMidRun
					capHit = true
				}
			}
		}
		if !r.OK && c.Required {
			failed[c.Group] = true
		}
		out.Results = append(out.Results, r)
	}
	for _, s := range p.Skipped {
		if s.Call.Required {
			failed[s.Call.Group] = true
		}
	}
	for g := range groups {
		out.Commit[g] = !failed[g]
	}
	return out
}

// SkippedCalls names the calls a plan or run left out, for the page's
// "skipped today" note. Names are unique and sorted.
func SkippedCalls(p Plan, o RunOutcome) []string {
	set := map[string]bool{}
	for _, s := range p.Skipped {
		set[s.Call.Name] = true
	}
	for _, r := range o.Results {
		if r.Skipped != "" {
			set[r.Call.Name] = true
		}
	}
	return sortedKeys(set)
}
