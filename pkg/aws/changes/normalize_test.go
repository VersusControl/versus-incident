package changes

import (
	"encoding/json"
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
