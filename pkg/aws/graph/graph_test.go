package graph

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

const (
	hub   = "123400000001"
	spoke = "123400000002"
	dns   = "123400000003"
	away  = "999900000009"
)

var lastRead = time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)

func res(account, arn, service, typ, name string, relations ...model.Relation) model.Resource {
	return model.Resource{ARN: arn, Account: account, Region: "eu-south-2", Service: service, Type: typ, Name: name, Relations: relations}
}

func rel(kind model.RelationType, how model.RelationKind, target string) model.Relation {
	return model.Relation{Type: kind, Kind: how, Target: target}
}

func connected(ids ...string) []Account {
	out := make([]Account, 0, len(ids))
	for _, id := range ids {
		out = append(out, Account{ID: id, Connected: true, LastReadAt: lastRead})
	}
	return out
}

func edgeBetween(t *testing.T, g Graph, from, to string, kind model.RelationType) Edge {
	t.Helper()
	for _, edge := range g.Edges {
		if edge.From == from && edge.To == to && edge.Type == kind {
			return edge
		}
	}
	t.Fatalf("no %s edge %s -> %s in %+v", kind, from, to, g.Edges)
	return Edge{}
}

func nodeOf(t *testing.T, g Graph, id string) Node {
	t.Helper()
	for _, node := range g.Nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("no node %s", id)
	return Node{}
}

// One table per link kind: the same stored evidence, read with the far
// account connected and not, gives a link seen from both sides or from one.
func TestJoinSidesPerLinkKind(t *testing.T) {
	tgw := "arn:aws:ec2:eu-south-2:" + hub + ":transit-gateway/tgw-1"
	attachment := "arn:aws:ec2:eu-south-2:" + spoke + ":transit-gateway-attachment/tgw-attach-2"
	attach := func(reader string) model.Resource {
		return res(reader, attachment, "transit-gateway", "transit-gateway-attachment", "tgw-attach-2",
			rel(model.RelationAttachedToTransit, model.RelationConfigured, tgw))
	}
	balancer := "arn:aws:elasticloadbalancing:eu-south-2:" + spoke + ":loadbalancer/app/web/1"
	lb := res(spoke, balancer, "elbv2", "load-balancer", "web")
	lb.Attributes = map[string]string{model.AttrDNSName: "web-1.eu-south-2.elb.amazonaws.com"}
	record := res(dns, "arn:aws:route53:::hostedzone/Z1/record/www.example.com", "route53", "dns-record", "www.example.com",
		rel(model.RelationAliasOf, model.RelationConfigured, "web-1.eu-south-2.elb.amazonaws.com"))
	caller := res(spoke, "arn:aws:iam::"+spoke+":role/ci", "iam", "role", "ci")
	trusting := res(hub, "arn:aws:iam::"+hub+":role/deploy", "iam", "role", "deploy",
		rel(model.RelationTrusts, model.RelationKindMayAccess, "arn:aws:iam::"+spoke+":role/ci"))
	bucket := res(hub, "arn:aws:s3:::exports", "s3", "bucket", "exports")
	policy := res(hub, "arn:aws:s3:::exports#policy", "s3-policy", "resource-policy", "exports",
		model.Relation{Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Target: spoke, Access: model.AccessRead})
	policy.Attributes = map[string]string{model.AttrSubject: "arn:aws:s3:::exports"}
	share := "arn:aws:ram:eu-south-2:" + hub + ":resource-share/s-1"
	owned := res(hub, share, "ram", "resource-share", "s-1", rel(model.RelationSharedVia, model.RelationConfigured, spoke))
	received := res(spoke, share, "ram", "resource-share", "s-1", rel(model.RelationSharedVia, model.RelationConfigured, spoke))

	cases := []struct {
		name      string
		resources []model.Resource
		from, to  string
		kind      model.RelationType
		far       string
	}{
		{"transit attachment read by both", []model.Resource{attach(hub), attach(spoke)}, attachment, tgw, model.RelationAttachedToTransit, spoke},
		{"dns record and load balancer", []model.Resource{record, lb}, record.ARN, balancer, model.RelationAliasOf, spoke},
		{"trust and caller role", []model.Resource{trusting, caller}, trusting.ARN, caller.ARN, model.RelationTrusts, spoke},
		{"bucket policy and named account", []model.Resource{bucket, policy, caller}, bucket.ARN, "account:" + spoke, model.RelationMayAccess, spoke},
		{"resource share and receiver", []model.Resource{owned, received}, share, "account:" + spoke, model.RelationSharedVia, spoke},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all := connected(hub, spoke, dns)
			if got := edgeBetween(t, Build(tc.resources, all, nil), tc.from, tc.to, tc.kind); got.Sides != BothSides {
				t.Fatalf("both connected: sides = %q", got.Sides)
			}
			var partial []Account
			for _, account := range all {
				if account.ID == tc.far {
					account.Connected = false
				}
				partial = append(partial, account)
			}
			got := edgeBetween(t, Build(tc.resources, partial, nil), tc.from, tc.to, tc.kind)
			if got.Sides != OneSide || !got.LastReadAt.Equal(lastRead) {
				t.Fatalf("far side disconnected: sides = %q, last read %v", got.Sides, got.LastReadAt)
			}
		})
	}
}

