package cost

import (
	"context"
	"sort"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// Backend is what the FinOps routes and the agent tools share: one
// finops.Service over the cost cache, built from the live configuration,
// and the paid operations only the admin routes may start.
type Backend struct {
	adapter *Adapter
	store   finops.Store
	config  func() config.AWSConnectorConfig
}

// NewBackend builds the backend. config returns the cloned live connector
// configuration.
func NewBackend(adapter *Adapter, store finops.Store, config func() config.AWSConnectorConfig) *Backend {
	return &Backend{adapter: adapter, store: store, config: config}
}

// Service is the FinOps service over the cache; it never calls AWS. With
// the inventory wired, increases name the change that likely started them.
func (b *Backend) Service() *finops.Service {
	svc := finops.NewService(b.adapter.Source(), b.store, FinOpsConfig(b.config()))
	if inv := b.adapter.options().Inventory; inv != nil {
		svc = svc.WithSuspectFinder(NewSuspectFinder(inv).WithResourceLines(b.adapter.Source()))
	}
	return svc
}

// OpenIncreaseServices are the provider service names with an open
// increase: the daily refresh reads resource-level cost for them, so the
// suspect change is narrowed to the resource that billed.
func (b *Backend) OpenIncreaseServices(ctx context.Context) []string {
	if b == nil || b.store == nil {
		return nil
	}
	recs, err := b.store.ListIncreases(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range recs {
		if r.Status == finops.StatusOpen && r.ServiceName != "" && !seen[r.ServiceName] {
			seen[r.ServiceName] = true
			out = append(out, r.ServiceName)
		}
	}
	sort.Strings(out)
	return out
}

// Reader is the read-only service for the agent tools.
func (b *Backend) Reader() finops.Reader { return b.Service() }

// Estimate prices a refresh without calling AWS.
func (b *Backend) Estimate(ctx context.Context, kind Kind) (finops.Plan, error) {
	return b.adapter.Estimate(ctx, kind)
}

// RefreshNow is the paid "Refresh now", at most once a day.
func (b *Backend) RefreshNow(ctx context.Context) (RefreshResult, error) {
	return b.adapter.Refresh(ctx, KindManual)
}

// DrillDown reads one account and service by usage type on click.
func (b *Backend) DrillDown(ctx context.Context, account, awsService string) error {
	return b.adapter.DrillDown(ctx, account, awsService)
}

// SuggestedCap is the daily request count full detail needs.
func (b *Backend) SuggestedCap(ctx context.Context) (int, error) { return b.adapter.SuggestedCap(ctx) }
