package changes

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

func raw(t *testing.T, document map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestNormalizeKeepsOnlyAllowlistedIdentifiers(t *testing.T) {
	event := Event{
		ID: "e-1", Name: "RegisterTaskDefinition", Source: "ecs.amazonaws.com", Time: time.Date(2026, 11, 9, 12, 0, 0, 0, time.FixedZone("x", 3600)),
		Raw: raw(t, map[string]any{
			"userIdentity": map[string]any{"type": "AssumedRole", "arn": "arn:aws:sts::111122223333:assumed-role/github-actions-deploy/person@example.com",
				"sessionContext": map[string]any{"sessionIssuer": map[string]any{"arn": "arn:aws:iam::111122223333:role/github-actions-deploy", "userName": "github-actions-deploy"}}},
			"requestParameters": map[string]any{"family": "checkout", "containerDefinitions": []any{map[string]any{"environment": []any{map[string]any{"value": "secret-env"}}}}},
			"responseElements":  map[string]any{"taskDefinition": map[string]any{"revision": 42, "taskDefinitionArn": "arn:aws:ecs:us-east-1:111122223333:task-definition/checkout:42"}},
		}),
	}
	change, ok := Normalize("111122223333", "us-east-1", event)
	if !ok {
		t.Fatal("write event dropped")
	}
	if change.Summary != "New task definition checkout:42 registered" || change.Template != "ecs.register_task_definition" || change.Service != "ecs" {
		t.Fatalf("change = %+v", change)
	}
	if change.Time.Location() != time.UTC || len(change.Resources) != 1 {
		t.Fatalf("change time/resources = %+v", change)
	}
	encoded, _ := json.Marshal(change)
	for _, secret := range []string{"secret-env", "person@example.com", "containerDefinitions"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("change kept %q: %s", secret, encoded)
		}
	}
	if change.Actor.Class != model.ActorPipeline || change.Actor.PrincipalARN != "arn:aws:iam::111122223333:role/github-actions-deploy" {
		t.Fatalf("actor = %+v", change.Actor)
	}
}

func TestNormalizeDropsReadOnlyAndRejectsFreeText(t *testing.T) {
	if _, ok := Normalize("111122223333", "us-east-1", Event{ID: "r", Name: "DescribeInstances", ReadOnly: "true"}); ok {
		t.Fatal("read-only event kept")
	}
	if _, ok := Normalize("111122223333", "us-east-1", Event{ID: "r", Name: "X", Raw: `{"readOnly":true}`}); ok {
		t.Fatal("read-only document kept")
	}
	event := Event{ID: "e", Name: "CreateStack", Source: "cloudformation.amazonaws.com",
		Raw: raw(t, map[string]any{"errorCode": "AccessDenied", "requestParameters": map[string]any{"stackName": "has spaces; DROP"}})}
	change, _ := Normalize("111122223333", "us-east-1", event)
	if change.Summary != "Failed attempt: CloudFormation stack created" || change.Fields != nil || change.ErrorCode != "AccessDenied" {
		t.Fatalf("free text field kept: %+v", change)
	}
	generic, _ := Normalize("111122223333", "us-east-1", Event{ID: "g", Name: "PutBucketTagging", Source: "s3.amazonaws.com", Resources: []string{"logs", "bad value"}})
	if generic.Template != GenericTemplateID || generic.Summary != "Put bucket tagging (S3)" || len(generic.Resources) != 1 {
		t.Fatalf("generic = %+v", generic)
	}
	oversized := Event{ID: "o", Name: "CreateStack", Source: "cloudformation.amazonaws.com", Raw: `{"requestParameters":{"stackName":"` + strings.Repeat("a", maxRawBytes) + `"}}`}
	if change, _ := Normalize("111122223333", "us-east-1", oversized); change.Fields != nil {
		t.Fatal("oversized document parsed")
	}
}