// Only one end read: the attachment seen by the hub alone, a record whose
// host no account answers to, a trust to an account never connected.
func TestOneSideEndsOnADanglingNode(t *testing.T) {
	tgw := "arn:aws:ec2:eu-south-2:" + hub + ":transit-gateway/tgw-1"
	attachment := "arn:aws:ec2:eu-south-2:" + away + ":transit-gateway-attachment/tgw-attach-9"
	resources := []model.Resource{
		res(hub, attachment, "transit-gateway", "transit-gateway-attachment", "tgw-attach-9",
			rel(model.RelationAttachedToTransit, model.RelationConfigured, tgw),
			rel(model.RelationAttachedTo, model.RelationConfigured, "arn:aws:ec2:eu-south-2:"+away+":vpc/vpc-9")),
		res(dns, "arn:aws:route53:::hostedzone/Z1/record/legacy.example.com", "route53", "dns-record", "legacy.example.com",
			rel(model.RelationAliasOf, model.RelationConfigured, "legacy-9.eu-south-2.elb.amazonaws.com")),
		res(hub, "arn:aws:iam::"+hub+":role/vendor", "iam", "role", "vendor", rel(model.RelationTrusts, model.RelationKindMayAccess, away)),
	}
	g := Build(resources, connected(hub, dns), nil)
	if edge := edgeBetween(t, g, attachment, tgw, model.RelationAttachedToTransit); edge.Sides != OneSide {
		t.Fatalf("attachment sides = %q", edge.Sides)
	}
	if node := nodeOf(t, g, "arn:aws:ec2:eu-south-2:"+away+":vpc/vpc-9"); !node.Dangling || node.Description != "a network in account "+away+" (not connected)" {
		t.Fatalf("vpc node = %+v", node)
	}
	host := nodeOf(t, g, "host:legacy-9.eu-south-2.elb.amazonaws.com")
	if !host.Dangling || host.Description != "a load balancer in an account not connected" {
		t.Fatalf("host node = %+v", host)
	}
	if edge := edgeBetween(t, g, "arn:aws:iam::"+hub+":role/vendor", "account:"+away, model.RelationTrusts); edge.Sides != OneSide {
		t.Fatalf("trust sides = %q", edge.Sides)
	}
	if node := nodeOf(t, g, "account:"+away); node.Description != "account "+away+" (not connected)" {
		t.Fatalf("account node = %+v", node)
	}
}

// Route 53 writes an alias with a dualstack prefix and a trailing dot, in
// any case; the load balancer reports its plain name. All match.
func TestDNSFormsMatchTheLoadBalancer(t *testing.T) {
	balancer := "arn:aws:elasticloadbalancing:eu-south-2:" + spoke + ":loadbalancer/app/web/1"
	lb := res(spoke, balancer, "elbv2", "load-balancer", "web")
	lb.Attributes = map[string]string{model.AttrDNSName: "Web-1.eu-south-2.elb.amazonaws.com"}
	for _, form := range []string{
		"web-1.eu-south-2.elb.amazonaws.com",
		"web-1.eu-south-2.elb.amazonaws.com.",
		"dualstack.web-1.eu-south-2.elb.amazonaws.com.",
		"DualStack.WEB-1.eu-south-2.elb.amazonaws.com",
	} {
		record := res(dns, "arn:aws:route53:::hostedzone/Z1/record/www", "route53", "dns-record", "www",
			rel(model.RelationAliasOf, model.RelationConfigured, form))
		g := Build([]model.Resource{record, lb}, connected(spoke, dns), nil)
		if edge := edgeBetween(t, g, record.ARN, balancer, model.RelationAliasOf); edge.Sides != BothSides {
			t.Fatalf("%q: sides = %q", form, edge.Sides)
		}
	}
	if got := model.NormalizeDNSName(" DualStack.Web-1.eu-south-2.elb.amazonaws.com. "); got != "web-1.eu-south-2.elb.amazonaws.com" {
		t.Fatalf("normalized = %q", got)
	}
}

