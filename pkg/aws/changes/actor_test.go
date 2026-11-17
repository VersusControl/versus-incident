package changes

import (
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

func assumed(role, session string) Identity {
	return Identity{Type: "AssumedRole", ARN: "arn:aws:sts::111122223333:assumed-role/" + role + "/" + session,
		SessionContext: sessionContext{SessionIssuer: sessionIssuer{ARN: "arn:aws:iam::111122223333:role/" + role, UserName: role}}}
}

func TestClassifyActor(t *testing.T) {
	cases := []struct {
		name      string
		identity  Identity
		userAgent string
		class     model.ActorClass
		session   string
	}{
		{"terraform agent", assumed("admin", "bob"), "HashiCorp/1.0 Terraform/1.9.0", model.ActorPipeline, SessionCI},
		{"deploy role", assumed("github-actions-deploy", "GitHubActions"), "aws-cli/2", model.ActorPipeline, SessionCI},
		{"ci session", assumed("ops", "ci-run-42"), "aws-cli/2", model.ActorPipeline, SessionCI},
		{"sso person", assumed("AWSReservedSSO_Admin_0123", "alice@example.com"), "aws-cli/2", model.ActorPerson, SessionSSO},
		{"console", assumed("ops", "bob"), "signin.amazonaws.com", model.ActorPerson, SessionConsole},
		{"email session", assumed("ops", "carol@example.com"), "aws-cli/2", model.ActorPerson, SessionOther},
		{"iam user", Identity{Type: "IAMUser", ARN: "arn:aws:iam::111122223333:user/dave"}, "aws-cli/2", model.ActorPerson, SessionOther},
		{"aws service", Identity{Type: "AWSService", InvokedBy: "autoscaling.amazonaws.com"}, "autoscaling.amazonaws.com", model.ActorAWSService, SessionService},
		{"service-linked role", assumed("aws-service-role/ecs.amazonaws.com/AWSServiceRoleForECS", "ecs"), "ecs.amazonaws.com", model.ActorAWSService, SessionService},
		{"unknown", assumed("ops", "x1"), "aws-cli/2", model.ActorUnknown, SessionOther},
	}
	for _, tc := range cases {
		identity := tc.identity
		if tc.name == "service-linked role" {
			identity.SessionContext.SessionIssuer.ARN = "arn:aws:iam::111122223333:role/aws-service-role/ecs.amazonaws.com/AWSServiceRoleForECS"
		}
		actor := ClassifyActor(identity, tc.userAgent)
		if actor.Class != tc.class || actor.SessionClass != tc.session {
			t.Errorf("%s = %+v, want %s/%s", tc.name, actor, tc.class, tc.session)
		}
		if strings.Contains(actor.PrincipalARN, ":assumed-role/") {
			t.Errorf("%s kept the session ARN: %s", tc.name, actor.PrincipalARN)
		}
	}
}
