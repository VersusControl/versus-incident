package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeKeepsClosedAttributesAndStableRelations(t *testing.T) {
	resource := Resource{
		ARN: "arn:aws:ec2:us-east-1:111122223333:instance/i-1", Service: "ec2", Type: "instance",
		Attributes: map[string]string{AttrID: "i-1", "user_data": "secret", AttrVPC: ""},
		Relations: []Relation{
			{Type: RelationInVPC, Target: "b"}, {Type: RelationInVPC, Target: "a"},
			{Type: RelationInVPC, Target: "a"}, {Type: RelationAttachedTo, Target: ""},
		},
		Tags: map[string]string{},
	}
	out := Sanitize(resource)
	if len(out.Attributes) != 1 || out.Attributes[AttrID] != "i-1" {
		t.Fatalf("attributes = %v", out.Attributes)
	}
	if len(out.Relations) != 2 || out.Relations[0].Target != "a" || out.Tags != nil {
		t.Fatalf("relations/tags = %+v / %v", out.Relations, out.Tags)
	}
	resource.Attributes[AttrID] = "changed"
	if out.Attributes[AttrID] != "i-1" {
		t.Fatal("sanitize shares the attribute map")
	}
	left, _ := json.Marshal(Sanitize(Resource{Relations: []Relation{{Type: RelationInVPC, Target: "b"}, {Type: RelationInVPC, Target: "a"}}}))
	right, _ := json.Marshal(Sanitize(Resource{Relations: []Relation{{Type: RelationInVPC, Target: "a"}, {Type: RelationInVPC, Target: "b"}}}))
	if string(left) != string(right) {
		t.Fatal("equal inventories encode differently")
	}
	if !AllowedAttribute(AttrTaskDef) || AllowedAttribute("environment") {
		t.Fatal("attribute allowlist wrong")
	}
}

func TestRedactTagsKeepsKeys(t *testing.T) {
	out := RedactTags(map[string]string{"app": "shop", "owner": "alice@example.com"}, map[string]bool{"app": true})
	if out["app"] != "shop" || out["owner"] != RedactedValue || len(out) != 2 {
		t.Fatalf("tags = %v", out)
	}
	if RedactTags(nil, nil) != nil {
		t.Fatal("empty tags not nil")
	}
	if RedactTags(map[string]string{"App": "x"}, map[string]bool{"app": true})["App"] != RedactedValue {
		t.Fatal("tag key match must be exact")
	}
}

func TestGapReasonForCodeNeverReadsAsZero(t *testing.T) {
	cases := map[string]GapReason{
		"denied": GapDenied, "trust_denied": GapDenied, "not_on_plan": GapNotOnPlan, "region_disabled": GapNotEnabled,
		"throttled": GapThrottled, "timeout": GapNotChecked, "network": GapNotChecked, "": GapNotChecked, "anything": GapNotChecked,
	}
	for code, want := range cases {
		if got := GapReasonForCode(code); got != want {
			t.Errorf("GapReasonForCode(%q) = %s, want %s", code, got, want)
		}
	}
}