// TestNormalizeResolvesCanonicalARNs: every template resource becomes a
// canonical ARN built from its type, the account and the Region, the way
// CloudTrail really logs it (names, IDs, queue URLs, versioned Lambda
// event names). A name that cannot name exactly one resource is kept as
// unresolved, never guessed into an ARN.
func TestNormalizeResolvesCanonicalARNs(t *testing.T) {
	const account, region = "111122223333", "eu-west-1"
	cases := []struct {
		name, source, event string
		request             map[string]any
		resources           []string
		region              string
		want, unresolved    []string
	}{
		{name: "auto scaling group by name", source: "autoscaling.amazonaws.com", event: "UpdateAutoScalingGroup",
			request: map[string]any{"autoScalingGroupName": "web", "desiredCapacity": 6},
			want:    []string{"arn:aws:autoscaling:eu-west-1:111122223333:autoScalingGroup:*:autoScalingGroupName/web"}},
		{name: "route table by ID", source: "ec2.amazonaws.com", event: "ReplaceRoute",
			request: map[string]any{"routeTableId": "rtb-0abc", "destinationCidrBlock": "0.0.0.0/0"},
			want:    []string{"arn:aws:ec2:eu-west-1:111122223333:route-table/rtb-0abc"}},
		{name: "database in mixed case", source: "rds.amazonaws.com", event: "ModifyDBInstance",
			request: map[string]any{"dBInstanceIdentifier": "Orders-DB", "dBInstanceClass": "db.r6g.xlarge"},
			want:    []string{"arn:aws:rds:eu-west-1:111122223333:db:orders-db"}},
		{name: "lambda versioned event with qualifier", source: "lambda.amazonaws.com", event: "UpdateFunctionConfiguration20150331v2",
			request: map[string]any{"functionName": "api:live", "memorySize": 1024},
			want:    []string{"arn:aws:lambda:eu-west-1:111122223333:function:api"}},
		{name: "lambda partial ARN", source: "lambda.amazonaws.com", event: "UpdateFunctionCode20150331v2",
			request: map[string]any{"functionName": "111122223333:function:api"},
			want:    []string{"arn:aws:lambda:eu-west-1:111122223333:function:api"}},
		{name: "ecs service with cluster", source: "ecs.amazonaws.com", event: "UpdateService",
			request: map[string]any{"cluster": "arn:aws:ecs:eu-west-1:111122223333:cluster/prod", "service": "api"},
			want:    []string{"arn:aws:ecs:eu-west-1:111122223333:service/prod/api"}},
		{name: "sqs queue url", source: "sqs.amazonaws.com", event: "SetQueueAttributes",
			request: map[string]any{"queueUrl": "https://sqs.eu-west-1.amazonaws.com/111122223333/orders"},
			want:    []string{"arn:aws:sqs:eu-west-1:111122223333:orders"}},
		{name: "iam role in the global stream", source: "iam.amazonaws.com", event: "PutRolePolicy", region: "global",
			request: map[string]any{"roleName": "checkout-task", "policyName": "inline"},
			want:    []string{"arn:aws:iam::111122223333:role/checkout-task"}},
		{name: "stack by name keeps the ID slot", source: "cloudformation.amazonaws.com", event: "UpdateStack",
			request: map[string]any{"stackName": "network-prod"},
			want:    []string{"arn:aws:cloudformation:eu-west-1:111122223333:stack/network-prod/*"}},
		{name: "generic event resources by ID prefix", source: "ec2.amazonaws.com", event: "CreateRoute",
			request: map[string]any{"routeTableId": "rtb-1"}, resources: []string{"rtb-1"},
			want: []string{"arn:aws:ec2:eu-west-1:111122223333:route-table/rtb-1"}},
		{name: "security group by name is unresolved", source: "ec2.amazonaws.com", event: "AuthorizeSecurityGroupIngress",
			request: map[string]any{"groupId": "default"}, unresolved: []string{"default"}},
		{name: "regional resource in the global stream is unresolved", source: "autoscaling.amazonaws.com", event: "UpdateAutoScalingGroup",
			region: "global", request: map[string]any{"autoScalingGroupName": "web"}, unresolved: []string{"web"}},
		{name: "untyped generic name is unresolved", source: "kinesis.amazonaws.com", event: "UpdateShardCount",
			resources: []string{"clicks"}, unresolved: []string{"clicks"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eventRegion := region
			if tc.region != "" {
				eventRegion = tc.region
			}
			event := Event{ID: "e", Name: tc.event, Source: tc.source, Resources: tc.resources,
				Raw: raw(t, map[string]any{"userIdentity": map[string]any{"type": "IAMUser", "arn": "arn:aws:iam::111122223333:user/ops"},
					"requestParameters": tc.request})}
			change, ok := Normalize(account, eventRegion, event)
			if !ok {
				t.Fatal("dropped")
			}
			if strings.Join(change.Resources, ",") != strings.Join(tc.want, ",") || strings.Join(change.Unresolved, ",") != strings.Join(tc.unresolved, ",") {
				t.Fatalf("resources = %v unresolved = %v, want %v / %v", change.Resources, change.Unresolved, tc.want, tc.unresolved)
			}
			for _, arn := range change.Resources {
				if !strings.HasPrefix(arn, "arn:") {
					t.Fatalf("short name stored as a resource: %q", arn)
				}
			}
		})
	}
}

