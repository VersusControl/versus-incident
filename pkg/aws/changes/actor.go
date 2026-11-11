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
	SessionOther   = "other"
)

// Identity is the subset of CloudTrail userIdentity the classifier reads.
type Identity struct {
	Type           string         `json:"type"`
	ARN            string         `json:"arn"`
	InvokedBy      string         `json:"invokedBy"`
	SessionContext sessionContext `json:"sessionContext"`
}

type sessionContext struct {
	SessionIssuer sessionIssuer `json:"sessionIssuer"`
}

type sessionIssuer struct {
	Type     string `json:"type"`
	ARN      string `json:"arn"`
	UserName string `json:"userName"`
}

var (
	pipelineAgent = regexp.MustCompile(`(?i)(terraform|hashicorp|pulumi|aws-cdk|codepipeline|codebuild|github|gitlab|jenkins|circleci|buildkite|atlantis|spinnaker|argocd|serverless|copilot|sam-cli)`)
	pipelineName  = regexp.MustCompile(`(?i)(github|gitlab|jenkins|circleci|codebuild|codepipeline|deploy|pipeline|terraform|atlantis|spinnaker|argo|buildkite|bitbucket|(^|[-_])ci([-_]|$)|(^|[-_])cd([-_]|$))`)
	consoleAgent  = regexp.MustCompile(`(?i)(console\.amazonaws\.com|signin\.amazonaws\.com|aws-internal)`)
	emailShape    = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// ClassifyActor decides pipeline, person or AWS service from the identity
// type, the session issuer, the session name pattern and the user agent.
// What it keeps is the class, the principal type and the issuing user or role
// ARN.
func ClassifyActor(identity Identity, userAgent string) model.Actor {
	actor := model.Actor{Class: model.ActorUnknown, PrincipalType: identity.Type, SessionClass: SessionOther}
	issuer := identity.SessionContext.SessionIssuer
	switch identity.Type {
	case "AssumedRole":
		actor.PrincipalARN = issuer.ARN
	case "IAMUser", "Root":
		actor.PrincipalARN = identity.ARN
	}

	roleName := issuer.UserName
	sessionName := ""
	if identity.Type == "AssumedRole" {
		if parts := strings.Split(identity.ARN, "/"); len(parts) >= 3 {
			sessionName = parts[len(parts)-1]
			if roleName == "" {
				roleName = parts[len(parts)-2]
			}
		}
	}

	switch {
	case identity.Type == "AWSService",
		strings.Contains(issuer.ARN, ":role/aws-service-role/"),
		identity.InvokedBy != "" && strings.HasSuffix(identity.InvokedBy, ".amazonaws.com") && !consoleAgent.MatchString(identity.InvokedBy):
		actor.Class = model.ActorAWSService
		actor.SessionClass = SessionService
	case pipelineAgent.MatchString(userAgent), pipelineName.MatchString(roleName), pipelineName.MatchString(sessionName):
		actor.Class = model.ActorPipeline
		actor.SessionClass = SessionCI
	case strings.HasPrefix(roleName, "AWSReservedSSO_"):
		actor.Class = model.ActorPerson
		actor.SessionClass = SessionSSO
	case consoleAgent.MatchString(userAgent):
		actor.Class = model.ActorPerson
		actor.SessionClass = SessionConsole
	case identity.Type == "IAMUser", identity.Type == "Root", emailShape.MatchString(sessionName):
		actor.Class = model.ActorPerson
	}
	return actor
}