// Every code a unit can record, and anything unknown, lands in exactly one
// read outcome; only not read and capped count as gaps.
func TestEveryGapCodeMapsToOneOutcome(t *testing.T) {
	want := map[string]ReadOutcome{
		"denied": OutcomeNotRead, "trust_denied": OutcomeNotRead, "external_id_mismatch": OutcomeNotRead,
		"role_not_found": OutcomeNotRead, "not_on_plan": OutcomeNotRead, "throttled": OutcomeNotRead,
		"timeout": OutcomeNotRead, "network": OutcomeNotRead, "no_server_identity": OutcomeNotRead,
		"storage": OutcomeNotRead, "invalid_snapshot": OutcomeNotRead, "": OutcomeNotRead, "anything": OutcomeNotRead,
		"capped": OutcomeNotRead, "region_disabled": OutcomeOffInAWS, "not_enabled": OutcomeOffInAWS,
		"not_available_in_region": OutcomeNotOffered,
	}
	outcomes := map[ReadOutcome]bool{OutcomeNotRead: true, OutcomeCapped: true, OutcomeOffInAWS: true, OutcomeNotOffered: true}
	for code, outcome := range want {
		got := Gap{Reason: GapReasonForCode(code)}.Outcome()
		if !outcomes[got] || got != outcome {
			t.Errorf("code %q = %s, want %s", code, got, outcome)
		}
	}
	reasons := map[GapReason]ReadOutcome{
		GapDenied: OutcomeNotRead, GapThrottled: OutcomeNotRead, GapNotOnPlan: OutcomeNotRead, GapNotChecked: OutcomeNotRead,
		GapCapped: OutcomeCapped, GapNotEnabled: OutcomeOffInAWS, GapNotInRegion: OutcomeNotOffered, "unknown": OutcomeNotRead,
	}
	for reason, outcome := range reasons {
		if got := OutcomeForReason(reason); got != outcome {
			t.Errorf("reason %s = %s, want %s", reason, got, outcome)
		}
		if gap := (outcome == OutcomeNotRead || outcome == OutcomeCapped); outcome.IsGap() != gap {
			t.Errorf("outcome %s IsGap = %t", outcome, outcome.IsGap())
		}
	}
}

func TestGapCountsAreOmittedWhenZero(t *testing.T) {
	encoded, err := json.Marshal(Gap{Account: "111122223333", Region: "us-east-1", Service: "s3", Reason: GapDenied})
	if err != nil || strings.Contains(string(encoded), "seen") || strings.Contains(string(encoded), "total") {
		t.Fatalf("gap = %s, %v", encoded, err)
	}
	encoded, _ = json.Marshal(Gap{Reason: GapCapped, Seen: 100, Total: 140})
	if !strings.Contains(string(encoded), `"seen":100,"total":140`) {
		t.Fatalf("capped gap = %s", encoded)
	}
}

func TestARNAndPartition(t *testing.T) {
	if PartitionForRegion("cn-north-1") != "aws-cn" || PartitionForRegion("us-gov-west-1") != "aws-us-gov" || PartitionForRegion("eu-west-1") != "aws" {
		t.Fatal("partition")
	}
	if got := ARN("aws", "sqs", "us-east-1", "111122223333", "jobs"); got != "arn:aws:sqs:us-east-1:111122223333:jobs" {
		t.Fatalf("arn = %s", got)
	}
	if (Unit{Account: "1", Region: "r", Service: "s"}).Key() != "1/r/s" {
		t.Fatal("unit key")
	}
}

