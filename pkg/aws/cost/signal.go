package cost

import (
	"context"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/finops/narrative"
)

// Logical signal names.
const (
	// SignalIncrease is a cost increase.
	SignalIncrease = "cost_increase"
	// SignalBudget is a budget that moved to At risk or Over budget.
	SignalBudget = "budget_status"
)

// BudgetSyncer is the optional budget half of the FinOps service: budgets
// whose status moved this month, and the record of each alert sent.
type BudgetSyncer interface {
	SyncBudgetAlerts(ctx context.Context) ([]finops.BudgetAlert, error)
	MarkBudgetAlerted(ctx context.Context, a finops.BudgetAlert) error
}

// IncreaseSyncer is the part of finops.Service the signal source needs:
// persist the current detection and return what must be alerted, then
// record each alert.
type IncreaseSyncer interface {
	SyncIncreases(ctx context.Context) ([]finops.IncreaseRecord, error)
	MarkIncreaseAlerted(ctx context.Context, id string) error
}

var _ IncreaseSyncer = (*finops.Service)(nil)

// SignalSource emits FinOps increases as signals so they reach Slack,
// Teams and email through the existing alert path. An increase is emitted
// once, and again only when its impact doubles; the alert is recorded only
// after the worker has made the signal durable (Commit), so a crash
// re-delivers rather than drops.
type SignalSource struct {
	name   string
	syncer IncreaseSyncer
	now    func() time.Time

	mu      sync.Mutex
	staged  []string
	budgets []finops.BudgetAlert
}

var (
	_ core.SignalSource    = (*SignalSource)(nil)
	_ core.SourceCommitter = (*SignalSource)(nil)
)

// NewSignalSource builds the "aws-cost:<deployment>" source.
func NewSignalSource(deployment string, syncer IncreaseSyncer, now func() time.Time) *SignalSource {
	if deployment == "" {
		deployment = "default"
	}
	if now == nil {
		now = time.Now
	}
	return &SignalSource{name: "aws-cost:" + deployment, syncer: syncer, now: now}
}

// Name is stable across restarts.
func (s *SignalSource) Name() string { return s.name }

// Pull reads the cache only (never AWS) and returns the increases due for
// an alert. The cursor is the pull time; the increase store, not the
// cursor, decides what is new.
func (s *SignalSource) Pull(ctx context.Context, since time.Time) ([]core.Signal, time.Time, error) {
	now := s.now()
	recs, err := s.syncer.SyncIncreases(ctx)
	if err != nil {
		return nil, since, err
	}
	out := make([]core.Signal, 0, len(recs))
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, increaseSignal(s.name, r, now))
		ids = append(ids, r.ID)
	}
	var alerts []finops.BudgetAlert
	if b, ok := s.syncer.(BudgetSyncer); ok {
		if alerts, err = b.SyncBudgetAlerts(ctx); err != nil {
			return nil, since, err
		}
		for _, a := range alerts {
			out = append(out, budgetSignal(s.name, a, now))
		}
	}
	s.mu.Lock()
	s.staged = append(s.staged, ids...)
	s.budgets = append(s.budgets, alerts...)
	s.mu.Unlock()
	cursor := now
	if since.After(cursor) {
		cursor = since
	}
	return out, cursor, nil
}

// Commit records the alerts of the signals the worker has stored.
func (s *SignalSource) Commit(ctx context.Context) error {
	s.mu.Lock()
	ids, budgets := s.staged, s.budgets
	s.staged, s.budgets = nil, nil
	s.mu.Unlock()
	if b, ok := s.syncer.(BudgetSyncer); ok {
		for i, a := range budgets {
			if err := b.MarkBudgetAlerted(ctx, a); err != nil {
				s.mu.Lock()
				s.budgets = append(append([]finops.BudgetAlert(nil), budgets[i:]...), s.budgets...)
				s.staged = append(append([]string(nil), ids...), s.staged...)
				s.mu.Unlock()
				return err
			}
		}
	}
	for i, id := range ids {
		if err := s.syncer.MarkIncreaseAlerted(ctx, id); err != nil {
			s.mu.Lock()
			s.staged = append(append([]string(nil), ids[i:]...), s.staged...)
			s.mu.Unlock()
			return err
		}
	}
	return nil
}

// increaseSignal carries only computed findings: no tag values and no raw
// cost lines.
func increaseSignal(source string, r finops.IncreaseRecord, now time.Time) core.Signal {
	label := narrative.ServiceLabel(r.Key.Service, r.ServiceName, narrative.Summary)
	msg := label + " in account " + r.Key.Account + " went up about " +
		narrative.Money(r.MonthlyImpact, "") + " a month, starting " + r.StartedOn.String() + "."
	return core.Signal{
		Source:    source,
		Timestamp: now,
		Severity:  "warning",
		Message:   msg,
		Fields: map[string]interface{}{
			core.FieldService:       r.Key.Service,
			core.FieldSignal:        SignalIncrease,
			"increase_id":           r.ID,
			"account":               r.Key.Account,
			"usage_type":            r.Key.UsageType,
			"started_on":            r.StartedOn.String(),
			"daily_before_micros":   r.DailyBefore,
			"daily_now_micros":      r.DailyNow,
			"monthly_impact_micros": r.MonthlyImpact,
		},
	}
}

// budgetSignal carries the computed budget figures only.
func budgetSignal(source string, a finops.BudgetAlert, now time.Time) core.Signal {
	word := "is at risk"
	if a.Status == finops.BudgetOver {
		word = "is over budget"
	}
	msg := "Budget " + a.Name + " " + word + " for " + a.Month + ": " + narrative.Money(a.Actual, a.Currency) +
		" so far, forecast " + narrative.Money(a.Forecast, a.Currency) + " against " + narrative.Money(a.Monthly, a.Currency) + "."
	return core.Signal{Source: source, Timestamp: now, Severity: "warning", Message: msg,
		Fields: map[string]interface{}{
			core.FieldSignal: SignalBudget, core.FieldService: "budget",
			"budget_id": a.BudgetID, "budget_name": a.Name, "status": a.Status, "month": a.Month,
			"monthly_micros": a.Monthly, "actual_micros": a.Actual, "forecast_micros": a.Forecast,
		}}
}
