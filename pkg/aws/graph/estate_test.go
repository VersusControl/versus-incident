package graph_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/VersusControl/versus-incident/pkg/aws/collect"
	"github.com/VersusControl/versus-incident/pkg/aws/graph"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

var estateNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// readEstate syncs the four estate accounts from the fake and returns what
// the inventory store holds.
func readEstate(t *testing.T) []model.Resource {
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
	result, err := store.Resources(context.Background(), snapshot.Query{PolicyEntries: true})
	if err != nil {
		t.Fatal(err)
	}
	return result.Resources
}

// fixtureAccount is the account the shared fixtures answer as for the
// calls the estate leaves to them (instances, alarms, stacks). Its links
// are not part of the estate.
const fixtureAccount = "111122223333"

type linkKey struct {
	from, to, far string
	kind          model.RelationType
	source        string
}

type linkCount struct{ count, both int }

func estateLinks(view graph.AccountsView) map[linkKey]linkCount {
	out := map[linkKey]linkCount{}
	for _, link := range view.Links {
		if link.From == fixtureAccount || link.To == fixtureAccount {
			continue
		}
		key := linkKey{from: link.From, to: link.To, far: link.Far, kind: link.Type, source: link.Source}
		got := out[key]
		out[key] = linkCount{count: got.count + link.Count, both: got.both + link.BothSides}
	}
	return out
}

func estateAccounts(disconnected string, at time.Time) []graph.Account {
	var accounts []graph.Account
	for _, id := range fakeaws.EstateAccounts() {
		accounts = append(accounts, graph.Account{ID: id, Connected: id != disconnected, LastReadAt: at})
	}
	return accounts
}

// The estate's link set at the Accounts level: the hub's transit gateway
// to every spoke, seen from both sides; the shared-services DNS names to
// load balancers and a distribution in the hub, the workloads and the data
// account, seen from both sides, and to a load balancer no connected
// account has, seen from one; role trusts to accounts outside, seen from
// one side; bucket, key and repository policies naming other estate
// accounts, seen from both.
func TestEstateGivesTheAccountsLinkSet(t *testing.T) {
	resources := readEstate(t)
	view := graph.Accounts(graph.Build(resources, estateAccounts("", estateNow), nil))
	const (
		hub, workloads, shared, data = fakeaws.EstateHub, fakeaws.EstateWorkloads, fakeaws.EstateShared, fakeaws.EstateData
		management, tooling          = fakeaws.EstateManagement, fakeaws.EstateSecurityTooling
		org                          = "org:" + fakeaws.EstateOrgID
		attachments, records         = "ec2:DescribeTransitGatewayAttachments", "route53:ListResourceRecordSets"
		trust, bucket, key, registry = "iam:ListRoles", "s3:GetBucketPolicy", "kms:GetKeyPolicy", "ecr:GetRepositoryPolicy"
		listed, associations         = "ram:ListResources", "ram:GetResourceShareAssociations"
	)
	transit, alias, trusts, access, shares := model.RelationAttachedToTransit, model.RelationAliasOf, model.RelationTrusts,
		model.RelationMayAccess, model.RelationSharedVia
	want := map[linkKey]linkCount{
		// Transit gateway: each spoke's attachment, read by the spoke and
		// by the hub; the peering to an outside account, read by the hub.
		{from: workloads, to: hub, kind: transit, source: attachments}:             {1, 1},
		{from: shared, to: hub, kind: transit, source: attachments}:                {1, 1},
		{from: data, to: hub, kind: transit, source: attachments}:                  {1, 1},
		{from: fakeaws.EstateOutside, to: hub, kind: transit, source: attachments}: {1, 0},
		// The transit share, received by each spoke and shared with the
		// organization's units; the resolver rules shared with shared
		// services.
		{from: hub, to: workloads, kind: shares, source: listed}:          {1, 1},
		{from: hub, to: shared, kind: shares, source: listed}:             {1, 1},
		{from: hub, to: data, kind: shares, source: listed}:               {1, 1},
		{from: hub, to: org, kind: shares, source: associations}:          {1, 1},
		{from: workloads, to: shared, kind: shares, source: associations}: {1, 1},
		// DNS: apex, api, cdn and the assets CNAME to the hub; the
		// wildcard and both payments names to the workloads; ingest to
		// data; legacy to a load balancer in no connected account.
		{from: shared, to: hub, kind: alias, source: records}:                       {4, 4},
		{from: shared, to: workloads, kind: alias, source: records}:                 {3, 3},
		{from: shared, to: data, kind: alias, source: records}:                      {1, 1},
		{from: shared, far: "accounts not connected", kind: alias, source: records}: {1, 0},
		// Role trusts: seen from both sides only where the trusted role was
		// read in its account; a trust to a whole account is seen from one.
		{from: hub, to: shared, kind: trusts, source: trust}:                                  {1, 1},
		{from: workloads, to: shared, kind: trusts, source: trust}:                            {1, 1},
		{from: shared, to: workloads, kind: trusts, source: trust}:                            {1, 0},
		{from: data, to: workloads, kind: trusts, source: trust}:                              {1, 0},
		{from: data, to: shared, kind: trusts, source: trust}:                                 {2, 1},
		{from: data, to: fakeaws.EstateVendor, kind: trusts, source: trust}:                   {1, 0},
		{from: data, to: fakeaws.EstateOutside, kind: trusts, source: trust}:                  {2, 0},
		{from: workloads, far: "a deleted or unknown principal", kind: trusts, source: trust}: {2, 0},
		// Resource policies keep the account they let in: the bucket grant to
		// the data account's ETL role and to its root is one link.
		{from: workloads, to: data, kind: access, source: bucket}:     {1, 1},
		{from: data, to: workloads, kind: access, source: bucket}:     {1, 1},
		{from: data, to: workloads, kind: access, source: key}:        {1, 1},
		{from: shared, to: workloads, kind: access, source: registry}: {1, 1},
		{from: shared, to: data, kind: access, source: registry}:      {1, 1},
		{from: shared, to: org, kind: access, source: registry}:       {1, 1},
	}
	// Every account's landing-zone admin and audit roles trust the
	// management and the security tooling accounts.
	for _, account := range fakeaws.EstateAccounts() {
		want[linkKey{from: account, to: management, kind: trusts, source: trust}] = linkCount{1, 0}
		want[linkKey{from: account, to: tooling, kind: trusts, source: trust}] = linkCount{1, 0}
	}
	got := estateLinks(view)
	for key, count := range want {
		if got[key] != count {
			t.Errorf("%+v = %+v, want %+v", key, got[key], count)
		}
	}
	for key, count := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("unexpected link %+v = %+v", key, count)
		}
	}
	labels := map[string]string{}
	for _, card := range view.Accounts {
		labels[card.ID] = card.Label
	}
	for id, label := range map[string]string{hub: graph.LabelConnected, management: graph.LabelSameOrg,
		fakeaws.EstateOutside: graph.LabelUnknown, fakeaws.EstateVendor: graph.LabelUnknown} {
		if labels[id] != label {
			t.Errorf("label of %s = %q, want %q", id, labels[id], label)
		}
	}
}

