package collect

import (
	"context"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

const policyOwner = "123400000002"

// Each allow statement is reduced to principal, access class and resource
// pattern. The owner's own root, service principals, denies and every
// condition value are dropped.
func TestReducePolicyKeepsPrincipalAccessAndPatternOnly(t *testing.T) {
	cases := []struct {
		name     string
		document string
		subject  string
		keep     func([]string) bool
		want     []policyGrant
	}{
		{
			name: "bucket grant to a role and an account root, deny ignored",
			document: `{"Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::123400000004:role/etl","arn:aws:iam::123400000004:root"]},` +
				`"Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::exports","arn:aws:s3:::exports/*"]},` +
				`{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::exports/*"}]}`,
			subject: "exports",
			want: []policyGrant{{principal: "arn:aws:iam::123400000004:role/etl", access: model.AccessRead},
				{principal: "123400000004", access: model.AccessRead}},
		},
		{
			name: "prefix grant keeps its pattern",
			document: `{"Statement":{"Effect":"Allow","Principal":{"AWS":"123400000004"},"Action":"s3:PutObject",` +
				`"Resource":"arn:aws:s3:::exports/prod-*"}}`,
			subject: "exports",
			want:    []policyGrant{{principal: "123400000004", access: model.AccessWrite, pattern: "prod-*"}},
		},
		{
			name: "wildcard narrowed to the organization",
			document: `{"Statement":[{"Effect":"Allow","Principal":"*","Action":["ecr:BatchGetImage"],` +
				`"Condition":{"StringEquals":{"aws:PrincipalOrgID":"o-a1b2c3d4e5"}}}]}`,
			want: []policyGrant{{principal: model.OrgTarget("o-a1b2c3d4e5"), access: model.AccessRead}},
		},
		{
			name: "wildcard narrowed to the owner is the owner's own use",
			document: `{"Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":["kms:Decrypt","kms:CreateGrant"],"Resource":"*",` +
				`"Condition":{"StringEquals":{"kms:CallerAccount":"` + policyOwner + `","kms:ViaService":"ec2.eu-south-2.amazonaws.com"}}},` +
				`{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::` + policyOwner + `:root"},"Action":"kms:*","Resource":"*"}]}`,
		},
		{
			name:     "an open wildcard is anyone, and a wildcard action is admin",
			document: `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"*","Resource":"*"}]}`,
			want:     []policyGrant{{principal: model.AnyPrincipal, access: model.AccessAdmin}},
		},
		{
			name: "trust: deleted principals stay as their IDs, services are left out",
			document: url.QueryEscape(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["AROAEXAMPLEDELETED001",` +
				`"arn:aws:iam::123400000003:role/ci-runner"]},"Action":"sts:AssumeRole"},{"Sid":"","Effect":"Allow","Principal":` +
				`{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"},{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::555500006666:root"},` +
				`"Action":"sts:AssumeRole","Condition":{"StringEquals":{"sts:ExternalId":"secret-external-id"}}},` +
				`{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::123400000009:root"},"Action":"sts:TagSession"}]}`),
			keep: assumeActions,
			want: []policyGrant{{principal: "AROAEXAMPLEDELETED001", access: model.AccessWrite},
				{principal: "arn:aws:iam::123400000003:role/ci-runner", access: model.AccessWrite},
				{principal: "555500006666", access: model.AccessWrite}},
		},
		{name: "not a policy", document: "<html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reducePolicy(tc.document, policyOwner, tc.subject, tc.keep)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("grants = %+v, want %+v", got, tc.want)
			}
		})
	}
	relations := grantRelations(model.RelationTrusts, sourceTrust, []policyGrant{{principal: "555500006666", access: model.AccessWrite}})
	if relations[0] != (model.Relation{Type: model.RelationTrusts, Target: "555500006666", Kind: model.RelationKindMayAccess, Source: sourceTrust}) {
		t.Fatalf("trust relation = %+v", relations[0])
	}
}

