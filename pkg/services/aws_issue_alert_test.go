package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/aws/issues"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

var awsAlertSince = time.Date(2026, 11, 25, 13, 2, 0, 0, time.UTC)

func awsAttackIssue() issues.Issue {
	return issues.Issue{
		ID: issues.IssueID("attack-sequence", "arn:aws:eks:eu-west-1:111122223333:cluster/ci-eks"), Rule: "attack-sequence",
		Kind: issues.KindSecurity, Severity: issues.SeverityCritical, Tier: issues.TierActNow, Account: "111122223333", Region: "eu-west-1",
		Owner: "arn:aws:eks:eu-west-1:111122223333:cluster/ci-eks", Title: "Attack sequence detected on cluster ci-eks",
		State: issues.StateOpen, Confirm: issues.Confirmed, Since: awsAlertSince, SinceKnown: true,
		Evidence: []issues.Evidence{
			{Text: "GuardDuty attack sequence, 141 runtime signals", Status: issues.EvidenceProven},
			{Text: "Node role may access 12 repositories and 2 buckets", Status: issues.EvidenceAssumed},
			{Text: "AWS Health needs a support plan", Status: issues.EvidenceNotRead},
		},
		Cause: &issues.Cause{Status: "found", Text: "Runner image updated by ci-deployer, 2 h before"},
		Steps: []issues.Step{{Text: "Roll the runners back to the image before 11:02", Command: "kubectl -n ci rollout undo deployment/ci-runner"}},
	}
}

// The alert says what the mock-up shows: the title, where and since, each
// link of the evidence with its status, the likely cause, what to do and
// a link to the issue.
func TestAWSIssueAlertContent(t *testing.T) {
	alert := issues.Alert{Kind: issues.AlertOpened, Issue: awsAttackIssue(), Key: "opened:1", AccountName: "platform"}
	content := awsIssueContent(alert, "https://versus.example")
	lines := strings.Join(content["Lines"].([]string), "\n")
	for _, want := range []string{
		"platform · eu-west-1 · cluster ci-eks · since 2026-11-25 13:02 UTC",
		"Proven: GuardDuty attack sequence, 141 runtime signals",
		"Assumed: Node role may access 12 repositories and 2 buckets",
		"Not read: AWS Health needs a support plan",
		"Likely cause: Runner image updated by ci-deployer, 2 h before",
		"Roll the runners back to the image before 11:02",
		"kubectl -n ci rollout undo deployment/ci-runner",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("alert lines miss %q:\n%s", want, lines)
		}
	}
	if content["AlertName"] != "Attack sequence detected on cluster ci-eks" || content["Severity"] != "critical" ||
		content["IssueURL"] != "https://versus.example/aws/issues/"+alert.Issue.ID {
		t.Fatalf("content = %+v", content)
	}
	none := awsAttackIssue()
	none.Cause = &issues.Cause{Status: "none", Text: "no change found before it"}
	if lines := strings.Join(awsIssueContent(issues.Alert{Kind: issues.AlertOpened, Issue: none, Key: "k"}, "")["Lines"].([]string), "\n"); strings.Contains(lines, "Likely cause") {
		t.Fatalf("a likely cause is shown only when one was found:\n%s", lines)
	}
	regressed := awsIssueContent(issues.Alert{Kind: issues.AlertRegressed, Issue: awsAttackIssue(), Key: "k",
		Detail: "rejected issue reopened: its material evidence changed"}, "")
	if !strings.HasPrefix(regressed["AlertName"].(string), "Reopened: ") ||
		!strings.Contains(strings.Join(regressed["Lines"].([]string), "\n"), "its material evidence changed") {
		t.Fatalf("regressed content = %+v", regressed)
	}
}

// A coverage notice names the source that can no longer be read, why, its
// last good read, the issue that can't be confirmed and the actions to add
// back, and links to the coverage page. It never pages.
func TestAWSIssueCoverageNoticeContent(t *testing.T) {
	alert := issues.Alert{Kind: issues.AlertCoverage, Issue: awsAttackIssue(), Key: "coverage:1", AccountName: "platform",
		Unread: []issues.Unread{{Source: "guardduty", Reason: model.GapDenied, LastGoodAt: time.Date(2026, 11, 25, 14, 12, 0, 0, time.UTC),
			Fix: []string{"guardduty:ListFindings"}}}}
	content := awsIssueContent(alert, "https://versus.example")
	lines := strings.Join(content["Lines"].([]string), "\n")
	if content["AlertName"] != "Versus can no longer read guardduty: permission removed" || content["Severity"] != "medium" ||
		!strings.Contains(lines, "platform · last good read 2026-11-25 14:12 UTC") ||
		!strings.Contains(lines, "1 open issue can't be confirmed and stays open") ||
		!strings.Contains(lines, "Add guardduty:ListFindings back to the Versus read role") ||
		content["IssueURL"] != "https://versus.example/aws/coverage" {
		t.Fatalf("coverage notice = %+v\n%s", content, lines)
	}
}