// 910 tables granting one role read give one link with the count and the
// prefix they share; a grant written as a prefix pattern is one link.
func TestWideGrantsCollapseToOneLink(t *testing.T) {
	role := "arn:aws:iam::" + spoke + ":role/reporting"
	var resources []model.Resource
	for index := range 910 {
		name := fmt.Sprintf("prod-orders-%04d", index)
		resources = append(resources, res(hub, "arn:aws:dynamodb:eu-south-2:"+hub+":table/"+name, "dynamodb", "table", name,
			model.Relation{Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Target: role, Access: model.AccessRead}))
	}
	resources = append(resources, res(hub, "arn:aws:s3:::logs", "s3", "bucket", "logs",
		model.Relation{Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Target: role, Access: model.AccessRead, Pattern: "prod-*"}))
	g := Build(resources, connected(hub, spoke), nil)
	var toRole []Edge
	for _, edge := range g.Edges {
		if edge.To == role || edge.To == "account:"+spoke {
			toRole = append(toRole, edge)
		}
	}
	if len(toRole) != 2 {
		t.Fatalf("edges to the role = %d, want 2: %+v", len(toRole), toRole)
	}
	var group Edge
	for _, edge := range toRole {
		if edge.From != "arn:aws:s3:::logs" {
			group = edge
		} else if edge.Pattern != "prod-*" || edge.Count != 1 {
			t.Fatalf("prefix grant = %+v", edge)
		}
	}
	if group.Count != 910 || group.Pattern != "prod-orders-*" {
		t.Fatalf("collapsed grant = %+v", group)
	}
	node := nodeOf(t, g, group.From)
	if node.Type != NodeGroup || len(node.Members) != 910 || node.Name != "table matching prod-orders-*" {
		t.Fatalf("group node = %s %q members=%d", node.Type, node.Name, len(node.Members))
	}
	// A member is still a place to start a focus view from.
	focus := Neighborhood(g, "arn:aws:dynamodb:eu-south-2:"+hub+":table/prod-orders-0007", 2, 10)
	if len(focus.Nodes) != 3 || len(focus.Edges) != 2 {
		t.Fatalf("focus from a member = %+v", focus)
	}
	if edge := edgeBetween(t, focus, group.From, role, model.RelationMayAccess); edge.Count != 910 {
		t.Fatalf("focus grant = %+v", edge)
	}
	view := Accounts(g)
	if len(view.Links) != 1 || view.Links[0].Count != 911 {
		t.Fatalf("accounts links = %+v", view.Links)
	}
}

// A trust on a whole organization is one organization-level link; a
// deleted principal reads as such, never as an unknown account.
func TestOrganizationAndUnknownPrincipalTrusts(t *testing.T) {
	org := res(hub, "arn:aws:organizations::123400000090:organization/o-a1b2c3d4e5", "organizations", "organization", "o-a1b2c3d4e5")
	org.Attributes = map[string]string{model.AttrOrganizationID: "o-a1b2c3d4e5", model.AttrManagementAccount: "123400000090"}
	role := res(hub, "arn:aws:iam::"+hub+":role/shared", "iam", "role", "shared",
		rel(model.RelationTrusts, model.RelationKindMayAccess, model.OrgTarget("o-a1b2c3d4e5")),
		rel(model.RelationTrusts, model.RelationKindMayAccess, "AROAEXAMPLEDELETED001"),
		rel(model.RelationTrusts, model.RelationKindMayAccess, "123400000090"))
	g := Build([]model.Resource{org, role}, connected(hub), nil)
	if edge := edgeBetween(t, g, role.ARN, "org:o-a1b2c3d4e5", model.RelationTrusts); edge.Sides != BothSides {
		t.Fatalf("organization trust sides = %q", edge.Sides)
	}
	deleted := nodeOf(t, g, "principal:AROAEXAMPLEDELETED001")
	if deleted.Account != "" || deleted.Description != "a deleted or unknown principal" {
		t.Fatalf("deleted principal = %+v", deleted)
	}
	view := Accounts(g)
	want := map[string]string{"org:o-a1b2c3d4e5": "", "": "a deleted or unknown principal", "123400000090": ""}
	if len(view.Links) != len(want) {
		t.Fatalf("links = %+v", view.Links)
	}
	for _, link := range view.Links {
		if far, ok := want[link.To]; !ok || far != link.Far {
			t.Fatalf("unexpected link %+v", link)
		}
	}
	labels := map[string]string{}
	for _, card := range view.Accounts {
		labels[card.ID] = card.Label
	}
	if labels["123400000090"] != LabelSameOrg || labels[hub] != LabelConnected {
		t.Fatalf("labels = %v", labels)
	}
}