func TestAccessClass(t *testing.T) {
	for _, tc := range []struct {
		actions []string
		want    string
	}{
		{[]string{"s3:GetObject", "s3:ListBucket"}, model.AccessRead},
		{[]string{"kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"}, model.AccessWrite},
		{[]string{"s3:PutBucketPolicy"}, model.AccessAdmin},
		{[]string{"ecr:*"}, model.AccessAdmin},
		{[]string{"ECR:BatchGetImage"}, model.AccessRead},
	} {
		if got := accessClass(tc.actions, nil); got != tc.want {
			t.Errorf("%v = %q, want %q", tc.actions, got, tc.want)
		}
	}
	if accessClass(nil, []string{"s3:DeleteObject"}) != model.AccessAdmin {
		t.Error("NotAction is not admin")
	}
}

// The estate's link reads: records keep only aliases and CNAMEs to load
// balancers and distributions, in their normalized form, one record per
// name; attachments are keyed by the network's owner; a received share
// names its owner; trusts and resource policies become relations.
func TestEstateLinkReads(t *testing.T) {
	h := newHarness(t, "estate-hub-spoke")
	tgt := func(account string) Target {
		return Target{AccountID: account, Regions: []string{"eu-south-2"}, Credentials: testCredentials(account)}
	}
	shared := "123400000003"
	mustSync(t, h, tgt(shared))
	records := map[string][]model.Relation{}
	for _, resource := range resourcesByARN(t, h, shared) {
		if resource.Type == "dns-record" {
			records[resource.Name] = resource.Relations
		}
	}
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	want := []string{"*.shop.example.com", "api.shop.example.com", "assets.shop.example.com", "cdn.shop.example.com",
		"ingest.shop.example.com", "legacy.shop.example.com", "payments.shop.example.com", "payments.svc.example.internal", "shop.example.com"}
	if len(names) != len(want) {
		t.Fatalf("records = %v", names)
	}
	for _, name := range want {
		if records[name] == nil {
			t.Errorf("record %s missing", name)
		}
	}
	alias := func(name string) model.Relation {
		for _, relation := range records[name] {
			if relation.Type == model.RelationAliasOf {
				return relation
			}
		}
		t.Fatalf("%s has no alias", name)
		return model.Relation{}
	}
	if got := alias("shop.example.com"); got.Target != "edge-public-1234567890.eu-south-2.elb.amazonaws.com" || got.Kind != model.RelationConfigured {
		t.Errorf("apex alias = %+v", got)
	}
	if got := alias("assets.shop.example.com"); got.Target != "d1a2b3c4d5e6f7.cloudfront.net" || got.Kind != model.RelationInferred {
		t.Errorf("cname = %+v", got)
	}
	if n := len(records["shop.example.com"]); n != 2 {
		t.Errorf("apex A and AAAA should be one alias plus its zone, got %+v", records["shop.example.com"])
	}
	roles := resourcesByARN(t, h, shared)
	ci := roles["arn:aws:iam::"+shared+":role/ci-runner"]
	promoter := roles["arn:aws:iam::"+shared+":role/image-promoter"]
	if !hasRelation(promoter, model.RelationTrusts, "123400000002") || len(ci.Relations) != 1 ||
		!strings.Contains(ci.Relations[0].Target, ":oidc-provider/token.actions.githubusercontent.com") {
		t.Errorf("trusts: promoter %+v, ci %+v", promoter.Relations, ci.Relations)
	}

	hub := "123400000001"
	mustSync(t, h, tgt(hub))
	hubResources := resourcesByARN(t, h, hub)
	spokeAttachment := "arn:aws:ec2:eu-south-2:123400000002:transit-gateway-attachment/tgw-attach-02222222222222222"
	attachment, ok := hubResources[spokeAttachment]
	if !ok || !hasRelation(attachment, model.RelationAttachedToTransit, "arn:aws:ec2:eu-south-2:"+hub+":transit-gateway/tgw-0e1f2a3b4c5d6e7f8") ||
		!hasRelation(attachment, model.RelationAttachedTo, "arn:aws:ec2:eu-south-2:123400000002:vpc/vpc-0200000000000b002") {
		t.Errorf("spoke attachment as the hub reads it = %+v", attachment)
	}
	share := hubResources["arn:aws:ram:eu-south-2:"+hub+":resource-share/0a0a0a0a-1b1b-4c4c-8d8d-0e0e0e0e0e01"]
	if !hasRelation(share, model.RelationSharedVia, "org:o-a1b2c3d4e5") {
		t.Errorf("transit share = %+v", share)
	}
	org := false
	for _, resource := range hubResources {
		if resource.Type == "organization" && resource.Attributes[model.AttrManagementAccount] == "123400000090" &&
			resource.Attributes[model.AttrOrganizationID] == "o-a1b2c3d4e5" {
			org = true
		}
	}
	if !org {
		t.Error("organization not read")
	}

	data := "123400000004"
	report := mustSync(t, h, tgt(data))
	for _, unit := range report.Units {
		if unit.Unit.Service == serviceS3Policy && unit.Resources != 0 {
			t.Errorf("the bucket policy unit reports %d resources", unit.Resources)
		}
		switch unit.Unit.Service {
		case serviceKMS, serviceECR, serviceRAM, serviceTransitGateway, serviceOrganizations, serviceS3Policy, "route53", "iam":
			if unit.Outcome != OutcomeCommitted {
				t.Errorf("%s = %+v", unit.Unit.Service, unit)
			}
		}
	}
	dataResources := resourcesByARN(t, h, data)
	key := dataResources["arn:aws:kms:eu-south-2:"+data+":key/0d0d0d0d-1111-4222-8333-444444444444"]
	if len(key.Relations) != 1 || key.Relations[0].Target != "123400000002" || key.Relations[0].Access != model.AccessWrite {
		t.Errorf("customer key = %+v", key)
	}
	if managed := dataResources["arn:aws:kms:eu-south-2:"+data+":key/0e0e0e0e-5555-4666-8777-888888888888"]; len(managed.Relations) != 0 {
		t.Errorf("managed key policy was read: %+v", managed)
	}
	for arn, resource := range dataResources {
		if resource.Type == model.ResourcePolicyType {
			t.Errorf("a resource listing carries the policy entry %s", arn)
		}
	}
	entries, err := h.store.Resources(context.Background(), snapshot.Query{Account: data, PolicyEntries: true})
	if err != nil {
		t.Fatal(err)
	}
	dataResources = map[string]model.Resource{}
	for _, resource := range entries.Resources {
		dataResources[resource.ARN] = resource
	}
	landing := dataResources["arn:aws:s3:::dat-landing-2a3b4c#policy"]
	if landing.Attributes[model.AttrSubject] != "arn:aws:s3:::dat-landing-2a3b4c" || !hasRelation(landing, model.RelationMayAccess, "123400000002") {
		t.Errorf("bucket policy entry = %+v", landing)
	}
	if _, ok := dataResources["arn:aws:s3:::dat-archive-5d6e7f#policy"]; ok {
		t.Error("a bucket with no policy has a policy entry")
	}
}