// Everything an alert carries goes through the redactor: a key pasted into
// a title, evidence, a step, the likely cause or a digest line never
// reaches a channel.
func TestAWSIssueAlertContentIsRedacted(t *testing.T) {
	const key = "AKIAIOSFODNN7EXAMPLE"
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	issue := awsAttackIssue()
	issue.Title = "Access key " + key + " used from a new address"
	issue.Evidence = append(issue.Evidence, issues.Evidence{Text: "aws_secret_access_key=" + secret, Status: issues.EvidenceProven})
	issue.Cause.Text = "Key " + key + " created by ci-deployer, 5 min before"
	issue.Steps = []issues.Step{{Text: "Deactivate " + key, Command: "aws iam update-access-key --access-key-id " + key + " --status Inactive"}}
	alert := issues.Alert{Kind: issues.AlertOpened, Issue: issue, Key: "k"}
	for name, content := range map[string]map[string]interface{}{
		"alert":  awsIssueContent(alert, "https://versus.example"),
		"digest": awsIssueDigestContent([]issues.Alert{alert, alert, alert, alert}, ""),
	} {
		encoded, _ := json.Marshal(content)
		if strings.Contains(string(encoded), key) || strings.Contains(string(encoded), secret) {
			t.Fatalf("%s carries a secret: %s", name, encoded)
		}
	}
}

// One alert goes out once through the shared episode: a repeat of the
// same transition coalesces, the next regress is a new alert, and a digest
// is one message.
func TestAWSIssueAlertsOnceThroughTheSharedEpisode(t *testing.T) {
	loadAgentTestConfig(t)
	previous := store
	SetStorage(storage.NewMemory())
	defer SetStorage(previous)
	var sent []string
	SetEmitInterceptor(func(content map[string]interface{}, _ string) EmitDecision {
		sent = append(sent, content["IssueAlert"].(string))
		return EmitDecision{Action: EmitProceed}
	})
	defer SetEmitInterceptor(nil)
	opened := issues.Alert{Kind: issues.AlertOpened, Issue: awsAttackIssue(), Key: "opened:1"}
	for range 2 {
		if err := CreateIncidentFromAWSIssue(context.Background(), "", opened); err != nil {
			t.Fatal(err)
		}
	}
	regressed := issues.Alert{Kind: issues.AlertRegressed, Issue: awsAttackIssue(), Key: "regressed:2"}
	if err := CreateIncidentFromAWSIssue(context.Background(), "", regressed); err != nil {
		t.Fatal(err)
	}
	if err := CreateIncidentFromAWSIssueDigest(context.Background(), "", []issues.Alert{opened, regressed}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sent, ",") != "opened,regressed,digest" {
		t.Fatalf("sent = %v; want one opening, one regress and one digest", sent)
	}
	if err := CreateIncidentFromAWSIssue(context.Background(), "", issues.Alert{Kind: "resolved", Issue: awsAttackIssue(), Key: "k"}); err == nil {
		t.Fatal("only opened, regressed and coverage alerts are sent")
	}
}

// A line longer than an alert carries is cut on a character boundary, so
// the cut never leaves a broken character in the message.
func TestAWSIssueAlertCutKeepsValidText(t *testing.T) {
	line := strings.Repeat("€", 300) // 900 bytes, three per character: 512 falls inside one
	got := awsScrub(line)
	if len(got) > awsAlertTextMax || !utf8.ValidString(got) || !strings.HasPrefix(line, got) {
		t.Fatalf("cut line is %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

// A secret wrapped over a line break is redacted in an alert line: the
// redactor reads the text with its line breaks, and only then is the
// whitespace folded.
func TestAWSIssueAlertRedactsWrappedSecrets(t *testing.T) {
	for text, secret := range map[string]string{
		"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG\n/bPxRfiCYEXAMPLEKEY": "bPxRfiCYEXAMPLEKEY",
		"the key is AKIAIOSFOD\nNN7EXAMPLE":                                  "NN7EXAMPLE",
	} {
		if got := awsScrub(text); strings.Contains(got, secret) || strings.Contains(got, "\n") {
			t.Errorf("awsScrub(%q) = %q; want the wrapped secret redacted and the line folded", text, got)
		}
	}
}
