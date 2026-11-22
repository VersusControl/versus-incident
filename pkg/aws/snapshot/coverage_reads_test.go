package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// unitsFailingProvider fails every unit read, the way a dropped database
// connection does.
type unitsFailingProvider struct {
	inventoryProvider
	err error
}

func (p *unitsFailingProvider) AWSUnits(context.Context, tenancy.OrgScope, storage.AWSUnitQuery) ([]storage.AWSUnitStatus, error) {
	return nil, p.err
}

// A storage failure is an error, never an empty view that reads as every
// source read in full.
func TestCoverageStorageFailureIsAnError(t *testing.T) {
	down := errors.New("connection reset")
	provider := &unitsFailingProvider{inventoryProvider: storage.NewMemory().(inventoryProvider), err: down}
	store := New(provider, tenancy.DefaultOrgScope())
	ctx := context.Background()

	view, err := store.Coverage(ctx, CoverageScope{Expected: []ScopeAccount{{Account: coverageAccounts[0]}}})
	if !errors.Is(err, down) {
		t.Fatalf("coverage error = %v, want the storage error; view = %+v", err, view)
	}
	warning, err := store.Warning(ctx, CoverageScope{})
	if !errors.Is(err, down) || warning != nil {
		t.Fatalf("warning = %+v, %v, want the storage error and no line", warning, err)
	}

	// A provider with no inventory tables is not ready, which is an error
	// too, never "nothing missing".
	bare := New(nil, tenancy.DefaultOrgScope())
	if _, err := bare.Coverage(ctx, CoverageScope{}); !errors.Is(err, ErrNoStorage) {
		t.Fatalf("coverage without inventory = %v, want ErrNoStorage", err)
	}
	if warning, err := bare.Warning(ctx, CoverageScope{}); !errors.Is(err, ErrNoStorage) || warning != nil {
		t.Fatalf("warning without inventory = %+v, %v", warning, err)
	}
}

// legacyGapsProvider serves the coverage document a unit stored before gaps
// carried counts: one gap per failed item, repeated, with no seen or total,
// and a cap recorded as not_checked.
type legacyGapsProvider struct {
	inventoryProvider
	documents map[string]string
}

func (p *legacyGapsProvider) AWSUnits(ctx context.Context, scope tenancy.OrgScope, query storage.AWSUnitQuery) ([]storage.AWSUnitStatus, error) {
	units, err := p.inventoryProvider.AWSUnits(ctx, scope, query)
	for index := range units {
		if document, ok := p.documents[units[index].Unit.Service]; ok {
			units[index].Sync.Gaps = json.RawMessage(document)
		}
	}
	return units, err
}

// Units stored before gaps were counted still decode and aggregate: their
// repeated per-item gaps make one row per source and operation, with no
// invented counts, and the old cap reason stays a not-read row.
func TestCoverageReadsGapsStoredBeforeCounts(t *testing.T) {
	provider := &legacyGapsProvider{inventoryProvider: storage.NewMemory().(inventoryProvider), documents: map[string]string{
		"sqs": `[{"account":"a","region":"us-east-1","service":"sqs","reason":"denied","operation":"GetQueueAttributes"},
			{"account":"a","region":"us-east-1","service":"sqs","reason":"denied","operation":"GetQueueAttributes"},
			{"account":"a","region":"us-east-1","service":"sqs","reason":"denied","operation":"GetQueueAttributes","last_good_at":"2026-11-20T08:00:00Z"}]`,
		"elbv2": `[{"account":"a","region":"us-east-1","service":"elbv2","reason":"not_checked","operation":"DescribeTargetHealth"}]`,
	}}
	store := New(provider, tenancy.DefaultOrgScope())
	for _, id := range coverageAccounts {
		for _, service := range []string{"sqs", "elbv2"} {
			mustCommit(t, store, model.Unit{Account: id, Region: region, Service: service},
				model.Gap{Reason: model.GapDenied, Operation: "placeholder"})
		}
	}

	view, err := store.Coverage(context.Background(), CoverageScope{})
	if err != nil {
		t.Fatal(err)
	}
	if view.Summary.NotRead != 2 || view.Summary.Capped != 0 || len(view.NotRead) != 2 {
		t.Fatalf("legacy coverage = %+v", view)
	}
	urls, ok := findRow(view.NotRead, "sqs", "GetQueueAttributes")
	if !ok || !slices.Equal(urls.Accounts, coverageAccounts) || urls.Seen != 0 || urls.Total != 0 ||
		!urls.LastGoodAt.IsZero() || !slices.Equal(urls.Fix, []string{"sqs:GetQueueAttributes"}) {
		t.Fatalf("legacy repeated gaps = %+v, want one uncounted row naming four accounts", urls)
	}
	health, ok := findRow(view.NotRead, "elbv2", "DescribeTargetHealth")
	if !ok || health.Outcome != model.OutcomeNotRead || health.Reason != model.GapNotChecked || len(health.Fix) != 0 || len(health.Accounts) != 4 {
		t.Fatalf("legacy cap = %+v, want a not-read row with no IAM fix", health)
	}
}

