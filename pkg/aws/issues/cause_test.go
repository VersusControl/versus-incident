package issues

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The likely-cause lookup reads the snapshot store's inventory, changes
// and change history: a change on a one-hop neighbour before the issue
// started is found; an issue older than the stored history is "older than
// our change history"; one that existed when first read has no window.
func TestCauseFinderOverTheSnapshotStore(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope(), snapshot.WithClock(func() time.Time { return now }))
	bucket := "arn:aws:s3:::docs-site"
	distribution := "arn:aws:cloudfront::" + testAccount + ":distribution/E123"
	if _, err := store.Commit(model.Unit{Account: testAccount, Region: model.GlobalRegion, Service: "s3"}, snapshot.Candidate{Resources: []model.Resource{
		{ARN: bucket, Service: "s3", Type: "bucket", Account: testAccount, Region: "eu-west-1", Name: "docs-site"},
		{ARN: distribution, Service: "cloudfront", Type: "distribution", Account: testAccount, Region: model.GlobalRegion, Name: "E123",
			Relations: []model.Relation{{Type: model.RelationOrigin, Target: bucket}}},
	}}); err != nil {
		t.Fatal(err)
	}
	change := model.Change{ID: "origin-change", Time: now.Add(-4 * time.Hour), Account: testAccount, Region: model.GlobalRegion,
		Service: "cloudfront", EventName: "UpdateDistribution", Summary: "Distribution origin changed", Resources: []string{distribution},
		Actor: model.Actor{Class: model.ActorPerson, PrincipalARN: "arn:aws:iam::" + testAccount + ":user/dana"}}
	for _, region := range []string{"eu-west-1", model.GlobalRegion} {
		poll := snapshot.ChangePoll{HistoryFrom: now.Add(-48 * time.Hour),
			Covered: []changes.Interval{{From: now.Add(-26 * time.Hour), To: now.Add(-15 * time.Minute)}},
			Read:    []changes.Interval{{From: now.Add(-26 * time.Hour), To: now}}}
		if region == model.GlobalRegion {
			poll.Found = []model.Change{change}
		}
		if err := store.RecordChangePoll(testAccount, region, poll); err != nil {
			t.Fatal(err)
		}
	}
	finder := NewCauseFinder(store, func() time.Duration { return 14 * 24 * time.Hour }, func() time.Time { return now })
	issue := Issue{ID: IssueID(testRuleID, bucket), Account: testAccount, Region: "eu-west-1", Owner: bucket, Since: now.Add(-2 * time.Hour), SinceKnown: true}
	for _, tc := range []struct {
		name   string
		since  time.Time
		known  bool
		status string
		text   string
	}{
		{"found", now.Add(-2 * time.Hour), true, "found", "Distribution origin changed by dana, 2 h before"},
		{"older than the history", now.Add(-30 * time.Hour), true, "incomplete", "older than our change history"},
		{"existed when first read", time.Time{}, false, "start_unknown", "existed when first read"},
		{"none since the history began", now.Add(-5 * time.Hour), true, "none", "no change found since our change history began on 2026-11-24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue.Since, issue.SinceKnown = tc.since, tc.known
			cause, err := finder(context.Background(), issue)
			if err != nil {
				t.Fatal(err)
			}
			if cause.Status != tc.status || cause.Text != tc.text || !cause.Checked.Equal(now) || cause.Found() != (tc.status == "found") {
				t.Fatalf("cause = %+v", cause)
			}
			if cause.Found() && (cause.Change != "origin-change" || cause.Actor == "") {
				t.Fatalf("found cause = %+v", cause)
			}
		})
	}
}