// Disconnecting the data account keeps every link it had, each now seen
// from one side with the time data was last read.
func TestEstateDisconnectedAccountKeepsItsLinks(t *testing.T) {
	resources := readEstate(t)
	before := estateLinks(graph.Accounts(graph.Build(resources, estateAccounts("", estateNow), nil)))
	lastRead := estateNow.Add(-3 * time.Hour)
	view := graph.Accounts(graph.Build(resources, estateAccounts(fakeaws.EstateData, lastRead), nil))
	after := estateLinks(view)
	if len(after) != len(before) {
		t.Fatalf("links = %d, want %d", len(after), len(before))
	}
	touched := 0
	for key, count := range before {
		got := after[key]
		if got.count != count.count {
			t.Errorf("%+v count = %d, want %d", key, got.count, count.count)
		}
		if key.from != fakeaws.EstateData && key.to != fakeaws.EstateData {
			if got.both != count.both {
				t.Errorf("%+v changed sides: %+v -> %+v", key, count, got)
			}
			continue
		}
		touched++
		if got.both != 0 {
			t.Errorf("%+v still seen from both sides: %+v", key, got)
		}
	}
	if touched < 8 {
		t.Fatalf("links touching data = %d", touched)
	}
	for _, link := range view.Links {
		if link.From == fixtureAccount {
			continue
		}
		if (link.From == fakeaws.EstateData || link.To == fakeaws.EstateData) && link.Count > link.BothSides && link.To != "" &&
			link.To != fakeaws.EstateVendor && link.To != fakeaws.EstateOutside && link.To != fakeaws.EstateManagement &&
			link.To != fakeaws.EstateSecurityTooling && !link.LastReadAt.Equal(lastRead) {
			t.Errorf("%+v has no last read time of the data account", link)
		}
	}
}

// Policy documents, trust conditions and record values are reduced before
// anything is stored: no ExternalId, statement or record value survives.
func TestEstateStoresNoPolicyDocumentOrRecordValue(t *testing.T) {
	data, err := json.Marshal(readEstate(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{fakeaws.EstateExternalIDValue, "Statement", "sts:ExternalId", "aws:SourceArn", "acm-validations.aws",
		"rds.amazonaws.com", "awsdns-hostmaster", "kms:ViaService", "token.actions.githubusercontent.com:sub"} {
		if strings.Contains(string(data), leaked) {
			t.Errorf("stored inventory carries %q", leaked)
		}
	}
}
