package changes

import (
	"regexp"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Session classes. The session name itself is never stored: it is often a
// person's email address.
const (
	SessionCI      = "ci"
	SessionSSO     = "sso"
	SessionConsole = "console"
	SessionService = "service"
	// SessionWorkload is a program running on AWS or elsewhere: a
	// Kubernetes service account's web-identity role, an instance role, or
	// an IAM user's access key used from an SDK.
	SessionWorkload = "workload"
	SessionOther    = "other"
)

// Identity is the subset of CloudTrail userIdentity the classifier reads.
type Identity struct {
	Type           string         `json:"type"`
	ARN            string         `json:"arn"`
	UserName       string         `json:"userName"`
	InvokedBy      string         `json:"invokedBy"`
	SessionContext sessionContext `json:"sessionContext"`
}

type sessionContext struct {
	SessionIssuer       sessionIssuer        `json:"sessionIssuer"`
	WebIDFederationData *webIDFederationData `json:"webIdFederationData"`
}

// webIDFederationData is set on calls made with credentials from
// AssumeRoleWithWebIdentity, such as a Kubernetes service account's role.
type webIDFederationData struct {
	FederatedProvider string `json:"federatedProvider"`
}

type sessionIssuer struct {
	Type     string `json:"type"`
	ARN      string `json:"arn"`
	UserName string `json:"userName"`
}

var (
	pipelineAgent = regexp.MustCompile(`(?i)(terraform|hashicorp|pulumi|aws-cdk|codepipeline|codebuild|github|gitlab|jenkins|circleci|buildkite|atlantis|spinnaker|argocd|serverless|copilot|sam-cli)`)
	pipelineName  = regexp.MustCompile(`(?i)(github|gitlab|jenkins|circleci|codebuild|codepipeline|deploy|pipeline|terraform|atlantis|spinnaker|argo|buildkite|bitbucket|(^|[-_])ci([-_]|$)|(^|[-_])cd([-_]|$))`)
	consoleAgent  = regexp.MustCompile(`(?i)(console\.amazonaws\.com|signin\.amazonaws\.com|aws[- ]internal)`)
	emailShape    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	// cliAgent is a person's command line: the AWS CLI (CloudShell runs
	// it too) or the PowerShell tools. The CLI's own agent also names
	// botocore, so it is checked before sdkAgent.
	cliAgent = regexp.MustCompile(`(?i)(^|[\s\[(])(aws-cli/|AWSPowerShell)`)
	// sdkAgent is a program calling through an AWS SDK.
	sdkAgent = regexp.MustCompile(`(?i)(aws-sdk-|boto3/|botocore/|aws-go-sdk|aws-sdk-go|okhttp/|aws-chalice|aws-lambda-powertools|karpenter|aws-load-balancer-controller)`)
	// workloadSession is a session name AWS gives a Kubernetes pod's role
	// (EKS Pod Identity) or an SDK default session.
	workloadSession = regexp.MustCompile(`^(eks-|botocore-session-|aws-go-sdk-|aws-sdk-)`)
)

// onBehalfServices make calls for the principal that started them. The
// change belongs to that principal, not to an AWS service.
var onBehalfServices = map[string]string{
	"cloudformation.amazonaws.com": "cloudformation",
	"servicecatalog.amazonaws.com": "servicecatalog",
}

// MaskedEmail replaces an email-shaped IAM user name in a principal ARN.
// Email-shaped names identify a person and are never stored or shown.
const MaskedEmail = "(email hidden)"

// MaskPrincipal hides an email-shaped final name in a user or role ARN.
func MaskPrincipal(arn string) string {
	index := strings.LastIndex(arn, "/")
	if index < 0 {
		return arn
	}
	if emailShape.MatchString(arn[index+1:]) {
		return arn[:index+1] + MaskedEmail
	}
	return arn
}

// ClassifyActor decides pipeline, person or AWS service from the identity
// type, the session issuer, the session name pattern and the user agent.
// What it keeps is the class, the principal type and the issuing user or role
// ARN, with an email-shaped user name masked. A call CloudFormation or
// Service Catalog made for a caller is classified as that caller's, with
// Via naming the service.
//
// Automation (the pipeline class) covers pipelines, web-identity and
// instance roles, roles a service acts through, and IAM users or roles
// calling from an SDK. A person is a console session, an SSO permission set,
// an email-shaped session, root, or an IAM user or role used from the AWS
// CLI. Unknown is left for a call with no principal name.
func ClassifyActor(identity Identity, userAgent string) model.Actor {
	actor := model.Actor{Class: model.ActorUnknown, PrincipalType: identity.Type, SessionClass: SessionOther}
	issuer := identity.SessionContext.SessionIssuer
	roleName := issuer.UserName
	sessionName := ""
	switch identity.Type {
	case "AssumedRole":
		actor.PrincipalARN = issuer.ARN
		if parts := strings.Split(identity.ARN, "/"); len(parts) >= 3 {
			sessionName = parts[len(parts)-1]
			if roleName == "" {
				roleName = strings.Join(parts[1:len(parts)-1], "/")
			}
			if actor.PrincipalARN == "" {
				actor.PrincipalARN = roleARN(identity.ARN, roleName)
			}
		}
	case "IAMUser", "Root", "FederatedUser":
		actor.PrincipalARN = identity.ARN
	case "AWSService":
		actor.PrincipalARN = identity.InvokedBy
	}
	actor.PrincipalARN = MaskPrincipal(actor.PrincipalARN)
	via := onBehalfServices[identity.InvokedBy]
	actor.Via = via

	userName := ""
	if identity.Type == "IAMUser" {
		userName = identity.UserName
		if userName == "" {
			if index := strings.LastIndex(identity.ARN, "/"); index >= 0 {
				userName = identity.ARN[index+1:]
			}
		}
	}
	webIdentity := identity.Type == "WebIdentityUser" || identity.SessionContext.WebIDFederationData != nil
	named := actor.PrincipalARN != "" || roleName != "" || userName != "" || identity.Type == "WebIdentityUser" && identity.UserName != ""

	set := func(class model.ActorClass, session string) model.Actor {
		actor.Class, actor.SessionClass = class, session
		return actor
	}
	switch {
	case identity.Type == "AWSService",
		strings.Contains(issuer.ARN, ":role/aws-service-role/"),
		via == "" && identity.InvokedBy != "" && strings.HasSuffix(identity.InvokedBy, ".amazonaws.com") && !consoleAgent.MatchString(identity.InvokedBy):
		return set(model.ActorAWSService, SessionService)
	case pipelineAgent.MatchString(userAgent):
		return set(model.ActorPipeline, SessionCI)
	case !named:
		return actor
	// A person's SSO permission set can be named for its job
	// (AWSReservedSSO_DeployAccess); the person is still a person.
	case strings.HasPrefix(roleName, "AWSReservedSSO_"):
		return set(model.ActorPerson, SessionSSO)
	case pipelineName.MatchString(roleName), pipelineName.MatchString(sessionName), userName != "" && !emailShape.MatchString(userName) && pipelineName.MatchString(userName):
		return set(model.ActorPipeline, SessionCI)
	case consoleAgent.MatchString(userAgent):
		return set(model.ActorPerson, SessionConsole)
	case via != "":
		// A role CloudFormation or Service Catalog acts through is the
		// stack's service role: automation, not a person.
		return set(model.ActorPipeline, SessionService)
	case webIdentity, workloadSession.MatchString(sessionName):
		return set(model.ActorPipeline, SessionWorkload)
	case identity.Type == "Root", emailShape.MatchString(sessionName):
		return set(model.ActorPerson, SessionOther)
	case cliAgent.MatchString(userAgent):
		return set(model.ActorPerson, SessionOther)
	case identity.Type == "IAMUser", identity.Type == "AssumedRole", identity.Type == "FederatedUser":
		// An access key or role used from an SDK, or from an agent that
		// names neither the console nor the CLI, is a program.
		return set(model.ActorPipeline, SessionWorkload)
	}
	return actor
}

// roleARN builds the role ARN of an assumed-role session ARN
// (arn:aws:sts::<account>:assumed-role/<role>/<session>) when the event
// carries no session issuer.
func roleARN(sessionARN, roleName string) string {
	parts := strings.SplitN(sessionARN, ":", 6)
	if len(parts) < 6 || roleName == "" {
		return ""
	}
	return parts[0] + ":" + parts[1] + ":iam::" + parts[4] + ":role/" + roleName
}

// IsEmail reports whether a value is shaped like an email address.
func IsEmail(value string) bool { return emailShape.MatchString(value) }
