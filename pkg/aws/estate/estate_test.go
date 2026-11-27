package estate_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/estate"
	"github.com/VersusControl/versus-incident/pkg/aws/graph"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

var estateNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// readEstate syncs the four estate accounts from the fake into a snapshot
// store and returns the store and the fake.
func readEstate(t *testing.T) (*snapshot.Store, *fakeaws.Server) {
	t.Helper()
	fake := fakeaws.New(t, fakeaws.ScenarioEstateHubSpoke)
	fake.SetClock(func() time.Time { return estateNow })
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	collector := collect.New(store, limit.NewManager(1000), collect.WithHTTPClient(fake.HTTPClient()), collect.WithRetries(0),
		collect.WithClock(func() time.Time { return estateNow }),
		collect.WithSleep(func(context.Context, time.Duration) error { return nil }))
	for _, account := range fakeaws.EstateAccounts() {
		target := collect.Target{AccountID: account, Regions: []string{fakeaws.EstateHomeRegion},
			Credentials: credentials.NewStaticCredentialsProvider("AKIDESTATE", "estate-secret", "session-"+account)}
		if _, err := collector.Sync(context.Background(), target); err != nil {
			t.Fatalf("sync %s: %v", account, err)
		}
	}
	return store, fake
}

func estateScope() estate.ScopeFunc {
	return func(context.Context) ([]snapshot.ScopeAccount, bool, error) {
		var out []snapshot.ScopeAccount
		for _, account := range fakeaws.EstateAccounts() {
			out = append(out, snapshot.ScopeAccount{Account: account, Regions: []string{fakeaws.EstateHomeRegion}})
		}
		return out, true, nil
	}
}

// hubTransitGateway is the network hub's transit gateway, as read.
func hubTransitGateway(t *testing.T, store *snapshot.Store) string {
	t.Helper()
	result, err := store.Resources(context.Background(), snapshot.Query{Account: fakeaws.EstateHub, Type: "transit-gateway"})
	if err != nil || len(result.Resources) == 0 {
		t.Fatalf("hub transit gateway: %+v, %v", result.Resources, err)
	}
	return result.Resources[0].ARN
}

// Reading Overview and every Topology view, at every hop count, makes no
// call to AWS: the pages read the committed snapshots only.
func TestEstateReadsMakeNoAWSCall(t *testing.T) {
	store, fake := readEstate(t)
	service := estate.New(store, tenancy.DefaultOrgScope(), estateScope())
	ctx := context.Background()
	before := fake.TotalRequests()
	if before == 0 {
		t.Fatal("the estate sync made no request; the fake is not wired")
	}
	overview, err := service.Overview(ctx, estate.Scope{})
	if err != nil || !overview.Enabled || overview.Summary.Resources == nil || *overview.Summary.Resources == 0 {
		t.Fatalf("overview = %+v, %v", overview.Summary, err)
	}
	queries := []estate.TopologyQuery{{}, {Level: estate.LevelAccounts}, {Level: estate.LevelGroups, Account: fakeaws.EstateHub},
		{Level: estate.LevelAccounts, Lenses: []string{estate.LensIdentity}}}
	for hops := 1; hops <= graph.MaxHops; hops++ {
		queries = append(queries, estate.TopologyQuery{Level: estate.LevelFocus, Ref: hubTransitGateway(t, store), Hops: hops})
	}
	for _, query := range queries {
		if _, err := service.Topology(ctx, query); err != nil {
			t.Fatalf("topology %+v: %v", query, err)
		}
	}
	if found, err := service.Search(ctx, estate.SearchQuery{Text: "edge"}); err != nil || !found.Enabled {
		t.Fatalf("search = %+v, %v", found, err)
	}
	if after := fake.TotalRequests(); after != before {
		t.Fatalf("page reads made %d AWS requests; want none", after-before)
	}
}

