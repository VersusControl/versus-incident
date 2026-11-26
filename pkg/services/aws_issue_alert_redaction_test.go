package services

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

const (
	pastedKeyID    = "AKIAIOSFODNN7EXAMPLE"
	pastedSecret   = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	pastedPassword = "hunter2-Sup3rSecret"
)

// alertCarries reports which of the secrets the encoded alert content still
// holds, whole or as a long fragment.
func alertCarries(t *testing.T, content map[string]interface{}, secrets ...string) []string {
	t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	var leaked []string
	for _, secret := range secrets {
		// A fragment of 12 characters or more of a secret is a leak too.
		for size := len(secret); size >= 12; size-- {
			if strings.Contains(text, secret[:size]) {
				leaked = append(leaked, secret[:size])
				break
			}
		}
	}
	return leaked
}

// A password pasted into a title or evidence, in the forms people paste it,
// never reaches the alert body, the digest or a coverage notice.
func TestAWSIssueAlertRedactsPasswords(t *testing.T) {
	forms := map[string]string{
		"name=value":         "password=" + pastedPassword,
		"name: value":        "Password: " + pastedPassword,
		"database URL":       "postgres://admin:" + pastedPassword + "@db.internal:5432/app",
		"secret=value":       "secret=" + pastedPassword,
		"pwd label":          "pwd=" + pastedPassword,
		"labelled key value": "aws_secret_access_key=" + pastedSecret,
		"bare key ID":        "key " + pastedKeyID + " is used",
	}
	for name, pasted := range forms {
		t.Run(name, func(t *testing.T) {
			issue := awsAttackIssue()
			issue.Title = "Credentials in a task definition: " + pasted
			issue.Evidence = []issues.Evidence{{Text: "environment holds " + pasted, Status: issues.EvidenceProven}}
			opened := issues.Alert{Kind: issues.AlertOpened, Issue: issue, Key: "opened:1"}
			regressed := issues.Alert{Kind: issues.AlertRegressed, Issue: issue, Key: "regressed:2", Detail: "rejected issue reopened: " + pasted}
			coverage := issues.Alert{Kind: issues.AlertCoverage, Issue: issue, Key: "coverage:1",
				Unread: []issues.Unread{{Source: "ecs", Reason: model.GapDenied}}}
			for kind, content := range map[string]map[string]interface{}{
				"opened":    awsIssueContent(opened, "https://versus.example"),
				"regressed": awsIssueContent(regressed, "https://versus.example"),
				"coverage":  awsIssueContent(coverage, "https://versus.example"),
				"digest":    awsIssueDigestContent([]issues.Alert{opened, opened, opened, opened}, ""),
			} {
				if leaked := alertCarries(t, content, pastedPassword, pastedSecret, pastedKeyID); len(leaked) > 0 {
					t.Errorf("%s alert carries %q", kind, leaked)
				}
			}
		})
	}
}

// A line longer than an alert carries is cut to its length. A secret that
// straddles the cut must not leave its first part in the alert: the cut
// may not turn a secret the redactor knows into a fragment it does not.
func TestAWSIssueAlertCutNeverLeavesASecretFragment(t *testing.T) {
	for name, pasted := range map[string]string{
		"labelled secret key": "aws_secret_access_key=" + pastedSecret,
		"password":            "password=" + pastedPassword,
		"key ID":              pastedKeyID,
	} {
		t.Run(name, func(t *testing.T) {
			secret := strings.TrimPrefix(strings.TrimPrefix(pasted, "aws_secret_access_key="), "password=")
			// Put the cut 8 characters before the end of the secret.
			pad := awsAlertTextMax - (len(pasted) - 8)
			for _, field := range []string{"title", "evidence"} {
				issue := awsAttackIssue()
				text := strings.Repeat("x", pad-1) + " " + pasted + " in the task definition"
				if field == "title" {
					issue.Title = text
				} else {
					issue.Evidence = []issues.Evidence{{Text: text, Status: issues.EvidenceProven}}
				}
				content := awsIssueContent(issues.Alert{Kind: issues.AlertOpened, Issue: issue, Key: "opened:1"}, "")
				if leaked := alertCarries(t, content, secret); len(leaked) > 0 {
					t.Errorf("%s cut at the alert length leaves %q (%d of %d characters)", field, leaked[0], len(leaked[0]), len(secret))
				}
			}
		})
	}
}
