package estate_test

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/estate"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	countsAccount = "123400000401"
	countsRegion  = "eu-south-2"
)

func countsService(t *testing.T) (*estate.Service, *snapshot.Store, string) {
	t.Helper()
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	function := "arn:aws:lambda:" + countsRegion + ":" + countsAccount + ":function:cart"
	if _, err := store.Commit(model.Unit{Account: countsAccount, Region: countsRegion, Service: "lambda"}, snapshot.Candidate{
		Resources: []model.Resource{{ARN: function, Service: "lambda", Type: "function", Account: countsAccount, Region: countsRegion,
			Name: "cart", Attributes: map[string]string{model.AttrStackName: "shop"}}}}); err != nil {
		t.Fatal(err)
	}
	scope := func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
		return []snapshot.ScopeAccount{{Account: countsAccount, Regions: []string{countsRegion}}}, true, nil
	}
	return estate.New(store, tenancy.DefaultOrgScope(), scope), store, function
}

// With no issue store and no change feed to read, the issue and change
// counts are not read (null) on Overview and on every card, never zero;
// the resource counts the snapshots hold are still known.
func TestEstateCountsNotReadWithoutTheirStores(t *testing.T) {
	service, _, function := countsService(t)
	ctx := context.Background()
	overview, err := service.Overview(ctx, estate.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	summary := overview.Summary
	if summary.ActNow != nil || summary.WorthALook != nil || summary.PeopleChanges24h != nil || summary.PeopleChangesCapped {
		t.Fatalf("summary without issues or changes = %+v; want those counts null", summary)
	}
	if summary.Resources == nil || *summary.Resources != 1 {
		t.Fatalf("resources = %v; want the known 1", summary.Resources)
	}
	if len(overview.Accounts) != 1 || overview.Accounts[0].OpenIssues != nil {
		t.Fatalf("account rows = %+v; want open issues null", overview.Accounts)
	}
	for _, query := range []estate.TopologyQuery{{Level: estate.LevelAccounts, Lenses: []string{estate.LensChanges}},
		{Level: estate.LevelGroups, Account: countsAccount, Lenses: []string{estate.LensChanges}},
		{Level: estate.LevelFocus, Ref: function, Lenses: []string{estate.LensChanges}}} {
		view, err := service.Topology(ctx, query)
		if err != nil {
			t.Fatalf("%+v: %v", query, err)
		}
		for _, node := range view.Nodes {
			if node.OpenIssues != nil || node.ActNow != nil || node.WorthALook != nil || node.Changes24h != nil {
				t.Fatalf("%s view, node %s = open %v act %v look %v changes %v; want every count null", query.Level, node.ID,
					node.OpenIssues, node.ActNow, node.WorthALook, node.Changes24h)
			}
		}
	}
}

// A resource whose Region's health read failed has health not read, never
// none; once a health read of its Region succeeds with no signal on it,
// none.
func TestFocusHealthNotReadWhenTheHealthReadFailed(t *testing.T) {
	service, store, function := countsService(t)
	ctx := context.Background()
	health := func() string {
		t.Helper()
		view, err := service.Topology(ctx, estate.TopologyQuery{Level: estate.LevelFocus, Ref: function})
		if err != nil {
			t.Fatal(err)
		}
		for _, node := range view.Nodes {
			if node.ID == function && node.Health != nil {
				return node.Health.State
			}
		}
		t.Fatalf("focus view has no health on %s: %+v", function, view.Nodes)
		return ""
	}
	if got := health(); got != estate.HealthNotRead {
		t.Fatalf("no health read = %q; want %q", got, estate.HealthNotRead)
	}
	unit := model.Unit{Account: countsAccount, Region: countsRegion, Service: "cloudwatch"}
	if err := store.RecordFailure(unit, "AccessDenied"); err != nil {
		t.Fatal(err)
	}
	if got := health(); got != estate.HealthNotRead {
		t.Fatalf("failed health read = %q; want %q", got, estate.HealthNotRead)
	}
	if _, err := store.Commit(unit, snapshot.Candidate{}); err != nil {
		t.Fatal(err)
	}
	if got := health(); got != estate.HealthNone {
		t.Fatalf("health read with no signal = %q; want %q", got, estate.HealthNone)
	}
}