// TestSummaryNamesTheResourceNotItsARN: a sentence names the target group,
// the listener's load balancer or the function, never the full ARN or the
// account ID inside it; the full ARN stays in the resources.
func TestSummaryNamesTheResourceNotItsARN(t *testing.T) {
	const account, region = "111122223333", "eu-west-1"
	const targetGroup = "arn:aws:elasticloadbalancing:eu-west-1:111122223333:targetgroup/k8s-shop-web-0a1b2c3d4e/0123456789abcdef"
	const listener = "arn:aws:elasticloadbalancing:eu-west-1:111122223333:listener/app/k8s-shop-alb/50dc6c495c0c9188/f2f7dc8efc522ab2"
	accountID := regexp.MustCompile(`(^|[^0-9A-Za-z])[0-9]{12}([^0-9A-Za-z]|$)`)
	cases := []struct {
		name, source, event string
		request             map[string]any
		summary, resource   string
	}{
		{name: "register targets", source: "elasticloadbalancing.amazonaws.com", event: "RegisterTargets",
			request: map[string]any{"targetGroupArn": targetGroup}, summary: "Targets registered with target group k8s-shop-web-0a1b2c3d4e", resource: targetGroup},
		{name: "deregister targets", source: "elasticloadbalancing.amazonaws.com", event: "DeregisterTargets",
			request: map[string]any{"targetGroupArn": targetGroup}, summary: "Targets removed from target group k8s-shop-web-0a1b2c3d4e", resource: targetGroup},
		{name: "modify target group", source: "elasticloadbalancing.amazonaws.com", event: "ModifyTargetGroup",
			request: map[string]any{"targetGroupArn": targetGroup}, summary: "Health check settings changed on target group k8s-shop-web-0a1b2c3d4e", resource: targetGroup},
		{name: "modify listener", source: "elasticloadbalancing.amazonaws.com", event: "ModifyListener",
			request: map[string]any{"listenerArn": listener}, summary: "Load balancer k8s-shop-alb listener f2f7dc8efc522ab2 changed", resource: listener},
		{name: "ecs task definition by ARN", source: "ecs.amazonaws.com", event: "UpdateService",
			request: map[string]any{"service": "arn:aws:ecs:eu-west-1:111122223333:service/prod/api", "taskDefinition": "arn:aws:ecs:eu-west-1:111122223333:task-definition/api:7"},
			summary: "Service api updated to task definition api:7", resource: "arn:aws:ecs:eu-west-1:111122223333:service/prod/api"},
		{name: "lambda function by ARN", source: "lambda.amazonaws.com", event: "UpdateFunctionCode20150331v2",
			request: map[string]any{"functionName": "arn:aws:lambda:eu-west-1:111122223333:function:api"}, summary: "Code updated for function api",
			resource: "arn:aws:lambda:eu-west-1:111122223333:function:api"},
		{name: "lambda function by partial ARN", source: "lambda.amazonaws.com", event: "DeleteFunction20150331",
			request: map[string]any{"functionName": "111122223333:function:api"}, summary: "Function api deleted",
			resource: "arn:aws:lambda:eu-west-1:111122223333:function:api"},
		{name: "sqs queue URL", source: "sqs.amazonaws.com", event: "SetQueueAttributes",
			request: map[string]any{"queueUrl": "https://sqs.eu-west-1.amazonaws.com/111122223333/orders"}, summary: "Settings changed on queue orders",
			resource: "arn:aws:sqs:eu-west-1:111122223333:orders"},
		{name: "customer managed policy", source: "iam.amazonaws.com", event: "AttachRolePolicy",
			request: map[string]any{"roleName": "checkout-task", "policyArn": "arn:aws:iam::111122223333:policy/team/read-orders"},
			summary: "Policy read-orders attached to role checkout-task", resource: "arn:aws:iam::111122223333:role/checkout-task"},
		{name: "cloudformation stack by ARN", source: "cloudformation.amazonaws.com", event: "UpdateStack",
			request: map[string]any{"stackName": "arn:aws:cloudformation:eu-west-1:111122223333:stack/network-prod/5b1c0a10-0000-0000-0000-0a1b2c3d4e5f"},
			summary: "Stack network-prod updated", resource: "arn:aws:cloudformation:eu-west-1:111122223333:stack/network-prod/5b1c0a10-0000-0000-0000-0a1b2c3d4e5f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := Event{ID: "e", Name: tc.event, Source: tc.source,
				Raw: raw(t, map[string]any{"userIdentity": map[string]any{"type": "AssumedRole", "arn": "arn:aws:sts::111122223333:assumed-role/lb-controller/session"},
					"requestParameters": tc.request})}
			change, ok := Normalize(account, region, event)
			if !ok {
				t.Fatal("dropped")
			}
			if change.Summary != tc.summary {
				t.Errorf("summary = %q, want %q", change.Summary, tc.summary)
			}
			if strings.Contains(change.Summary, "arn:") || accountID.MatchString(change.Summary) {
				t.Errorf("summary carries an ARN or account ID: %q", change.Summary)
			}
			if !contains(change.Resources, tc.resource) {
				t.Errorf("resources = %v, want %s kept", change.Resources, tc.resource)
			}
		})
	}
}

