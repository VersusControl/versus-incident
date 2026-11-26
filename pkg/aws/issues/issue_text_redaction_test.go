package issues

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// A rule that puts a pasted key in a finding's title, evidence and steps
// stores the issue redacted, every reader gets it redacted, and evaluating
// it again does not rewrite it.
func TestFindingTextIsStoredRedacted(t *testing.T) {
	const keyID = "AKIAIOSFODNN7EXAMPLE"
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	for name, provider := range singleReplicaBackends(t) {
		t.Run(name, func(t *testing.T) {
			scope := tenancy.DefaultOrgScope()
			rules := NewRegistry()
			if err := rules.Register(&testRule{id: "key-in-task", kind: KindSecurity, inputs: []Input{{Service: "s3"}}, also: []Finding{{
				Owner: "arn:aws:ecs:eu-west-1:" + testAccount + ":task-definition/app:3", Region: "eu-west-1", Severity: SeverityHigh,
				Title:    "Access key " + keyID + " in task definition app",
				Evidence: []Evidence{{Text: "environment holds aws_secret_access_key=" + secret, Status: EvidenceProven}},
				Steps:    []Step{{Text: "Rotate " + keyID, Command: "aws iam update-access-key --access-key-id " + keyID + " --status Inactive"}},
				Material: Material{"key": "app"},
			}}}); err != nil {
				t.Fatal(err)
			}
			now := &clock{now: start}
			evaluator, _ := NewEvaluator(provider, scope, newFakeSource(testAccount), WithRules(rules), WithClock(now.Now))
			if _, err := evaluator.Evaluate(context.Background()); err != nil {
				t.Fatal(err)
			}
			actions, _ := NewActions(provider, scope, now.Now)
			list, err := actions.List(context.Background(), storage.AWSIssueQuery{})
			if err != nil || len(list) != 1 {
				t.Fatalf("issues = %d, %v", len(list), err)
			}
			report := BuildReport(list, Filter{}, CoverageRead{}, start)
			for what, value := range map[string]any{"issue": list[0], "report": report} {
				encoded, _ := json.Marshal(value)
				if strings.Contains(string(encoded), keyID) || strings.Contains(string(encoded), secret) {
					t.Fatalf("the %s carries the key: %s", what, encoded)
				}
			}
			store, _ := storage.AWSIssuesOf(provider)
			records, _ := store.AWSIssues(context.Background(), scope, storage.AWSIssueQuery{})
			if strings.Contains(records[0].Title, keyID) {
				t.Fatalf("stored title = %q", records[0].Title)
			}
			version := list[0].Version
			for range 3 {
				now.Add(time.Minute)
				if _, err := evaluator.Evaluate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if again, _, _ := actions.Get(context.Background(), list[0].ID); again.Version != version {
				t.Fatalf("an unchanged finding was rewritten: version %d -> %d", version, again.Version)
			}
		})
	}
}

// Text that reaches an issue outside the rules, such as the likely cause
// the alert pump looks up, is redacted when the issue is recorded too.
func TestLikelyCauseIsStoredRedacted(t *testing.T) {
	const keyID = "AKIAIOSFODNN7EXAMPLE"
	provider := storage.NewMemory()
	source := newFakeSource(testAccount)
	source.public(testAccount, "1", "docs-site")
	source.setSeverity(testAccount, SeverityMedium)
	now := &clock{now: start}
	finder := func(context.Context, Issue) (*Cause, error) {
		return &Cause{Status: "found", Text: "Key " + keyID + " created by deployer, 5 min before", Checked: now.Now()}, nil
	}
	replica := newAlerting(t, provider, tenancy.DefaultOrgScope(), source, severityRules(), now, WithCauseFinder(finder))
	replica.evaluate(t)
	actions, _ := NewActions(provider, tenancy.DefaultOrgScope(), now.Now)
	issue, _, err := actions.Get(context.Background(), IssueID(testRuleID, "arn:aws:s3:::docs-site"))
	if err != nil || !issue.Cause.Found() || strings.Contains(issue.Cause.Text, keyID) {
		t.Fatalf("stored cause = %+v, %v", issue.Cause, err)
	}
}
