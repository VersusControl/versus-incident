package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	awstools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/aws"
	commontools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools/common"
	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func infrastructureViewer() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true,
		Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

// awsChangeList stores one ECS release 30 minutes ago and covers the
// Region and the global stream from coverFrom up to the settled tail.
func awsChangeList(t *testing.T, coverFrom time.Time) awstools.ChangesFunc {
	t.Helper()
	now := time.Now().UTC()
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope())
	release, ok := changes.Normalize("111122223333", "us-east-1", changes.Event{ID: "evt-release", Name: "UpdateService", Source: "ecs.amazonaws.com",
		Time: now.Add(-30 * time.Minute),
		Raw: `{"userIdentity":{"type":"AssumedRole","arn":"arn:aws:sts::111122223333:assumed-role/github-actions-deploy/run-1",` +
			`"sessionContext":{"sessionIssuer":{"arn":"arn:aws:iam::111122223333:role/github-actions-deploy","userName":"github-actions-deploy"}}},` +
			`"requestParameters":{"cluster":"prod","service":"checkout","taskDefinition":"checkout:42"}}`})
	if !ok {
		t.Fatal("release dropped")
	}
	covered := []changes.Interval{{From: coverFrom, To: now.Add(-15 * time.Minute)}}
	for region, found := range map[string][]model.Change{"us-east-1": {release}, model.GlobalRegion: nil} {
		poll := snapshot.ChangePoll{Found: found, Covered: covered, Read: []changes.Interval{{From: coverFrom, To: now}}, HistoryFrom: coverFrom}
		if err := store.RecordChangePoll("111122223333", region, poll); err != nil {
			t.Fatal(err)
		}
	}
	feed := snapshot.NewChangeFeed(store)
	return func() awstools.ChangeAnswerer { return feed }
}

// TestRecentChangesReadsAWSWithoutGitHub: with no Git repository
// configured, recent_changes still returns the recorded AWS changes, by
// the same masked actor the Changes page shows.
func TestRecentChangesReadsAWSWithoutGitHub(t *testing.T) {
	feed := mergeChangeFeeds(commontools.NewGitChangeFeed(nil), nil, nil, newAWSChangeFeed(awsChangeList(t, time.Now().Add(-48*time.Hour))))
	if feed == nil {
		t.Fatal("no change feed without GitHub")
	}
	result, err := commontools.RecentChanges{Feed: feed}.Invoke(infrastructureViewer(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !result.Found || !strings.Contains(string(encoded), "aws-change:evt-release") ||
		!strings.Contains(string(encoded), "github-actions-deploy: Service checkout updated to task definition checkout:42") {
		t.Fatalf("recent_changes = %s", encoded)
	}
	// A caller without infrastructure:view reads no AWS change.
	records, err := newAWSChangeFeed(awsChangeList(t, time.Now().Add(-48*time.Hour))).Changes(context.Background(), time.Now().Add(-time.Hour))
	if err != nil || len(records) != 0 {
		t.Fatalf("unauthorized records = %+v %v", records, err)
	}
}

// TestRecentChangesNamesUnreadAWSStretches: a stretch that was not read
// is listed as unknown, so an empty AWS answer is never "nothing changed".
func TestRecentChangesNamesUnreadAWSStretches(t *testing.T) {
	records, err := newAWSChangeFeed(awsChangeList(t, time.Now().Add(-40*time.Minute))).Changes(infrastructureViewer(), time.Now().Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	unknown := 0
	for _, record := range records {
		if record.Kind == awsGapKind {
			unknown++
			if !strings.HasPrefix(record.Summary, "AWS changes unknown in 111122223333") {
				t.Fatalf("gap record = %+v", record)
			}
		}
	}
	if unknown == 0 {
		t.Fatalf("no unknown stretch named: %+v", records)
	}
}

// fakeAnswerer answers every page with one change and the given cursor and
// truncation, or fails.
type fakeAnswerer struct {
	err       error
	next      bool
	truncated bool
}

func (f fakeAnswerer) Answer(context.Context, snapshot.FeedRequest) (snapshot.ChangesAnswer, error) {
	if f.err != nil {
		return snapshot.ChangesAnswer{}, f.err
	}
	answer := snapshot.ChangesAnswer{Items: []snapshot.FeedItem{{Kind: snapshot.ItemChange, ID: "evt", Time: time.Now().Add(-time.Minute),
		Service: "ecs", Actor: "deploy", Summary: "Service checkout updated"}}}
	answer.Summary.Truncated = f.truncated
	if f.next {
		cursor := "next"
		answer.NextCursor = &cursor
	}
	return answer, nil
}

// gitOnly is a source that always answers, standing in for Git.
type gitOnly struct{}

func (gitOnly) Changes(context.Context, time.Time) ([]commontools.ChangeRecord, error) {
	return []commontools.ChangeRecord{{Timestamp: time.Now().Add(-time.Minute), Service: "checkout", Kind: "commit", Summary: "Bump"}}, nil
}

// TestRecentChangesNeverHidesAnUnreadAWSHistory: an AWS read that fails
// while Git answers, and an AWS list cut short by the page cap or the read
// cap, each show as unknown AWS changes, never as "no AWS changes".
func TestRecentChangesNeverHidesAnUnreadAWSHistory(t *testing.T) {
	for name, answerer := range map[string]fakeAnswerer{
		"read fails":   {err: errors.New("storage down")},
		"page cap hit": {next: true},
		"read cap hit": {truncated: true},
	} {
		t.Run(name, func(t *testing.T) {
			aws := newAWSChangeFeed(func() awstools.ChangeAnswerer { return answerer })
			result, err := commontools.RecentChanges{Feed: mergeChangeFeeds(gitOnly{}, aws)}.Invoke(infrastructureViewer(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result)
			if !strings.Contains(string(encoded), awsGapKind) || !strings.Contains(string(encoded), "AWS changes unknown") {
				t.Fatalf("unread AWS history hidden: %s", encoded)
			}
		})
	}
}

// TestChangeFeedCapabilityNeedsTheAWSConnector: a bound change list does
// not configure the change_feed capability, or add an AWS source to
// recent_changes, while the AWS connector is off.
func TestChangeFeedCapabilityNeedsTheAWSConnector(t *testing.T) {
	ConfigureAWSChanges(awsChangeList(t, time.Now().Add(-48*time.Hour)))
	t.Cleanup(func() { ConfigureAWSChanges(nil) })
	off := configuredToolAvailabilitySnapshot(config.AgentConfig{}, nil, config.ConnectorsConfig{})
	if off.Capabilities["change_feed"].Configured || awsChangeSource(config.ConnectorsConfig{}) != nil {
		t.Fatalf("change_feed configured with AWS off: %+v", off.Capabilities["change_feed"])
	}
	on := config.ConnectorsConfig{AWS: config.AWSConnectorConfig{Enable: true}}
	if !configuredToolAvailabilitySnapshot(config.AgentConfig{}, nil, on).Capabilities["change_feed"].Configured || awsChangeSource(on) == nil {
		t.Fatal("change_feed not configured with AWS on and a change list bound")
	}
}
