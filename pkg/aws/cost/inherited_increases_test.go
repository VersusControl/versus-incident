package cost

import (
	"context"
	"slices"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// An open increase the default organization kept for a payer removed under
// the licence never adds the paid resource-level call to the daily plan;
// an open increase of a payer the deployment reads still does.
func TestRemovedPayerIncreasePlansNoResourceCall(t *testing.T) {
	const removed, kept = "111111111111", "222222222222"
	ctx := context.Background()
	provider := storage.NewMemory()
	inherited := NewStore(provider, tenancy.DefaultOrgScope())
	open := func(id, account, service string) finops.IncreaseRecord {
		return finops.IncreaseRecord{Detected: finops.Detected{ID: id, Key: finops.LineKey{Account: account, Service: "compute", RecordType: "Usage"},
			ServiceName: service, StartedOn: finops.MustDate("2026-11-01")}, Status: finops.StatusOpen}
	}
	if err := inherited.PutIncrease(ctx, open("increase-removed", removed, "EC2 - Other")); err != nil {
		t.Fatal(err)
	}
	scope := tenancy.NewOrgScope("acme", tenancy.DefaultOrgID)
	adapter, err := New(provider, scope, Options{Payers: []Payer{{AccountID: kept}}})
	if err != nil {
		t.Fatal(err)
	}
	backend := NewBackend(adapter, NewStore(provider, scope), func() config.AWSConnectorConfig { return config.AWSConnectorConfig{} })
	plan := func() []string {
		o := adapter.options()
		o.ResourceServices = func() []string { return backend.OpenIncreaseServices(ctx) }
		st := &statusRecord{Payers: map[string]*payerState{kept: {HistoryStart: finops.MustDate("2026-09-01"), ClosedThrough: "2026-10"}}}
		var names []string
		for _, call := range planCalls(o.withDefaults(), st, nil, KindScheduled, finops.MustDate("2026-11-05"), 100).calls {
			names = append(names, call.Name)
		}
		return names
	}
	if got := backend.OpenIncreaseServices(ctx); len(got) != 0 {
		t.Fatalf("open increase services = %v; a removed payer's increase pays for nothing", got)
	}
	if calls := plan(); slices.Contains(calls, callResources) {
		t.Fatalf("daily plan %v holds a resource-level call for a removed payer's increase", calls)
	}
	if err := inherited.PutIncrease(ctx, open("increase-kept", kept, "Amazon RDS")); err != nil {
		t.Fatal(err)
	}
	if got := backend.OpenIncreaseServices(ctx); !slices.Equal(got, []string{"Amazon RDS"}) {
		t.Fatalf("open increase services = %v, want the read payer's service only", got)
	}
	if calls := plan(); !slices.Contains(calls, callResources) {
		t.Fatalf("daily plan %v has no resource-level call for a read payer's open increase", calls)
	}
}
