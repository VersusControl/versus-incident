package cost

import (
	"context"
	"sort"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
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
	cfg := FinOpsConfig(b.config())
	cfg.AccountScope = CallerAccounts
	svc := finops.NewService(b.adapter.Source(), b.store, cfg)
	if inv := b.adapter.options().Inventory; inv != nil {
		svc = svc.WithSuspectFinder(NewSuspectFinder(inv).WithResourceLines(b.adapter.Source()))
	}
	return svc
}

// CallerAccounts is the request caller's account scope, for
// finops.Config.AccountScope: the FinOps page and tools read only the
// accounts it lists. No scope reads every account.
func CallerAccounts(ctx context.Context) ([]string, bool) {
	scope := core.CallerAccountScope(ctx)
	if scope == nil {
		return nil, false
	}
	return scope.IDs, true
}

// OpenIncreaseServices are the provider service names with an open
// increase on a payer the deployment reads: the daily refresh reads
// resource-level cost for them, a paid call, so the suspect change is
// narrowed to the resource that billed. An increase of a payer that was
// removed, or one only an inherited organization still lists, pays for
// nothing and is left out.
func (b *Backend) OpenIncreaseServices(ctx context.Context) []string {
	if b == nil || b.store == nil {
		return nil
	}
	recs, err := b.store.ListIncreases(ctx)
	if err != nil {
		return nil
	}
	configured := map[string]bool{}
	var st *statusRecord
	if b.adapter != nil {
		for _, p := range b.adapter.Source().payers() {
			configured[p.AccountID] = true
		}
		if st, err = b.adapter.cache.status(); err != nil {
			return nil
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range recs {
		if b.adapter != nil && !configured[increasePayer(st, r.Key.Account)] {
			continue
		}
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

// increasePayer is the payer an increase's account bills through: the one
// the refresh state names, else the account itself.
func increasePayer(st *statusRecord, account string) string {
	if st != nil {
		if payer := payerOf(st, account); payer != "" {
			return payer
		}
	}
	return account
}