// RedactName hides a name taken from the Name tag unless Name is
// allowlisted, including a row stored before names were redacted at write
// (raw name beside an already redacted tag), and keeps a resource's own
// name when it does not come from the tag.
func TestRedactNameAppliesTheTagValueAllowlist(t *testing.T) {
	const instanceARN = "arn:aws:ec2:us-east-1:111122223333:instance/i-1"
	cases := []struct {
		name      string
		resource  Resource
		allowlist map[string]bool
		wantName  string
		wantTag   string
	}{
		{"raw name, not allowlisted",
			Resource{ARN: instanceARN, Type: "instance", Name: "web", Tags: map[string]string{"Name": "web"}},
			map[string]bool{"app": true}, "i-1", RedactedValue},
		{"legacy row: raw name beside a redacted tag",
			Resource{ARN: instanceARN, Type: "instance", Name: "web", Tags: map[string]string{"Name": RedactedValue}},
			map[string]bool{}, "i-1", RedactedValue},
		{"allowlisted",
			Resource{ARN: instanceARN, Type: "instance", Name: "web", Tags: map[string]string{"Name": "web"}},
			map[string]bool{"Name": true}, "web", "web"},
		{"lower-case key is not the Name tag",
			Resource{ARN: instanceARN, Type: "instance", Name: "web", Tags: map[string]string{"Name": "web"}},
			map[string]bool{"name": true}, "i-1", RedactedValue},
		{"own name that equals the Name tag value",
			Resource{ARN: "arn:aws:lambda:us-east-1:111122223333:function:checkout", Type: "function", Name: "checkout",
				Tags: map[string]string{"Name": "checkout"}},
			map[string]bool{}, "checkout", RedactedValue},
		{"own name beside a different Name tag",
			Resource{ARN: "arn:aws:rds:us-east-1:111122223333:db:orders", Type: "db-instance", Name: "orders",
				Tags: map[string]string{"Name": "Orders DB"}},
			map[string]bool{}, "orders", RedactedValue},
		{"no Name tag",
			Resource{ARN: instanceARN, Type: "instance", Name: "i-1"},
			map[string]bool{}, "i-1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.resource
			RedactName(&r, c.allowlist)
			if r.Name != c.wantName || r.Tags["Name"] != c.wantTag {
				t.Fatalf("name = %q, Name tag = %q; want %q, %q", r.Name, r.Tags["Name"], c.wantName, c.wantTag)
			}
		})
	}
	if got := ResourceID("arn:aws:ec2:us-east-1:111122223333:natgateway/nat-1"); got != "nat-1" {
		t.Fatalf("ResourceID = %q", got)
	}
}

// ResourceID fails closed: a value that looks like an ARN but is not a
// well-formed one is never returned raw, so its account ID cannot leak.
func TestResourceIDFailsClosedOnMalformedARNs(t *testing.T) {
	cases := map[string]string{
		"arn:aws:ec2:us-east-1:111122223333:instance/i-0abc":      "i-0abc",
		"arn:aws:lambda:us-east-1:111122223333:function:checkout": "checkout",
		"arn:aws:s3:::bucket-a":                                   "bucket-a",
		"i-0abc":                                                  "i-0abc",
		"ARN:aws:ec2:us-east-1:111122223333:instance/i-0abc":      RedactedValue,
		"  arn:aws:ec2:us-east-1:111122223333:instance/i-0abc":    RedactedValue,
		"arn:aws:ec2:us-east-1:111122223333":                      RedactedValue,
		"arn:aws:ec2:us-east-1:111122223333:":                     RedactedValue,
		"arn::ec2:us-east-1:111122223333:instance/i-0abc":         RedactedValue,
		"arn:aws:ec2:us-east-1:111122223333:instance/":            RedactedValue,
		"arn:aws:ec2:us-east-1:111122223333:instance/i 0abc":      RedactedValue,
	}
	for in, want := range cases {
		got := ResourceID(in)
		if got != want || (want == RedactedValue && strings.Contains(got, "111122223333")) {
			t.Errorf("ResourceID(%q) = %q, want %q", in, got, want)
		}
	}
}

// An application read from a tag is a tag value: RedactName keeps it only
// while its tag key is allowlisted, with an exact, case-sensitive match.
// An application from AppRegistry or CloudFormation is not a tag value.
func TestRedactNameShowsATagApplicationOnlyWhenItsKeyIsAllowlisted(t *testing.T) {
	cases := []struct {
		name      string
		resource  Resource
		allowlist map[string]bool
		wantApp   string
	}{
		{"service key not allowlisted",
			Resource{App: "checkout", AppSource: AppSourceTags, AppTag: "service", Tags: map[string]string{"service": "checkout"}},
			map[string]bool{"app": true, "team": true}, ""},
		{"service key allowlisted",
			Resource{App: "checkout", AppSource: AppSourceTags, AppTag: "service", Tags: map[string]string{"service": "checkout"}},
			map[string]bool{"service": true}, "checkout"},
		{"allowlist key differs in case",
			Resource{App: "checkout", AppSource: AppSourceTags, AppTag: "service", Tags: map[string]string{"service": "checkout"}},
			map[string]bool{"Service": true}, ""},
		{"tag application without its key",
			Resource{App: "checkout", AppSource: AppSourceTags},
			map[string]bool{"service": true}, ""},
		{"application from AppRegistry",
			Resource{App: "shop", AppSource: "appregistry"},
			map[string]bool{}, "shop"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.resource
			RedactName(&r, c.allowlist)
			if r.App != c.wantApp {
				t.Fatalf("app = %q, want %q", r.App, c.wantApp)
			}
			if r.App == "" && r.AppSource != "" {
				t.Fatalf("an empty app keeps its source %q", r.AppSource)
			}
		})
	}
}