// The accounts view of the estate names the hub and its spokes, draws the
// transit gateway links seen from both sides, and counts every connected
// account's resources as read.
func TestEstateAccountsViewNamesHubAndSpokes(t *testing.T) {
	store, _ := readEstate(t)
	service := estate.New(store, tenancy.DefaultOrgScope(), estateScope())
	view, err := service.Topology(context.Background(), estate.TopologyQuery{Level: estate.LevelAccounts})
	if err != nil || view.TooMany {
		t.Fatalf("accounts view = %+v, %v", view, err)
	}
	roles, details := map[string]string{}, map[string]string{}
	for _, node := range view.Nodes {
		if node.Kind == estate.NodeKindAccount && node.Read != "" {
			roles[node.Account], details[node.Account] = node.Role, node.RoleDetail
			if node.Resources == nil {
				t.Fatalf("connected account %s read without a resource count", node.Account)
			}
		}
	}
	if roles[fakeaws.EstateHub] != estate.RoleHub || roles[fakeaws.EstateWorkloads] != estate.RoleSpoke {
		t.Fatalf("roles = %v; want the hub and its spokes", roles)
	}
	if details[fakeaws.EstateHub] != "transit gateway" || details[fakeaws.EstateWorkloads] == "" {
		t.Fatalf("role details = %v; want the transit gateway that made the hub", details)
	}
	overview, err := service.Overview(context.Background(), estate.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	hubRow := false
	for _, row := range overview.Accounts {
		if row.ID == fakeaws.EstateHub {
			hubRow = row.Role == estate.RoleHub && row.RoleDetail == "transit gateway"
		}
	}
	if !hubRow || overview.Organization == nil || overview.Organization.ID != fakeaws.EstateOrgID || overview.Organization.Name == "" {
		t.Fatalf("overview hub row %v, organization %+v; want the hub's detail and the organization read", hubRow, overview.Organization)
	}
	both := false
	for _, link := range view.Links {
		if link.Type == model.RelationAttachedToTransit && link.From == graph.AccountEndpointPrefix+fakeaws.EstateWorkloads &&
			link.To == graph.AccountEndpointPrefix+fakeaws.EstateHub {
			both = link.SeenFrom == estate.SeenBoth
		}
	}
	if !both {
		t.Fatalf("the workloads attachment to the hub is not seen from both sides: %+v", view.Links)
	}
}

// addAttachments commits, in the hub, count network interfaces attached
// to its transit gateway: a hub that fans out to thousands of resources.
func addAttachments(t *testing.T, store *snapshot.Store, gateway string, count int) {
	t.Helper()
	resources := make([]model.Resource, 0, count)
	for index := range count {
		id := fmt.Sprintf("eni-%08d", index)
		resources = append(resources, model.Resource{ARN: "arn:aws:ec2:" + fakeaws.EstateHomeRegion + ":" + fakeaws.EstateHub + ":network-interface/" + id,
			Service: "ec2", Type: "network-interface", Account: fakeaws.EstateHub, Region: fakeaws.EstateHomeRegion, Name: id,
			Relations: []model.Relation{{Type: model.RelationAttachedTo, Target: gateway, Kind: model.RelationConfigured}}})
	}
	if _, err := store.Commit(model.Unit{Account: fakeaws.EstateHub, Region: fakeaws.EstateHomeRegion, Service: "ec2-interfaces"},
		snapshot.Candidate{Resources: resources}); err != nil {
		t.Fatal(err)
	}
}

// From the network hub's transit gateway at three hops, a hub with 5,000
// attached interfaces draws at most MaxFanOut new neighbours per node and
// never more than MaxNodes nodes; the walk's work is bounded by the cap,
// not the estate's size, and what the fan-out cap left out is counted.
func TestFocusFromTheHubIsBoundedAtThreeHops(t *testing.T) {
	store, _ := readEstate(t)
	gateway := hubTransitGateway(t, store)
	addAttachments(t, store, gateway, 5000)
	service := estate.New(store, tenancy.DefaultOrgScope(), estateScope())
	view, err := service.Topology(context.Background(), estate.TopologyQuery{Level: estate.LevelFocus, Ref: gateway, Hops: 3})
	if err != nil {
		t.Fatal(err)
	}
	if view.TooMany || len(view.Nodes) > graph.MaxNodes || !view.FanOutCapped {
		t.Fatalf("hops 3 from the hub: too many %v, %d nodes, fan-out capped %v; want a capped view within %d nodes",
			view.TooMany, len(view.Nodes), view.FanOutCapped, graph.MaxNodes)
	}
	hidden := 0
	for _, node := range view.Nodes {
		if node.ID == gateway {
			hidden = node.Hidden
		}
	}
	if hidden < 5000-graph.MaxFanOut {
		t.Fatalf("the gateway counts %d neighbours left out; want at least %d", hidden, 5000-graph.MaxFanOut)
	}

	// The walk itself: on the same graph, the work is bounded by the node
	// cap however large the hub's fan-out.
	result, err := store.Resources(context.Background(), snapshot.Query{PolicyEntries: true})
	if err != nil {
		t.Fatal(err)
	}
	var accounts []graph.Account
	for _, account := range fakeaws.EstateAccounts() {
		accounts = append(accounts, graph.Account{ID: account, Connected: true})
	}
	g := graph.Build(result.Resources, accounts, nil)
	walk := graph.Focus(g, gateway, 3, graph.MaxNodes, graph.MaxFanOut, nil)
	if walk.Expanded > graph.MaxNodes+1 || walk.Reached > graph.MaxNodes+1 {
		t.Fatalf("walk expanded %d nodes and reached %d; want both within %d", walk.Expanded, walk.Reached, graph.MaxNodes+1)
	}
	if len(walk.Graph.Nodes) > graph.MaxNodes {
		t.Fatalf("walk drew %d nodes", len(walk.Graph.Nodes))
	}
	if walk.TooMany || walk.Hidden[gateway] < 5000-graph.MaxFanOut {
		t.Fatalf("the fan-out cap did not count the gateway's hidden neighbours: %d", walk.Hidden[gateway])
	}
}

// Without the per-hop fan-out cap the hub alone reaches past the node cap
// at one hop: the view is too many, with no node, and says that no hop
// count fits, never a silently cut graph.
func TestFocusOverTheCapIsTooManyNotCut(t *testing.T) {
	store, _ := readEstate(t)
	gateway := hubTransitGateway(t, store)
	addAttachments(t, store, gateway, 500)
	result, err := store.Resources(context.Background(), snapshot.Query{PolicyEntries: true})
	if err != nil {
		t.Fatal(err)
	}
	g := graph.Build(result.Resources, nil, nil)
	walk := graph.Focus(g, gateway, 1, graph.MaxNodes, 10000, nil)
	if !walk.TooMany || len(walk.Graph.Nodes) != 0 || walk.FitsHops != 0 || walk.Reached <= graph.MaxNodes {
		t.Fatalf("walk = too many %v, %d nodes, fits %d, reached %d", walk.TooMany, len(walk.Graph.Nodes), walk.FitsHops, walk.Reached)
	}
}