// Labels: connected wins, then the organization's management account, then
// a user's label or an AWS service account, then unknown. A user's name is
// kept on any label.
func TestAccountLabels(t *testing.T) {
	org := res(hub, "arn:aws:organizations::123400000090:organization/o-a1b2c3d4e5", "organizations", "organization", "o")
	org.Attributes = map[string]string{model.AttrOrganizationID: "o-a1b2c3d4e5", model.AttrManagementAccount: "123400000090"}
	role := res(hub, "arn:aws:iam::"+hub+":role/r", "iam", "role", "r",
		rel(model.RelationTrusts, model.RelationKindMayAccess, "123400000090"),
		rel(model.RelationTrusts, model.RelationKindMayAccess, "555500006666"),
		rel(model.RelationTrusts, model.RelationKindMayAccess, "127311923021"),
		rel(model.RelationTrusts, model.RelationKindMayAccess, away))
	labels := []Label{{AccountID: "555500006666", Label: LabelKnown, Name: "Datadog"}, {AccountID: "123400000090", Label: LabelKnown, Name: "management"}}
	g := Build([]model.Resource{org, role}, connected(hub), labels)
	want := map[string][2]string{
		hub:            {LabelConnected, ""},
		"123400000090": {LabelSameOrg, "management"},
		"555500006666": {LabelKnown, "Datadog"},
		"127311923021": {LabelKnown, "Elastic Load Balancing (us-east-1)"},
		away:           {LabelUnknown, ""},
	}
	for _, card := range g.Accounts {
		if got := [2]string{card.Label, card.Name}; got != want[card.ID] {
			t.Errorf("%s = %v, want %v", card.ID, got, want[card.ID])
		}
	}
	if len(g.Accounts) != len(want) {
		t.Fatalf("cards = %+v", g.Accounts)
	}
}

func TestNeighborhoodBoundsHopsAndNodes(t *testing.T) {
	var resources []model.Resource
	for index := range 6 {
		arn := fmt.Sprintf("arn:aws:sqs:eu-south-2:%s:q%d", hub, index)
		next := fmt.Sprintf("arn:aws:sqs:eu-south-2:%s:q%d", hub, index+1)
		resources = append(resources, res(hub, arn, "sqs", "queue", fmt.Sprint(index), rel(model.RelationDeadLetter, model.RelationConfigured, next)))
	}
	g := Build(resources, connected(hub), nil)
	root := "arn:aws:sqs:eu-south-2:" + hub + ":q2"
	if got := Neighborhood(g, root, 1, 0); len(got.Nodes) != 3 || len(got.Edges) != 2 || got.Truncated {
		t.Fatalf("one hop = %d nodes, %d edges", len(got.Nodes), len(got.Edges))
	}
	if got := Neighborhood(g, root, 9, 0); len(got.Nodes) != 6 {
		t.Fatalf("hops cap at %d: %d nodes", MaxHops, len(got.Nodes))
	}
	if got := Neighborhood(g, root, 3, 2); len(got.Nodes) != 2 || !got.Truncated {
		t.Fatalf("node cap: %d nodes truncated=%v", len(got.Nodes), got.Truncated)
	}
	if got := Neighborhood(g, "arn:aws:sqs:eu-south-2:"+hub+":missing", 2, 0); len(got.Nodes) != 0 {
		t.Fatalf("missing ref = %+v", got)
	}
}

