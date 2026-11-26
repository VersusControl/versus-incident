package issues

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Confirm is decided where an issue lives. An eu-west-1 bucket resolves
// on a complete eu-west-1 read while us-east-1 is unreadable, and the
// us-east-1 bucket can't be confirmed. An account-wide IAM issue can't be
// confirmed while IAM is not read or capped, opens nothing new then, and
// resolves once IAM is read in full, whatever a Region of another service
// reads.
func TestConfirmFollowsTheIssuesOwnInputs(t *testing.T) {
	buckets := bucketRule()
	iam := &scriptedRule{id: "flow-iam-user-key", inputs: []Input{{Service: "iam"}}}
	source := newScriptedSource(flowAccount)
	source.read(flowAccount, "s3", "eu-west-1", ReadOK)
	source.read(flowAccount, "s3", "us-east-1", ReadOK)
	source.read(flowAccount, "iam", model.GlobalRegion, ReadOK)
	f := newFlow(t, storage.NewMemory(), newScope("confirm"), source, nil, buckets, iam)
	euBucket, usBucket := "arn:aws:s3:::confirm-eu", "arn:aws:s3:::confirm-us"
	ciUser, newUser := "arn:aws:iam::"+flowAccount+":user/ci", "arn:aws:iam::"+flowAccount+":user/new"
	keyFinding := func(owner string) Finding {
		return Finding{Owner: owner, Severity: SeverityHigh, Title: "Access key older than 90 days", Material: Material{"used_by": "ci"}}
	}
	buckets.set(publicFinding(euBucket, "eu-west-1", SeverityHigh, "*", "1"), publicFinding(usBucket, "us-east-1", SeverityHigh, "*", "1"))
	iam.set(keyFinding(ciUser))
	f.evaluate()
	eu, us, ci := IssueID(buckets.id, euBucket), IssueID(buckets.id, usBucket), IssueID(iam.id, ciUser)

	// us-east-1 and IAM become unreadable; every finding is gone, and IAM
	// shows a user it could not have read.
	source.read(flowAccount, "s3", "us-east-1", ReadNotRead)
	source.read(flowAccount, "iam", model.GlobalRegion, ReadNotRead)
	buckets.set()
	iam.set(keyFinding(newUser))
	f.clock.Add(time.Minute)
	f.evaluate()
	if issue, _ := f.get(eu); issue.State != StateResolved || issue.Confirm != Confirmed {
		t.Fatalf("eu-west-1 issue while us-east-1 is unreadable: %+v", issue)
	}
	if issue, _ := f.get(us); issue.State != StateOpen || issue.Confirm != CantConfirm {
		t.Fatalf("us-east-1 issue: %+v", issue)
	}
	issue, events := f.get(ci)
	if issue.State != StateOpen || issue.Confirm != CantConfirm || !strings.Contains(events[len(events)-1].Detail, "iam not read") {
		t.Fatalf("IAM issue while IAM is unreadable: %+v, %+v", issue, events)
	}
	if list := f.list(storage.AWSIssueQuery{}); slices.ContainsFunc(list, func(issue Issue) bool { return issue.Owner == newUser }) {
		t.Fatal("an IAM issue opened while IAM was not read")
	}

	// A capped IAM read still can't confirm the gone finding.
	source.read(flowAccount, "iam", model.GlobalRegion, ReadCapped)
	iam.set()
	f.clock.Add(time.Minute)
	f.evaluate()
	if issue, _ := f.get(ci); issue.State != StateOpen || issue.Confirm != CantConfirm {
		t.Fatalf("IAM issue on a capped read: %+v", issue)
	}

	// IAM read in full: the IAM issue resolves although us-east-1 s3 is
	// still not read.
	source.read(flowAccount, "iam", model.GlobalRegion, ReadOK)
	f.clock.Add(time.Minute)
	f.evaluate()
	if issue, _ := f.get(ci); issue.State != StateResolved {
		t.Fatalf("IAM issue on a complete IAM read: %+v", issue)
	}
	if issue, _ := f.get(us); issue.State != StateOpen || issue.Confirm != CantConfirm {
		t.Fatalf("us-east-1 issue after IAM resolved: %+v", issue)
	}
}

