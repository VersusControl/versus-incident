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
		{"role from the cli", assumed("ops", "x1"), "aws-cli/2", model.ActorPerson, SessionOther},
		{"no principal", Identity{Type: "Unknown"}, "aws-cli/2", model.ActorUnknown, SessionOther},
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

// Three misclassifications: CloudFormation acting for a pipeline is the
// pipeline's change, an SSO person whose permission set is named for
// deploys is a person, and an IAM user named for a CI system is a pipeline.
func TestActorClassFixes(t *testing.T) {
	viaStack := assumed("github-actions-deploy", "GitHubActions")
	viaStack.InvokedBy = "cloudformation.amazonaws.com"
	cases := []struct {
		name     string
		identity Identity
		class    model.ActorClass
		via      string
	}{
		{"cloudformation for a pipeline", viaStack, model.ActorPipeline, "cloudformation"},
		{"sso deploy permission set", assumed("AWSReservedSSO_DeployAccess_0123", "alice"), model.ActorPerson, ""},
		{"ci iam user", Identity{Type: "IAMUser", ARN: "arn:aws:iam::111122223333:user/github-actions", UserName: "github-actions"}, model.ActorPipeline, ""},
	}
	for _, tc := range cases {
		actor := ClassifyActor(tc.identity, "aws-cli/2")
		if actor.Class != tc.class || actor.Via != tc.via {
			t.Errorf("%s = %+v, want %s via %q", tc.name, actor, tc.class, tc.via)
		}
	}
}

// An email-shaped IAM user name is never kept.
func TestEmailPrincipalMasked(t *testing.T) {
	actor := ClassifyActor(Identity{Type: "IAMUser", ARN: "arn:aws:iam::111122223333:user/jane@corp.com", UserName: "jane@corp.com"}, "aws-cli/2")
	if strings.Contains(actor.PrincipalARN, "jane@corp.com") || actor.Class != model.ActorPerson {
		t.Fatalf("email user = %+v", actor)
	}
}

// Real-shaped identities from a common EKS account. Workload roles, the
// roles a service acts through and IAM users calling from an SDK are
// automation; an IAM user is a person only from the console or the CLI;
// unknown is left only when the call names no principal. Every named
// principal keeps its name for the actor column.
func TestClassifyActorRealShapes(t *testing.T) {
	webIdentity := func(role, session string) Identity {
		identity := assumed(role, session)
		identity.SessionContext.WebIDFederationData = &webIDFederationData{FederatedProvider: "arn:aws:iam::111122223333:oidc-provider/oidc.eks.example.amazonaws.com/id/EXAMPLE"}
		return identity
	}
	stackRole := assumed("stack-service-role", "AWSCloudFormation")
	stackRole.InvokedBy = "cloudformation.amazonaws.com"
	backendUser := Identity{Type: "IAMUser", ARN: "arn:aws:iam::111122223333:user/auth-backend", UserName: "auth-backend"}
	noIssuer := Identity{Type: "AssumedRole", ARN: "arn:aws:sts::111122223333:assumed-role/node-role/i-0abc"}
	cases := []struct {
		name      string
		identity  Identity
		userAgent string
		class     model.ActorClass
		session   string
		principal string
	}{
		{"irsa controller role", webIdentity("karpenter-controller", "1700000000000000000"), "aws-sdk-go-v2/1.30.0 os/linux lang/go#1.22", model.ActorPipeline, SessionWorkload, "arn:aws:iam::111122223333:role/karpenter-controller"},
		{"irsa role with an unrecognised agent", webIdentity("reporting-irsa", "botocore-session-1700000000"), "custom-agent/1.0", model.ActorPipeline, SessionWorkload, "arn:aws:iam::111122223333:role/reporting-irsa"},
		{"pod identity session", assumed("lb-controller", "eks-prod-aws-load-b-0a1b2c3d"), "elbv2.k8s.aws/v2.8.0", model.ActorPipeline, SessionWorkload, "arn:aws:iam::111122223333:role/lb-controller"},
		{"cloudformation service role", stackRole, "cloudformation.amazonaws.com", model.ActorPipeline, SessionService, "arn:aws:iam::111122223333:role/stack-service-role"},
		{"iam user from the java sdk", backendUser, "aws-sdk-java/2.25.0 Linux/5.10 OpenJDK_64-Bit_Server_VM/17", model.ActorPipeline, SessionWorkload, "arn:aws:iam::111122223333:user/auth-backend"},
		{"iam user from boto3", backendUser, "Boto3/1.34.0 md/Botocore#1.34.0 ua/2.0 os/linux", model.ActorPipeline, SessionWorkload, "arn:aws:iam::111122223333:user/auth-backend"},
		{"iam user from the cli", backendUser, "aws-cli/2.15.0 md/awscrt#0.19 ua/2.0 os/macos botocore/2.4.5", model.ActorPerson, SessionOther, "arn:aws:iam::111122223333:user/auth-backend"},
		{"iam user in the console", backendUser, "AWS Internal", model.ActorPerson, SessionConsole, "arn:aws:iam::111122223333:user/auth-backend"},
		{"instance role from the sdk", noIssuer, "aws-sdk-go/1.50.0", model.ActorPipeline, SessionWorkload, "arn:aws:iam::111122223333:role/node-role"},
		{"sso person from the sdk", assumed("AWSReservedSSO_Admin_0123", "alice"), "Boto3/1.34.0", model.ActorPerson, SessionSSO, "arn:aws:iam::111122223333:role/AWSReservedSSO_Admin_0123"},
		{"aws service names itself", Identity{Type: "AWSService", InvokedBy: "config.amazonaws.com"}, "config.amazonaws.com", model.ActorAWSService, SessionService, "config.amazonaws.com"},
		{"anonymous token call", Identity{Type: "Unknown"}, "Mozilla/5.0", model.ActorUnknown, SessionOther, ""},
	}
	for _, tc := range cases {
		actor := ClassifyActor(tc.identity, tc.userAgent)
		if actor.Class != tc.class || actor.SessionClass != tc.session || actor.PrincipalARN != tc.principal {
			t.Errorf("%s = %+v, want %s/%s %q", tc.name, actor, tc.class, tc.session, tc.principal)
		}
	}
}