// A relation stored before relation kinds existed has only a type and a
// target; it reads as configured.
func TestRelationStoredWithoutKindReadsAsConfigured(t *testing.T) {
	var resource Resource
	stored := `{"arn":"arn:aws:elasticloadbalancing:us-east-1:111122223333:loadbalancer/app/web/1","service":"elbv2",` +
		`"type":"load-balancer","account":"111122223333","region":"us-east-1","name":"web",` +
		`"relations":[{"type":"routes_to","target":"arn:aws:elasticloadbalancing:us-east-1:111122223333:targetgroup/web/2"}]}`
	if err := json.Unmarshal([]byte(stored), &resource); err != nil {
		t.Fatal(err)
	}
	if len(resource.Relations) != 1 || resource.Relations[0].Kind != RelationConfigured || resource.Relations[0].Source != "" {
		t.Fatalf("relations = %+v", resource.Relations)
	}
	var grant Relation
	if err := json.Unmarshal([]byte(`{"type":"may_access","target":"123400000004","kind":"may_access","access":"read"}`), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Kind != RelationKindMayAccess || grant.Access != AccessRead {
		t.Fatalf("grant = %+v", grant)
	}
}

// Two reads of one link (an A and an AAAA alias, two statements granting
// the same) stay one link; links that differ in kind, access or pattern
// stay apart, and the order they were read in does not change the result.
func TestSanitizeKeepsLinksApartByKindAccessAndPattern(t *testing.T) {
	relations := []Relation{
		{Type: RelationAliasOf, Target: "web.elb.amazonaws.com", Kind: RelationConfigured, Source: "route53:ListResourceRecordSets"},
		{Type: RelationAliasOf, Target: "web.elb.amazonaws.com", Source: "route53:ListResourceRecordSets"},
		{Type: RelationAliasOf, Target: "web.elb.amazonaws.com", Kind: RelationInferred},
		{Type: RelationMayAccess, Target: "123400000004", Kind: RelationKindMayAccess, Access: AccessRead, Count: 3},
		{Type: RelationMayAccess, Target: "123400000004", Kind: RelationKindMayAccess, Access: AccessRead, Count: 7},
		{Type: RelationMayAccess, Target: "123400000004", Kind: RelationKindMayAccess, Access: AccessWrite},
		{Type: RelationMayAccess, Target: "123400000004", Kind: RelationKindMayAccess, Access: AccessRead, Pattern: "logs/*"},
	}
	out := Sanitize(Resource{Relations: relations}).Relations
	if len(out) != 5 {
		t.Fatalf("relations = %+v", out)
	}
	if out[0].Kind != RelationConfigured || out[0].Source != "route53:ListResourceRecordSets" || out[1].Kind != RelationInferred {
		t.Fatalf("aliases = %+v", out[:2])
	}
	if out[2].Count != 7 || out[2].Pattern != "" || out[3].Pattern != "logs/*" || out[4].Access != AccessWrite {
		t.Fatalf("grants = %+v", out[2:])
	}
	reversed := make([]Relation, len(relations))
	for index, relation := range relations {
		reversed[len(relations)-1-index] = relation
	}
	left, _ := json.Marshal(out)
	right, _ := json.Marshal(Sanitize(Resource{Relations: reversed}).Relations)
	if string(left) != string(right) {
		t.Fatalf("read order changes the result:\n%s\n%s", left, right)
	}
}
