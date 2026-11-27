package estate

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/graph"
	"github.com/VersusControl/versus-incident/pkg/aws/grouping"
	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	readsAccount = "123400000301"
	readsRegion  = "eu-south-2"
)

// countingIssues counts the issue reads one by one; counts read through.
type countingIssues struct {
	storage.Provider
	storage.AWSIssueStore
	reads int
}

func (store *countingIssues) AWSIssues(ctx context.Context, scope tenancy.OrgScope, query storage.AWSIssueQuery) ([]storage.AWSIssueRecord, error) {
	store.reads++
	return store.AWSIssueStore.AWSIssues(ctx, scope, query)
}

// panickingLabels and panickingCorrections stand for the stores a search
// must not read.
type panickingLabels struct{ storage.AWSAccountLabelStore }

func (panickingLabels) AWSAccountLabels(context.Context, tenancy.OrgScope) ([]storage.AWSAccountLabel, error) {
	panic("a search read the account labels")
}

type panickingCorrections struct {
	storage.AWSGroupCorrectionStore
}

func (panickingCorrections) AWSGroupCorrections(context.Context, tenancy.OrgScope, string) ([]storage.AWSGroupCorrection, error) {
	panic("a search read the group corrections")
}

func readsFixture(t *testing.T, options ...Option) (*Service, *snapshot.Store) {
	t.Helper()
	provider := storage.NewMemory()
	store := snapshot.New(provider, tenancy.DefaultOrgScope())
	var resources []model.Resource
	for _, name := range []string{"checkout-web", "checkout-api", "billing"} {
		resources = append(resources, model.Resource{ARN: "arn:aws:lambda:" + readsRegion + ":" + readsAccount + ":function:" + name,
			Service: "lambda", Type: "function", Account: readsAccount, Region: readsRegion, Name: name,
			Attributes: map[string]string{model.AttrStackName: "shop"}})
	}
	if _, err := store.Commit(model.Unit{Account: readsAccount, Region: readsRegion, Service: "lambda"}, snapshot.Candidate{Resources: resources}); err != nil {
		t.Fatal(err)
	}
	scope := func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
		return []snapshot.ScopeAccount{{Account: readsAccount, Regions: []string{readsRegion}}}, true, nil
	}
	return New(store, tenancy.DefaultOrgScope(), scope, options...), store
}

// A search reads the scope and the stored resources only: no labels, no
// corrections, no graph and no grouping.
func TestSearchMakesNoHeavyRead(t *testing.T) {
	service, _ := readsFixture(t, WithAccountLabels(panickingLabels{}), WithGroupCorrections(panickingCorrections{}))
	graphs, groupings := 0, 0
	buildGraph = func([]model.Resource, []graph.Account, []graph.Label) graph.Graph {
		graphs++
		panic("a search built the graph")
	}
	buildGroups = func([]model.Resource, grouping.TagKeys, []grouping.Correction, grouping.Options) grouping.Result {
		groupings++
		panic("a search built the groups")
	}
	t.Cleanup(func() { buildGraph, buildGroups = graph.Build, grouping.Build })
	found, err := service.Search(context.Background(), SearchQuery{Text: "checkout", Scope: Scope{Accounts: []string{readsAccount}, Region: readsRegion}})
	if err != nil || len(found.Items) != 2 || graphs+groupings != 0 {
		t.Fatalf("search = %+v, %v; graph builds %d, grouping builds %d", found, err, graphs, groupings)
	}
}

// The Accounts view needs each account's issue summary only: it reads no
// issue one by one. A view that shows the account's groups does.
func TestAccountCardsReadNoIssueList(t *testing.T) {
	provider := storage.NewMemory()
	inner, ok := storage.AWSIssuesOf(provider)
	if !ok {
		t.Fatal("no issue store")
	}
	counting := &countingIssues{Provider: provider, AWSIssueStore: inner}
	actions, err := issues.NewActions(counting, tenancy.DefaultOrgScope(), nil)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := readsFixture(t, WithIssues(actions))
	if _, err := service.Topology(context.Background(), TopologyQuery{Level: LevelAccounts}); err != nil {
		t.Fatal(err)
	}
	if counting.reads != 0 {
		t.Fatalf("the accounts view read the issue list %d times; want none", counting.reads)
	}
	if _, err := service.Topology(context.Background(), TopologyQuery{Level: LevelGroups, Account: readsAccount}); err != nil {
		t.Fatal(err)
	}
	if counting.reads == 0 {
		t.Fatal("the groups view did not read the issue list it places on groups")
	}
}

// With no change feed to read, a Focus resource's last change is not
// known: never "no change".
func TestFocusLastChangeNotKnownWithoutAFeed(t *testing.T) {
	service, _ := readsFixture(t)
	view, err := service.Topology(context.Background(), TopologyQuery{Level: LevelFocus,
		Ref: "arn:aws:lambda:" + readsRegion + ":" + readsAccount + ":function:billing"})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range view.Nodes {
		if node.Kind == NodeKindResource && (node.LastChange != nil || node.LastChangeKnown == nil || *node.LastChangeKnown) {
			t.Fatalf("node %s = last change %+v, known %v; want not known", node.ID, node.LastChange, node.LastChangeKnown)
		}
	}
}
