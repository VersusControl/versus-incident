package awsdeploy

import (
	"encoding/json"
	"strings"
	"testing"
)

// quickstartPolicyFile is the IAM policy for the quickstart IAM user. The
// user signs in with an access key and reads one account directly, so this
// one document carries what the two roles grant: the versus-reader metadata
// reads and the versus-cost-reader cost reads, with the explicit denies of
// both.
const quickstartPolicyFile = "versus-quickstart-policy.json"

// inlineUserPolicyLimit is the IAM size limit for a policy put on a user with
// put-user-policy, counted without whitespace.
const inlineUserPolicyLimit = 2048

func readQuickstartPolicy(t *testing.T) (cliPolicyDocument, string) {
	t.Helper()

	body := readArtifact(t, quickstartPolicyFile)
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	var policy cliPolicyDocument
	if err := decoder.Decode(&policy); err != nil {
		t.Fatalf("parse %s: %v", quickstartPolicyFile, err)
	}
	return policy, body
}

// The quickstart policy allows exactly the reads the two role templates
// allow unconditionally (versus-reader's metadata reads plus
// versus-cost-reader's cost reads) and denies exactly what they deny by
// name. The opt-in posture reads of versus-reader are not part of it, and
// the cost reader's deny-everything-else statement is left out because it
// would deny the metadata reads.
func TestQuickstartPolicyMatchesReaderTemplates(t *testing.T) {
	reader := parseRoleTemplate(t, readerArtifact)
	costReader := parseRoleTemplate(t, costReaderArtifact)
	wantAllow := map[string]bool{}
	wantDeny := map[string]bool{}
	for _, set := range []map[string]bool{reader.allow, costReader.allow} {
		for action := range set {
			wantAllow[action] = true
		}
	}
	for _, set := range []map[string]bool{reader.deny, costReader.deny} {
		for action := range set {
			wantDeny[action] = true
		}
	}

	policy, _ := readQuickstartPolicy(t)
	gotAllow := map[string]bool{}
	gotDeny := map[string]bool{}
	for _, statement := range policy.Statement {
		for _, action := range statement.Action {
			switch statement.Effect {
			case "Allow":
				if gotAllow[action] {
					t.Errorf("%s allows %q twice", quickstartPolicyFile, action)
				}
				gotAllow[action] = true
			case "Deny":
				if gotDeny[action] {
					t.Errorf("%s denies %q twice", quickstartPolicyFile, action)
				}
				gotDeny[action] = true
			}
		}
	}
	if missing, extra := diffSets(wantAllow, gotAllow); len(missing)+len(extra) > 0 {
		t.Errorf("%s allow list differs from the templates: missing %v, extra %v", quickstartPolicyFile, missing, extra)
	}
	if missing, extra := diffSets(wantDeny, gotDeny); len(missing)+len(extra) > 0 {
		t.Errorf("%s deny list differs from the templates: missing %v, extra %v", quickstartPolicyFile, missing, extra)
	}
	for _, action := range []string{"s3:GetObject", "secretsmanager:GetSecretValue", "ssm:GetParameter*", "kms:Decrypt", "logs:GetLogEvents"} {
		if !gotDeny[action] {
			t.Errorf("%s does not deny %q", quickstartPolicyFile, action)
		}
	}
	for action := range reader.conditionalAll() {
		if gotAllow[action] && !costReader.allow[action] {
			t.Errorf("%s allows the opt-in read %q, which the quickstart does not use", quickstartPolicyFile, action)
		}
	}
}

// Every statement is a plain unconditional allow or deny over every
// resource, the allow list holds reads only, and the document fits the IAM
// size limit for a user's inline policy.
func TestQuickstartPolicyAllowsOnlyReads(t *testing.T) {
	policy, body := readQuickstartPolicy(t)
	if policy.Version != "2012-10-17" {
		t.Errorf("Version = %q, want 2012-10-17", policy.Version)
	}
	if len(policy.Statement) == 0 {
		t.Fatalf("%s has no statements", quickstartPolicyFile)
	}
	sids := map[string]bool{}
	for _, statement := range policy.Statement {
		if statement.Sid == "" || sids[statement.Sid] {
			t.Errorf("statement Sid %q is empty or repeated", statement.Sid)
		}
		sids[statement.Sid] = true
		if statement.Effect != "Allow" && statement.Effect != "Deny" {
			t.Errorf("statement %q Effect = %q, want Allow or Deny", statement.Sid, statement.Effect)
		}
		if statement.Resource != "*" {
			t.Errorf("statement %q Resource = %q, want *", statement.Sid, statement.Resource)
		}
		if statement.Principal != nil || statement.Condition != nil {
			t.Errorf("statement %q sets a Principal or Condition; an identity policy needs neither", statement.Sid)
		}
		if len(statement.Action) == 0 {
			t.Errorf("statement %q names no action", statement.Sid)
		}
		if statement.Effect != "Allow" {
			continue
		}
		for _, action := range statement.Action {
			if !isReadAction(action) {
				t.Errorf("%s allows %q, which is not a read", quickstartPolicyFile, action)
			}
		}
	}
	if size := len(strings.Join(strings.Fields(body), "")); size > inlineUserPolicyLimit {
		t.Errorf("%s is %d characters without whitespace; put-user-policy accepts at most %d", quickstartPolicyFile, size, inlineUserPolicyLimit)
	}
}
