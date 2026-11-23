package graph

import (
	"reflect"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/grouping"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// The Groups level collapses one account's nodes into its groups: links
// between two groups, and to and from other accounts, are summarised by
// kind with a count; a link inside one group is not shown.
func TestGroupsCollapseNodesAndSummariseLinks(t *testing.T) {
	tagged := func(resource model.Resource, app string) model.Resource {
		resource.Tags = map[string]string{"app": app}
		return resource
	}
	lbARN := "arn:aws:elasticloadbalancing:eu-south-2:" + spoke + ":loadbalancer/app/checkout/1"
	lb := tagged(res(spoke, lbARN, "elbv2", "load-balancer", "checkout"), "checkout")
	lb.Attributes = map[string]string{model.AttrDNSName: "checkout-1.eu-south-2.elb.amazonaws.com"}
	key := res(spoke, "arn:aws:kms:eu-south-2:"+spoke+":key/k-1", "kms", "key", "k-1")
	key.Attributes = map[string]string{model.AttrStackName: "baseline"}
	fn := func(name, app string, relations ...model.Relation) model.Resource {
		return tagged(res(spoke, "arn:aws:lambda:eu-south-2:"+spoke+":function:"+name, "lambda", "function", name, relations...), app)
	}
	resources := []model.Resource{
		lb, key,
		fn("checkout-a", "checkout", rel(model.RelationMayAccess, model.RelationKindMayAccess, key.ARN),
			rel(model.RelationRoutesTo, model.RelationConfigured, lbARN)),
		fn("checkout-b", "checkout", rel(model.RelationMayAccess, model.RelationKindMayAccess, key.ARN)),
		fn("pay-a", "payments", rel(model.RelationRoutesTo, model.RelationConfigured, lbARN)),
		tagged(res(spoke, "arn:aws:iam::"+spoke+":role/vendor", "iam", "role", "vendor",
			rel(model.RelationTrusts, model.RelationKindMayAccess, "arn:aws:iam::"+away+":role/reader")), "checkout"),
		res(spoke, "arn:aws:sqs:eu-south-2:"+spoke+":lonely", "sqs", "queue", "lonely"),
	}
	for _, name := range []string{"www", "shop", "api"} {
		resources = append(resources, res(dns, "arn:aws:route53:::hostedzone/Z1/record/"+name+".example.com", "route53", "dns-record",
			name+".example.com", rel(model.RelationAliasOf, model.RelationConfigured, "dualstack.checkout-1.eu-south-2.elb.amazonaws.com.")))
	}
	g := Build(resources, connected(spoke, dns), nil)
	groups := grouping.Build(resources, grouping.TagKeys{spoke: {"app"}}, nil, grouping.Options{}).Groups
	view := Groups(g, spoke, groups)

	if view.Account != spoke || view.Resources != 7 {
		t.Fatalf("view = %s %d", view.Account, view.Resources)
	}
	var cards []string
	for _, card := range view.Groups {
		cards = append(cards, card.ID)
	}
	if !reflect.DeepEqual(cards, []string{"stack:baseline", "tag:app=checkout", "tag:app=payments", grouping.UngroupedID}) {
		t.Fatalf("cards = %v", cards)
	}
	if view.Groups[1].Resources != 4 || view.Groups[1].ByType["function"] != 2 {
		t.Fatalf("checkout card = %+v", view.Groups[1])
	}
	want := []GroupLink{
		{From: AccountEndpointPrefix + dns, To: "tag:app=checkout", Type: model.RelationAliasOf, Kind: model.RelationConfigured, Count: 3, BothSides: 3},
		{From: "tag:app=checkout", To: AccountEndpointPrefix + away, Type: model.RelationTrusts, Kind: model.RelationKindMayAccess, Count: 1},
		{From: "tag:app=checkout", To: "stack:baseline", Type: model.RelationMayAccess, Kind: model.RelationKindMayAccess, Count: 2},
		{From: "tag:app=payments", To: "tag:app=checkout", Type: model.RelationRoutesTo, Kind: model.RelationConfigured, Count: 1},
	}
	if !reflect.DeepEqual(view.Links, want) {
		t.Fatalf("links =\n%+v\nwant\n%+v", view.Links, want)
	}
	// From the other side, the records are Ungrouped and the same link
	// ends on the account.
	other := Groups(g, dns, groups)
	wantOther := []GroupLink{{From: grouping.UngroupedID, To: AccountEndpointPrefix + spoke, Type: model.RelationAliasOf,
		Kind: model.RelationConfigured, Count: 3, BothSides: 3}}
	if len(other.Groups) != 1 || other.Groups[0].Resources != 3 || !reflect.DeepEqual(other.Links, wantOther) {
		t.Fatalf("dns account = %+v", other)
	}
}