// Only a positive condition that must hold narrows an open grant. A negated
// or IfExists operator, ForAllValues, or a value that names no single
// organization or account leaves the grant open to anyone; every value of a
// narrowing key is read.
func TestOpenGrantNarrowsOnlyOnPositiveConditions(t *testing.T) {
	open := func(condition string) string {
		return `{"Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":` + condition + `}]}`
	}
	for name, tc := range map[string]struct {
		condition string
		want      []string
	}{
		"not equals organization":     {`{"StringNotEquals":{"aws:PrincipalOrgID":"o-a1b2c3d4e5"}}`, []string{model.AnyPrincipal}},
		"if exists organization":      {`{"StringEqualsIfExists":{"aws:PrincipalOrgID":"o-a1b2c3d4e5"}}`, []string{model.AnyPrincipal}},
		"for all values paths":        {`{"ForAllValues:StringLike":{"aws:PrincipalOrgPaths":["o-a1b2c3d4e5/*"]}}`, []string{model.AnyPrincipal}},
		"wildcard organization":       {`{"StringLike":{"aws:PrincipalOrgID":"o-*"}}`, []string{model.AnyPrincipal}},
		"one bad account":             {`{"StringEquals":{"aws:PrincipalAccount":["123400000004","1234*"]}}`, []string{model.AnyPrincipal}},
		"if exists account":           {`{"StringEqualsIfExists":{"kms:CallerAccount":"123400000004"}}`, []string{model.AnyPrincipal}},
		"two organizations":           {`{"StringEquals":{"aws:PrincipalOrgID":["o-zzzzzzzzzz","o-a1b2c3d4e5"]}}`, []string{"org:o-a1b2c3d4e5", "org:o-zzzzzzzzzz"}},
		"two accounts":                {`{"StringEquals":{"aws:PrincipalAccount":["123400000009","123400000004"]}}`, []string{"123400000004", "123400000009"}},
		"any value like organization": {`{"ForAnyValue:StringLike":{"aws:PrincipalOrgPaths":["o-a1b2c3d4e5/r-ab12/*"]}}`, []string{"org:o-a1b2c3d4e5"}},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, grant := range reducePolicy(open(tc.condition), policyOwner, "b", nil) {
				got = append(got, grant.principal)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("principals = %v, want %v", got, tc.want)
			}
		})
	}
}