// TestSecretAndTagFieldsKeepOnlyThatTheyChanged: Lambda environment
// variables, an SSM or Secrets Manager value, user data, a new secret
// access key and tag values are recorded as fields, so the change list can
// show "value hidden", but no value ever leaves the event.
func TestSecretAndTagFieldsKeepOnlyThatTheyChanged(t *testing.T) {
	const canary = "vrs-canary-value-9f3a"
	cases := []struct {
		source, event string
		request       map[string]any
		response      map[string]any
		want          map[string]string
	}{
		{source: "lambda.amazonaws.com", event: "UpdateFunctionConfiguration20150331v2",
			request: map[string]any{"functionName": "api", "environment": map[string]any{"variables": map[string]any{"DB_PASSWORD": canary}}},
			want:    map[string]string{"environment": SecretSet, "function": "api"}},
		{source: "ssm.amazonaws.com", event: "PutParameter",
			request: map[string]any{"name": "/checkout/db-password", "type": "SecureString", "value": canary},
			want:    map[string]string{"secret_value": SecretSet, "parameter": "/checkout/db-password"}},
		{source: "secretsmanager.amazonaws.com", event: "PutSecretValue",
			request: map[string]any{"secretId": "checkout/db", "secretString": canary},
			want:    map[string]string{"secret_value": SecretSet, "name": "checkout/db"}},
		{source: "ec2.amazonaws.com", event: "RunInstances",
			request: map[string]any{"instanceType": "t3.micro", "userData": canary},
			want:    map[string]string{"user_data": SecretSet, "instance_type": "t3.micro"}},
		{source: "iam.amazonaws.com", event: "CreateAccessKey",
			request:  map[string]any{"userName": "deploy-bot"},
			response: map[string]any{"accessKey": map[string]any{"accessKeyId": "AKIAVERSUSHIDDEN0006", "secretAccessKey": canary}},
			want:     map[string]string{"access_key": SecretSet, "user": "deploy-bot"}},
		{source: "ec2.amazonaws.com", event: "CreateTags",
			request: map[string]any{"resourcesSet": map[string]any{"items": []any{map[string]any{"resourceId": "i-0abc"}}},
				"tagSet": map[string]any{"items": []any{map[string]any{"key": "owner", "value": canary}, map[string]any{"key": "team", "value": "payments"}}}},
			want: map[string]string{"resource": "i-0abc", "tag:owner": model.RedactedValue, "tag:team": model.RedactedValue}},
		{source: "lambda.amazonaws.com", event: "TagResource20170331v2",
			request: map[string]any{"resource": "arn:aws:lambda:us-east-1:111122223333:function:api", "tags": map[string]any{"owner": canary}},
			want:    map[string]string{"function": "arn:aws:lambda:us-east-1:111122223333:function:api", "tag:owner": model.RedactedValue}},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			change, ok := Normalize("111122223333", "us-east-1", Event{ID: "e", Name: tc.event, Source: tc.source,
				Raw: raw(t, map[string]any{"requestParameters": tc.request, "responseElements": tc.response})})
			if !ok || change.Template == GenericTemplateID {
				t.Fatalf("no template: %+v", change)
			}
			encoded, _ := json.Marshal(change)
			if strings.Contains(string(encoded), canary) || strings.Contains(string(encoded), "payments") {
				t.Fatalf("a secret or tag value was kept: %s", encoded)
			}
			for name, value := range tc.want {
				if change.Fields[name] != value {
					t.Fatalf("field %s = %q, want %q (fields %v)", name, change.Fields[name], value, change.Fields)
				}
			}
			// A secret named without its ARN and an instance launch without
			// its response cannot name the resource.
			if len(change.Resources) == 0 && tc.event != "PutSecretValue" && tc.event != "RunInstances" {
				t.Fatalf("resource not resolved: %+v", change)
			}
		})
	}
	if !SecretFieldName("environment") || !SecretFieldName("secret_value") || SecretFieldName("function") {
		t.Fatal("secret field names")
	}
}