// A partial count sums across accounts, a denial with a capped read of the
// same operation stays two rows, and a stored reason this build does not
// know reads as not read, never as clear.
func TestCoverageCountsAndUnknownReasons(t *testing.T) {
	store := coverageStore(t)
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "sqs"},
		model.Gap{Reason: model.GapDenied, Operation: "GetQueueAttributes", Seen: 28, Total: 35},
		model.Gap{Reason: model.GapCapped, Operation: "GetQueueAttributes", Seen: 35, Total: 400})
	mustCommit(t, store, model.Unit{Account: coverageAccounts[1], Region: "eu-west-1", Service: "sqs"},
		model.Gap{Reason: model.GapDenied, Operation: "GetQueueAttributes", Seen: 2, Total: 5})
	mustCommit(t, store, model.Unit{Account: coverageAccounts[2], Region: region, Service: "kinesis"},
		model.Gap{Reason: "a_reason_from_a_newer_build"})

	view, err := store.Coverage(context.Background(), CoverageScope{})
	if err != nil {
		t.Fatal(err)
	}
	denied, ok := findRow(view.NotRead, "sqs", "GetQueueAttributes")
	if !ok || denied.Seen != 30 || denied.Total != 40 || len(denied.Accounts) != 2 {
		t.Fatalf("partial denial = %+v, want 30 of 40 over two accounts", denied)
	}
	capped, ok := findRow(view.Capped, "sqs", "GetQueueAttributes")
	if !ok || capped.Seen != 35 || capped.Total != 400 || len(capped.Accounts) != 1 {
		t.Fatalf("capped = %+v", capped)
	}
	unknown, ok := findRow(view.NotRead, "kinesis", "")
	if !ok || unknown.Outcome != model.OutcomeNotRead {
		t.Fatalf("unknown reason = %+v (listed %t), want not read", unknown, ok)
	}
	if view.Summary.NotRead != 2 || view.Summary.Capped != 1 {
		t.Fatalf("summary = %+v", view.Summary)
	}
	warning, err := store.Warning(context.Background(), CoverageScope{})
	if err != nil || warning == nil || warning.NotRead != 2 {
		t.Fatalf("warning = %+v, %v", warning, err)
	}

	// An account filter keeps only that account's share of the counts.
	one, err := store.Coverage(context.Background(), CoverageScope{Account: coverageAccounts[1]})
	if err != nil || len(one.NotRead) != 1 || one.NotRead[0].Seen != 2 || one.NotRead[0].Total != 5 {
		t.Fatalf("one account = %+v, %v", one, err)
	}
}

// The oldest last good read of a row wins, and one unit that never read
// clears it: a row never claims a newer read than its stalest account.
func TestCoverageLastGoodAtIsTheOldest(t *testing.T) {
	c := &clock{now: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
	store := New(storage.NewMemory(), tenancy.DefaultOrgScope(), WithClock(c.Now))
	older := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	mustCommit(t, store, model.Unit{Account: coverageAccounts[0], Region: region, Service: "ecs"},
		model.Gap{Reason: model.GapDenied, Operation: "DescribeServices", LastGoodAt: newer})
	mustCommit(t, store, model.Unit{Account: coverageAccounts[1], Region: region, Service: "ecs"},
		model.Gap{Reason: model.GapDenied, Operation: "DescribeServices", LastGoodAt: older})
	view, err := store.Coverage(context.Background(), CoverageScope{})
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := findRow(view.NotRead, "ecs", "DescribeServices"); !ok || !row.LastGoodAt.Equal(older) {
		t.Fatalf("row = %+v, want last good at %s", row, older)
	}
	mustCommit(t, store, model.Unit{Account: coverageAccounts[2], Region: region, Service: "ecs"},
		model.Gap{Reason: model.GapDenied, Operation: "DescribeServices"})
	view, err = store.Coverage(context.Background(), CoverageScope{})
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := findRow(view.NotRead, "ecs", "DescribeServices"); !ok || !row.LastGoodAt.IsZero() || len(row.Accounts) != 3 {
		t.Fatalf("row = %+v, want no last good read", row)
	}
}
