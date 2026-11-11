package finops

import "context"

// Reader is the read-only half of Service: what the agent tools may call.
// It has no write, no status change, no alert bookkeeping and no paid
// call, so code holding a Reader cannot change state by construction.
type Reader interface {
	Summary(ctx context.Context, q Query) (SummaryView, error)
	Drivers(ctx context.Context, q Query) (BridgeView, error)
	Trend(ctx context.Context, q Query) (TrendView, error)
	Breakdown(ctx context.Context, q Query) (BreakdownView, error)
	Increases(ctx context.Context, q Query) (IncreasesView, error)
	Increase(ctx context.Context, q Query, id string) (IncreaseDetailView, error)
	Savings(ctx context.Context, q Query) (SavingsView, error)
	Budget(ctx context.Context, q Query) (BudgetView, error)
	Accounts(ctx context.Context, q Query) (AccountsView, error)
}

var _ Reader = (*Service)(nil)