// A trust keeps the trusted role; a session ARN is cut to its role, never
// keeping the session name; a web identity provider is kept as the
// provider; a resource policy grant keeps the account only.
func TestPrincipalsKeepRoleProviderOrAccountOnly(t *testing.T) {
	trust := url.QueryEscape(`{"Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:sts::123400000003:assumed-role/ci-runner/jane.doe@example.com",` +
		`"arn:aws:iam::123400000003:role/team/deployer"],"Federated":"accounts.google.com"},"Action":["sts:AssumeRole","sts:AssumeRoleWithWebIdentity"],` +
		`"Condition":{"StringEquals":{"accounts.google.com:sub":"person-subject-0001"}}}]}`)
	relations := grantRelations(model.RelationTrusts, sourceTrust, reducePolicy(trust, policyOwner, "", assumeActions))
	var targets []string
	for _, relation := range relations {
		targets = append(targets, relation.Target)
	}
	want := []string{"arn:aws:iam::123400000003:role/ci-runner", "arn:aws:iam::123400000003:role/team/deployer", "accounts.google.com"}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("trust targets = %v, want %v", targets, want)
	}
	bucket := `{"Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::123400000004:role/etl","arn:aws:sts::123400000009:assumed-role/x/jane",` +
		`"AIDAJQABLZS4A3QDU576Q"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`
	stored := model.Sanitize(model.Resource{Relations: grantRelations(model.RelationMayAccess, sourceBucket, reducePolicy(bucket, policyOwner, "b", nil))})
	targets = targets[:0]
	for _, relation := range stored.Relations {
		targets = append(targets, relation.Target)
	}
	if !reflect.DeepEqual(targets, []string{"123400000004", "123400000009", "AIDAJQABLZS4A3QDU576Q"}) {
		t.Fatalf("grant targets = %v", targets)
	}
	for _, relation := range append(relations, stored.Relations...) {
		if strings.Contains(relation.Target, "jane") || strings.Contains(relation.Target, "person-subject") {
			t.Errorf("%+v keeps a session or subject name", relation)
		}
	}
}

// A federated user's ARN names the person who signed in: a trust keeps its
// account only. An IAM user's ARN is an identity, not a session, and stays.
func TestTrustCutsFederatedUsersToTheirAccount(t *testing.T) {
	trust := url.QueryEscape(`{"Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:sts::123400000009:federated-user/jane.doe@example.com",` +
		`"arn:aws:iam::123400000003:user/deploy-bot"]},"Action":"sts:AssumeRole"}]}`)
	var targets []string
	for _, relation := range grantRelations(model.RelationTrusts, sourceTrust, reducePolicy(trust, policyOwner, "", assumeActions)) {
		targets = append(targets, relation.Target)
	}
	if !reflect.DeepEqual(targets, []string{"123400000009", "arn:aws:iam::123400000003:user/deploy-bot"}) {
		t.Fatalf("trust targets = %v", targets)
	}
}