// An account removed from the connector has its open issues resolved as
// no longer connected, once the settings were read. A settings read error
// changes no issue, version or event. Its rejected issue stays rejected
// with no event, and after the account is connected again with the same
// findings there is no new issue and the rejected issue raises nothing.
// Another connected account is untouched throughout.
func TestDisconnectThenReconnectKeepsRejections(t *testing.T) {
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			const other = "123400000042"
			rule := bucketRule()
			source := newScriptedSource(flowAccount, other)
			source.read(flowAccount, "s3", "eu-west-1", ReadOK)
			source.read(other, "s3", "eu-west-1", ReadOK)
			var connected []string
			var connectedErr error
			f := newFlow(t, backend.provider, newScope("reconnect"), source,
				[]Option{WithConnectedAccounts(func(context.Context) ([]ConnectedAccount, error) { return connectedOnly(connected), connectedErr })}, rule)
			// Owners that are not ARNs are scoped to each account, so the
			// same findings on both accounts are separate issues.
			rule.set(publicFinding("docs", "eu-west-1", SeverityHigh, "*", "1"), publicFinding("uploads", "eu-west-1", SeverityHigh, "*", "1"))
			connected = []string{flowAccount, other}
			f.evaluate()
			docs, uploads := IssueID(rule.id, OwnerKey(flowAccount, "docs")), IssueID(rule.id, OwnerKey(flowAccount, "uploads"))
			if len(f.list(storage.AWSIssueQuery{AccountID: other})) != 2 {
				t.Fatal("the other account has no issues to watch")
			}
			if _, err := f.actions.Reject(f.ctx, docs, "ana@example.com", "public on purpose: docs site", ""); err != nil {
				t.Fatal(err)
			}
			all := func() map[string]issueMark {
				out := map[string]issueMark{}
				for _, issue := range f.list(storage.AWSIssueQuery{}) {
					_, events := f.get(issue.ID)
					out[issue.ID] = issueMark{issue.State, issue.Version, len(events)}
				}
				return out
			}
			before := all()

			for _, failure := range []error{errors.New("settings store closed"), ErrConnectedUnknown} {
				connected, connectedErr = nil, failure
				f.clock.Add(time.Minute)
				_, _ = f.evaluator.Evaluate(f.ctx)
				if after := all(); !mapsEqual(before, after) {
					t.Fatalf("settings failure %v changed issues: %+v -> %+v", failure, before, after)
				}
			}

			connected, connectedErr = []string{other}, nil
			f.clock.Add(time.Minute)
			f.evaluate()
			issue, events := f.get(uploads)
			if issue.State != StateResolved || events[len(events)-1].Detail != "account no longer connected" {
				t.Fatalf("open issue of a removed account: %+v, %+v", issue, events)
			}
			rejected, rejectedEvents := f.get(docs)
			if rejected.State != StateRejected || rejected.Version != before[docs].version || len(rejectedEvents) != before[docs].events {
				t.Fatalf("rejected issue of a removed account: %+v, %+v", rejected, rejectedEvents)
			}

			connected = []string{flowAccount, other}
			for range 3 {
				f.clock.Add(time.Minute)
				f.evaluate()
			}
			after := all()
			if len(after) != len(before) {
				t.Fatalf("reconnect made new issues: %+v -> %+v", before, after)
			}
			rejected, rejectedEvents = f.get(docs)
			if rejected.State != StateRejected || alertWorthy(rejectedEvents[before[docs].events:]) != 0 ||
				len(rejectedEvents) != before[docs].events {
				t.Fatalf("rejected issue after reconnect: %+v, %+v", rejected, rejectedEvents)
			}
			if issue, _ := f.get(uploads); !issue.State.Active() {
				t.Fatalf("open issue after reconnect: %+v", issue)
			}
			for _, issue := range f.list(storage.AWSIssueQuery{AccountID: other}) {
				if mark := before[issue.ID]; issue.State != mark.state || issue.Version != mark.version {
					t.Fatalf("the connected account changed: %+v", issue)
				}
			}
		})
	}
}

func mapsEqual(a, b map[string]issueMark) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
