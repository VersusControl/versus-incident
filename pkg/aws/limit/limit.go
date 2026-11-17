// Package limit bounds how fast Versus calls AWS. Every read path shares one
// token bucket per account and region, and the same bucket feeds the API-use
// meter shown in settings. A throttling reply backs that bucket off so the
// next probe waits instead of piling on more requests.
package limit

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// Burst is the small allowance above the steady rate that lets a probe
	// sweep start without stretching over many seconds.
	Burst = 4
	// Window is the sliding window the usage meter reports over.
	Window = 60 * time.Second
	// baseBackoff is the first pause after a throttling reply; it doubles on
	// consecutive replies up to maxBackoff.
	baseBackoff = 2 * time.Second
	maxBackoff  = 30 * time.Second
)

// ErrBackoff means the account and region are pausing after a throttling
// reply. Callers report the probe as not checked rather than as a denial.
var ErrBackoff = errors.New("backoff")

// Usage is the API-use meter for one account: the granted request rate over
// the sliding window and the configured budget.
type Usage struct {
	UsedRPS   float64 `json:"used_rps"`
	BudgetRPS int     `json:"budget_rps"`
}

// Manager owns one limiter per account and region.
type Manager struct {
	mu       sync.Mutex
	maxRPS   int
	now      func() time.Time
	entries  map[string]*entry
	expected map[string]map[string]struct{}
}

type entry struct {
	account      string
	region       string
	limiter      *rate.Limiter
	grants       []time.Time
	backoff      time.Duration
	backoffUntil time.Time
}

// NewManager builds a manager whose limiters allow maxRPS steady requests per
// second with a burst of Burst. A non-positive rate is treated as one request
// per second so a missing setting cannot disable the guard.
func NewManager(maxRPS int) *Manager {
	return newManager(maxRPS, time.Now)
}

func newManager(maxRPS int, now func() time.Time) *Manager {
	if maxRPS < 1 {
		maxRPS = 1
	}
	return &Manager{
		maxRPS:   maxRPS,
		now:      now,
		entries:  make(map[string]*entry),
		expected: make(map[string]map[string]struct{}),
	}
}

// Expect registers the account's enabled regions so the budget reflects the
// whole account before the first probe reaches a region. The set replaces the
// previous selection, and it is the only input to the account's budget: a
// global service probe that lands in a region the account does not use must
// not enlarge the budget.
func (manager *Manager) Expect(accountID string, regions []string) {
	set := make(map[string]struct{}, len(regions))
	for _, region := range regions {
		set[region] = struct{}{}
		manager.entry(accountID, region)
	}

	manager.mu.Lock()
	manager.expected[accountID] = set
	manager.mu.Unlock()
}

// SetMaxRPS changes the steady rate for every limiter, including ones already
// created. A saved budget applies immediately instead of waiting for a
// restart.
func (manager *Manager) SetMaxRPS(maxRPS int) {
	if maxRPS < 1 {
		maxRPS = 1
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.maxRPS = maxRPS
	for _, item := range manager.entries {
		item.limiter.SetLimit(rate.Limit(maxRPS))
		item.limiter.SetBurst(Burst)
	}
}

// MaxRPS reports the steady rate every limiter currently allows.
func (manager *Manager) MaxRPS() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.maxRPS
}

// Wait grants one call for the account and region, blocking until the token
// bucket allows it. It returns ErrBackoff while a throttling pause is active.
func (manager *Manager) Wait(ctx context.Context, accountID, region string) error {
	item := manager.entry(accountID, region)

	manager.mu.Lock()
	paused := manager.now().Before(item.backoffUntil)
	manager.mu.Unlock()
	if paused {
		return ErrBackoff
	}

	if err := item.limiter.Wait(ctx); err != nil {
		return err
	}

	manager.mu.Lock()
	item.grants = append(item.grants, manager.now())
	manager.mu.Unlock()
	return nil
}

// Throttled records a throttling reply and pauses this account and region for
// a growing interval.
func (manager *Manager) Throttled(accountID, region string) time.Duration {
	item := manager.entry(accountID, region)

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if item.backoff == 0 {
		item.backoff = baseBackoff
	} else {
		item.backoff *= 2
		if item.backoff > maxBackoff {
			item.backoff = maxBackoff
		}
	}
	item.backoffUntil = manager.now().Add(item.backoff)
	return item.backoff
}

// Succeeded clears the throttling pause for this account and region.
func (manager *Manager) Succeeded(accountID, region string) {
	item := manager.entry(accountID, region)

	manager.mu.Lock()
	item.backoff = 0
	item.backoffUntil = time.Time{}
	manager.mu.Unlock()
}

// Usage sums the granted rate and the budget across every region of one
// account. The rate is the count of granted calls in the trailing Window
// divided by the window length, rounded to two decimals.
func (manager *Manager) Usage(accountID string) Usage {
	manager.mu.Lock()
	defer manager.mu.Unlock()

	cutoff := manager.now().Add(-Window)
	used := 0.0
	budget := 0
	regions := make(map[string]struct{})
	for _, item := range manager.entries {
		if item.account != accountID {
			continue
		}
		item.grants = prune(item.grants, cutoff)
		used += float64(len(item.grants)) / Window.Seconds()
		regions[item.region] = struct{}{}
	}
	expected := manager.expected[accountID]
	if len(expected) > 0 {
		budget = manager.maxRPS * len(expected)
	} else {
		// Without a registered selection, every region that has actually
		// taken a call is the best available proxy for the account.
		budget = manager.maxRPS * len(regions)
	}
	return Usage{
		UsedRPS:   math.Round(used*100) / 100,
		BudgetRPS: budget,
	}
}

func (manager *Manager) entry(accountID, region string) *entry {
	key := accountID + "\x00" + region

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if item, ok := manager.entries[key]; ok {
		return item
	}
	item := &entry{
		account: accountID,
		region:  region,
		limiter: rate.NewLimiter(rate.Limit(manager.maxRPS), Burst),
	}
	manager.entries[key] = item
	return item
}

func prune(grants []time.Time, cutoff time.Time) []time.Time {
	kept := grants[:0]
	for _, granted := range grants {
		if granted.After(cutoff) {
			kept = append(kept, granted)
		}
	}
	return kept
}
