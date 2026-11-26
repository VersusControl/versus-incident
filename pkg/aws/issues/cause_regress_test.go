package issues

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/suspect"
)

// changeList serves changes from a list; every window is read.
type changeList []model.Change

func (list changeList) Changes(_ context.Context, account, region string, from, to time.Time) ([]model.Change, error) {
	var out []model.Change
	for _, change := range list {
		if change.Account == account && change.Region == region && !change.Time.Before(from) && change.Time.Before(to) {
			out = append(out, change)
		}
	}
	return out, nil
}

func (changeList) Settled(string, string, time.Time, time.Time) bool { return true }

// An issue that existed when first read has no likely cause to look for;
// when it resolves and regresses, the pump looks the cause up again,
// anchored on the regress, and the regress alert carries the change made
// just before it.
func TestRegressLooksUpTheCauseOnTheNewSince(t *testing.T) {
	const owner = "arn:aws:s3:::cause-docs"
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			rule := &scriptedRule{id: "cause-public-bucket", inputs: []Input{{Service: "s3"}}}
			registry := NewRegistry()
			if err := registry.Register(rule); err != nil {
				t.Fatal(err)
			}
			source := newScriptedSource(flowAccount)
			source.read(flowAccount, "s3", "eu-west-1", ReadOK)
			now := &clock{now: start}
			regressAt := start.Add(20 * time.Minute)
			changes := changeList{
				{ID: "before-open", Time: start.Add(-time.Hour), Account: flowAccount, Region: "eu-west-1", EventName: "PutBucketPolicy",
					Summary: "Bucket policy changed", Resources: []string{owner}, Actor: model.Actor{PrincipalARN: "arn:aws:iam::" + flowAccount + ":user/old"}},
				{ID: "before-regress", Time: regressAt.Add(-5 * time.Minute), Account: flowAccount, Region: "eu-west-1", EventName: "PutBucketPolicy",
					Summary: "Bucket policy changed", Resources: []string{owner}, Actor: model.Actor{PrincipalARN: "arn:aws:iam::" + flowAccount + ":role/deployer"}},
			}
			var mu sync.Mutex
			var anchors []suspect.IssueAnchor
			finder := func(ctx context.Context, issue Issue) (*Cause, error) {
				anchor := IssueAnchorOf(issue)
				anchor.History = 14 * 24 * time.Hour
				mu.Lock()
				anchors = append(anchors, anchor)
				mu.Unlock()
				result, err := suspect.Find(ctx, suspect.AnchorIssue(anchor, nil), changes, changes)
				if err != nil {
					return nil, err
				}
				return CauseOf(result, anchor.Start, now.Now()), nil
			}
			sink := &recordingSink{}
			scope := newScope("cause")
			alerter, err := NewAlerter(backend.provider, scope, sink, WithAlertClock(now.Now), WithCauseFinder(finder))
			if err != nil {
				t.Fatal(err)
			}
			evaluator, err := NewEvaluator(backend.provider, scope, source, WithRules(registry), WithClock(now.Now),
				WithAfterEvaluate(func(ctx context.Context, report Report) { _ = alerter.Pump(ctx, report) }))
			if err != nil {
				t.Fatal(err)
			}
			evaluate := func() {
				t.Helper()
				if _, err := evaluator.Evaluate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}

			rule.set(publicFinding(owner, "eu-west-1", SeverityCritical, "*", "1"))
			evaluate()
			now.Add(10 * time.Minute)
			rule.set()
			evaluate()
			now.Add(10 * time.Minute)
			rule.set(publicFinding(owner, "eu-west-1", SeverityCritical, "*", "2"))
			evaluate()
			now.Add(time.Minute)
			evaluate()

			mu.Lock()
			defer mu.Unlock()
			if len(anchors) != 2 || !anchors[0].Start.IsZero() || !anchors[1].Start.Equal(regressAt) {
				t.Fatalf("cause lookups anchored on %+v; want first read (no start), then the regress at %s", anchors, regressAt)
			}
			sent, _ := sink.alerts()
			if len(sent) != 2 || sent[0].Kind != AlertOpened || sent[1].Kind != AlertRegressed {
				t.Fatalf("alerts = %+v", sent)
			}
			if cause := sent[0].Issue.Cause; cause == nil || cause.Status != string(suspect.StatusStartUnknown) || cause.Text != "existed when first read" {
				t.Fatalf("opening cause = %+v", cause)
			}
			if cause := sent[1].Issue.Cause; !cause.Found() || cause.Change != "before-regress" || cause.Text != "Bucket policy changed by deployer, 5 min before" ||
				!cause.Checked.Equal(regressAt) {
				t.Fatalf("regress cause = %+v", cause)
			}
		})
	}
}