// TestEmailNamesAndAccessKeysNeverStored: an IAM user named after a
// person and the new access key ID reach no summary, field or resource;
// the person's ARN is kept masked and unresolved, so it never matches
// another masked user.
func TestEmailNamesAndAccessKeysNeverStored(t *testing.T) {
	document := `{"userIdentity":{"type":"IAMUser","arn":"arn:aws:iam::111122223333:user/admin"},` +
		`"requestParameters":{"userName":"jane@corp.com"},"responseElements":{"accessKey":{"accessKeyId":"AKIAABCDEFGHIJKLMNOP","secretAccessKey":"s3cr3t","userName":"jane@corp.com"}}}`
	change, ok := Normalize("111122223333", "global", Event{ID: "e1", Name: "CreateAccessKey", Source: "iam.amazonaws.com", Raw: document,
		Resources: []string{"AKIAABCDEFGHIJKLMNOP", "jane@corp.com"}})
	if !ok {
		t.Fatal("dropped")
	}
	encoded, _ := json.Marshal(change)
	for _, leak := range []string{"jane@corp.com", "AKIAABCDEFGHIJKLMNOP", "s3cr3t"} {
		if strings.Contains(string(encoded), leak) {
			t.Fatalf("stored %q: %s", leak, encoded)
		}
	}
	if !strings.Contains(change.Summary, MaskedEmail) || len(change.Resources) != 0 || len(change.Unresolved) == 0 {
		t.Fatalf("change = %+v", change)
	}
	// The same rule holds for every template that names a principal.
	for _, event := range []Event{
		{ID: "e2", Name: "PutRolePolicy", Source: "iam.amazonaws.com", Raw: `{"requestParameters":{"roleName":"bob@corp.com","policyName":"p"}}`},
		{ID: "e3", Name: "CreateStack", Source: "cloudformation.amazonaws.com", Raw: `{"requestParameters":{"stackName":"x"}}`, Resources: []string{"ann@corp.com"}},
	} {
		change, _ := Normalize("111122223333", "us-east-1", event)
		encoded, _ := json.Marshal(change)
		if strings.Contains(string(encoded), "@corp.com") {
			t.Fatalf("%s stored an email: %s", event.Name, encoded)
		}
	}
}