// Only a policy grant is seen from both sides because the account it names
// is connected. A trust needs the trusted role read in its account, and a
// share needs the far account to have read it as received.
func TestConnectedAccountAloneJoinsOnlyPolicyGrants(t *testing.T) {
	rootTrust := res(hub, "arn:aws:iam::"+hub+":role/deploy", "iam", "role", "deploy",
		rel(model.RelationTrusts, model.RelationKindMayAccess, spoke))
	share := "arn:aws:ram:eu-south-2:" + hub + ":resource-share/s-1"
	owned := res(hub, share, "ram", "resource-share", "s-1", rel(model.RelationSharedVia, model.RelationConfigured, spoke))
	bucket := res(hub, "arn:aws:s3:::exports", "s3", "bucket", "exports",
		model.Relation{Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Target: spoke, Access: model.AccessRead})
	other := res(spoke, "arn:aws:iam::"+spoke+":role/other", "iam", "role", "other")
	g := Build([]model.Resource{rootTrust, owned, bucket, other}, connected(hub, spoke), nil)
	if edge := edgeBetween(t, g, rootTrust.ARN, "account:"+spoke, model.RelationTrusts); edge.Sides != OneSide {
		t.Errorf("trust to a connected account's root: sides = %q", edge.Sides)
	}
	if edge := edgeBetween(t, g, share, "account:"+spoke, model.RelationSharedVia); edge.Sides != OneSide {
		t.Errorf("share the receiver did not read: sides = %q", edge.Sides)
	}
	if edge := edgeBetween(t, g, bucket.ARN, "account:"+spoke, model.RelationMayAccess); edge.Sides != BothSides {
		t.Errorf("grant to a connected account: sides = %q", edge.Sides)
	}
}

// A trail the cost read keeps is not part of the topology: no node, even
// when the trail is another account's organization trail.
func TestTrailsAreNotNodes(t *testing.T) {
	trail := model.Resource{ARN: "arn:aws:cloudtrail:us-east-1:123400000090:trail/org-trail", Service: model.TrailService, Type: model.TrailType,
		Account: "123400000002", Region: "eu-south-2", Name: "org-trail", Attributes: map[string]string{model.AttrOwnerAccount: "123400000090"}}
	function := model.Resource{ARN: "arn:aws:lambda:eu-south-2:123400000002:function:api", Service: "lambda", Type: "function",
		Account: "123400000002", Region: "eu-south-2", Name: "api"}
	g := Build([]model.Resource{trail, function}, []Account{{ID: "123400000002", Connected: true}}, nil)
	for _, node := range g.Nodes {
		if node.ID == trail.ARN || node.Type == model.TrailType {
			t.Fatalf("a trail is a node: %+v", node)
		}
	}
	if len(g.Nodes) == 0 {
		t.Fatal("the function is missing too: the graph is empty")
	}
}

// A resource whose policy a call budget did not read shows "policy not
// read" on its node, never a resource with no grants.
func TestUnreadPolicyShowsOnTheNode(t *testing.T) {
	bucket := "arn:aws:s3:::logs"
	entry := model.Resource{ARN: bucket + "#policy", Service: model.PolicyEntryService, Type: model.ResourcePolicyType, Account: "123400000002",
		Region: "global", Name: "logs", Attributes: map[string]string{model.AttrSubject: bucket, model.AttrNotRead: "GetBucketPolicy"}}
	key := model.Resource{ARN: "arn:aws:kms:eu-south-2:123400000002:key/1", Service: "kms", Type: "key", Account: "123400000002",
		Region: "eu-south-2", Name: "1", Attributes: map[string]string{model.AttrNotRead: "GetKeyPolicy"}}
	read := model.Resource{ARN: "arn:aws:kms:eu-south-2:123400000002:key/2", Service: "kms", Type: "key", Account: "123400000002",
		Region: "eu-south-2", Name: "2"}
	g := Build([]model.Resource{entry, key, read}, []Account{{ID: "123400000002", Connected: true}}, nil)
	notRead := map[string][]string{}
	for _, node := range g.Nodes {
		notRead[node.ID] = node.NotRead
	}
	if !slices.Contains(notRead[bucket], "policy") || !slices.Contains(notRead[key.ARN], "policy") || len(notRead[read.ARN]) != 0 {
		t.Fatalf("not read = %v", notRead)
	}
}