// CloudTrail flags sign-ins, token calls and some reads as writes
// (readOnly false). None of them is a change; a real write still is.
func TestNormalizeDropsNonChangesFlaggedAsWrites(t *testing.T) {
	writeFlag := func(name, source string) Event {
		return Event{ID: "e-" + name, Name: name, Source: source, ReadOnly: "false", Time: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
			Raw: raw(t, map[string]any{"readOnly": false, "userIdentity": map[string]any{"type": "IAMUser", "arn": "arn:aws:iam::111122223333:user/app"}})}
	}
	dropped := []struct{ name, source string }{
		{"ConsoleLogin", "signin.amazonaws.com"},
		{"GetSigninToken", "signin.amazonaws.com"},
		{"AssumeRoleWithWebIdentity", "sts.amazonaws.com"},
		{"GetRoleCredentials", "sso.amazonaws.com"},
		{"CreateToken", "sso-oauth.amazonaws.com"},
		{"Token_POST", "cognito-idp.amazonaws.com"},
		{"InitiateAuth", "cognito-idp.amazonaws.com"},
		{"AdminInitiateAuth", "cognito-idp.amazonaws.com"},
		{"RespondToAuthChallenge", "cognito-idp.amazonaws.com"},
		{"AdminRespondToAuthChallenge", "cognito-idp.amazonaws.com"},
		{"GetId", "cognito-identity.amazonaws.com"},
		{"GetCredentialsForIdentity", "cognito-identity.amazonaws.com"},
		{"DescribeDomainConfiguration", "iot.amazonaws.com"},
		{"ListDomainConfigurations", "iot.amazonaws.com"},
		{"GetBucketPolicy", "s3.amazonaws.com"},
		{"BatchGetItem", "dynamodb.amazonaws.com"},
		{"LookupEvents", "cloudtrail.amazonaws.com"},
		{"SearchResources", "resource-groups.amazonaws.com"},
		{"HeadBucket", "s3.amazonaws.com"},
		{"SelectResourceConfig", "config.amazonaws.com"},
	}
	for _, tc := range dropped {
		if !IsNotChange(tc.source, tc.name) {
			t.Errorf("%s:%s is not reported as a non-change", tc.source, tc.name)
		}
		if _, ok := Normalize("111122223333", "ap-southeast-1", writeFlag(tc.name, tc.source)); ok {
			t.Errorf("%s:%s kept as a change", tc.source, tc.name)
		}
	}
	kept := []struct{ name, source string }{
		{"RunInstances", "ec2.amazonaws.com"},
		{"PutEvaluations", "config.amazonaws.com"},
		{"AdminCreateUser", "cognito-idp.amazonaws.com"},
		{"CreateListener", "elasticloadbalancing.amazonaws.com"},
		{"Listen", "example.amazonaws.com"},
		{"Getaway", "example.amazonaws.com"},
	}
	for _, tc := range kept {
		if IsNotChange(tc.source, tc.name) {
			t.Errorf("%s:%s reported as a non-change", tc.source, tc.name)
		}
		if _, ok := Normalize("111122223333", "ap-southeast-1", writeFlag(tc.name, tc.source)); !ok {
			t.Errorf("%s:%s dropped", tc.source, tc.name)
		}
	}
}

// An underscore separates words in the fallback sentence and is not kept.
func TestGenericSummarySplitsUnderscores(t *testing.T) {
	if got := GenericSummary("example.amazonaws.com", "Token_POST"); got != "Token POST (example.amazonaws.com)" {
		t.Fatalf("GenericSummary = %q", got)
	}
}
